# Brief: per-host agent processes in poe-acp ("host switching" for ssh-fir bots)

Repo: ~/src/poe-acp (main @ 0e1750b). Work on a NEW git worktree + branch
(work/agent-host-pool), not in the main worktree — it is shared by other agents.

## Why

The `sea-miki` bot runs `--agent-cmd "ssh -T miki .local/bin/fir --mode acp"`
with `agent_ssh_host: miki`. That transport works well. The owner now wants the
user to CHOOSE which host the fir agent runs on, from the Poe Options panel,
per conversation.

Today's `hosts` / `defaults.host` dropdown does NOT do this: per
docs/poe-acp-design.md it only sends `_meta.host` on `session/new` as a hint to
the ONE agent process (acp-tmux semantics — where it places a tmux pane). fir in
`--mode acp` cannot relocate itself. So real host switching means the relay must
run one agent PROCESS per host — the doc's explicitly-deferred "multi-agent
pool" (docs/poe-acp-design.md:43, :140, :379).

## What to build

A lazily-started, per-host agent process pool, strictly opt-in.

Sketch (you own the final shape; justify deviations in the commit):

- Config: a way to declare, per host entry, the agent command that reaches it,
  e.g. extend `hosts[i]` with `agent_cmd` (and reuse `value` as the ssh
  destination for provisioning), or a separate `agent_hosts` list. Reserved
  `"local"` sentinel must keep meaning "the relay's own --agent-cmd".
  Unknown keys still fail loudly at boot.
- Per-host process: each host gets its own child + ACP handshake + session map.
  Started lazily on first conversation that resolves to it. Prefer never reaping
  a pool entry while it still has live sessions.
- `agent_ssh_host` (cwd + attachment provisioning over ssh/tar, and fetch-back
  of agent-produced attachments) must become PER-HOST, derived from the chosen
  host, not a single global.
- Host stays CREATE-TIME only and pinned for the conversation's life, exactly as
  documented today; a mid-conversation change keeps replying the existing
  `_(host takes effect on the next conversation…)_` note.
- Boot-time model probe / parameter_controls / command broker / drain+swap
  supervisor / GC currently assume exactly one agent. Audit every one of those
  assumptions. Suggested: probe only the DEFAULT host at boot for the model
  catalog, start other hosts lazily; a host whose catalog differs is not a boot
  error.
- Back-compat is non-negotiable: with no per-host agent commands configured the
  binary must behave EXACTLY as today (single process; `_meta.host` hint
  semantics unchanged for acp-tmux bots).

## Definition of done

- `make` (fmt/vet/lint/test) passes; new unit tests cover config validation,
  host resolution, pool lifecycle, and per-host provisioning.
- README.md + CHANGELOG.md + docs/poe-acp-design.md updated (the design doc
  currently asserts "no pool" in several places — fix those).
- Follow your `review-and-fix` then `ship-it` skills: merge to main, cut and
  publish a release.
- Then STOP and report. Do NOT deploy to sea-racknerd and do not touch any
  running bot config — the owner's agent (sea-miki) does the rollout and the
  sea-miki config change.

## Reporting

Keep a short status file current at ~/src/poe-acp-hostswitch-STATUS.md (design
decided / tests green / released version). Another agent polls it.
