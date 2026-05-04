---
builtin: true
name: deploy
description: Deploy poe-acp to a remote host behind Tailscale Funnel, start it as a Poe server bot, and verify end-to-end.
---

# Deploy Skill

Deploy `poe-acp` to a remote host fronted by `tailscale funnel`. The relay listens on loopback; funnel terminates TLS and forwards. Per conversation the relay spawns an ACP agent (`fir --mode acp`, `claude-code --acp`, etc.).

## Confirm with the user before acting

1. **Host** — ssh target (`user@host`).
2. **Poe access key** — server-bot secret from poe.com; lands in `POEACP_ACCESS_KEY` on the host.
3. **ACP agent command** — default `fir --mode acp`.
4. **Funnel layout**:
   - **(a) Dedicated** — funnel `127.0.0.1:8080` on `/`. Relay uses default `--poe-path /poe`. Public URL: `https://<host>.<tailnet>.ts.net/poe`.
   - **(b) Prefix** — funnel `127.0.0.1:<port>` on `/<prefix>`. Funnel strips `/<prefix>` before forwarding, so set `--poe-path /<prefix>` to match. Default loopback port: **8347** (phone-keypad "8FIR": F=3, I=4, R=7). Any free port works; 8347 is our convention when the agent is `fir`.
5. **Permission policy** — `allow-all` (default), `read-only`, `deny-all`.

## Steps

### 1. Ship the binary

`make deploy` cross-builds, detects remote arch, scp's the right binary to `~/.local/bin/poe-acp`, and runs `--version`:

```bash
make deploy HOST=<host>
```

Alternatively (released to tap):

```bash
ssh <host> 'brew install kfet/fir/poe-acp'
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
ssh <host> 'tailscale funnel --bg --set-path=/poe-acp 127.0.0.1:8347'
```

Verify: `ssh <host> 'tailscale funnel status'`.

### 4. Install secret + service

Write `~/.config/poe-acp/env` (mode `0600`) on the host:

```
POEACP_ACCESS_KEY=<poe-server-bot-secret>
```

Optionally drop a config file at `~/.config/poe-acp/config.json` (see `docs/config.example.json`):

```json
{
  "bot_name": "<poe-bot-slug>",
  "defaults": {
    "model": "anthropic/claude-sonnet-4-6",
    "thinking": "medium",
    "hide_thinking": false
  },
  "agent": {"profile": "fir"}
}
```

`bot_name` enables auto-invalidation of Poe's cached settings response when the relay's schema changes between boots (`bot/fetch_settings/<bot>/<key>/1.1`). `defaults.model` decouples the bot's UI default from fir's own current model so it stays stable across restarts. Missing file = built-in defaults; safe to skip on first deploy and add later.

Prefer a supervised service over nohup/tmux. Use **systemd** on Linux or **launchd** on macOS.

#### Linux: systemd user unit

Write `~/.config/systemd/user/poe-acp.service`:

```ini
[Unit]
Description=poe-acp
After=network-online.target

[Service]
EnvironmentFile=%h/.config/poe-acp/env
ExecStart=%h/.local/bin/poe-acp -http-addr 127.0.0.1:8080 -agent-cmd "fir --mode acp"
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=default.target
```

For prefix layout, swap the `ExecStart` to match:

```
ExecStart=%h/.local/bin/poe-acp -http-addr 127.0.0.1:8347 -poe-path /poe-acp -agent-cmd "fir --mode acp"
```

Enable:

```bash
ssh <host> 'systemctl --user daemon-reload && systemctl --user enable --now poe-acp && loginctl enable-linger $USER'
```

(`enable-linger` keeps the user unit running across logouts/reboots.)

#### macOS: launchd user agent

launchd plists can't load an `EnvironmentFile` directly, so wrap the binary in a `sh -c` that sources the env file. Write `~/Library/LaunchAgents/dev.<you>.poe-acp.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.<you>.poe-acp</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>set -a; . "$HOME/.config/poe-acp/env"; set +a; exec /opt/homebrew/bin/poe-acp -http-addr 127.0.0.1:8347 -poe-path /poe-acp -agent-cmd "fir --mode acp" -introduction "fir over ACP — one Poe conv = one ACP session"</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>/Users/<you>/go/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    <key>HOME</key><string>/Users/<you></string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/Users/<you>/Library/Logs/poe-acp.out.log</string>
  <key>StandardErrorPath</key><string>/Users/<you>/Library/Logs/poe-acp.err.log</string>
</dict>
</plist>
```

Notes:
- `PATH` must include the directory holding the ACP agent binary (e.g. `fir`). launchd does **not** inherit your shell PATH. For Node-based agents (e.g. `claude-code --acp`), also include your nvm/node bin dir.
- The wrapper `set -a; . env; set +a` exports every `KEY=value` in the env file to the child.
- Use Apple-Silicon `/opt/homebrew/bin`; on Intel use `/usr/local/bin`.
- `-introduction "<text>"` sets the greeting shown in Poe on first message. Easy to forget on a fresh deploy — users see the default otherwise.

Load / reload / stop:

```bash
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/dev.<you>.poe-acp.plist    # start + enable
launchctl kickstart -k gui/$UID/dev.<you>.poe-acp                              # restart (e.g. after upgrade)
launchctl bootout   gui/$UID/dev.<you>.poe-acp                                 # stop + disable
launchctl print     gui/$UID/dev.<you>.poe-acp | head                          # status
tail -f ~/Library/Logs/poe-acp.err.log                                         # logs
```

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
ssh <host> 'journalctl --user -u poe-acp -f'
```

Look for per-conversation cwd, ACP `initialize` handshake, and `session/prompt` traffic.

## Upgrading

See the `update` skill (`.fir/skills/update/SKILL.md`) for the per-host upgrade flow. Quick reference:

- **Brew-managed (macOS local):** `brew upgrade poe-acp && launchctl kickstart -k gui/$UID/dev.<you>.poe-acp`.
- **Brew-managed (remote):** `ssh <host> 'brew upgrade poe-acp' && ssh <host> 'systemctl --user restart poe-acp'`.
- **Direct hotfix:** `make deploy HOST=<host> && ssh <host> 'systemctl --user restart poe-acp'`.

## Pitfalls

- **Prefix 404** — funnel `--set-path=/X` strips `/X`; `--poe-path` must equal `/X`.
- **401** — host's `POEACP_ACCESS_KEY` doesn't match what Poe sends.
- **Agent not found** — `--agent-cmd` resolves against the service user's PATH. On launchd you must set PATH explicitly in `EnvironmentVariables`; shell PATH is not inherited.
- **launchd env file** — plists have no `EnvironmentFile`; wrap in `sh -c 'set -a; . ~/.config/poe-acp/env; set +a; exec …'`.
- **Multiple bots on one host** — one relay process per bot, each on its own loopback port + funnel prefix + access key.

## Handoff checklist

- [ ] `poe-acp --version` on the host matches the intended release.
- [ ] `tailscale funnel status` shows the expected mapping.
- [ ] Curl smoke test returns `200` with SSE headers.
- [ ] Poe test message round-trips.
- [ ] Service supervisor enabled: systemd user unit + `loginctl enable-linger` (Linux) **or** launchd user agent with `RunAtLoad` + `KeepAlive` (macOS).
- [ ] `~/.config/poe-acp/env` is mode `0600`.
- [ ] `~/.config/poe-acp/config.json` exists with `bot_name` matching the Poe slug (or intentionally omitted; auto-refetch will be skipped).
- [ ] `-introduction` flag set to the intended greeting (or intentionally omitted).
