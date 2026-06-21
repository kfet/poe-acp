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

## Steps

### 1. Ship the binary

`make deploy` cross-builds, detects remote arch, scp's the right binary to `~/.local/bin/poe-acp`, and runs `--version`:

```bash
make deploy HOST=<host>
```

Alternatively (released to tap):

```bash
ssh <host> 'brew install kfet/ai/poe-acp'
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
Type=notify
ExecStart=%h/.local/bin/poe-acp -http-addr 127.0.0.1:8080 -agent-cmd "fir --mode acp"
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=default.target
```

`Type=notify` (poe-acp ≥ 0.36.0): the process systemd launches and tracks
as `MainPID` is the **supervisor S** — a tiny master that binds the listen
socket once and forks a worker to run the relay. S signals readiness
(`READY=1`) once its first worker is serving, so systemd does not hang in
`activating`. `MainPID` is **stable across upgrades**: S never exits during
a worker swap, so the v0.35.0 `NotifyAccess=all` + `MAINPID` re-point
handshake is gone — **do not add `NotifyAccess=all`**.

`ExecReload=/bin/kill -HUP $MAINPID` enables **graceful zero-downtime
restart**: `systemctl --user reload poe-acp` sends `SIGHUP` to S, which
forks a NEW worker (the new on-disk binary) on the same socket, lets it
start accepting, then tells the old worker to stop accepting and drain its
in-flight Poe SSE replies to completion before exiting. In-flight streams
finish (no truncation), new POSTs hit the new worker (no `ECONNREFUSED`),
and S — the tracked `MainPID` — never moves. A plain `restart` still works
but drops in-flight streams.

##### Seamless upgrades (the cutover rule)

A `SIGHUP`-driven worker swap is handled by the **currently running**
supervisor, so it only works once the running binary already speaks the
shim protocol (≥ 0.36.0):

1. `make deploy HOST=<host>` — scp the new binary onto the host.
2. **FIRST cutover onto a v0.36.0 binary must be a `restart`, not a
   `reload`:** `systemctl --user restart poe-acp` (a brief blip). The
   currently-running ≤ 0.35.0 binary uses the old server-is-PID model and
   does not know how to fork/drain a worker, so reloading *through* it
   would not perform a shim swap.
3. **Only after the v0.36.0 supervisor is the running one** do subsequent
   `systemctl --user reload poe-acp` become true zero-downtime worker
   swaps (in-flight streams preserved, `MainPID` unchanged, no blip).

In short: restart once to get onto the shim; reload forever after. The
identical rule applies on launchd (`kickstart -k` once, then
`launchctl kill SIGHUP`) — see the macOS section.



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

**Graceful (zero-downtime) restart on macOS.** poe-acp ≥ 0.36.0 runs a
master/worker supervisor, so `SIGHUP` to the launchd-tracked process (the
supervisor S) triggers a **drained worker swap** — identical behaviour to
systemd `reload`, and safe on launchd because S never exits, so launchd
never sees its tracked PID vanish and never KeepAlive-relaunches into an
`EADDRINUSE` crash-loop. To upgrade without dropping in-flight Poe SSE
replies, send the signal instead of `kickstart -k`:

```bash
launchctl kill SIGHUP gui/$UID/dev.<you>.poe-acp     # drained worker swap (preserves in-flight streams)
# or: kill -HUP $(launchctl print gui/$UID/dev.<you>.poe-acp | awk '/pid =/{print $3}')
```

S forks a new worker on the new on-disk binary, lets it start accepting,
then drains the old worker to completion; launchd sees the same job and the
same PID throughout. `kickstart -k` still hard-restarts when you do not care
about mid-stream survival.

> **Cutover rule (same as systemd):** the swap is driven by the running
> supervisor, so the **first cutover onto a v0.36.0 binary must be a
> `launchctl kickstart -k`** (restart) — the older ≤ 0.35.0 binary used the
> server-is-PID model and, critically, its `SIGHUP` re-exec self-exits,
> which under launchd's `KeepAlive` raced the relaunch into `EADDRINUSE`.
> Only after 0.36.0 is the running supervisor does `launchctl kill SIGHUP`
> become a seamless worker swap. Restart once; SIGHUP-swap forever after.

