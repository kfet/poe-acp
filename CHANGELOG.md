# Changelog

## [Unreleased]

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
