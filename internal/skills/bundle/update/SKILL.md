---
builtin: true
name: update
description: Update poe-acp on a single host, or restart / reload a running bot. Covers supervisor control (systemd or launchd) and graceful SIGHUP worker swap vs hard restart.
---

# Update Skill

> **Fleet hosts: converge is the only sanctioned way to touch a host.** For a
> bot with a spec in `bots/<name>.json`, version moves go through the lock:
> `scripts/converge.sh --tot` (rewrites `dist.lock`; review + commit), then
> `scripts/converge.sh <bot> --apply` per host. Do not hand-upgrade a fleet
> host to an unlocked version. The steps below remain valid for non-fleet
> hosts and for the supervisor-control mechanics converge itself relies on
> (restart/reload semantics, cutover rules).

Upgrade `poe-acp` on **one** host (local or remote) and restart the supervisor. Use after a release publishes, when a specific host is stale, **or whenever you just need to restart or reload a running bot** — §3 and the pitfalls below are the authority on supervisor control.

> **Restarting the bot you are talking through is an ordinary, inline action.** Run the restart directly. Do not detach it, do not delay it, do not warn the user first. The open SSE reply is dropped, but the conversation is on disk: Poe redrives it and the next turn resumes with full history. Scheduling a delayed or `setsid`-detached restart is always wrong — see the pitfalls.

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
poe-acp --version 2>/dev/null || echo not-installed
brew list --versions poe-acp 2>/dev/null         # brew install?
ls -l ~/.local/bin/poe-acp 2>/dev/null           # direct deploy?
systemctl --user is-active poe-acp 2>/dev/null   # Linux supervisor
launchctl list 2>/dev/null | grep -i poe-acp     # macOS supervisor
```

If installed version already equals target, tell the user and stop unless they want a forced restart.

### 3. Pick the upgrade path

**Brew + launchd (typical macOS):**
```bash
brew update && brew upgrade poe-acp
launchctl kickstart -k gui/$UID/<label>
```
Find `<label>` in `~/Library/LaunchAgents/dev.*.poe-acp.plist` (e.g. `dev.<user>.poe-acp`). On remote, use `gui/$(id -u)/<label>` inside the ssh command.

Never schedule a delayed reloader and never use `launchctl bootout` + `bootstrap` for a routine restart. `kickstart -k` stops and immediately relaunches the already-registered job without changing the plist or racing launchd registration.

**Graceful (zero-downtime) restart.** poe-acp ≥ 0.36.0 runs a master/worker supervisor: the tracked process (supervisor S) binds the socket once and forks a worker for the relay. To upgrade without dropping in-flight Poe SSE replies, signal S to do a **drained worker swap** instead of hard-restarting: `launchctl kill SIGHUP gui/$UID/<label>` (launchd) or `systemctl --user reload poe-acp` (systemd). S forks a new worker on the new binary, lets it start accepting, then drains the old worker to completion before it exits — no `ECONNREFUSED`, no truncated replies, and S's tracked PID never moves. Swap the binary on disk first, then SIGHUP/reload.

> **The swap is driven by the *currently running* supervisor**, so it requires poe-acp ≥ 0.36.0 to already be running. On systemd the unit must be `Type=notify` + `ExecReload=/bin/kill -HUP $MAINPID` (no `NotifyAccess=all` — that was the retired v0.35.0 MAINPID handshake). The **first cutover onto a v0.36.0 binary must be a plain restart** (`systemctl --user restart` / `launchctl kickstart -k`, a brief blip), because the older ≤ 0.35.0 binary uses the server-is-PID model and cannot perform a shim swap. Only after 0.36.0 is the running supervisor do subsequent SIGHUP/reload become seamless worker swaps — **identical and safe on BOTH launchd and systemd** (this supersedes the old, incorrect claim that launchd SIGHUP re-exec was always safe; under the pre-0.36.0 model it raced launchd's KeepAlive into an `EADDRINUSE` crash-loop). See the deploy skill's "Seamless upgrades" section.

Use plain `restart`/`kickstart -k` when mid-stream survival does not matter.

**Brew + systemd (typical Linux):**
```bash
brew update && brew upgrade poe-acp
systemctl --user restart poe-acp
```

**Direct deploy (`~/.local/bin`, hotfix):**
From the repo:
```bash
make deploy HOST=<host>
ssh <host> 'systemctl --user restart poe-acp'   # or launchctl kickstart
```

If `brew upgrade` reports "already up-to-date" but the version still lags, the tap index is stale — re-run `brew update`. Persistent miss → fall back to `make deploy`.

### 4. Verify

```bash
poe-acp --version                       # must equal target
systemctl --user is-active poe-acp      # → active   (Linux)
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
- **Upgrading the *agent* binary (e.g. `fir update`) is the same trap** — each relay worker holds ONE long-lived agent process shared by all its conversations (a Poe conv is an ACP *session*, not a process). A new agent binary on disk is inert until the worker is cycled: `systemctl --user reload` / SIGHUP forks a new worker with a fresh agent, so **new** conversations get the new agent; conversations pinned to the draining old worker keep the old binary until it exits. Verify with the process tree (`ps --ppid <supervisor>`, then `readlink /proc/<agent-pid>/exe`), not with `<agent> --version` on disk.
- **launchd label varies** — embeds the deploying user (`dev.<user>.poe-acp`). Read it from the plist, don't guess.
- **Mixed install methods** — a host may have both `~/.local/bin/poe-acp` and a brew copy; the supervisor's `ExecStart` pins one. Upgrade whichever the unit/plist points at.
- **In-flight turn interrupts briefly** — a plain `restart`/`kickstart -k` ends the open SSE response; Poe retries and the conversation redrives from transcript, so nothing is lost. Prefer the graceful SIGHUP worker swap (see §3) to preserve mid-stream replies; otherwise avoid hard-restarting during peak use if avoidable.
- **Never detach or delay a restart** — no `setsid`, no `sleep N`, no one-shot timer unit, no background reloader. Beyond being unnecessary (previous pitfall), it does not even work: under systemd the unit's default `KillMode=control-group` tears down the whole **cgroup**, and `setsid` only escapes the process *group*, not the cgroup — a detached restart command sits inside its own blast radius. It appears to succeed only because `systemctl` hands the job to systemd over D-Bus before it is killed.
- **Do not mutate launchd for config-only changes** — if only `config.json`, env, or the binary changed, restart with `launchctl kickstart -k gui/$UID/<label>`. Do not edit plist, create one-shot reloader jobs, or run bootout/bootstrap unless first installing/removing a service or intentionally changing the plist registration.

## Checklist

- [ ] Target version confirmed (latest pushed tag).
- [ ] Install method + supervisor identified on the host.
- [ ] Binary upgraded via the matching path.
- [ ] Supervisor restarted.
- [ ] `poe-acp --version` matches target.
- [ ] Service active.
