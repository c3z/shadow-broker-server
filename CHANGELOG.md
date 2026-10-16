# Changelog

## 2026-10-16 — QoS merged, health, deploy kit, four client libraries

Server (`main.go`):
- **QoS dispatch**, ported from the `barber-cut` fork (2026-09-19): publish wakes the dispatcher immediately instead of waiting for the 1 s tick; queues ending in `.control` are dispatched before the rest; every WebSocket subscriber has its own bounded writer (64 messages, 1 s write deadline) so a slow reader is disconnected instead of stalling everyone; `X-Broker-Lane: control` gets a separate rate-limit bucket. Measured on a shared VM with concurrent content traffic: median control latency 912 → 62 ms, p95 920 → 184 ms. Covered by `qos_test.go` (real SQLite/HTTP/WS).
- Publish body capped at 16 MiB; WebSocket token must arrive within 5 s and client frames are capped at 4 KiB; HTTP read-header/read/idle timeouts 5/15/60 s; server-side WebSocket ping every 15 s.
- `BIND_HOST` (default `127.0.0.1`; the Docker images set `0.0.0.0`).
- **`GET /health`** — unauthenticated liveness probe; used by the compose healthcheck, the runtime image's `HEALTHCHECK` and `upgrade.py`.
- **`TRUST_PROXY=1`** — rate limiting and logs use `X-Forwarded-For` / `X-Real-IP` behind a reverse proxy (previously every client behind a proxy shared one bucket).

Deploy:
- `deploy/exedev/` — `deploy.sh` (test → cross-compile → upload → runtime image → swap → external check), `upgrade.py` (container swap with rollback container, encrypted volume backup, health gate, automatic revert), `Dockerfile.runtime`.

Clients:
- `client-go/` — new importable package `shadowbroker` (publish/pull/subscribe/health, lanes, reconnect with backoff, live test).
- `client-rs/` — new crate `shadow-broker-client` (async, same surface, `examples/roundtrip.rs`).
- `client-js/` — `publish(queue, message, { lane })`, TypeScript declarations `shadowClient.d.ts`.
- `client-py/` — `publish(queue, message, lane=None)`.
- `sb` CLI — `sb send --lane control`, `sb version` (stamped by `compile.sh`, which now builds the whole package instead of `main.go` alone — the old script produced a binary that did not compile).

Docs: README, protocol (lanes, limits, health, no-handshake-ack caveat), architecture, deployment (SSH/Docker-host path, env table, rollback, retention), clients, security (keygen file names fixed), skill.

## 2026-07-26 — initial public release

Self-hostable broker with embedded SQLite, AES-256-GCM at rest, Ed25519 signed envelopes, `sb` CLI with TUI and self-test, JS and Python clients, webhook proxy, VPS bootstrap with Caddy, Fly.io config.
