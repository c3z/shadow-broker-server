#!/usr/bin/env bash
#
# Deploy the broker to an exe.dev VM (or any Docker host reachable over SSH
# where you do not want a Go toolchain): test, cross-compile, ship a runtime
# image, swap the container with a rollback point, verify from the outside.
#
# Usage:  deploy/exedev/deploy.sh [user@host] [--tag <tag>] [--no-test]
#         default host: broker.exe.xyz   default tag: <git short sha>[-dirty]
#
# Requirements: go (local), ssh access to the host with sudo docker, python3
# on the host. SHADOW_BROKER_CONFIG in the local env is only used for the
# final `sb test` and is optional.
set -euo pipefail

HOST="broker.exe.xyz"
TAG=""
RUN_TESTS=1
while [ $# -gt 0 ]; do
  case "$1" in
    --tag) TAG="$2"; shift 2 ;;
    --no-test) RUN_TESTS=0; shift ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) HOST="$1"; shift ;;
  esac
done

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
[ -n "$TAG" ] || TAG="$(git describe --tags --always --dirty 2>/dev/null || date +%Y%m%d%H%M)"
IMAGE="shadow-broker:$TAG"
REMOTE_DIR="/opt/shadow-broker/deploy-exedev"
BUILD="$(mktemp -d)"
trap 'rm -rf "$BUILD"' EXIT

if [ "$RUN_TESTS" = 1 ]; then
  echo "==> go test -race ./..."
  go test -race ./...
fi

echo "==> cross-compiling linux/amd64 -> $BUILD/run-app"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.version=$TAG" -o "$BUILD/run-app" .
cp deploy/exedev/Dockerfile.runtime "$BUILD/Dockerfile"
cp deploy/exedev/upgrade.py "$BUILD/upgrade.py"

echo "==> uploading to $HOST:$REMOTE_DIR"
ssh "$HOST" "mkdir -p $REMOTE_DIR"
COPYFILE_DISABLE=1 tar --no-xattrs -C "$BUILD" -cf - run-app Dockerfile upgrade.py | ssh "$HOST" "tar -C $REMOTE_DIR -xf -"

echo "==> building $IMAGE on $HOST"
ssh "$HOST" "cd $REMOTE_DIR && sudo docker build -q -t $IMAGE . && sudo docker tag $IMAGE shadow-broker:latest-deployed"

echo "==> swapping container (rollback kept)"
# backups land in the SSH user's home even though docker needs sudo
ssh "$HOST" "cd $REMOTE_DIR && sudo env SHADOW_BROKER_BACKUPS=\$HOME/.local/share/shadow-broker-backups python3 upgrade.py --image $IMAGE"

echo "==> external check"
URL="$(ssh "$HOST" "sudo docker inspect shadow-broker --format '{{range .Config.Env}}{{println .}}{{end}}'" \
  | grep '^SHADOW_BROKER_CONFIG=' | cut -d= -f2- | base64 -d | python3 -c 'import json,sys; print(json.load(sys.stdin)["url"])')"
curl -sf "$URL/health" && echo
if [ -n "${SHADOW_BROKER_CONFIG:-}" ] && command -v sb >/dev/null; then
  sb test "deploy-check" && echo "==> sb test OK"
else
  echo "(set SHADOW_BROKER_CONFIG and have sb on PATH for an end-to-end sb test)"
fi
