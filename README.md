# Shadow Broker

A minimal, self-hostable, open-source message broker for **agent-to-agent communication** — encryption at rest, cryptographically verifiable sender identity, zero external dependencies.

```
agent A ──publish──▶ broker ──AES-256-GCM──▶ embedded SQLite (sees only ciphertext)
                        │
                        └─dispatch→ decrypt→──WebSocket──▶ agent B  [agent-a ✓]
```

- **Self-hostable in one container**: embedded SQLite — no database to provision, no external services.
- **Durable**: messages persist first, deliver second. Offline clients catch up with `/pull?since=`.
- **Encrypted at rest**: AES-256-GCM per message; the storage layer never holds plaintext.
- **Verifiable**: optional Ed25519 signed envelopes let receivers prove *who* sent a message — without trusting the broker.
- **Control before content (QoS)**: publishes wake the dispatcher immediately (no polling tick on the hot path), queues named `*.control` are dispatched before the rest, every WebSocket subscriber has its own bounded buffer so a slow reader never stalls the others, and `X-Broker-Lane: control` gets its own rate-limit budget. Measured on a shared VM: median control latency 912 → 62 ms with concurrent content traffic.
- **Clients in four languages**: Go package, Rust crate, JavaScript/TypeScript, Python — plus the `sb` CLI with `--json` for agents and `sb tui` for humans.
- **Agent-native**: ships with an [agent skill](skill/SKILL.md) so AI agents can use it out of the box.

## Deploy your own broker

### Docker host over SSH, no toolchain on the server (exe.dev and friends)

```bash
deploy/exedev/deploy.sh user@host          # test → cross-compile → runtime image → swap with rollback → /health
```

Keeps the previous container as `shadow-broker-before-<stamp>` and an encrypted copy of the data volume. TLS is the host's reverse proxy's job (the container binds to `127.0.0.1:8080` on the host). Details: [docs/deployment.md](docs/deployment.md).

### One command, any VPS with its own TLS (Caddy + Let's Encrypt)

Point a DNS A record at your server, then:

```bash
./deploy/bootstrap.sh root@your-server --domain broker.example.com
```

Installs Docker if needed, copies the repo, generates secrets, brings up broker + Caddy, prints your `SHADOW_BROKER_CONFIG`. Re-running it redeploys.

### Docker Compose (manual)

```bash
cp .env.example .env        # set DOMAIN and SHADOW_BROKER_CONFIG
docker compose up -d
```

### Plain Docker (no TLS termination)

```bash
docker build -t shadow-broker .
docker run -d -p 127.0.0.1:8080:8080 -v broker-data:/data \
  -e SHADOW_BROKER_CONFIG='<base64 JSON>' shadow-broker
```

The image listens on all interfaces *inside* the container (`BIND_HOST=0.0.0.0`); restrict exposure on the host side as above. Behind a reverse proxy add `-e TRUST_PROXY=1` so rate limiting sees client IPs, not the proxy.

### Local development

```bash
go run .                    # embedded SQLite in ./data, binds 127.0.0.1:8080, no setup needed
```

### Fly.io

Still supported — see [docs/deployment.md](docs/deployment.md).

## Use it

```bash
cd sb-cli-go && go build -o sb . && export PATH="$PWD:$PATH"
export SHADOW_BROKER_CONFIG='<from the deploy output>'
sb test                     # end-to-end self-test of the broker
sb subscribe demo &  sb send demo "hello"
sb send jobs.control '{"cmd":"stop"}' --lane control   # priority queue + control budget
curl https://broker.example.com/health                 # {"status":"ok","uptime_s":123}
```

Signed, verifiable messaging:

```bash
sb keygen -out agent                            # one time per identity → agent, agent.pub
sb send demo "done" --sign --key agent --from agent-a
sb subscribe demo --verify-pubkey agent.pub     # → [agent-a ✓] done
```

