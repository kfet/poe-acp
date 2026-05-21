---
builtin: true
name: refresh-models
description: Make a newly added agent model visible in the Poe bot's model dropdown after the underlying ACP agent's catalog changes.
---

# Refresh model list in Poe

The relay probes the ACP agent for its model catalog **once at process start** and embeds the result into the `bot/fetch_settings` response. New models added to the agent's config after launch are invisible until the relay restarts **and** Poe refetches.

## Trigger

User added/changed a provider or model on the agent side (e.g. edited the agent's `models.json`) and wants it to appear in the Poe model picker.

## Steps

1. Restart the relay supervisor on the host running it:
   - **macOS:** `launchctl kickstart -k gui/$UID/<launchd-label>` (commonly `dev.<you>.poe-acp`)
   - **Linux:** `systemctl --user restart <unit>` (commonly `poe-acp.service`)
2. Tail the relay log and verify two lines from this boot:
   - `probed N models (current=…)` — N should reflect the new total. If unchanged, the agent didn't actually see the new entries; fix on the agent side first.
   - `settings refetch: ok (hash=…)` — hash MUST differ from the prior boot. `schema unchanged` means the model set is identical to last boot and Poe will keep serving its cached settings.
3. To make a new model the bot **default**, set `defaults.model` in the relay's `config.json` to the fully-qualified id (`<provider>/<model>`) as it appears in the probed list, then restart again. A warning line `paramctl: configured defaults.model … not in the agent's available list` means the id doesn't match anything fir actually exposes — fix the id and restart.

Log locations:
- macOS: `~/Library/Logs/poe-acp.err.log`
- Linux: `journalctl --user -u <unit> -f`

## Notes

- The bot must have `bot_name` set in `config.json` for the relay to auto-invalidate Poe's cached settings — otherwise the hash bump has nothing to push and the dropdown stays stale until the user manually reopens the bot config in Poe.
- The agent's catalog is whatever `<agent-cmd>` reports via ACP `session/new`. For `fir --mode acp` that's everything `fir --list-models` shows; verify there first if a model is missing post-restart.
