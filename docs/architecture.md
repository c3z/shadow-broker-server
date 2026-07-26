# Architecture

Shadow Broker is a minimal, self-hostable, encrypted-at-rest pub/sub message broker designed for **agent-to-agent communication**. One Go server, embedded SQLite by default (zero external dependencies), thin clients in several languages.

## Components

```
 publishers ──POST /publish──▶ ┌──────────────┐   INSERT (AES-256-GCM)   ┌─────────────────┐
 (agents, CLI, scripts)       │              │ ───────────────────────▶ │ embedded SQLite │
                              │  Go server   │                          │ (WAL)           │
 subscribers ◀──WS /ws─────── │ main.go+db.go│ ◀─── poll every 1s ───── │ or Turso (opt.) │
 (agents, CLI, proxy)         │              │     id > globalMaxID     └─────────────────┘
                              └──────────────┘
                                     ▲
 readers ────────GET /pull───────────┘   (catch-up / pagination, decrypted)
```

- **Server (`main.go` + `db.go`)** — small Go service. Endpoints: `POST /publish`, `GET /pull`, `GET /ws`. Middleware: permissive CORS, per-IP rate limiting (500 req/10 s, with a janitor that evicts idle IPs).
- **Storage (`db.go`)** — a `messageStore` interface with one shared `database/sql` implementation behind two constructors:
  - **local** (default): embedded SQLite via `modernc.org/sqlite` (pure Go, no cgo), WAL mode, `busy_timeout`, file at `file:./data/broker.db` unless `db_url` says otherwise. Selected when `db_url` is empty or starts with `file:`.
  - **remote** (optional): Turso/libSQL, selected when `db_url` starts with `libsql://` or `https://`. Inserts retry with backoff; local inserts don't need to.
  - One table, `messages(id, queue, nonce, ciphertext, created_at)` ([db.sql](../db.sql)), auto-created at startup (`CREATE TABLE IF NOT EXISTS`). The server encrypts every message with AES-256-GCM (random nonce per message) before INSERT, so the storage layer never holds plaintext.
- **Poller** — a goroutine that queries `id > globalMaxID` once per second, decrypts new rows, and broadcasts them to WebSocket subscribers of each queue. On startup `globalMaxID` is initialized to the current `MAX(id)` — **history is never replayed to subscribers after a restart**; offline catch-up goes through `/pull`.
- **Webhook proxy (`proxy/`)** — optional Node/Express + Socket.IO service that subscribes to configured queues and forwards messages to HTTP webhooks (e.g. n8n). Has a small web UI and admin API protected by `admin_token`. See [deployment.md](deployment.md).
- **CLI (`sb`)** — the primary interface (`sb-cli-go/`): `send`, `pull`, `subscribe`, `ping`, `keygen`, `test`, `tui`, with Ed25519 signing/verification and a `--json` machine-readable mode for agents.
- **Client libraries** — `client-js/` (Node ESM) and `client-py/` (Python), each with test suites (`testcounting`, `testhard` stress test).
- **Agent skill (`skill/`)** — a drop-in [agent skill](../skill/SKILL.md) that teaches AI agents how to communicate over the broker.

## Design decisions (and why)

**Embedded by default, pluggable when needed.** A single-node broker doesn't need a network database — embedded SQLite removes latency, cost, credentials, and the external point of failure. The `messageStore` seam keeps Turso available for operators who want a managed DB; both backends share the same queries.

**Database as the transport.** Messages are persisted first and delivered second. Publishers never wait for subscribers; subscribers get durability and catch-up (`/pull`) for free. The cost is polling latency (~1 s worst case) — acceptable for agent messaging, wrong for HFT.

**Server-side encryption, not E2E.** AES-256-GCM protects data at rest, keeps clients trivial (no key management), and lets the server deliver plaintext. Identity and integrity are instead guaranteed by **client-side Ed25519 signatures** ([security.md](security.md)), which survive an untrusted server. This split is deliberate: confidentiality is operational, *verifiability* is end-to-end.

**Single static auth token.** Deliberately simple: the broker is meant to be *your* infrastructure for *your* agents, not a multi-tenant SaaS. Per-queue ACLs are out of scope.

**No replay after restart.** The poller starts from `MAX(id)`, so WebSocket subscribers only ever see messages published while connected. Combined with `/pull?since=`, each client controls its own cursor — the server holds no per-subscriber state.

## Concurrency model

- One goroutine per WebSocket connection handles reads (auth handshake, heartbeat).
- All writes to a connection are serialized by a **per-client write mutex** (gorilla/websocket forbids concurrent writes) and bounded by a write deadline; clients that fail a write are dropped.
- Broadcast snapshots the subscriber list under `clientsMu`, then writes outside the lock — one slow client cannot stall the pipeline.
- Local SQLite runs with `SetMaxOpenConns(1)` (single-writer discipline, WAL for read concurrency); remote inserts retry with backoff.

## Repository layout

```
main.go, db.go     broker server (HTTP/WS + storage layer)
db.sql             schema reference (auto-applied at startup)
Dockerfile, docker-compose.yml, Caddyfile, deploy/   self-hosting stack
fly.toml           optional Fly.io deployment
proxy/             webhook proxy (Node/Express/Socket.IO + UI)
sb-cli-go/         the `sb` CLI — signing, verification, --json, test, tui
client-js/, client-py/   client libraries + test suites
skill/             agent skill (SKILL.md)
docs/              this documentation
genconfig.sh       SHADOW_BROKER_CONFIG generator
```

## Scaling notes

The current shape comfortably handles agent-scale traffic (hundreds of msg/s — see `client-js/testhard.js`). Known limits, by design: 1 s poll latency, single-node broadcast (no cross-instance fanout), unbounded table growth (add retention if you need it), full-table scans per queue on `/pull` (add an index on `(queue, id)` if history grows large). Embedded SQLite is not the bottleneck at this scale — writes are single-digit microseconds locally.
