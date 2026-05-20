---
builtin: true
name: custom-bots
description: Create or update custom Poe server bots with separate poe-acp config, access keys, models, fir agent dirs, and credentials.
---

# Custom Poe Bots

Use when the user wants another Poe Server Bot on an existing poe-acp host: model-specific bots, test bots, friend/client bots, or any bot needing separate credentials.

One Poe bot = one `poe-acp` process with its own Poe access key, config dir, loopback port, Funnel prefix, supervisor unit, and usually its own fir config/state root via `--agent-dir`.

## Confirm first

- **Host**: local or `user@host`.
- **Bot slug**: exact Poe server-bot slug for `bot_name`.
- **Poe access key**: server-bot secret; store as `POEACP_ACCESS_KEY`, mode `0600`.
- **Public path + port**: e.g. `/sakana` → `127.0.0.1:8347`; each bot gets a free port (`8347`, `8348`, ... by convention).
- **Model defaults**: `defaults.model`, `defaults.thinking`, `defaults.hide_thinking`.
- **Credential boundary**: shared fir creds or a per-bot fir root such as `~/.config/fir-sakana`.
- **Agent command**: default `fir --mode acp`.
- **Introduction** and permission policy if non-default.

## Layout

Use a per-bot poe-acp config directory:

```text
~/.config/poe-acp/<bot>/
  env                  # POEACP_ACCESS_KEY=..., chmod 600
  config.json          # bot_name + model/thinking defaults
  skills/              # optional bot-specific skills / overrides
  state/               # derived automatically from dirname(config)
```

Use a per-bot fir root when credentials or defaults should differ:

```text
~/.config/fir-<bot>/
  auth.json            # OAuth/API auth used by fir
  settings.json        # fir-side default provider/model/thinking
  sessions/, cache/, packages/, skills/, extensions/
```

`poe-acp --agent-dir <dir>` passes `FIR_AGENT_DIR=<dir>` to the spawned child. fir itself currently uses the env var for setup commands; there is no fir CLI flag for this root. Create the root before login. When `--config ~/.config/poe-acp/<bot>/config.json` is set and `--state-dir` is omitted, poe-acp derives state as `~/.config/poe-acp/<bot>/state`.

## Seed per-bot fir credentials

Run interactive logins from a real terminal, not from a service:

```bash
mkdir -p ~/.config/fir-<bot>
FIR_AGENT_DIR=$HOME/.config/fir-<bot> fir login poe
FIR_AGENT_DIR=$HOME/.config/fir-<bot> fir login google-gemini-cli   # if needed
```

Optionally pin fir-side defaults:

```bash
cat > ~/.config/fir-<bot>/settings.json <<'JSON'
{
  "defaultProvider": "poe",
  "defaultModel": "<model-id>",
  "defaultThinkingLevel": "medium"
}
JSON
```

Pin both layers when the bot should stay model-specific:

- `~/.config/poe-acp/<bot>/config.json` `defaults.model`: what Poe advertises and sends by default.
- `~/.config/fir-<bot>/settings.json` `defaultModel`: what fir falls back to if no model override arrives.

Do not use `agent.profile` as a credential selector. It is reserved for relay control-schema selection; today only `fir` is wired. Credential isolation is `--agent-dir` / `FIR_AGENT_DIR`, or a separate Unix user/host for non-fir tool auth.

## Create bot files

```bash
mkdir -p ~/.config/poe-acp/<bot>
umask 077
cat > ~/.config/poe-acp/<bot>/env <<'EOF'
POEACP_ACCESS_KEY=<poe-server-bot-secret>
EOF
umask 022
```

```json
{
  "bot_name": "<exact-poe-bot-slug>",
  "defaults": {
    "model": "<model-id>",
    "thinking": "medium",
    "hide_thinking": false
  },
  "agent": {"profile": "fir"}
}
```

Write that JSON to `~/.config/poe-acp/<bot>/config.json`. `agent.profile: "fir"` selects the relay's fir control schema; it is not a credential profile. Omit it only if auto-detecting from `--agent-cmd` is desired.

## Linux systemd user unit

