# Wire Protocol

Shadow Broker exposes three HTTP endpoints on the server (default port `8080`). All requests use JSON. The full message flow is: `publish → encrypted store (Turso) → poller → WebSocket broadcast`, with `/pull` for history catch-up.

## Authentication

All endpoints require the shared auth token:

- **HTTP** (`/publish`, `/pull`): header `X-Auth: <auth_token>`
- **WebSocket** (`/ws`): the first message after the upgrade must be `{"token": "<auth_token>"}`

A single static token protects the whole broker — there are no per-queue permissions. See [security.md](security.md) for the threat model and message-level signatures.

## `POST /publish`

Publishes a message to a queue. The server encrypts the message with AES-256-GCM before storing it.

Request:

```http
POST /publish
X-Auth: <auth_token>
Content-Type: application/json

{"queue": "noti", "message": "hello world"}
```

Response `200 OK`:

```json
{"id": 123}
```

`id` is the monotonic row id used for pagination and catch-up. Errors: `401` bad token, `400` invalid body, `429` rate limited.

## `GET /pull`

Returns stored messages for a queue, decrypted, oldest first. Used for catch-up and pagination.

```
GET /pull?queue=noti&since=120&limit=50
X-Auth: <auth_token>
```

- `queue` (required) — queue name
- `since` (optional, default `0`) — return only messages with `id > since`
- `limit` (optional, server-capped) — max messages per call

Response `200 OK`:

```json
{
  "messages": [
    {"id": 121, "message": "hello world", "created_at": "2026-07-26T05:00:00Z"}
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
2. Client's **first message** must be `{"token": "<auth_token>"}`. On failure the server closes the connection.
3. Server pushes messages as they arrive:

```json
{"type": "message", "id": 123, "message": "hello world"}
```

Keepalive: application-level ping/pong every 30 s. Clients should reconnect with backoff on disconnect (the reference CLIs do this automatically).

Note: after a server restart, only **new** messages are broadcast — the server does not replay history to WebSocket subscribers. Use `/pull` to catch up on messages published while you were offline.

## Rate limiting & CORS

- 500 requests per 10 s per client IP, across all endpoints (WebSocket upgrades included — reconnect loops count against the limit).
- CORS is permissive (`*`), as the API is token-protected and consumed by CLI/server-side clients.

## Message envelopes (optional, client-side)

The `message` field is an opaque string to the server. Clients may put a **signed envelope** in it to make the sender cryptographically verifiable — see [security.md](security.md) for the envelope format and Ed25519 signature spec. The broker neither requires nor validates envelopes; verification is entirely client-side.
