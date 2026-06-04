# Backlog

Deferred / candidate work for poe-acp. Versioned here so it survives
across sessions and is visible to anyone on the repo. Keep entries short;
move to a CHANGELOG `[Unreleased]` entry when picked up.

## Privileged commands (need operator identity first)

- **Operator allowlist** — gate privileged actions on Poe's `user_id`
  (config `operator_user_ids`). Prerequisite for everything below.
  Today the access key only proves "from Poe", not *which* user.
- **`!reexec` / graceful restart** — DIY listener-fd handoff (design in
  `docs/graceful-restart-design.md`). Swap the binary without dropping
  in-flight SSE. Expose as a **signal (SIGUSR2) or bearer-authed admin
  endpoint**, not a chat verb. Bonus: kills the deploy `ETXTBSY`.
- **`!update`** — fetch-latest + reexec. Operator-side only (update
  skill / admin endpoint); never a chat command.

## Commands

- **Wider agent-command passthrough** — today an allowlist
  (`reload/compact/session/changelog`) ∩ the agent's catalog. Consider
  an operator-config to extend, but keep curated (safety/noise; fir
  advertises ~70 incl. `install`/`uninstall`/`skill:*`).
- **`!think <level>`** — thinking-level override (mirrors `!model`).
  Uses fir's `session/set_config_option`, which is not yet ACP-standard.

## fir (upstream — enables live binary upgrades)

- **Expose `/n` (re-exec) in fir's ACP command registry.** The re-exec
  machinery exists (`pkg/session/reexec/` + `ReexecSidecar`, used by the
  TUI `/n`), but `pkg/modes/acp/commands.go` does NOT register it — so
  `/n` sent over observe/send hits the model as text (verified
  2026-06-03). Registering it would let a running ACP fir re-exec into a
  new binary **in place** (same PID/stdio FDs → the poe-acp parent never
  disconnects), upgrading the fir serving a *live* conversation with no
  relay drop. NON-TRIVIAL: unlike the TUI (single local session), an ACP
  fir serves the parent over stdio and may host multiple sessions; the
  re-exec'd process must resume the existing ACP connection (no
  re-`initialize`) and restore all sessions from the sidecar/store. Lives
  in the fir repo.

## acp-kit (reusable)

- **Move the auth-broker core to acp-kit** — the interactive-OAuth state
  machine (pending map, two-call flow, OfferLogin) is relay-agnostic;
  only the sigil + markdown rendering is Poe-specific. Reusable for
  slack-acp. Bigger refactor + release.
- **`AgentProc.AgentInfo()`** — expose `agentInfo{name,version}` from the
  initialize response so `!status` can show "fir 0.54.0". Currently
  skipped to avoid an acp-kit release.

## Deploy / ops

- **Makefile `deploy` ETXTBSY** — `scp` over a running binary fails;
  change the target to upload `<bin>.new` then remote `mv -f` (atomic,
  works on a busy binary). Currently done by hand.

## Cosmetic

- **`command.list()` duplicated description** — renders fir's name AND
  description which overlap: "Login with Anthropic (Login with Anthropic
  via OAuth)". `OfferLogin()` already avoids it (name only); align
  `list()`.
