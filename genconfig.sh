#!/bin/zsh
# Generates the SHADOW_BROKER_CONFIG env var (base64-encoded JSON).
# Pass all values as arguments — never hardcode secrets in this file.
#
# Usage (local mode — embedded SQLite file, default ./data/broker.db):
#   ./genconfig.sh <BROKER_URL> <AES_SECRET_KEY> <AUTH_TOKEN>
#
# Usage (Turso mode — remote libsql database):
#   ./genconfig.sh <BROKER_URL> <AES_SECRET_KEY> <AUTH_TOKEN> <TURSO_DB_URL> <TURSO_AUTH_TOKEN>
#
# Examples:
#   ./genconfig.sh "http://localhost:8080" "$(openssl rand -hex 16)" "$(openssl rand -hex 24)"
#   ./genconfig.sh "https://your-broker.fly.dev" "$(openssl rand -hex 16)" "$(openssl rand -hex 24)" "libsql://your-db.turso.io" "$TURSO_AUTH_TOKEN"

if [ $# -lt 3 ]; then
  echo "Usage (local): $0 <BROKER_URL> <AES_SECRET_KEY> <AUTH_TOKEN>"
  echo "Usage (turso): $0 <BROKER_URL> <AES_SECRET_KEY> <AUTH_TOKEN> <TURSO_DB_URL> <TURSO_AUTH_TOKEN>"
  exit 1
fi

go run . genconfig "$@"
