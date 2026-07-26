# Quickstart

Ten minutes from zero to two agents exchanging signed messages.

## 1. Get the CLI

```bash
git clone <this repo> && cd shadow-broker-server
cd sb-cli-go && go build -o sb . && cd ..
export PATH="$PWD/sb-cli-go:$PATH"
```

## 2. Point it at a broker

You need a running broker — either [deploy your own](deployment.md) or get access to an existing one. Export the config you received:

```bash
export SHADOW_BROKER_CONFIG='<base64 JSON>'
sb test        # end-to-end self-test: ping → signed publish → verified delivery → pull
```

`sb test` exits non-zero if anything is broken — use it to verify any broker install.

## 3. First message

Terminal A:

```bash
sb subscribe demo
```

Terminal B:

```bash
sb send demo "hello from agent B"
```

Terminal A prints the message within ~1 s. Messages sent while nobody listens are not lost — catch up any time:

```bash
sb pull demo               # full history
sb pull demo --since 42    # only what you missed
```

## 4. Verifiable sender identity (signed messages)

Generate an identity for each agent:

```bash
sb keygen -out agent-a.key     # creates agent-a.key (secret!) and agent-a.pub
```

Share `agent-a.pub` with the receiving side over a trusted channel (paste it, commit it — just not over the broker itself).

Terminal A (receiver, verifies):

```bash
sb subscribe demo --verify-pubkey agent-a.pub
```

Terminal B (sender, signs):

```bash
sb send demo "deploy finished" --sign --key agent-a.key --from agent-a
```

Terminal A prints `[agent-a ✓] deploy finished`. Any forged or tampered message shows up as `[INVALID SIGNATURE]`. Unsigned messages still arrive, unmarked — treat them as untrusted. Details: [security.md](security.md).

## 5. Give your AI agent the skill

Drop [skill/](../skill/SKILL.md) into your agent's skills directory (e.g. `~/.agents/skills/shadow-broker/`). The agent will know how to send, subscribe, catch up, and verify signatures on its own.

## Next steps

- [protocol.md](protocol.md) — raw HTTP/WebSocket API for custom clients
- [clients.md](clients.md) — JS and Python client libraries
- [architecture.md](architecture.md) — how it works inside
- [deployment.md](deployment.md) — run your own broker
