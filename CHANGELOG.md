# Changelog

## [Unreleased]

### Added

- Animated `> _Thinking._` spinner shown while the agent is in its thinking phase and `hide_thinking=true`, so the user sees liveness instead of a blank reply. Cycles 1→3 dots every 1.5s via `replace_response`, and is cleared the moment the first real message chunk arrives. When `hide_thinking=false` the heartbeat keeps its prior zero-width-space behaviour (thoughts are streamed as a blockquote already).
- Attachments support. Poe-supplied attachments on the latest user message are forwarded to the agent as ACP content blocks alongside the text prompt. When Poe has computed `parsed_content` (text-ish files) and the agent advertises `promptCapabilities.embeddedContext`, the relay emits `ContentBlock::Resource` with `TextResourceContents` so the agent has the text inline; otherwise the relay falls back to `ResourceLink` (the mandatory ACP baseline). The relay never downloads files itself. Only the latest user turn's attachments are forwarded — prior turns are part of the agent session history. Attachments with empty URLs are dropped. New `--allow-attachments` flag (default true) gates both `allow_attachments` and `expand_text_attachments` in the settings response.
- `acpclient.Caps.EmbeddedContext` parsed from `agentCapabilities.promptCapabilities.embeddedContext`.

## [0.6.0] - 2026-04-29

### Added

- Interactive OAuth login over Poe chat. Users send `/login` to list available providers and `/login <provider>` (e.g. `/login anthropic`) to start a flow. Relay surfaces fir's auth URL as a chat message; the user opens it, pastes the redirect URL back into the chat, and the relay completes the flow via fir. Backed by a new `authbroker` package and a new `_meta.auth.interactive` extension on the ACP `authenticate` RPC. Per-conversation isolation: concurrent logins from different conversations carry distinct opaque ids and never cross-contaminate. `/cancel-login` aborts an in-flight login. Requires fir with the matching `_meta.auth.interactive` support.
- `acpclient.AgentProc.AuthMethods()` and `Authenticate(ctx, methodID, id, redirect, cancel)` for ACP authentication plumbing.

## [0.5.0] - 2026-04-30

### Added

- JSON config file at `$XDG_CONFIG_HOME/poe-acp-relay/config.json` (override with `--config /path`). Holds the bot's identity (`bot_name`), per-conversation defaults (`defaults.model`, `defaults.thinking`, `defaults.hide_thinking`), and reserved `agent.profile` field. Unknown keys fail loudly at boot (DisallowUnknownFields). See `docs/config.example.json`. Empty/missing file preserves zero-config behavior.
- Auto-invalidation of Poe's cached settings response when `parameter_controls` change between boots. Relay hashes the schema, persists to `<state-dir>/last_schema_hash`, and POSTs `https://api.poe.com/bot/fetch_settings/<bot_name>/<key>/1.1` on change. Skipped when `bot_name` is unset.

### Changed

- `paramctl.Build` and the new `paramctl.Resolve` decouple the operator-configured default from the agent's currently-running model. Resolution: `config.json` → probe's `CurrentModelId` (backward-compat) → built-in fallback. The configured `defaults.model` is validated against the probed list; an out-of-list value drops the schema's `default_value` rather than substituting a phantom one. This stops silent UI/agent drift when fir's own model changes between relay restarts.

## [0.4.3] - 2026-04-30

### Fixed

- New conversations now apply schema defaults on the first turn. Poe materialises `default_value`s into the UI display only — empty `parameters` on turn 1 used to leave the agent on its own internal default while the UI promised something else (silent drift). `paramctl.Defaults` is now the single source of truth for UI defaults, fed into `router.Config.Defaults` and overlaid by `ParseOptions(params, defaults)`. A sync test pins `Build()` and `Defaults()` together so they cannot diverge.

## [0.4.2] - 2026-04-29

### Fixed

- Always emit `response_version: 2` on the settings response. Per `fastapi_poe.types.SettingsResponse`: when omitted, Poe applies *response version 0* defaults, under which `parameter_controls` is not honoured. v0.4.1 fixed the schema literals but the missing `response_version` still made Poe ignore the controls. With this release the Options panel actually renders.

## [0.4.1] - 2026-04-29

### Fixed

- `parameter_controls` schema is now accepted by Poe. Two wire-format bugs were silently causing Poe to drop the entire `parameter_controls` object (Pydantic validates with `extra="forbid"` and rejects unknown literals):
  - control type was emitted as `"dropdown"`; Poe expects `"drop_down"`.
  - `parameter_controls.api_version` was missing; Poe requires `"2"`.
  Symptom: bots showed no Options panel even though the relay served a populated settings response.

### Added

- JSON-Schema validation of emitted `parameter_controls` / `SettingsResponse` against the upstream `fastapi_poe.types` Pydantic models. Schemas are vendored to `internal/poeproto/testdata/` and regenerated by `scripts/regen-poe-schema.sh` (pinned to a `fastapi-poe` release). Tests fail at build time on any drift from the official wire format.

## [0.4.0] - 2026-04-28

### Added

- Poe `parameter_controls`: `model` dropdown (sourced live from the agent's authed-model list, probed once at relay startup), `thinking` dropdown (`off/minimal/low/medium/high`), and `hide_thinking` toggle. User selections arrive on each `query` and are diff-applied to the agent via `session/set_model` and `session/set_config_option` (`thinking_level`) only when changed.
- Multi-chunk thinking is rendered as one Markdown blockquote (`> _Thinking…_`) with proper transitions to/from message chunks, instead of italicising each chunk independently.

### Removed

- `settings.commands` field. Poe's protocol does not define a `commands` field on the settings response, so it never reached the UI. The agent's `available_commands_update` is now ignored.

## [0.3.0] - 2026-04-25

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
