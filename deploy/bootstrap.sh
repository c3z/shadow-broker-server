#!/usr/bin/env bash
#
# Single-command provisioning of a shadow-broker server on a fresh
# Ubuntu/Debian VPS.
#
# Usage:
#   ./deploy/bootstrap.sh <user@host> [--domain broker.example.com] [--repo-url <git-url>]
#
# Examples:
#   ./deploy/bootstrap.sh root@dev.xyz --domain broker.dev.xyz
#   ./deploy/bootstrap.sh root@203.0.113.10 --domain broker.dev.xyz
#   ./deploy/bootstrap.sh root@dev.xyz --repo-url https://github.com/you/shadow-broker-server.git
#
# What it does:
#   1. Installs Docker on the remote host (via get.docker.com) if missing.
#   2. Uploads this repository to /opt/shadow-broker (tar over ssh), or clones
#      it with git when --repo-url is given.
#   3. On first run, generates secret_key + auth_token, builds the
#      SHADOW_BROKER_CONFIG (embedded SQLite at file:/data/broker.db) and writes
#      a chmod-600 .env on the host. No secrets are stored in this repo.
#   4. Runs `docker compose up -d --build` (broker + Caddy with automatic HTTPS).
#   5. Prints the server URL, auth_token and a ready-to-use SHADOW_BROKER_CONFIG
#      export for clients.
#
# Re-running the script is a redeploy: code is re-uploaded, containers are
# rebuilt, and the existing .env (secrets) on the host is kept.

set -euo pipefail

REMOTE_DIR=/opt/shadow-broker

usage() {
  sed -n '2,20p' "$0"
  exit "${1:-1}"
}

err() { echo "ERROR: $*" >&2; exit 1; }

# --- Parse arguments -------------------------------------------------------
HOST=""
DOMAIN=""
REPO_URL=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --domain)
      [[ $# -ge 2 ]] || usage
      DOMAIN="$2"; shift 2 ;;
    --repo-url)
      [[ $# -ge 2 ]] || usage
      REPO_URL="$2"; shift 2 ;;
    -h|--help)
      usage 0 ;;
    *)
      if [[ -z "$HOST" ]]; then HOST="$1"; shift; else usage; fi ;;
  esac
done

[[ -n "$HOST" ]] || usage

# Default domain: the host itself, unless it is a bare IP address
# (Let's Encrypt cannot issue certificates for IPs).
if [[ -z "$DOMAIN" ]]; then
  hostpart="${HOST#*@}"
  if [[ "$hostpart" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    err "--domain is required when deploying to an IP address (HTTPS needs a DNS name)."
  fi
  DOMAIN="$hostpart"
fi

# --- Local requirements ----------------------------------------------------
for cmd in ssh tar openssl; do
  command -v "$cmd" >/dev/null 2>&1 || err "missing required command: $cmd"
done
if [[ -n "$REPO_URL" ]] && ! command -v git >/dev/null 2>&1; then
  err "missing required command: git (needed for --repo-url)"
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> Target host : $HOST"
echo "==> Domain      : https://$DOMAIN"
echo "==> Remote dir  : $REMOTE_DIR"
echo

# --- 1. Docker on the host -------------------------------------------------
echo "==> Ensuring Docker is installed on $HOST"
ssh "$HOST" 'set -e
if ! command -v docker >/dev/null 2>&1; then
  echo "    installing docker..."
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL https://get.docker.com | sh
  else
    wget -qO- https://get.docker.com | sh
  fi
fi
docker compose version >/dev/null 2>&1 || { echo "docker compose plugin missing" >&2; exit 1; }
'

# --- 2. Upload the code ----------------------------------------------------
echo "==> Uploading code to $HOST:$REMOTE_DIR"
ssh "$HOST" "mkdir -p '$REMOTE_DIR'"
if [[ -n "$REPO_URL" ]]; then
  ssh "$HOST" "set -e
    if [ -d '$REMOTE_DIR/.git' ]; then
      git -C '$REMOTE_DIR' pull --ff-only
    else
      rm -rf '$REMOTE_DIR'
      git clone '$REPO_URL' '$REMOTE_DIR'
    fi"
else
  # tar over ssh - works on minimal hosts without rsync.
  tar -C "$REPO_ROOT" -czf - \
    --exclude='.git' \
    --exclude='node_modules' \
    --exclude='.venv' \
    --exclude='__pycache__' \
    --exclude='data' \
    --exclude='.env' \
    . | ssh "$HOST" "tar -xzf - -C '$REMOTE_DIR'"
fi

# --- 3. Server config (.env with secrets) ----------------------------------
FRESH_INSTALL=0
if ssh "$HOST" "test -f '$REMOTE_DIR/.env'"; then
  echo "==> Existing .env found on the host - keeping current secrets (redeploy)"
  SHADOW_BROKER_CONFIG="$(ssh "$HOST" "grep '^SHADOW_BROKER_CONFIG=' '$REMOTE_DIR/.env' | cut -d= -f2-")"
else
  FRESH_INSTALL=1
  echo "==> First deploy - generating secrets"
  # secret_key must be 32 chars (see main.go); openssl rand -hex 16 -> 32 hex chars.
  SECRET_KEY="$(openssl rand -hex 16)"
  AUTH_TOKEN="$(openssl rand -hex 32)"
  SHADOW_BROKER_CONFIG="$(printf '{"url":"https://%s","secret_key":"%s","auth_token":"%s","db_url":"file:/data/broker.db"}' \
    "$DOMAIN" "$SECRET_KEY" "$AUTH_TOKEN" | base64 | tr -d '\n')"
  # umask 077 + chmod 600: the .env holds the only copy of these secrets.
  ssh "$HOST" "umask 077 && printf 'DOMAIN=%s\nSHADOW_BROKER_CONFIG=%s\n' \
    '$DOMAIN' '$SHADOW_BROKER_CONFIG' > '$REMOTE_DIR/.env' && chmod 600 '$REMOTE_DIR/.env'"
fi

# --- 4. Deploy -------------------------------------------------------------
echo "==> Building and starting containers"
ssh "$HOST" "cd '$REMOTE_DIR' && docker compose up -d --build --remove-orphans"

echo
echo "==> Deployment finished."
echo "    Broker URL : https://$DOMAIN"
echo "    (first HTTPS request may take a few seconds while Caddy obtains the certificate)"
echo "    Make sure the DNS A/AAAA record for $DOMAIN points at this server."

# --- 5. Summary for clients ------------------------------------------------
if [[ "$FRESH_INSTALL" -eq 1 ]]; then
  echo
  echo "    auth_token (keep it secret):"
  echo "      $AUTH_TOKEN"
fi
echo
echo "    Export for clients:"
echo "      export SHADOW_BROKER_CONFIG=$SHADOW_BROKER_CONFIG"
