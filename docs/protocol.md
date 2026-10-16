# Wire Protocol

Shadow Broker exposes three authenticated endpoints plus an unauthenticated health probe (default port `8080`). All requests use JSON. The message flow is: `publish → encrypted store (SQLite) → dispatcher → WebSocket broadcast`, with `/pull` for history catch-up.

## Authentication

All endpoints except `/health` require the shared auth token:

- **HTTP** (`/publish`, `/pull`): header `X-Auth: <auth_token>`
- **WebSocket** (`/ws`): the first message after the upgrade must be `{"token": "<auth_token>"}`, within 5 s

A single static token protects the whole broker — there are no per-queue permissions. See [security.md](security.md) for the threat model and message-level signatures.

## `POST /publish`

Publishes a message to a queue. The server encrypts the message with AES-256-GCM before storing it, then wakes the dispatcher immediately.

Request:

```http
POST /publish
X-Auth: <auth_token>
X-Broker-Lane: control          (optional, see Lanes)
Content-Type: application/json

{"queue": "noti", "message": "hello world"}
```

Response `200 OK`:

```json
{"id": 123}
```

`id` is the monotonic row id used for pagination and catch-up. Errors: `401` bad token, `400` invalid body or body over **16 MiB**, `429` rate limited. The whole request, body included, must arrive within 15 s (`ReadTimeout`), so large payloads on slow links will fail — put bulk bytes elsewhere and publish a reference.

`message` is an opaque string: JSON, base64, or a signed envelope — the broker never inspects it.

## `GET /pull`

Returns stored messages for a queue, decrypted, oldest first. Used for catch-up and pagination.

```
GET /pull?queue=noti&since=120&limit=50
X-Auth: <auth_token>
```

- `queue` (required) — queue name
- `since` (optional, default `0`) — return only messages with `id > since`
- `limit` (optional, default `100`, max `1000`) — max messages per call

Response `200 OK`:

```json
{
  "messages": [
    {"id": 121, "message": "hello world", "created_at": "2026-07-26 05:00:00"}
  ],
  "last_id": 121
}
```

Pass `last_id` as the next `since` to page forward. Messages whose stored ciphertext fails to decrypt are skipped (and logged server-side).

## `GET /ws` (WebSocket)

Streams new messages for a queue in real time.

```
GET /ws?queue=noti        (Upgrade: websocket)
```

Handshake:

1. Client opens the WebSocket with query param `queue`.
2. Client's **first message** must be `{"token": "<auth_token>"}` (≤ 4 KiB, within 5 s). On failure the server closes the connection.
3. Server pushes messages as they arrive:

```json
{"type": "message", "id": 123, "message": "hello world"}
```

**No handshake ack.** The server registers the subscriber after it has read and validated the token; there is no "ready" frame. A message published by someone else in that window is stored but not streamed to you. Clients that need gapless delivery track the last id they processed and call `/pull?since=<id>` right after every (re)connect.

Keepalive: the server sends a WebSocket **ping every 15 s**; standard libraries answer with a pong automatically. Application-level `{"type":"ping"}` → `{"type":"pong"}` is also supported for clients that want their own heartbeat. Frames from the client are limited to 4 KiB (it is a subscribe-only channel).

Delivery: each subscriber has its own writer with a **64-message buffer** and a 1 s write deadline per frame. A subscriber that cannot keep up is disconnected (and can `/pull`) instead of stalling everyone else. Reconnect with backoff on disconnect (the reference clients do this automatically).

After a server restart, only **new** messages are broadcast — the server does not replay history to WebSocket subscribers. Use `/pull` to catch up on messages published while you were offline.

## `GET /health`

Unauthenticated liveness probe for load balancers, container healthchecks and monitoring.

```json
{"status":"ok","uptime_s":4242}
```

`200` means the process is up and its store answers a query; `503 {"status":"degraded","store":"error"}` otherwise. It reveals nothing about queues or payloads. A `503` coming from a reverse proxy without this body means the broker is not running at all.

## Lanes and dispatch order (QoS)

Two independent mechanisms:

- **Queue name suffix `.control`** — when the dispatcher wakes with messages for several queues, queues ending in `.control` are broadcast first. Order *within* a queue is always by id. Nothing else is special about the name.
- **Header `X-Broker-Lane: control`** on an authenticated request — the request is counted in a separate rate-limit bucket (`<ip>:control`), so a burst of content publishes from the same host cannot starve its control traffic. Any other value, or no header, is the default bucket.

Convention used by the clients: a logical channel `jobs` becomes two queues, `jobs.control` (state, stop, errors — small, urgent) and `jobs.content` (payloads), with two subscriptions. Publishing is best-effort priority on the broker; CPU, SQLite and the network are still shared.

## Rate limiting & CORS

- 500 requests per 10 s per client IP per lane, across all endpoints (WebSocket upgrades included — reconnect loops count against the limit).
- The client IP is `RemoteAddr`, or with `TRUST_PROXY=1` the first `X-Forwarded-For` hop (then `X-Real-IP`). Enable that only behind a proxy you control; otherwise anyone could pick their bucket.
- CORS is permissive (`*`), as the API is token-protected and consumed by CLI/server-side clients.

## Server limits (defaults)

| What | Value |
|---|---|
| Publish body | 16 MiB |
| HTTP read header / read / idle timeout | 5 s / 15 s / 60 s |
| WebSocket client frame | 4 KiB |
| WebSocket token deadline | 5 s |
| Subscriber buffer | 64 messages, then disconnect |
| Subscriber write deadline | 1 s per frame |
| Server ping interval | 15 s |
| Rate limit | 500 req / 10 s / IP / lane |

## Message envelopes (optional, client-side)

The `message` field is an opaque string to the server. Clients may put a **signed envelope** in it to make the sender cryptographically verifiable — see [security.md](security.md) for the envelope format and Ed25519 signature spec. The broker neither requires nor validates envelopes; verification is entirely client-side.
