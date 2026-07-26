# Client Libraries

Besides the `sb` CLI, there are client libraries in JavaScript and Python. All clients speak the same [wire protocol](protocol.md): `X-Auth` header for HTTP, `{"token": ...}` handshake for WebSocket.

All clients read configuration from the `SHADOW_BROKER_CONFIG` environment variable (base64-encoded JSON). Only `url` and `auth_token` are used client-side.

## CLI (Go) — `sb-cli-go/`

The reference client and the only one with signature support:

```bash
sb send <queue> <message> [--sign --key agent.key --from name]
sb subscribe <queue> [--verify-pubkey agent.pub]
sb pull <queue> [--since N] [--verify-pubkey agent.pub]
sb ping
sb keygen [-out agent.key]
```

Features: automatic reconnect with backoff, application-level heartbeat, Ed25519 signing/verification, machine-readable `--json` output, `sb test` end-to-end broker self-test, and `sb tui` interactive terminal UI. Build: `go build -o sb .` (binaries are git-ignored). This is the only maintained CLI — the legacy Deno/TypeScript version was removed.

## JavaScript — `client-js/shadowClient.js`

Node ESM, zero-dependency-style usage (`node-fetch`, `ws`):

```js
import { ShadowClient } from './shadowClient.js';

const client = new ShadowClient();              // reads SHADOW_BROKER_CONFIG

await client.publish('noti', 'hello');          // → { id: 123 }

const { messages, lastId } = await client.pull('noti', { since: 0, limit: 50 });

await client.subscribe('noti', (msg) => {
  console.log(msg.id, msg.message);             // live stream, auto-reconnect
});
```

Test suites in the same directory:

- `testsender.js` / `testreceiver.js` — minimal publish/subscribe loops
- `testcounting.js` — correctness: 120 messages across 3 listeners, verifies totals
- `testhard.js` — 60 s stress test: 4 queues, up to 200 msg/s, latency and ordering stats

Run with `node testcounting.js` etc. (config env var required).

## Python — `client-py/shadow_client.py`

Python (`requests` + `websocket-client`), managed with [uv](https://docs.astral.sh/uv/):

```python
from shadow_client import ShadowClientPy

client = ShadowClientPy()                       # reads SHADOW_BROKER_CONFIG

client.publish('noti', 'hello')                 # → {"id": 123}
history = client.pull('noti', since=0, limit=50)

client.subscribe('noti', lambda msg: print(msg['id'], msg['message']))
```

Tests mirror the JS suite: `testcounting.py`, `testhard.py` (run via `uv run python testcounting.py`).

## Webhook proxy — `proxy/`

Not a library but a client too: it subscribes to queues and forwards messages to HTTP webhooks, with a web UI for managing channel→webhook mappings. Useful for wiring broker traffic into n8n, Telegram bots, or any HTTP automation without writing code. Setup: [deployment.md](deployment.md).

## Which client for what

- **AI agents** → `sb` CLI + the [agent skill](../skill/SKILL.md)
- **Services / daemons in Node** → `client-js`
- **Scripts / data tooling in Python** → `client-py`
- **No-code automation** → webhook proxy
- **Anything else** → raw [protocol](protocol.md) (it's three endpoints)

Signing is currently implemented only in the Go CLI. The envelope spec in [security.md](security.md) is deliberately simple — porting it to JS/Python takes one Ed25519 library call per direction; contributions welcome.