Pick a free port first: `ss -ltn | grep ':8347\|:8348\|:8349'` on Linux or `lsof -nP -iTCP -sTCP:LISTEN | grep 834` on macOS. Use one unit per bot, e.g. `~/.config/systemd/user/poe-acp-<bot>.service`:

```ini
[Unit]
Description=poe-acp (<bot>)
After=network-online.target

[Service]
EnvironmentFile=%h/.config/poe-acp/<bot>/env
ExecStart=%h/.local/bin/poe-acp \
  --http-addr 127.0.0.1:<port> \
  --poe-path /<bot-path> \
  --config %h/.config/poe-acp/<bot>/config.json \
  --agent-dir %h/.config/fir-<bot> \
  --agent-cmd "fir --mode acp" \
  -introduction "<intro>"
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=default.target
```

Enable:

```bash
systemctl --user daemon-reload
systemctl --user enable --now poe-acp-<bot>.service
loginctl enable-linger "$USER"
```

## macOS launchd user agent

launchd has no `EnvironmentFile`; source the env file through `sh -c` and set PATH explicitly. This is a plist fragment; wrap it in a full `~/Library/LaunchAgents/dev.<you>.poe-acp.<bot>.plist` with a unique `Label`, log paths, `RunAtLoad`, and `KeepAlive`:

```xml
<key>ProgramArguments</key>
<array>
  <string>/bin/sh</string>
  <string>-c</string>
  <string>set -a; . "$HOME/.config/poe-acp/<bot>/env"; set +a; exec /opt/homebrew/bin/poe-acp --http-addr 127.0.0.1:<port> --poe-path /<bot-path> --config "$HOME/.config/poe-acp/<bot>/config.json" --agent-dir "$HOME/.config/fir-<bot>" --agent-cmd "fir --mode acp" -introduction "<intro>"</string>
</array>
<key>EnvironmentVariables</key>
<dict>
  <key>PATH</key><string>/Users/<you>/go/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
  <key>HOME</key><string>/Users/<you></string>
</dict>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
```

Use a unique label such as `dev.<you>.poe-acp.<bot>` and log paths per bot.

## Funnel and Poe setup

The Funnel path must match `--poe-path` exactly because Funnel strips the prefix before forwarding:

```bash
tailscale funnel --bg --set-path=/<bot-path> http://127.0.0.1:<port>
```

In Poe, create/configure the Server Bot:

- Server URL: `https://<host>.<tailnet>.ts.net/<bot-path>`
- Access key: same value as `POEACP_ACCESS_KEY`

## Verify

```bash
curl -i https://<host>.<tailnet>.ts.net/<bot-path> -X POST \
  -H 'Authorization: Bearer <poe-server-bot-secret>' \
  -H 'Content-Type: application/json' \
  -d '{"version":"1.0","type":"query","query":[]}'
```

Expect `200` with SSE headers. `401` means key mismatch. `404` means Funnel prefix and `--poe-path` do not match.

Then send a real Poe message and tail logs:

```bash
journalctl --user -u poe-acp-<bot> -f     # Linux
# or tail the bot-specific launchd stderr log on macOS
```

## Credential isolation rules

`--agent-dir` / `FIR_AGENT_DIR` isolates fir's `auth.json`, `settings.json`, sessions, installed fir packages/skills/extensions, cache, and model catalog.

It does not isolate external tools or environment inherited by the same Unix user: `~/.config/gh`, `~/.config/gcloud`, `~/.aws`, MCP server state, or API keys exported globally in the service environment. For those, use per-bot env files for keys, or a separate Unix user/host when true isolation is required.

## Handoff checklist

- [ ] `~/.config/poe-acp/<bot>/env` exists, mode `0600`, correct key.
- [ ] `config.json` has exact `bot_name` and pinned `defaults.model` if model-specific.
- [ ] Per-bot fir root created and logged into when credentials differ.
- [ ] Service uses `--agent-dir`, not shell-embedded `FIR_AGENT_DIR`, unless no alternative exists.
- [ ] Unique loopback port and supervisor unit label/name.
- [ ] Funnel `--set-path=/X` equals relay `--poe-path /X`.
- [ ] Curl smoke test returns `200`; Poe message round-trips.
