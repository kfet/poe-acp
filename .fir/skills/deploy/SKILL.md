---
name: deploy
description: Deploy poe-acp-relay to a remote host behind Tailscale Funnel, start it as a Poe server bot, and verify end-to-end.
---

# Deploy Skill

Deploy `poe-acp-relay` to a remote host fronted by `tailscale funnel`. The relay listens on loopback; funnel terminates TLS and forwards. Per conversation the relay spawns an ACP agent (`fir --mode acp`, `claude-code --acp`, etc.).

## Confirm with the user before acting

1. **Host** — ssh target (`user@host`).
2. **Poe access key** — server-bot secret from poe.com; lands in `POEACP_ACCESS_KEY` on the host.
3. **ACP agent command** — default `fir --mode acp`.
4. **Funnel layout**:
   - **(a) Dedicated** — funnel `127.0.0.1:8080` on `/`. Relay uses default `--poe-path /poe`. Public URL: `https://<host>.<tailnet>.ts.net/poe`.
   - **(b) Prefix** — funnel `127.0.0.1:<port>` on `/<prefix>`. Funnel strips `/<prefix>` before forwarding, so set `--poe-path /<prefix>` to match.
5. **Permission policy** — `allow-all` (default), `read-only`, `deny-all`.

## Steps

### 1. Ship the binary

`make deploy` cross-builds, detects remote arch, scp's the right binary to `~/.local/bin/poe-acp-relay`, and runs `--version`:

```bash
make deploy HOST=<host>
```

Alternatively (released to tap):

```bash
ssh <host> 'brew install kfet/fir/poe-acp-relay'
```

### 2. Confirm the ACP agent is on the host's PATH

```bash
ssh <host> 'command -v fir && fir --version'
```

### 3. Enable Funnel

Dedicated:
```bash
ssh <host> 'tailscale funnel --bg 127.0.0.1:8080'
```

Prefix:
```bash
ssh <host> 'tailscale funnel --bg --set-path=/poe-acp 127.0.0.1:8081'
```

Verify: `ssh <host> 'tailscale funnel status'`.

### 4. Install secret + service

Write `~/.config/poe-acp-relay/env` (mode `0600`) on the host:

```
POEACP_ACCESS_KEY=<poe-server-bot-secret>
```

Prefer a systemd user unit over nohup/tmux. Write `~/.config/systemd/user/poe-acp-relay.service`:

```ini
[Unit]
Description=poe-acp-relay
After=network-online.target

[Service]
EnvironmentFile=%h/.config/poe-acp-relay/env
ExecStart=%h/.local/bin/poe-acp-relay -http-addr 127.0.0.1:8080 -agent-cmd "fir --mode acp"
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=default.target
```

For prefix layout, swap the `ExecStart` to match:

```
ExecStart=%h/.local/bin/poe-acp-relay -http-addr 127.0.0.1:8081 -poe-path /poe-acp -agent-cmd "fir --mode acp"
```

Enable:

```bash
ssh <host> 'systemctl --user daemon-reload && systemctl --user enable --now poe-acp-relay && loginctl enable-linger $USER'
```

(`enable-linger` keeps the user unit running across logouts/reboots.)

### 5. Verify

From your workstation:

```bash
curl -i https://<host>.<tailnet>.ts.net/<poe-path> -X POST \
  -H 'Authorization: Bearer <poe-server-bot-secret>' \
  -H 'Content-Type: application/json' \
  -d '{"version":"1.0","type":"query","query":[]}'
```

Expect `200` with SSE headers. `401` → key mismatch. `404` → path layout mismatch (see Funnel prefix note).

Then set the Poe bot's Server URL to `https://<host>.<tailnet>.ts.net/<poe-path>` and the access key to the same value as `POEACP_ACCESS_KEY`. Send a test message from Poe and confirm a reply.

### 6. Tail logs during first conversations

```bash
ssh <host> 'journalctl --user -u poe-acp-relay -f'
```

Look for per-conversation cwd, ACP `initialize` handshake, and `session/prompt` traffic.

## Upgrading

- **Brew-managed:** `ssh <host> 'brew upgrade poe-acp-relay' && ssh <host> 'systemctl --user restart poe-acp-relay'`.
- **Direct hotfix:** `make deploy HOST=<host> && ssh <host> 'systemctl --user restart poe-acp-relay'`.

## Pitfalls

- **Prefix 404** — funnel `--set-path=/X` strips `/X`; `--poe-path` must equal `/X`.
- **401** — host's `POEACP_ACCESS_KEY` doesn't match what Poe sends.
- **Agent not found** — `--agent-cmd` resolves against the service user's PATH.
- **Multiple bots on one host** — one relay process per bot, each on its own loopback port + funnel prefix + access key.

## Handoff checklist

- [ ] `poe-acp-relay --version` on the host matches the intended release.
- [ ] `tailscale funnel status` shows the expected mapping.
- [ ] Curl smoke test returns `200` with SSE headers.
- [ ] Poe test message round-trips.
- [ ] Systemd user unit enabled + `loginctl enable-linger` set.
- [ ] `~/.config/poe-acp-relay/env` is mode `0600`.
