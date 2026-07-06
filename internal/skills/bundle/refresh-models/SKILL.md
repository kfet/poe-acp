---
builtin: true
name: refresh-models
description: Make a newly added agent model visible in the Poe bot's model dropdown after the underlying ACP agent's catalog changes.
---

# Refresh model list in Poe

Each relay **worker** probes the ACP agent for its model catalog at startup and
embeds the result into the `bot/fetch_settings` response; on boot it also
auto-invalidates Poe's cached settings when the schema hash changed (requires
`bot_name` set — see Notes). There is no live re-probe of an already-running
worker, so a model added to the agent's config after the worker started is
invisible until a **new worker** probes it.

On poe-acp ≥ 0.36.0 (master/worker supervisor) you get a fresh worker — and
therefore a fresh probe + settings push — **without a hard restart**: a graceful
`reload` forks a new worker on the same binary, which re-runs the full init
(re-loads `config.json`, starts a fresh agent, re-probes, re-pushes settings),
then drains the old worker's in-flight Poe SSE to completion before it exits.
The supervisor PID never moves and live conversations are never dropped. **This
is the preferred path** — you can run it even while the bot is actively serving
(including the very turn that triggers it).

## Trigger

User added/changed a provider or model on the agent side (e.g. edited the
agent's `models.json`) and wants it to appear in the Poe model picker.

## Steps

1. **Seamless re-probe (poe-acp ≥ 0.36.0 — preferred).** SIGHUP the supervisor
   for a drained worker swap:
   - **Linux:** `systemctl --user reload poe-acp-<bot>` (needs the
     `Type=notify` + `ExecReload=/bin/kill -HUP $MAINPID` drop-in;
     `systemctl --user reload` fails cleanly if absent — then add the drop-in
     or fall back to restart).
   - **macOS:** `launchctl kill SIGHUP gui/$UID/<launchd-label>` (commonly
     `dev.<you>.poe-acp`).
   - **Any OS:** `POST /admin/reexec` on the relay also triggers the swap.

   The new worker re-loads `config.json` too, so a `defaults.*` edit is picked
   up by the same reload — no separate step.

   *Fallback (pre-0.36.0, or no supervisor shim installed):* hard-restart, which
   briefly drops any in-flight turn (Poe redrives from transcript, so no data is
   lost):
   - **Linux:** `systemctl --user restart poe-acp-<bot>`
   - **macOS:** `launchctl kickstart -k gui/$UID/<launchd-label>`

2. Verify the fresh worker probed and pushed. If the unit's stdout reaches a log
   (see locations below — note some hosts have no persistent user journal, in
   which case verify by process tree instead), look for two lines from the new
   worker:
   - `probed N models (current=…)` — N should reflect the new total. If
     unchanged, the agent didn't actually see the new entries; fix on the agent
     side first.
   - `settings refetch: ok (hash=…)` — hash MUST differ from the prior boot.
     `schema unchanged` means the model set is identical to last boot and Poe
     keeps serving its cached settings.

   **No log? Verify by process tree.** A successful graceful reload shows the
   supervisor PID unchanged with a *new* worker PID (and, momentarily, the old
   worker draining alongside it), each worker parenting its own fresh agent:
   ```bash
   sup=$(systemctl --user show -p MainPID --value poe-acp-<bot>)
   ps --ppid $sup -o pid,etime,cmd            # workers under the supervisor
   # for each worker pid W:  ps --ppid W -o pid,etime,cmd | grep 'mode acp'
   ```
   A worker younger than the supervisor with its own fresh agent process is
   proof the re-probe ran.

3. To make a new model the bot **default**, set `defaults.model` in the relay's
   `config.json` to the fully-qualified id (`<provider>/<model>`) as it appears
   in the probed list, then reload again (step 1). A warning line
   `paramctl: configured defaults.model … not in the agent's available list`
   means the id doesn't match anything fir actually exposes — fix the id and
   reload.

Log locations:
- macOS: `~/Library/Logs/poe-acp.err.log`
- Linux: `journalctl --user -u poe-acp-<bot> -f` (only if a persistent user
  journal exists; `No journal files were found` ⇒ use the process-tree check)

## Notes

- The bot must have `bot_name` set in `config.json` for the relay to
  auto-invalidate Poe's cached settings — otherwise the hash bump has nothing to
  push and the dropdown stays stale until the user manually reopens the bot
  config in Poe.
- The agent's catalog is whatever `<agent-cmd>` reports via ACP `session/new`.
  For `fir --mode acp` that's everything `fir --list-models` shows; verify there
  first if a model is missing post-reload.
- **Why reload suffices (and restart is overkill):** the catalog probe and the
  settings push live in every worker's startup path, and a graceful reload spins
  up a brand-new worker. So the "restart to refresh" instinct is a relic of the
  pre-supervisor design — since 0.36.0 the seamless worker swap does the same
  re-probe without the blip.
