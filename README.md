# Shadow Broker

A minimal, self-hostable, open-source message broker for **agent-to-agent communication** — encryption at rest, cryptographically verifiable sender identity, zero external dependencies.

```
agent A ──sb send──▶ broker ──AES-256-GCM──▶ embedded SQLite (sees only ciphertext)
                        │
                        └─poll→ decrypt→──WebSocket──▶ agent B  [agent-a ✓]
```

- **Self-hostable in one command**: a single Docker container with embedded SQLite — no database to provision, no external services.
- **Durable**: messages persist first, deliver second. Offline clients catch up with `/pull?since=`.
- **Encrypted at rest**: AES-256-GCM per message; the storage layer never holds plaintext.
- **Verifiable**: optional Ed25519 signed envelopes let receivers prove *who* sent a message — without trusting the broker.
- **Dual-interface CLI**: scriptable commands with `--json` and `sb test` for agents, `sb tui` for humans.
- **Agent-native**: ships with an [agent skill](skill/SKILL.md) so AI agents can use it out of the box.

## Deploy your own broker

### One command, any VPS (DigitalOcean, Hetzner, dev.xyz, …)

Point a DNS A record at your server, then:

```bash
./deploy/bootstrap.sh root@your-server --domain broker.example.com
```

The script installs Docker if needed, copies the repo, generates secrets, and brings up the stack with automatic HTTPS (Caddy + Let's Encrypt). It prints your `SHADOW_BROKER_CONFIG` at the end. Re-running it redeploys.

### Docker Compose (manual)

```bash
cp .env.example .env        # set DOMAIN and SHADOW_BROKER_CONFIG
docker compose up -d
```

### Plain Docker (no TLS termination)

```bash
docker build -t shadow-broker .
docker run -d -p 8080:8080 -v broker-data:/data \
  -e SHADOW_BROKER_CONFIG='<base64 JSON>' shadow-broker
```

### Local development

```bash
go run main.go              # embedded SQLite in ./data, no setup needed
```

### Fly.io

Still supported as an option — see [docs/deployment.md](docs/deployment.md) (includes the persistent volume setup).

## Use it

```bash
cd sb-cli-go && go build -o sb . && export PATH="$PWD:$PATH"
export SHADOW_BROKER_CONFIG='<from the deploy output>'
sb test                     # end-to-end self-test of the broker
sb subscribe demo &  sb send demo "hello"
```

Signed, verifiable messaging:

```bash
sb keygen -out agent.key                        # one time per identity
sb send demo "done" --sign --key agent.key --from agent-a
sb subscribe demo --verify-pubkey agent.pub     # → [agent-a ✓] done
```

Machine-readable for agents (`--json` on every command), interactive for humans (`sb tui`). Full walkthrough: [docs/quickstart.md](docs/quickstart.md).

## Documentation

| Doc | Contents |
|---|---|
| [docs/quickstart.md](docs/quickstart.md) | zero → signed agent messaging in 10 minutes |
| [docs/architecture.md](docs/architecture.md) | components, data flow, design decisions, scaling limits |
| [docs/protocol.md](docs/protocol.md) | HTTP/WebSocket wire protocol reference |
| [docs/security.md](docs/security.md) | threat model + signed envelope (Ed25519) specification |
| [docs/clients.md](docs/clients.md) | CLI, JS, Python clients and test suites |
| [docs/deployment.md](docs/deployment.md) | VPS/DigitalOcean/Fly.io/local, TLS, operations |

## Repository layout

```
main.go                  the broker server (embedded SQLite by default, Turso optional)
deploy/bootstrap.sh      one-command VPS deployment
docker-compose.yml, Caddyfile, Dockerfile   self-hosting stack with auto-HTTPS
fly.toml                 optional Fly.io deployment
proxy/                   webhook proxy (Node/Express + UI) — forward queues to webhooks
sb-cli-go/               the `sb` CLI: send/pull/subscribe/ping/keygen/test/tui + signatures
client-js/ client-py/    client libraries + stress/correctness test suites
skill/                   agent skill (SKILL.md) for AI agents
docs/                    documentation
db.sql                   schema reference (auto-applied at startup)
genconfig.sh             SHADOW_BROKER_CONFIG generator
```

## Status

Working and deployed, personal-project grade. Deliberate non-goals: multi-tenancy, per-queue ACLs, end-to-end encryption (verifiability instead — see the [threat model](docs/security.md)). Contributions welcome, especially Ed25519 envelope support in the JS/Python clients.

## License

MIT — see [LICENSE](LICENSE).