A rare **supervisor self-upgrade** (when S's own tiny code changes) is a
distinct trigger: `launchctl kill USR2 gui/$UID/dev.<you>.poe-acp` drains
the current worker, then re-execs S in place on the same socket while
quiescent. Default updates are worker swaps (`SIGHUP`); reach for `USR2`
only when the supervisor binary itself changed.

### 5. Seed agent notes (if repo paths known)

If you know where the operator's source repos live on the host, write them so spawned agents can find them:

```bash
ssh <host> 'mkdir -p ~/.local/state/poe-acp/notes && cat > ~/.local/state/poe-acp/notes/repos.md' <<'EOF'
# Repos on this host

- /Users/<you>/dev/poe-acp
- /Users/<you>/dev/fir
EOF
```

Free-form Markdown. Agents read this when the user references repos by name. Skip if unknown.

### 6. Verify

From your workstation:

```bash
curl -i https://<host>.<tailnet>.ts.net/<poe-path> -X POST \
  -H 'Authorization: Bearer <poe-server-bot-secret>' \
  -H 'Content-Type: application/json' \
  -d '{"version":"1.0","type":"query","query":[]}'
```

Expect `200` with SSE headers. `401` → key mismatch. `404` → path layout mismatch (see Funnel prefix note).

Then set the Poe bot's Server URL to `https://<host>.<tailnet>.ts.net/<poe-path>` and the access key to the same value as `POEACP_ACCESS_KEY`. Send a test message from Poe and confirm a reply.

### 7. Tail logs during first conversations

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

## Multi-bot on one host

Run several Poe bots from a single host by giving each its own config dir, supervisor unit, loopback port, and funnel prefix.

### Layout

```
~/.config/poe-acp/
  bot-foo/
    config.json          # bot_name="foo", model defaults, etc.
    env                  # POEACP_ACCESS_KEY=<foo's key>, mode 0600
    skills/              # host-supplied skills, foo-specific (optional)
      release-foo/
        SKILL.md
    state/               # auto-created; per-conv state, schema-hash file
  bot-bar/
    config.json
    env
    state/
```

Each bot gets its own `--config` path. The relay derives `--state-dir` to `<dirname(config)>/state` automatically when you pass `--config` and leave `--state-dir` empty, so you only need to set one path per bot. Host skills under `<dirname(config)>/skills/` are loaded automatically — no config.json key needed.

### Service units

One unit per bot, named `poe-acp-<bot>.service` (Linux) or `dev.fir.poe-acp.<bot>.plist` (macOS). On Linux:

```ini
# ~/.config/systemd/user/poe-acp-foo.service
[Unit]
Description=poe-acp (foo)
After=network-online.target

[Service]
EnvironmentFile=%h/.config/poe-acp/bot-foo/env
Type=notify
ExecStart=%h/.local/bin/poe-acp \
  --http-addr 127.0.0.1:8347 \
  --poe-path /foo \
  --config %h/.config/poe-acp/bot-foo/config.json
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure

[Install]
WantedBy=default.target
```

(`Type=notify` + `ExecReload` are required for graceful reload to work under systemd — see the single-bot notes and the "Seamless upgrades" cutover rule above. `NotifyAccess=all` is **no longer used** (poe-acp ≥ 0.36.0): the supervisor is a stable `MainPID`. The same restart-once-then-SIGHUP-swap-forever rule applies per bot, on both systemd and launchd.)


```bash
systemctl --user daemon-reload
systemctl --user enable --now poe-acp-foo.service poe-acp-bar.service
```

### Funnel layout (prefix, single port)

Run all bots behind one funnel port with distinct prefixes. Each bot listens on its own loopback port; each prefix targets that port. The relay's `--poe-path /<prefix>` must equal the funnel prefix because funnel strips it before forwarding.

```bash
tailscale funnel --bg --set-path=/foo http://127.0.0.1:8347
tailscale funnel --bg --set-path=/bar http://127.0.0.1:8348
```

Public URLs become `https://<host>.<tailnet>.ts.net/foo` and `.../bar`. Configure each Poe server bot's URL accordingly.

### Minimal host skill example

```
~/.config/poe-acp/bot-foo/skills/foo-runbook/SKILL.md
```

```markdown
---
name: foo-runbook
description: Foo-specific operations runbook — restart, log locations, on-call paging.
---

# Foo runbook

Service unit: `poe-acp-foo.service`.
Logs: `journalctl --user -u poe-acp-foo -f`.
…
```

To override a built-in (e.g. replace `deploy` with a foo-specific procedure), drop a `SKILL.md` under `skills/deploy/` with `name: deploy` — last-wins by name silences the built-in. New conversations pick up the change without restarting poe-acp. Verify the current catalog with `poe-acp --print-catalog --config <path>`.
