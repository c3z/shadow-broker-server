//! Minimal async client for [Shadow Broker](https://github.com/c3z/shadow-broker-server):
//! publish, pull, subscribe and health over the three-endpoint wire protocol
//! (`docs/protocol.md`). Messages are opaque strings to the broker; put JSON
//! (or a signed envelope, see `docs/security.md`) in them.
//!
//! ```no_run
//! # async fn demo() -> Result<(), shadow_broker_client::Error> {
//! let client = shadow_broker_client::Client::from_env()?;           // SHADOW_BROKER_CONFIG
//! let id = client.publish("jobs.control", r#"{"cmd":"stop"}"#, Some("control")).await?;
//! let (msgs, last) = client.pull("jobs.control", 0, 100).await?;
//! client.subscribe("jobs.control", |m| println!("{} {}", m.id, m.message)).await?;
//! # Ok(()) }
//! ```
//!
//! The broker never replays history to a WebSocket subscriber: keep the last
//! id you processed and `pull` after a reconnect if you need gapless delivery.

use base64::Engine;
use futures_util::{SinkExt, StreamExt};
use serde::Deserialize;
use std::time::Duration;
use tokio_tungstenite::tungstenite::Message as WsMessage;

#[derive(Debug)]
pub enum Error {
    Config(String),
    Http(reqwest::Error),
    Status(u16, String),
    Ws(tokio_tungstenite::tungstenite::Error),
    Protocol(String),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::Config(s) => write!(f, "config: {s}"),
            Error::Http(e) => write!(f, "http: {e}"),
            Error::Status(c, b) => write!(f, "broker returned {c}: {b}"),
            Error::Ws(e) => write!(f, "websocket: {e}"),
            Error::Protocol(s) => write!(f, "protocol: {s}"),
        }
    }
}
impl std::error::Error for Error {}
impl From<reqwest::Error> for Error {
    fn from(e: reqwest::Error) -> Self {
        Error::Http(e)
    }
}
impl From<tokio_tungstenite::tungstenite::Error> for Error {
    fn from(e: tokio_tungstenite::tungstenite::Error) -> Self {
        Error::Ws(e)
    }
}

/// Client-side part of `SHADOW_BROKER_CONFIG`.
#[derive(Clone, Debug, Deserialize)]
pub struct Config {
    pub url: String,
    pub auth_token: String,
}

impl Config {
    /// Decode `SHADOW_BROKER_CONFIG` (base64 JSON). Server-only fields are ignored.
    pub fn from_env() -> Result<Self, Error> {
        let raw = std::env::var("SHADOW_BROKER_CONFIG")
            .map_err(|_| Error::Config("SHADOW_BROKER_CONFIG is not set".into()))?;
        let bytes = base64::engine::general_purpose::STANDARD
            .decode(raw.trim())
            .map_err(|e| Error::Config(format!("not base64: {e}")))?;
        let cfg: Config =
            serde_json::from_slice(&bytes).map_err(|e| Error::Config(format!("not JSON: {e}")))?;
        if cfg.url.is_empty() || cfg.auth_token.is_empty() {
            return Err(Error::Config("url and auth_token are required".into()));
        }
        Ok(cfg)
    }
}

/// One stored or streamed message.
#[derive(Clone, Debug, Deserialize)]
pub struct Message {
    pub id: i64,
    pub message: String,
    /// RFC 3339, present on `pull` only.
    #[serde(default)]
    pub created_at: Option<String>,
}

#[derive(Deserialize)]
struct PullResponse {
    #[serde(default)]
    messages: Vec<Message>,
    #[serde(default)]
    last_id: i64,
}

#[derive(Deserialize)]
struct WsEvent {
    #[serde(rename = "type")]
    kind: String,
    #[serde(default)]
    id: i64,
    #[serde(default)]
    message: String,
}

/// Talks to one broker. Cheap to clone.
#[derive(Clone)]
pub struct Client {
    cfg: Config,
    http: reqwest::Client,
}

impl Client {
    pub fn new(cfg: Config) -> Result<Self, Error> {
        if !(cfg.url.starts_with("http://") || cfg.url.starts_with("https://")) {
            return Err(Error::Config(format!("invalid broker url {}", cfg.url)));
        }
        let http = reqwest::Client::builder()
            .timeout(Duration::from_secs(15))
            .build()?;
        Ok(Client {
            cfg: Config {
                url: cfg.url.trim_end_matches('/').to_string(),
                ..cfg
            },
            http,
        })
    }

