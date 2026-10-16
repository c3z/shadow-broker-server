# Client Libraries

Besides the `sb` CLI there are client libraries in Go, Rust, JavaScript/TypeScript and Python. All speak the same [wire protocol](protocol.md): `X-Auth` header for HTTP, `{"token": ...}` handshake for WebSocket, optional `X-Broker-Lane: control` on publish.

All clients read configuration from the `SHADOW_BROKER_CONFIG` environment variable (base64-encoded JSON). Only `url` and `auth_token` are used client-side. Every library ships a live round-trip test that runs when that variable is set and is skipped otherwise.

Two things every client must handle, whatever the language:

1. **No handshake ack on WebSocket** — after connecting, `pull` from the last id you processed; the broker never replays history on its own.
2. **Reconnect with backoff** — the server disconnects slow subscribers (64-message buffer) and anything idle for too long; reconnect loops count against the rate limit.

## CLI (Go) — `sb-cli-go/`

The reference client and the only one with signature support:

```bash
sb send <queue> <message> [--sign --key agent --from name] [--lane control]
sb subscribe <queue> [--verify-pubkey agent.pub]
sb pull <queue> [since] [limit] [--verify-pubkey agent.pub]
sb ping · sb test [queue] · sb keygen [-out agent] · sb tui · sb version
```

Features: automatic reconnect with backoff, application-level heartbeat, Ed25519 signing/verification, machine-readable `--json` output, `sb test` end-to-end broker self-test, `sb tui` interactive terminal UI. Build: `go build -o sb .` or `./compile.sh` (stamps `sb version` from `git describe`). Binaries are git-ignored.

## Go — `client-go/` (package `shadowbroker`)

```bash
go get github.com/c3z/shadow-broker-server/client-go
```

```go
cfg, _ := shadowbroker.ConfigFromEnv()
c, _ := shadowbroker.New(cfg)

id, _ := c.Publish(ctx, "jobs.control", `{"cmd":"stop"}`, shadowbroker.Lane("control"))
msgs, last, _ := c.Pull(ctx, "jobs.content", 0, 100)
_ = c.Health(ctx)

// blocks until ctx is done; reconnects with backoff; OnConnect is the place to Pull the gap
_ = c.Subscribe(ctx, "jobs.content", func(m shadowbroker.Message) { ... },
	shadowbroker.OnConnect(func() { /* pull since last id */ }))
```

One dependency (`gorilla/websocket`). Test: `SHADOW_BROKER_CONFIG=… go test ./...`.

## Rust — `client-rs/` (crate `shadow-broker-client`)

```toml
[dependencies]
shadow-broker-client = { git = "https://github.com/c3z/shadow-broker-server", subdir = "client-rs" }
```

```rust
let client = shadow_broker_client::Client::from_env()?;
let id = client.publish("jobs.control", r#"{"cmd":"stop"}"#, Some("control")).await?;
let (msgs, last) = client.pull("jobs.content", 0, 100).await?;
client.health().await?;
// returns when the connection drops; wrap in your own reconnect loop
client.subscribe("jobs.content", |m| println!("{} {}", m.id, m.message)).await?;
```

Async (tokio), `reqwest` with rustls, `tokio-tungstenite`. Example: `SHADOW_BROKER_CONFIG=… cargo run --example roundtrip`.

## JavaScript / TypeScript — `client-js/shadowClient.js`

Node ESM (`node-fetch`, `ws`), typings in `shadowClient.d.ts`:

```js
import { ShadowClient } from './shadowClient.js';

const client = new ShadowClient();                       // reads SHADOW_BROKER_CONFIG

await client.publish('jobs.control', '{"cmd":"stop"}', { lane: 'control' });   // → { id: 123 }
const { messages, last_id } = await client.pull('jobs.content', { since: 0, limit: 50 });

await client.subscribe('jobs.content', (msg) => {
  console.log(msg.id, msg.message);                      // live stream, auto-reconnect
});
```

Test suites in the same directory: `testsender.js` / `testreceiver.js` (minimal loops), `testcounting.js` (120 messages, 3 listeners, verifies totals — listeners stay connected, stop it with Ctrl-C once the sums print), `testhard.js` (60 s stress: 4 queues, up to 200 msg/s, latency and ordering stats).

## Python — `client-py/shadow_client.py`

Python (`requests` + `websocket-client`), managed with [uv](https://docs.astral.sh/uv/):

```python
from shadow_client import ShadowClientPy

client = ShadowClientPy()                               # reads SHADOW_BROKER_CONFIG

client.publish('jobs.control', '{"cmd":"stop"}', lane='control')   # → 123
history = client.pull('jobs.content', since=0, limit=50)

client.subscribe('jobs.content', lambda msg: print(msg['id'], msg['message']))
```

Tests mirror the JS suite: `testcounting.py`, `testhard.py` (run via `uv run python testcounting.py`).

## Webhook proxy — `proxy/`

Not a library but a client too: it subscribes to queues and forwards messages to HTTP webhooks, with a web UI for managing channel→webhook mappings. Useful for wiring broker traffic into n8n, Telegram bots, or any HTTP automation without writing code.

## Which client for what

- **AI agents** → `sb` CLI + the [agent skill](../skill/SKILL.md)
- **Services in Go / Rust** → `client-go` / `client-rs`
- **Services / daemons in Node or TypeScript** → `client-js`
- **Scripts / data tooling in Python** → `client-py`
- **No-code automation** → webhook proxy
- **Anything else** → raw [protocol](protocol.md) (three endpoints + health)

## What the libraries do not do

- **Signing** is implemented only in the Go CLI. The envelope spec in [security.md](security.md) is one Ed25519 call per direction; port it when you need verifiable senders from a library.
- **Binary payloads**: `message` is a string. Base64 costs 33 %; the 16 MiB publish limit and the 15 s request read timeout apply to the encoded size. For media, upload the bytes to your own HTTPS endpoint and publish the reference.
- **Delivery guarantees**: no acks, no consumer groups. If two processes subscribe to the same queue, both get every message.
