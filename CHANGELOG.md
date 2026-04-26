# Changelog

## [Unreleased]

### Added

- Conversation resume: on the cold path for a conv_id, the relay now calls `session/list` + `session/resume` (when the agent advertises those unstable capabilities) so subsequent prompts continue where a prior agent session left off — the equivalent of `fir -c` per Poe conversation.
- Cold-path history seeding: when resume is unavailable (caps absent, no prior session, or resume errors), the first prompt to a new agent session is seeded with the full Poe transcript (role-tagged) so the agent has context for the latest user turn.

### Fixed

- Concurrent cold-path requests for the same conv_id no longer double-seed the winning session's history (race loser now correctly takes the hot path).
- GC no longer evicts a session while a prompt is in flight; long generations exceeding `--session-ttl` are protected by an in-use guard.

## [0.2.0] - 2026-04-22

### Added

- M0 skeleton: design doc and compiling scaffold for `poe-acp-relay`, an HTTP server that implements Poe's server-bot protocol and relays each conversation to a spawned ACP-speaking agent over stdio.
- Extracted to its own standalone Go module (`github.com/kfet/poe-acp-relay`) so it can be vendored/deployed independently of fir.
- M1 build: per-conversation cwd, heartbeat keep-alive, cancellation, session GC, and unit tests for the HTTP handler and router.
- Capture of `available_commands_update` notifications from the agent; M1 complete.
- Review pass cleanups.
- `--poe-path` flag for deploy-specific path mapping (e.g. Funnel prefix stripping).
- Poe server-bot protocol reference doc.
- Deployment section in the design doc capturing the Funnel prefix-strip gotcha.
- README.