    pub fn from_env() -> Result<Self, Error> {
        Self::new(Config::from_env()?)
    }

    /// Store `message` in `queue`; returns its id. `lane = Some("control")`
    /// sets `X-Broker-Lane` (separate rate-limit budget); dispatch priority
    /// comes from the queue name ending in `.control`.
    pub async fn publish(
        &self,
        queue: &str,
        message: &str,
        lane: Option<&str>,
    ) -> Result<i64, Error> {
        if queue.is_empty() || message.is_empty() {
            return Err(Error::Protocol(
                "queue and message must be non-empty".into(),
            ));
        }
        let mut req = self
            .http
            .post(format!("{}/publish", self.cfg.url))
            .header("X-Auth", &self.cfg.auth_token)
            .json(&serde_json::json!({ "queue": queue, "message": message }));
        if let Some(l) = lane {
            req = req.header("X-Broker-Lane", l);
        }
        let resp = req.send().await?;
        let status = resp.status().as_u16();
        let body = resp.text().await?;
        if status != 200 {
            return Err(Error::Status(status, body.trim().to_string()));
        }
        let v: serde_json::Value = serde_json::from_str(&body)
            .map_err(|e| Error::Protocol(format!("publish response: {e}")))?;
        v["id"]
            .as_i64()
            .filter(|id| *id > 0)
            .ok_or_else(|| Error::Protocol("publish: missing id".into()))
    }

    /// Stored messages with `id > since`, oldest first, plus the next `since`.
    pub async fn pull(
        &self,
        queue: &str,
        since: i64,
        limit: usize,
    ) -> Result<(Vec<Message>, i64), Error> {
        let resp = self
            .http
            .get(format!("{}/pull", self.cfg.url))
            .header("X-Auth", &self.cfg.auth_token)
            .query(&[
                ("queue", queue),
                ("since", &since.to_string()),
                ("limit", &limit.to_string()),
            ])
            .send()
            .await?;
        let status = resp.status().as_u16();
        let body = resp.text().await?;
        if status != 200 {
            return Err(Error::Status(status, body.trim().to_string()));
        }
        let p: PullResponse = serde_json::from_str(&body)
            .map_err(|e| Error::Protocol(format!("pull response: {e}")))?;
        let last = if p.last_id == 0 { since } else { p.last_id };
        Ok((p.messages, last))
    }

    /// Unauthenticated liveness probe (`GET /health`).
    pub async fn health(&self) -> Result<(), Error> {
        let resp = self
            .http
            .get(format!("{}/health", self.cfg.url))
            .send()
            .await?;
        let status = resp.status().as_u16();
        if status != 200 {
            return Err(Error::Status(status, resp.text().await.unwrap_or_default()));
        }
        Ok(())
    }

    /// Stream new messages for `queue` to `on_message` until the connection
    /// drops; returns the error that ended it. Reconnect policy is the
    /// caller's (loop with a backoff). The broker pings every 15 s; a 60 s
    /// silence ends the connection.
    pub async fn subscribe<F: FnMut(Message)>(
        &self,
        queue: &str,
        mut on_message: F,
    ) -> Result<(), Error> {
        let ws_url = format!(
            "{}/ws?queue={}",
            self.cfg.url.replacen("http", "ws", 1),
            urlencode(queue)
        );
        let (mut ws, _) = tokio_tungstenite::connect_async(ws_url.as_str()).await?;
        ws.send(WsMessage::Text(
            serde_json::json!({ "token": self.cfg.auth_token })
                .to_string()
                .into(),
        ))
        .await?;
        loop {
            let next = tokio::time::timeout(Duration::from_secs(60), ws.next()).await;
            let frame = match next {
                Err(_) => return Err(Error::Protocol("no frame for 60 s".into())),
                Ok(None) => return Err(Error::Protocol("connection closed".into())),
                Ok(Some(f)) => f?,
            };
            match frame {
                WsMessage::Text(t) => {
                    if let Ok(ev) = serde_json::from_str::<WsEvent>(t.as_str()) {
                        if ev.kind == "message" {
                            on_message(Message {
                                id: ev.id,
                                message: ev.message,
                                created_at: None,
                            });
                        }
                    }
                }
                WsMessage::Ping(p) => ws.send(WsMessage::Pong(p)).await?,
                WsMessage::Close(_) => return Err(Error::Protocol("closed by broker".into())),
                _ => {}
            }
        }
    }
}

fn urlencode(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(b as char)
            }
            _ => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}
