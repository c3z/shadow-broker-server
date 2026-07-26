# Deployment

Shadow Broker is self-hostable: one container, embedded SQLite, no external services required. Pick your path:

| Target | Effort | TLS |
|---|---|---|
| Any VPS (DigitalOcean, Hetzner, dev.xyz, …) | one command | automatic (Caddy + Let's Encrypt) |
| Docker Compose (manual) | 2 files | automatic |
| Plain Docker | 2 commands | bring your own |
| Local dev | `go run main.go` | none needed |
| Fly.io | `fly deploy` | handled by Fly |

## One-command VPS deployment

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

The stack: `broker` (Go server, named volume `broker-data:/data`, healthcheck) behind `caddy` (ports 80/443, automatic HTTPS via [Caddyfile](../Caddyfile): `{$DOMAIN}` → `reverse_proxy broker:8080`).

## Plain Docker

```bash
docker build -t shadow-broker .
docker run -d -p 8080:8080 -v broker-data:/data \
  -e SHADOW_BROKER_CONFIG='<base64 JSON>' shadow-broker
```

Without Caddy you get plain HTTP on 8080 — put your own reverse proxy in front, or use it on a trusted network.

## Local development

```bash
export SHADOW_BROKER_CONFIG='...'   # url + secret_key + auth_token; db_url optional
go run main.go                      # embedded SQLite in ./data/broker.db, schema auto-created
```

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

## Operations

- **Data**: everything lives in the `broker-data` volume (`/data/broker.db`). Back up by copying the SQLite file (WAL mode — use `sqlite3 broker.db ".backup ..."` or stop the container first).
- **Restarts are safe**: subscribers reconnect and use `/pull?since=<last_id>`; the server never replays history to WebSocket clients.
- **Logs**: connections, publishes, decrypt failures — never tokens or plaintext payloads.
- **Secret rotation**: rotate `secret_key` and `auth_token` together; changing the AES key makes old ciphertexts undecryptable (history effectively resets).
- **Turso mode**: if you prefer a managed database, set `db_url=libsql://…` + `turso_auth_token` and skip the volume. Same schema, applied automatically at startup.
