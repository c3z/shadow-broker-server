# Architecture

Shadow Broker is a minimal, self-hostable, encrypted-at-rest pub/sub message broker designed for **agent-to-agent communication**. One Go server, embedded SQLite by default (zero external dependencies), thin clients in four languages.

## Components

```
 publishers ──POST /publish──▶ ┌──────────────┐   INSERT (AES-256-GCM)   ┌─────────────────┐
 (agents, CLI, scripts)       │              │ ───────────────────────▶ │ embedded SQLite │
                              │  Go server   │        wake ─┐           │ (WAL)           │
 subscribers ◀──WS /ws─────── │ main.go+db.go│ ◀── dispatch │ ───────── │ or Turso (opt.) │
 (agents, CLI, proxy)         │              │   id > globalMaxID        └─────────────────┘
                              └──────────────┘   (on publish, 1 s tick as fallback)
                                     ▲
 readers ────────GET /pull───────────┘   (catch-up / pagination, decrypted)
 monitors ───────GET /health─────────┘   (no auth; process + store alive)
```

- **Server (`main.go` + `db.go`)** — small Go service. Endpoints: `POST /publish`, `GET /pull`, `GET /ws`, `GET /health`. Middleware: permissive CORS, per-IP rate limiting (500 req/10 s per lane, janitor evicts idle IPs, `TRUST_PROXY=1` reads `X-Forwarded-For`). Listens on `BIND_HOST:PORT` (`127.0.0.1:8080` by default, `0.0.0.0` in the Docker images).
- **Storage (`db.go`)** — a `messageStore` interface with one shared `database/sql` implementation behind two constructors:
  - **local** (default): embedded SQLite via `modernc.org/sqlite` (pure Go, no cgo), WAL mode, `busy_timeout`, file at `file:./data/broker.db` unless `db_url` says otherwise. Selected when `db_url` is empty or starts with `file:`.
  - **remote** (optional): Turso/libSQL, selected when `db_url` starts with `libsql://` or `https://`. Inserts retry with backoff; local inserts don't need to.
  - One table, `messages(id, queue, nonce, ciphertext, created_at)` ([db.sql](../db.sql)), auto-created at startup (`CREATE TABLE IF NOT EXISTS`). The server encrypts every message with AES-256-GCM (random nonce per message) before INSERT, so the storage layer never holds plaintext.
- **Dispatcher** — one goroutine that fetches `id > globalMaxID`, decrypts new rows and hands them to the subscribers of each queue. It runs when a publish wakes it (non-blocking signal channel) and, as a safety net, on a 1 s tick. Queues ending in `.control` are dispatched before the others in the same batch; within a queue, order is by id. On startup `globalMaxID` is initialized to the current `MAX(id)` — **history is never replayed to subscribers after a restart**; offline catch-up goes through `/pull`.
- **Subscriber writers** — every WebSocket connection owns a bounded outbound queue (64 messages) drained by its own goroutine with a 1 s write deadline per frame and a protocol-level ping every 15 s. The dispatcher only enqueues; it never blocks on the network. A subscriber whose queue is full is closed (it can `/pull` the gap) so that one slow reader cannot delay the rest.
- **Webhook proxy (`proxy/`)** — optional Node/Express + Socket.IO service that subscribes to configured queues and forwards messages to HTTP webhooks (e.g. n8n). Has a small web UI and admin API protected by `admin_token`.
- **CLI (`sb`)** — the primary interface (`sb-cli-go/`): `send` (with `--lane`), `pull`, `subscribe`, `ping`, `keygen`, `test`, `tui`, `version`, with Ed25519 signing/verification and a `--json` machine-readable mode for agents.
- **Client libraries** — `client-go/` (package), `client-rs/` (crate), `client-js/` (Node ESM + `.d.ts`), `client-py/` (Python). Same surface everywhere: publish / pull / subscribe / health, lanes, live round-trip test. See [clients.md](clients.md).
- **Agent skill (`skill/`)** — a drop-in [agent skill](../skill/SKILL.md) that teaches AI agents how to communicate over the broker.

