//! Live round trip against the broker in SHADOW_BROKER_CONFIG:
//! health -> subscribe -> publish (control lane) -> receive on WS -> pull.
use shadow_broker_client::Client;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let client = Client::from_env()?;
    client.health().await?;
    println!("health: ok");

    let nanos = SystemTime::now().duration_since(UNIX_EPOCH)?.as_nanos();
    let queue = format!("client-rs.test.{nanos}.control");

    let (tx, mut rx) = tokio::sync::mpsc::channel(8);
    let sub = client.clone();
    let q = queue.clone();
    tokio::spawn(async move {
        let _ = sub
            .subscribe(&q, |m| {
                let _ = tx.try_send(m);
            })
            .await;
    });
    tokio::time::sleep(Duration::from_millis(700)).await; // let the handshake land

    let id = client
        .publish(&queue, r#"{"hello":"rust"}"#, Some("control"))
        .await?;
    println!("published id={id}");

    let got = tokio::time::timeout(Duration::from_secs(5), rx.recv())
        .await?
        .ok_or("stream closed")?;
    assert_eq!(got.id, id, "websocket delivered a different id");
    println!("websocket: id={} message={}", got.id, got.message);

    let (msgs, last) = client.pull(&queue, 0, 10).await?;
    assert_eq!(msgs.len(), 1);
    assert_eq!(last, id);
    println!("pull: {} message(s), last_id={last}", msgs.len());
    println!("ROUND TRIP OK");
    Ok(())
}
