References are relative to /Users/kfet/dev/ai/poe-acp-relay/.fir/skills/deploy.

# Deploy Skill

Deploy `poe-acp-relay` to a remote host and expose it as a Poe server bot.

The target architecture: host on the user's tailnet with `tailscale funnel` fronting a loopback-bound relay. The relay spawns an ACP agent (e.g. `fir --mode acp`, `claude-code --acp`) per Poe conversation.

## Inputs the user must provide

Before deploying, confirm with the user:

1. **Host** — ssh-reachable hostname (e.g. `myhost` or `user@myhost`).
2. **Poe access key** — the server-bot secret from poe.com. Stored in `POEACP_ACCESS_KEY` on the host.
3. **ACP agent command** — default `fir --mode acp`. Ask if a different agent (e.g. `claude-code --acp`) is wanted.
4. **Funnel path layout** — pick one:
   - **(a) Dedicated host** — funnel `127.0.0.1:8080` on `/` → relay serves `/poe`. Public URL: `https://<host>.<tailnet>.ts.net/poe`.
   - **(b) Shared host with prefix** — funnel `127.0.0.1:<port>` on `/<prefix>` → relay serves `/<prefix>` (because funnel strips the prefix before forwarding). Use `--poe-path /<prefix>`.
5. **Permission policy** — `allow-all` (default), `read-only`, or `deny-all`.

## Steps

### 1. Ship the binary

Prefer `make deploy` — it builds all 5 cross-compile targets, detects the remote arch, and scp's the matching binary to `~/.local/bin/poe-acp-relay`:

```bash
make deploy HOST=<host>
```

Alternative (release already published to Homebrew tap):

```bash
ssh <host> 'brew install kfet/fir/poe-acp-relay'
```

Verify:

```bash
ssh <host> 'poe-acp-relay --version'
```

### 2. Ensure the ACP agent is installed on the host

Whatever `--agent-cmd` resolves to must be on the host's `PATH` for the user that runs the relay. For fir:

```bash
ssh <host> 'which fir && fir --version'
```

### 3. Configure Tailscale Funnel

**(a) Dedicated host, default path:**

```bash
ssh <host> 'tailscale funnel --bg 127.0.0.1:8080'
```

**(b) Shared host, prefix-mounted:**

```bash
ssh <host> 'tailscale funnel --bg --set-path=/poe-acp 127.0.0.1:8081'
```

Funnel **strips** the prefix before forwarding, so the relay must register its handler at the same prefix (see next step).

Confirm funnel is up:

```bash
ssh <host> 'tailscale funnel status'
```

### 4. Start the relay

Put the access key in a file or env (never on the CLI). Recommended: a small wrapper that sources secrets from `~/.config/poe-acp-relay/env`.

Example `~/.config/poe-acp-relay/env` on the host:

```
POEACP_ACCESS_KEY=<poe-server-bot-secret>
```

Then, for layout (a):

```bash
ssh <host> '
  set -a; . ~/.config/poe-acp-relay/env; set +a
  nohup poe-acp-relay \
    -http-addr 127.0.0.1:8080 \
    -agent-cmd "fir --mode acp" \
    >> ~/.local/state/poe-acp-relay.log 2>&1 &
'
```

For layout (b) with prefix `/poe-acp` on port `8081`:

```bash
ssh <host> '
  set -a; . ~/.config/poe-acp-relay/env; set +a
  nohup poe-acp-relay \
    -http-addr 127.0.0.1:8081 \
    -poe-path /poe-acp \
    -agent-cmd "fir --mode acp" \
    >> ~/.local/state/poe-acp-relay.log 2>&1 &
'
```

For a more durable setup, prefer a `tmux` window or a user-level `systemd` / `launchd` unit. A systemd template is not yet in-repo (see README "Deployment"), but minimally:

```ini
# ~/.config/systemd/user/poe-acp-relay.service
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

```bash
ssh <host> 'systemctl --user daemon-reload && systemctl --user enable --now poe-acp-relay'
```

### 5. Verify end-to-end

From your workstation:

```bash
# (a) default layout
curl -i https://<host>.<tailnet>.ts.net/poe -X POST \
     -H 'Authorization: Bearer <poe-server-bot-secret>' \
     -H 'Content-Type: application/json' \
     -d '{"version":"1.0","type":"query","query":[]}'

# (b) prefix layout
curl -i https://<host>.<tailnet>.ts.net/poe-acp -X POST ...
```

Expect a `200` with an SSE stream (even on malformed query). A `401` means the bearer token didn't match `POEACP_ACCESS_KEY`; a `404` means the path layout is wrong.

Then in the Poe bot settings:

- **Server URL:** `https://<host>.<tailnet>.ts.net/<poe-path>`
- **Access Key:** same value as `POEACP_ACCESS_KEY`

Send a test message from Poe and confirm a reply appears.

### 6. Tail logs for the first few conversations

```bash
ssh <host> 'tail -f ~/.local/state/poe-acp-relay.log'
```

Look for the per-conversation cwd, `initialize` handshake, and `session/prompt` traffic. Cancellation / heartbeat / GC messages confirm the full state machine is exercised.

## Upgrading a live deployment

Two options:

- **Homebrew-managed:** `ssh <host> 'brew upgrade poe-acp-relay'`, then restart the service/tmux/nohup process.
- **Direct hotfix:** from the repo, `make deploy HOST=<host>`. This scp's the matching arch binary to `~/.local/bin/poe-acp-relay` and calls `--version`. Restart the process afterward.

## Common pitfalls

- **Prefix layout 404s.** Funnel `--set-path=/X` strips `/X` before forwarding. The relay must register at `/X`, not at `/poe`. Set `--poe-path /X`.
- **401 from Poe.** Bearer key on the host doesn't match what Poe is sending. Check `POEACP_ACCESS_KEY` in the service env.
- **Agent not found.** `--agent-cmd` runs as the relay's user; ensure the agent binary is on that user's `PATH`.
- **Process dies on reboot.** v1 is manual — use systemd user unit (above) or tmux.
- **Multiple bots on one host.** Each bot = one relay process on its own loopback port + its own funnel prefix. `POEACP_ACCESS_KEY` is per-bot.

## Post-deploy handoff checklist

- [ ] `poe-acp-relay --version` matches the intended release on the host.
- [ ] `tailscale funnel status` shows the expected port/prefix mapping.
- [ ] Curl smoke test returns `200` with SSE headers.
- [ ] Poe test message round-trips.
- [ ] Process supervisor (systemd unit / tmux window / launchd plist) is in place.
- [ ] Access key is stored in an env file with mode `0600`, not on the CLI.