## Design decisions (and why)

**Embedded by default, pluggable when needed.** A single-node broker doesn't need a network database — embedded SQLite removes latency, cost, credentials, and the external point of failure. The `messageStore` seam keeps Turso available for operators who want a managed DB; both backends share the same queries.

**Database as the transport.** Messages are persisted first and delivered second. Publishers never wait for subscribers; subscribers get durability and catch-up (`/pull`) for free. Since the QoS change the cost is a wake-up and one query (tens of milliseconds end to end on a shared VM), with the 1 s tick only as a fallback — fine for agent messaging and interactive control, still wrong for anything that needs hard real-time.

**Control before content, without a second system.** Instead of a priority queue or a separate channel, QoS is three small rules on the existing path: wake on publish, sort `.control` queues first in a batch, give each subscriber its own buffer. Plus one header (`X-Broker-Lane`) so control traffic has its own rate-limit budget. Measured with a content flood on the same host: median control delivery 912 → 62 ms. It is best-effort priority — CPU, SQLite and the link are still shared.

**Server-side encryption, not E2E.** AES-256-GCM protects data at rest, keeps clients trivial (no key management), and lets the server deliver plaintext. Identity and integrity are instead guaranteed by **client-side Ed25519 signatures** ([security.md](security.md)), which survive an untrusted server. This split is deliberate: confidentiality is operational, *verifiability* is end-to-end.

**Single static auth token.** Deliberately simple: the broker is meant to be *your* infrastructure for *your* agents, not a multi-tenant SaaS. Per-queue ACLs are out of scope.

**No replay after restart, no handshake ack.** The dispatcher starts from `MAX(id)`, so WebSocket subscribers only ever see messages published while connected — and only after the server has processed their token, which it does not acknowledge. Combined with `/pull?since=`, each client controls its own cursor — the server holds no per-subscriber state. Clients that need gapless delivery pull from their last id after every (re)connect.

## Concurrency model

- One goroutine per WebSocket connection reads (auth handshake within 5 s, 4 KiB frame limit, application-level heartbeat); a second one writes from the connection's bounded queue (gorilla/websocket forbids concurrent writes).
- The dispatcher snapshots the subscriber set under `clientsMu`, then enqueues outside the lock. Enqueue never blocks: a full queue closes that subscriber.
- `pollMu` serializes dispatcher runs; `wakePoller` is a 1-slot channel, so bursts of publishes coalesce into one run.
- Rate-limit state is a map of recent timestamps per `<ip>` or `<ip>:control`, evicted by a janitor.
- Local SQLite runs with `SetMaxOpenConns(1)` (single-writer discipline, WAL for read concurrency); remote inserts retry with backoff.

## Repository layout

```
main.go, db.go     broker server (HTTP/WS + storage layer)
qos_test.go        dispatch order and slow-subscriber behaviour (real SQLite/HTTP/WS)
db.sql             schema reference (auto-applied at startup)
Dockerfile, docker-compose.yml, Caddyfile, deploy/bootstrap.sh   self-hosting stack with Caddy
deploy/exedev/     SSH deploy to a Docker host: cross-compile, runtime image, swap + rollback
fly.toml           optional Fly.io deployment
proxy/             webhook proxy (Node/Express/Socket.IO + UI)
sb-cli-go/         the `sb` CLI — signing, verification, --json, test, tui
client-go/ client-rs/ client-js/ client-py/   client libraries + round-trip tests
skill/             agent skill (SKILL.md)
docs/              this documentation
genconfig.sh       SHADOW_BROKER_CONFIG generator
```

## Scaling notes

The current shape comfortably handles agent-scale traffic (hundreds of msg/s — see `client-js/testhard.js`). Known limits, by design: single-node broadcast (no cross-instance fanout), unbounded table growth (add retention if you need it), full-table scans per queue on `/pull` (add an index on `(queue, id)` if history grows large), a 16 MiB publish limit that has to arrive within 15 s (bulk bytes belong on an object store with a reference in the message). Embedded SQLite is not the bottleneck at this scale — writes are single-digit microseconds locally.
