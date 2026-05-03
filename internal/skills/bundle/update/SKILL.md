---
name: update
description: Update poe-acp-relay on a single host to the latest released version and restart its supervisor (systemd or launchd) so the new binary is live.
---

# Update Skill

Upgrade `poe-acp-relay` on **one** host (local or remote) and restart the supervisor. Use after a release publishes or when a specific host is stale.

> Releasing lives in `.fir/skills/release/SKILL.md`. For multi-host rollouts, repeat this skill per host.

## Inputs

Confirm with the user before acting:

1. **Host** — `local` or `user@host`. Default to local if omitted.
2. **Target version** — default: latest `vX.Y.Z` tag on `origin`. Override only if the user asks.

## Steps

### 1. Determine target version

```bash
git fetch --tags origin
git tag --sort=-v:refname | head -1
```

If `VERSION` is ahead of every pushed tag, an unpublished release exists — stop and run the `release` skill first.

### 2. Probe the host

Detect installed version, install method, and supervisor. For remote use `ssh <host>` prefix; for local run directly.

```bash
poe-acp-relay --version 2>/dev/null || echo not-installed
brew list --versions poe-acp-relay 2>/dev/null         # brew install?
ls -l ~/.local/bin/poe-acp-relay 2>/dev/null           # direct deploy?
systemctl --user is-active poe-acp-relay 2>/dev/null   # Linux supervisor
launchctl list 2>/dev/null | grep -i poe-acp-relay     # macOS supervisor
```

If installed version already equals target, tell the user and stop unless they want a forced restart.

### 3. Pick the upgrade path

**Brew + launchd (typical macOS):**
```bash
brew update && brew upgrade poe-acp-relay
launchctl kickstart -k gui/$UID/<label>
```
Find `<label>` in `~/Library/LaunchAgents/dev.*.poe-acp-relay.plist` (e.g. `dev.<user>.poe-acp-relay`). On remote, use `gui/$(id -u)/<label>` inside the ssh command.

**Brew + systemd (typical Linux):**
```bash
brew update && brew upgrade poe-acp-relay
systemctl --user restart poe-acp-relay
```

**Direct deploy (`~/.local/bin`, hotfix):**
From the repo:
```bash
make deploy HOST=<host>
ssh <host> 'systemctl --user restart poe-acp-relay'   # or launchctl kickstart
```

If `brew upgrade` reports "already up-to-date" but the version still lags, the tap index is stale — re-run `brew update`. Persistent miss → fall back to `make deploy`.

### 4. Verify

```bash
poe-acp-relay --version                       # must equal target
systemctl --user is-active poe-acp-relay      # → active   (Linux)
launchctl print gui/$UID/<label> | grep state # → state = running  (macOS)
```

If the host has a known public Funnel URL + access key, optional smoke:

```bash
curl -i https://<host>.<tailnet>.ts.net/<poe-path> -X POST \
  -H 'Authorization: Bearer <key>' -H 'Content-Type: application/json' \
  -d '{"version":"1.0","type":"query","query":[]}'
```

Expect `200` with SSE headers.

### 5. Report

One-line summary: `<host>: <old> → <new>, supervisor active`. If anything failed, surface the error and stop — do not paper over.

## Pitfalls

- **Stale tap** — `brew upgrade` is a no-op until `brew update` refreshes the tap.
- **Missed restart** — replacing the binary on disk does not reload the running process. Always restart the supervisor.
- **launchd label varies** — embeds the deploying user (`dev.<user>.poe-acp-relay`). Read it from the plist, don't guess.
- **Mixed install methods** — a host may have both `~/.local/bin/poe-acp-relay` and a brew copy; the supervisor's `ExecStart` pins one. Upgrade whichever the unit/plist points at.
- **Active conversations drop** — restart kills in-flight SSE; Poe will retry. Avoid during peak use if avoidable.

## Checklist

- [ ] Target version confirmed (latest pushed tag).
- [ ] Install method + supervisor identified on the host.
- [ ] Binary upgraded via the matching path.
- [ ] Supervisor restarted.
- [ ] `poe-acp-relay --version` matches target.
- [ ] Service active.
