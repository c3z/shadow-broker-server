# Deployment

Shadow Broker is self-hostable: one container, embedded SQLite, no external services required. Pick your path:

| Target | Effort | TLS |
|---|---|---|
| Docker host over SSH, no Go on the server (exe.dev, any VM behind a proxy) | one script | the host's reverse proxy |
| Any VPS (DigitalOcean, Hetzner, dev.xyz, …) | one command | automatic (Caddy + Let's Encrypt) |
| Docker Compose (manual) | 2 files | automatic |
| Plain Docker | 2 commands | bring your own |
| Local dev | `go run .` | none needed |
| Fly.io | `fly deploy` | handled by Fly |

## Docker host over SSH (exe.dev)

For hosts where a reverse proxy already terminates TLS and you do not want a Go toolchain or 1 GB of build cache on the box.

```bash
deploy/exedev/deploy.sh user@host [--tag <tag>] [--no-test]
```

What it does, in order:

1. `go test -race ./...` locally.
2. Cross-compiles a static `linux/amd64` binary (`-X main.version=<tag>`, tag defaults to `git describe`).
3. Uploads binary + [`Dockerfile.runtime`](../deploy/exedev/Dockerfile.runtime) + [`upgrade.py`](../deploy/exedev/upgrade.py) to `/opt/shadow-broker/deploy-exedev` and builds `shadow-broker:<tag>` there (debian-slim runtime image, ~80 MB, `BIND_HOST=0.0.0.0`, `TRUST_PROXY=1`, Docker `HEALTHCHECK` on `/health`).
4. Runs `upgrade.py` on the host: refuses to touch a container whose port binding is not `127.0.0.1:8080` or whose storage is not the `broker-data` volume; stops it; archives the encrypted volume to `~/.local/share/shadow-broker-backups/<stamp>/broker-data.tgz`; renames it to `shadow-broker-before-<stamp>`; starts the new image with the **same env** (secrets never leave the host) and the same volume; waits for `GET /health` = 200. On failure it removes the new container and restarts the old one. Rollback containers beyond the two newest are removed (`--keep`).
5. Hits `<url>/health` from the outside and, if `sb` and `SHADOW_BROKER_CONFIG` are available locally, runs `sb test`.

First-time setup on such a host (once): create the volume and the first container by hand or with the compose file's `broker` service, publish the port through the host's proxy (on exe.dev: `share port <vm> 8080` + `set-public`), and make sure `docker.service` is enabled so the container returns after a reboot (`--restart unless-stopped` only helps if dockerd itself starts).

Manual rollback on the host:

```bash
docker stop shadow-broker && docker rm shadow-broker
docker rename shadow-broker-before-<stamp> shadow-broker && docker start shadow-broker
```

Both containers share the volume, so rolling back the binary keeps messages published in between. Restoring `broker-data.tgz` is a separate, lossy operation.

## One-command VPS deployment (Caddy)

Point a DNS A record at your server first (Let's Encrypt needs a domain, not an IP). Then:

```bash
./deploy/bootstrap.sh root@your-server --domain broker.example.com
```

The script:

1. Installs Docker on the host (if missing).
2. Uploads the repo to `/opt/shadow-broker` (tar over SSH — no rsync needed; or `--repo-url` to git clone).
3. Generates `secret_key` and `auth_token`, builds `SHADOW_BROKER_CONFIG` with `db_url=file:/data/broker.db`, writes `.env` with mode `600`.
4. Runs `docker compose up -d --build`.
5. Prints the broker URL and a ready-to-use `export SHADOW_BROKER_CONFIG=...` for clients.

Re-running the script redeploys and **keeps the existing `.env`** (idempotent). Use it against any fresh Ubuntu/Debian box — a $5 DigitalOcean droplet works fine.

## Docker Compose (manual)

```bash
cp .env.example .env
# edit .env: set DOMAIN and generate SHADOW_BROKER_CONFIG (command included in the file)
docker compose up -d
```

The stack: `broker` (Go server, named volume `broker-data:/data`, healthcheck on `/health`) behind `caddy` (ports 80/443, automatic HTTPS via [Caddyfile](../Caddyfile): `{$DOMAIN}` → `reverse_proxy broker:8080`). Set `TRUST_PROXY=1` on the broker service if you want per-client rate limiting behind Caddy.

## Plain Docker

```bash
docker build -t shadow-broker .
docker run -d -p 127.0.0.1:8080:8080 -v broker-data:/data \
  -e SHADOW_BROKER_CONFIG='<base64 JSON>' shadow-broker
```

Without Caddy you get plain HTTP on 8080 — put your own reverse proxy in front (then add `-e TRUST_PROXY=1`), or bind to a trusted interface only.

## Local development

```bash
export SHADOW_BROKER_CONFIG='...'   # url + secret_key + auth_token; db_url optional
go run .                            # embedded SQLite in ./data/broker.db, schema auto-created
```

Binds `127.0.0.1:8080` by default; set `BIND_HOST=0.0.0.0` to expose it on the LAN.

## Fly.io (optional)

[fly.toml](../fly.toml) still works. With embedded SQLite, create the volume once so data survives deploys:

```bash
fly volumes create broker_data --region fra --size 1
fly secrets set SHADOW_BROKER_CONFIG='...'
fly deploy
```

## Configuration

`SHADOW_BROKER_CONFIG` is base64-encoded JSON:

```json
{
  "url": "https://broker.example.com",
  "secret_key": "<32 chars, AES-256 key>",
  "auth_token": "<shared admin token>",
  "db_url": "file:/data/broker.db",
  "turso_auth_token": ""
}
```

- `url`, `secret_key`, `auth_token` — required.
- `db_url` — optional; `file:…` (or omitted) → embedded SQLite (`file:./data/broker.db` by default); `libsql://…` → remote Turso.
- `turso_auth_token` — only for Turso.

Generate one with `./genconfig.sh <url> <secret_key> <auth_token> [db_url] [turso_token]` or by hand (`openssl rand -hex 16` etc. — see `.env.example`).

Environment variables read by the server:

| Variable | Default | Meaning |
|---|---|---|
| `SHADOW_BROKER_CONFIG` | — | the config above (required) |
| `PORT` | `8080` | listen port |
| `BIND_HOST` | `127.0.0.1` (`0.0.0.0` in the Docker images) | listen address |
| `TRUST_PROXY` | unset | `1` → use `X-Forwarded-For` / `X-Real-IP` for rate limiting and logs |

## Operations

- **Health**: `GET /health` (no auth) → `200 {"status":"ok","uptime_s":N}`. Wire it into your monitor; the Docker images also run it as `HEALTHCHECK`.
- **Data**: everything lives in the `broker-data` volume (`/data/broker.db`). Back up by copying the SQLite file (WAL mode — use `sqlite3 broker.db ".backup ..."` or stop the container first; `upgrade.py` does the latter automatically before every swap).
- **Retention**: none built in. The `messages` table grows until you prune it (`DELETE FROM messages WHERE created_at < ...` with the container stopped, then `VACUUM`).
- **Restarts are safe**: subscribers reconnect and use `/pull?since=<last_id>`; the server never replays history to WebSocket clients.
- **Logs**: connections (client IP, queue), publishes (queue, id), decrypt failures, health store errors — never tokens or plaintext payloads.
- **Secret rotation**: rotate `secret_key` and `auth_token` together; changing the AES key makes old ciphertexts undecryptable (history effectively resets). With `upgrade.py` the env is carried over unchanged; to rotate, recreate the container with the new `SHADOW_BROKER_CONFIG`.
- **Turso mode**: if you prefer a managed database, set `db_url=libsql://…` + `turso_auth_token` and skip the volume. Same schema, applied automatically at startup.
