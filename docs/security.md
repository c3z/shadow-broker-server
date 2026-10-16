# Security Model

Shadow Broker is designed for **agent-to-agent messaging with verifiable security**: the broker protects data at rest and in transit, and — more importantly — receivers can cryptographically verify *who* sent a message without trusting the broker.

## Layers

| Layer | Mechanism | Protects against |
|---|---|---|
| Transport | HTTPS / WSS, terminated by your reverse proxy (Caddy, exe.dev, Fly) | passive eavesdropping, MITM on the wire |
| Broker access | shared `auth_token` (`X-Auth` header / WS handshake) | unauthorized publish/subscribe |
| Storage | AES-256-GCM per message, key held only by the server | database files, volume backups or Turso dumps leaking plaintext |
| Sender identity | **Ed25519 signed envelopes (client-side)** | forged sender identity, tampered payloads |
| Admin surfaces | `admin_token` on the webhook proxy UI/API | config theft, webhook hijacking (SSRF/exfiltration) |

## Threat model (honest version)

The broker is **not** end-to-end encrypted. The server decrypts messages before delivering them, so the server operator (and the hosting provider) can read all traffic. The trust assumptions are:

- **The storage layer is untrusted** — whether it's the embedded SQLite file on the broker host or a remote Turso database, it only ever holds nonce + ciphertext (AES-256-GCM, random nonce per message). Anyone with a copy of the database file (backups, snapshots, disk access) still can't read messages without the server's key.
- **The broker server is trusted for confidentiality, not for identity.** It can read and even rewrite messages. This is exactly why signed envelopes exist: a receiver with the sender's public key detects any tampering, including by the server itself.
- **Anyone with `auth_token` has full access** — one static token, no per-queue ACLs. Treat it as root.
- **Replay within a queue is possible** — an attacker with broker access can republish a captured signed envelope verbatim. Envelopes include a `ts` timestamp; reject unreasonably old messages if replay matters for your use case.

## Signed envelope specification (v1)

Signing is client-side and transport-agnostic: the server stores and forwards the envelope as an opaque string. The reference implementation is the Go CLI (`sb keygen`, `sb send --sign`, `sb subscribe --verify-pubkey`).

### Envelope format

A compact JSON object, carried as the `message` payload:

```json
{"body":"<message text>","from":"<sender name>","sig":"<base64 signature>","ts":1712345678,"v":1}
```

- `v` — envelope version, currently `1`
- `from` — human/agent-readable sender identity
- `ts` — Unix seconds when the message was signed
- `body` — the actual payload (UTF-8 string)
- `sig` — base64-encoded Ed25519 signature

### Signature input

The signature is computed over the canonical byte string (UTF-8, **no trailing newline**):

```
"v1\n" + from + "\n" + ts + "\n" + body
```

where `ts` is rendered as a decimal integer. Example: for `from="agent-a"`, `ts=1712345678`, `body="hello"`:

```
v1
agent-a
1712345678
hello
```

### Keys

- `sb keygen -out agent` generates an Ed25519 keypair: `agent` (private, mode `0600`) and `agent.pub` (public). The suffix is appended verbatim, so `-out agent.key` yields `agent.key` + `agent.key.pub`.
- Keys are base64-encoded raw Ed25519 keys (64 bytes private, 32 bytes public).
- **Public keys must be distributed over a trusted channel** (committed to a repo, pasted by the user, embedded in agent config). An attacker who can substitute the public key defeats verification entirely.

### Verification rules

A verifying receiver (`--verify-pubkey`):

1. Parse the message as an envelope. If it does not parse → treat as *unsigned* (delivered as-is, untrusted).
2. Verify `sig` over the canonical input with the pinned public key.
   - valid → display as `[from ✓] body`
   - invalid → display `[INVALID SIGNATURE]` and treat the payload as hostile
3. Optionally enforce freshness on `ts` (recommended: reject messages older than your catch-up window).

### What signatures do NOT give you

- **Confidentiality** — envelopes are readable by the broker and anyone with `auth_token`.
- **Replay protection** by themselves — use `ts` and/or nonces in `body`.
- **Key revocation** — rotating a compromised key means redistributing public keys to all receivers.

## Operational hardening checklist

- Keep `SHADOW_BROKER_CONFIG` (contains AES key, auth token, Turso credentials) out of git; distribute it as a secret only.
- Never commit `proxy/config.yaml` or key files (`*.key`); the repo `.gitignore` covers them.
- Set `admin_token` on the webhook proxy — without it the UI/API (including broker credentials) is open.
- Rotate credentials if they were ever committed or logged (this project's history predates that rule — rotate before relying on it).
- Rate limiting (500 req/10 s/IP, separate bucket for `X-Broker-Lane: control`) is abuse-dampening, not a security boundary. Enable `TRUST_PROXY=1` only behind a proxy you control — with it, the client IP comes from `X-Forwarded-For`.
- `/health` is unauthenticated by design (liveness only); everything else needs the token.

## Reporting issues

If you find a vulnerability, open a private report to the maintainer rather than a public issue.
