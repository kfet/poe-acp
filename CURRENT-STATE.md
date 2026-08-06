# CURRENT-STATE.md — live fleet audit, 2026-08-06

Captured by the operator directly from the four hosts. This is ground truth for
transcribing `bots/*.json`. Reproduce it faithfully.

poe-acp is **0.51.0 on all four**. fir differs on all four. fir is released from
`github.com/kfet/fir-dist` (tags `v0.9x.y`); latest is **0.95.0**.

fir-exts rev is identical on kopitwo / rn / airm1:
`2b2caa7a8504bc5c41aa88b17c3cbe8389884ee0` (committed 2026-08-03T11:35:02+03:00).
(ko1 not separately captured — assume same, verify at converge time.)

---

## 1. kopitwo — bot `two-fir` — linux/arm64 — systemd --user

unit: `~/.config/systemd/user/poe-acp-two-fir.service`
poe-acp 0.51.0 · **fir 0.93.0** · binary `~/.local/bin/poe-acp`

config `~/.config/poe-acp/bot-two-fir/config.json`:
```json
{
  "bot_name": "two-fir",
  "defaults": {
    "model": "anthropic/claude-opus-5",
    "thinking": "medium",
    "hide_thinking": false,
    "coalesce_ms": 3000,
    "coalesce_grid": true,
    "spinner_animate": false
  }
}
```

unit:
```
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
EnvironmentFile=%h/.config/poe-acp/bot-two-fir/env
ExecStart=%h/.local/bin/poe-acp --http-addr 127.0.0.1:8347 --poe-path /two-fir --config %h/.config/poe-acp/bot-two-fir/config.json --agent-cmd "fir --mode acp" --heartbeat-interval 3s --session-ttl 10m --enable-mcp-attach --introduction "two-fir — fir over ACP on kopitwo (arm64). Send !help (!login, !model, !status, !new). Any provider; default opus-5."
```
env file: `POEACP_ACCESS_KEY` only.

---

## 2. ko1 (kopione) — bot `kopi-fir` — linux/arm64 — systemd --user

unit: `~/.config/systemd/user/poe-acp-kopi-fir.service`
poe-acp 0.51.0 · **fir 0.94.0** · binary `~/.local/bin/poe-acp`

config `~/.config/poe-acp/bot-kopi-fir/config.json`:
```json
{
  "bot_name": "kopi-fir",
  "defaults": {
    "model": "anthropic/claude-opus-5",
    "thinking": "medium",
    "hide_thinking": false,
    "coalesce_ms": 3000,
    "coalesce_grid": true,
    "spinner_animate": false
  }
}
```

unit:
```
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
EnvironmentFile=%h/.config/poe-acp/bot-kopi-fir/env
ExecStart=%h/.local/bin/poe-acp   --http-addr 127.0.0.1:8347   --poe-path /kopi-fir   --config %h/.config/poe-acp/bot-kopi-fir/config.json   --agent-cmd "fir --mode acp"   --heartbeat-interval 3s --session-ttl 10m   --enable-mcp-attach   --introduction "kopi-fir — fir over ACP on kopione (arm64). Send !help (!login, !model, !status, !new). Any provider; default opus-4-8."
```
env file: `POEACP_ACCESS_KEY` only.

QUIRK: introduction says "default opus-4-8" but config default model is
`anthropic/claude-opus-5`. Stale prose. Transcribe as-is; flag it.

---

## 3. rn (sea-racknerd) — bot `sea-fir` — linux/amd64 — systemd --user

unit: `~/.config/systemd/user/poe-acp-sea-fir.service`
poe-acp 0.51.0 · **fir 0.95.0 (current)** · binary `~/.local/bin/poe-acp`

config `~/.config/poe-acp/bot-sea-fir/config.json`:
```json
{
  "bot_name": "sea-fir",
  "defaults": {
    "model": "anthropic/claude-opus-5",
    "thinking": "medium",
    "hide_thinking": false,
    "coalesce_ms": 3000,
    "coalesce_grid": true,
    "spinner_animate": false
  },
  "poe_mcp": true
}
```

unit:
```
ExecStart=%h/.local/bin/poe-acp \
  --http-addr 127.0.0.1:8347 \
  --poe-path /sea-fir \
  --config %h/.config/poe-acp/bot-sea-fir/config.json \
  --agent-cmd "fir --mode acp" \
  --heartbeat-interval 3s \
  --session-ttl 10m \
  --introduction "sea-fir — fir over ACP on sea-racknerd. Send !help for commands (!login, !model, !status, !new). Any provider; default opus-4-8/low."
Restart=on-failure
RestartSec=2s
```
env file: `POEACP_ACCESS_KEY` only.

QUIRKS: has `"poe_mcp": true` in config; unit does **NOT** pass
`--enable-mcp-attach` (the other two linux bots do); introduction prose again
says opus-4-8/low.

---

## 4. airm1 (kfetairm1) — bot `fir-air` — darwin/arm64 — launchd + homebrew

plist: `~/Library/LaunchAgents/dev.kfet.poe-acp.plist`
poe-acp 0.51.0 · **fir 0.90.1 (oldest)** · binary `/opt/homebrew/bin/poe-acp`

config: **default path** `~/.config/poe-acp/config.json` (no `--config` flag —
this host's poe-acp build/invocation has no `--config`):
```json
{
  "bot_name": "fir-air",
  "defaults": {
    "model": "anthropic/claude-opus-5",
    "thinking": "medium",
    "hide_thinking": false,
    "coalesce_ms": 3000,
    "coalesce_grid": true,
    "spinner_animate": false
  },
  "agent": { "profile": "fir" }
}
```

plist ProgramArguments:
```
/bin/sh -c 'set -a; . "$HOME/.config/poe-acp/env"; set +a; exec /opt/homebrew/bin/poe-acp -http-addr 127.0.0.1:8347 -poe-path /poe-acp -heartbeat-interval 3s -agent-cmd "fir --mode acp" -introduction "fir over ACP — one Poe conv = one ACP session. Send !help for commands (!login, !model, !status, !new)."'
```
PATH: `/Users/kfet/go/bin:/Users/kfet/.nvm/versions/node/v22.14.0/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin`
logs: `~/Library/Logs/poe-acp.{out,err}.log`

QUIRKS: single-dash flags; poe_path `/poe-acp` not `/fir-air`; no
`--session-ttl`, no `--enable-mcp-attach`, no `--config`; extra
`agent.profile` key in config; also has package
`github.com/anthropics/claude-plugins-official` installed (2 skills) in
addition to fir-exts.

macOS GOTCHA (learned the hard way): after editing the plist you MUST
`launchctl bootout gui/$UID/dev.kfet.poe-acp` then `launchctl bootstrap
gui/$UID <plist>`. `launchctl kickstart -k` re-runs the CACHED job definition
and silently ignores the edited plist.