Machine-readable for agents (`--json` on every command), interactive for humans (`sb tui`). Full walkthrough: [docs/quickstart.md](docs/quickstart.md).

## Use it as a library

| Language | Where | Install |
|---|---|---|
| Go | [`client-go/`](client-go/) — package `shadowbroker` | `go get github.com/c3z/shadow-broker-server/client-go` |
| Rust | [`client-rs/`](client-rs/) — crate `shadow-broker-client` | `shadow-broker-client = { git = "https://github.com/c3z/shadow-broker-server", subdir = "client-rs" }` |
| JavaScript / TypeScript | [`client-js/`](client-js/) — `ShadowClient` (ESM, `.d.ts` included) | copy or `npm install` from the directory |
| Python | [`client-py/`](client-py/) — `ShadowClientPy` | copy `shadow_client.py` (deps: `requests`, `websocket-client`) |

All four do publish / pull / subscribe / health and understand lanes; each has a live round-trip test. The wire protocol is three authenticated endpoints plus `/health`, so a client in any other language is an afternoon — see [docs/clients.md](docs/clients.md) and [docs/protocol.md](docs/protocol.md).

## Documentation

| Doc | Contents |
|---|---|
| [docs/quickstart.md](docs/quickstart.md) | zero → signed agent messaging in 10 minutes |
| [docs/architecture.md](docs/architecture.md) | components, data flow, QoS dispatch, design decisions, scaling limits |
| [docs/protocol.md](docs/protocol.md) | HTTP/WebSocket wire protocol reference, lanes, limits, `/health` |
| [docs/security.md](docs/security.md) | threat model + signed envelope (Ed25519) specification |
| [docs/clients.md](docs/clients.md) | CLI, Go, Rust, JS/TS, Python clients and test suites |
| [docs/deployment.md](docs/deployment.md) | SSH/Docker-host, VPS, Compose, Fly.io, operations, rollback |
| [CHANGELOG.md](CHANGELOG.md) | what changed when |

## Repository layout

```
main.go, db.go           the broker server (embedded SQLite by default, Turso optional)
qos_test.go              dispatch-order and slow-subscriber tests (real SQLite/HTTP/WS)
deploy/exedev/           SSH deploy to a Docker host: cross-compile, runtime image, swap + rollback
deploy/bootstrap.sh      one-command VPS deployment with Caddy
docker-compose.yml, Caddyfile, Dockerfile   self-hosting stack with auto-HTTPS
fly.toml                 optional Fly.io deployment
sb-cli-go/               the `sb` CLI: send/pull/subscribe/ping/keygen/test/tui/version + signatures
client-go/ client-rs/ client-js/ client-py/   client libraries + round-trip tests
proxy/                   webhook proxy (Node/Express + UI) — forward queues to webhooks
skill/                   agent skill (SKILL.md) for AI agents
docs/                    documentation
db.sql                   schema reference (auto-applied at startup)
genconfig.sh             SHADOW_BROKER_CONFIG generator
```

## Status and limits

Working and deployed, personal-project grade. Know what you are buying:

- **One static token = full access.** No per-queue ACL, no multi-tenancy. Every client that can publish can also read every queue.
- **No retention policy.** Messages are kept until you delete them from the SQLite file; the table only grows.
- **No delivery acks, no consumer groups.** Each subscriber keeps its own cursor (`/pull?since=`); WebSocket delivery is at-most-once per connection with `/pull` as the catch-up path.
- **No handshake ack on WebSocket.** A message published right after `subscribe` returns may land before the broker has registered you — pull from your last id after connecting.
- **Not end-to-end encrypted.** The broker decrypts for delivery; signed envelopes give verifiability instead (see the [threat model](docs/security.md)).

Deliberate non-goals rather than roadmap. If you need those, use Redis Streams or NATS and keep this for what it is good at: a tiny, auditable, encrypted-at-rest mailbox between a handful of agents you control.

## License

MIT — see [LICENSE](LICENSE).
