# Changelog

## [Unreleased]

### Changed

- **No-provider prompts now offer login instead of a cryptic error.** When the agent rejects a prompt with "Authentication required" (JSON-RPC `-32000` — e.g. a freshly deployed bot with no connected provider), the relay no longer surfaces the raw `acp prompt: ...` error. Instead it streams a friendly onboarding message listing the loginable providers as `!login <provider>` commands (via the new `authbroker.Broker.OfferLogin`, wired through `router.Config.AuthErrorHint`). Detection is structural (`errors.As` on `*acp.RequestError`, code `-32000` + message), not string-scraping. The hook lives on the *prompt* path rather than gating at session start on purpose: `session/new` does expose model availability (fir omits the `models` field when no provider is connected), but that list is cached at session creation and login is handled out-of-band, so it goes stale right after the user authenticates and would loop the offer. The prompt error reflects live auth state, self-heals the moment login succeeds, and (verified) costs no tokens — fir rejects before any model call, and the result renders as ordinary assistant text, not an error.
- **Login commands now accept the `!` and `.` sigils (and still `/`).** Poe's chat client intercepts `/`-prefixed messages as native slash commands and rejects unknown ones before they reach the bot, so `/login` usually never arrived (and was flaky/first-message-only when it did). The auth broker now recognises `!login`, `.login`, `!cancel-login`, etc., and **user-facing prose suggests the `!` form** (`DisplaySigil`) which passes straight through Poe untouched. `/login` remains accepted for anyone already in the habit. This unblocks the existing OAuth relay (`internal/authbroker`), whose only entry point was the swallowed slash command. No new auth machinery — just a reachable doorbell.
- Removed the `--permission` CLI flag and the per-conversation `permission` parameter path; poe-acp no longer exposes a relay-owned permission policy.

## [0.17.2] - 2026-05-27

### Changed

- **Bumped `github.com/kfet/acp-kit` to tagged `v0.1.4`.** Replaces the pseudo-version `v0.1.4-0.20260528015243-2525b4370ac5` that 0.17.1 pinned while the status-line wire-contract migration was still on an untagged commit. No behaviour change — `v0.1.4` is the released tag of that same tree.

## [0.17.1] - 2026-05-27

### Changed

- **Status-line extension id renamed to `dev.acp-kit.status-line/v1`** and the wire-contract pieces (id, length cap, `Status`, provider→emoji map, `ParseMeta`, `Segments`, `CapRunes`) now live in `github.com/kfet/acp-kit/statusline` so `slack-acp` and any future relay render the same agent identically. `internal/statusline` retains only the poe-acp-specific renderers (`Header`, `Spinner`) and re-exports the kit symbols under their existing names so call sites are unchanged. Requires acp-kit ≥ commit `2525b43` (pseudo-version `v0.1.4-0.20260528015243-2525b4370ac5`). The old `dev.poe-acp.status-line/v1` id is dead — agents emitting under the old key will no longer be picked up.

## [0.17.0] - 2026-05-26

### Added

- **Mobile-friendly status line: `dev.poe-acp.status-line/v1` ACP extension.** poe-acp now prepends a compact one-line header — `<provider-emoji> • <mood> • <plan>` — to assistant responses and the live "Thinking…" heartbeat spinner, so users on Poe's mobile / web chat surfaces see fir-style indicators they'd otherwise miss without a TUI. The provider emoji is **relay-owned**, resolved from the model id (`anthropic/...` → 🏛️, `openai/...` → 🌐, `google/...` → ✨, and 10 other slug families; case-insensitive; unknown → segment dropped). Mood and plan are **agent-owned**, opaque strings carried on `session/update._meta["dev.poe-acp.status-line/v1"]` and rendered with a 12-rune length cap each. Empty segments are dropped; if nothing would render, no header is emitted. Capability is exchanged in the `initialize` handshake (`clientCapabilities._meta` / `agentCapabilities._meta`), but rendering does not gate on negotiation — agents that never emit `_meta` still get the provider-emoji-only header for backwards compat. Wire spec lives in `docs/ext/status-line.md`; renderer + slug map in new `internal/statusline` package; router parses `_meta` in `drainProcessChunk`; SSE sink builds the spinner via `statusline.Spinner` and prepends the final header exactly once on the first `text` event of the turn (skipped on `replace_response` / `error` paths since those overwrite the body). Required acp-kit ≥ v0.1.2: adds `client.Config.ClientMeta` (extra `_meta` entries merged into outgoing `clientCapabilities._meta`) and `client.Caps.Extensions` (parsed non-kit entries from `agentCapabilities._meta`). Renderer covered by `internal/statusline/statusline_test.go` (provider mapping, empty-segment drops, rune-aware length cap, unknown providers, spinner frames, meta parsing); sink behaviour by `TestSink_StatusLine*` in `internal/httpsrv/branches_test.go`; router forwarding by `TestRouter_StatusLine*` in `internal/router/router_test.go`. The fir-side emitter that populates `_meta` is tracked separately.

## [0.16.1] - 2026-05-24

### Fixed

- **Debug logging no longer 400s on Poe requests >16 KiB.** `poeproto.Decode` previously fed a 16 KiB-`LimitReader`-truncated buffer to the JSON decoder when debug logging was enabled, so any real Poe `query` carrying a non-trivial `parsed_content` attachment failed to decode and returned 400 — Poe then surfaced this to operators as a `report_error` and the bot stopped responding. The decoder now streams from the full body via `io.TeeReader` into an internal `capWriter` that captures only the first 16 KiB for the log line (suffixed with `...[truncated]` when more bytes streamed past). No per-request cap on what the decoder sees, matching the non-debug path — image / video / large transcript attachments stream through unimpeded. Regression covered by `TestDecode_DebugPath` decoding a 64 KiB body under `kitlog.SetEnabled(true)`.

### Changed

- **Single-provider bots collapse to a flat `Model` dropdown.** `paramctl.Build` now adapts the `parameter_controls` schema to provider count: 0 providers → no model controls (unchanged), exactly 1 provider → a single flat `drop_down` with `parameter_name: "model"` (no Provider picker, no `condition` wrapper), ≥2 providers → cascading Provider → `model_<sanitised-provider>` shape (unchanged). A one-option Provider dropdown is pure noise on bots wired to a single provider (Sakana-only, Anthropic-only, etc.); they now get the minimum-surface UI their operators expect. `router.ParseOptions` already accepts both shapes (bare `model` wins over `provider` + `model_<provider>`), so no router change was needed. `TestBuild_SingleProvider_{CollapsesToBareModel,DefaultsFirstModelWhenUnpinned,DefaultModelOutsideGroupFallsToFirst}` and `TestBuildAndResolveAgree_SingleProvider` pin the collapsed shape; the existing single-provider fixtures in `TestBuild_ProviderParamSanitisation` and `TestBuild_DefaultModelProviderNotInList` were lifted to two providers so the cascading invariants they targeted stay covered.

## [0.16.0] - 2026-05-23

### Added

- **Operator-configurable durable system prompt via external file (`system_prompt_file` / `disable_system_prompt`).** Two new top-level keys in `~/.config/poe-acp/config.json` (or per-bot `<dir>/config.json`). `system_prompt_file` names a file whose trimmed contents are prepended to the skills catalog and injected into the authoritative system slot of every new ACP session — relative paths resolve against the config's directory, absolute paths are used as-is. `disable_system_prompt: true` suppresses the prompt file, the skills catalog, *and* the relay's transport-contract clause (full opt-out, wins over a configured file). The file is read per new conversation, so edits take effect on the next new chat without a relay restart; at startup a fail-fast read surfaces typos and missing files immediately (`log.Fatalf`), while per-session re-read errors are logged and treated as empty so live conversations stay up. The file-based shape (no inline alternative) keeps substantial Markdown prompts editable, diff-friendly, and free of JSON's no-multiline-strings escape tax. Composition uses `acp-kit/sysprompt.Compose`; matches `slack-acp`'s `disable_system_prompt` key for a shared operator mental model. `internal/config.Config` grows `SystemPromptFile` and `DisableSystemPrompt`; `cmd/poe-acp/helpers.go` swaps `skillsCatalogProvider` for `systemPromptProvider` + `readSystemPromptFile` (router-facing API unchanged). Existing conversations keep their snapshot — only new ones pick up edits.

### Changed

- **Migrated shared ACP relay primitives to `acp-kit`.** The relay now imports `github.com/kfet/acp-kit/{client,log,skills}` v0.1.0 in place of the old `internal/{acpclient,debuglog,policy}` packages, deleting ~2.7k lines of locally maintained code. The same primitives back `slack-acp`, so wire-level bugs and capability-parsing tweaks get fixed once. Behaviour preserved across the swap: same initialize handshake, same `_meta` parsing, same `POEACP_DEBUG=1` env activation, same `--debug` flag, same auth-method shape, same model-probe semantics. `internal/skills` is now a thin wrapper that owns the embedded `bundle` FS and the `"poe-acp"` tmp-dir prefix; everything else lives in `acp-kit/skills`.
- **`custom-bots` and `update` skills: prescribe `launchctl kickstart -k` for routine restarts.** Both bundled skill bodies now explicitly call out that config-only or binary-only changes should restart the existing launchd job via `launchctl kickstart -k gui/$UID/<label>` rather than scheduling a delayed reloader or running `bootout` + `bootstrap` (which has an async registration race that can leave the service stopped). `custom-bots` also grows an "Update an existing bot" section pointing operators at `~/.config/poe-acp/<bot>/config.json` as the relay source of truth for model/thinking/default changes.

### Fixed

- **Attachment-only Poe user turns no longer rejected as empty.** `router.Prompt` now accepts turns with usable attachments but no text, adds a concise placeholder text block before attachment blocks, preserves the existing truly-empty error, and avoids anonymous message-dir collisions for attachment-only turns.

## [0.15.0] - 2026-05-21

### Added

- **`refresh-models` builtin skill.** New ops skill explaining how to make a newly added agent model appear in the Poe model dropdown after editing the agent's model catalog: restart the supervisor, verify the relay's startup log shows the new probe count and a schema-hash change, optionally update `defaults.model` in the relay config. Auto-discovered by the bundle walker; no code wiring needed.
- **Cascading provider → model dropdowns.** `parameter_controls` now renders a top-level `provider` drop_down with options grouped from each model id's prefix-before-first-slash (first-seen provider order, slash-less ids bucketed under `other`), followed by one `condition`-gated `Model` drop_down per provider with parameter_name `model_<sanitised-provider>` so each provider keeps its own remembered selection. Default-value plumbing is consistent end-to-end: `paramctl.Resolve` still produces a single `defaults.Model`, `paramctl.Build` mirrors it into the provider default and the matching `model_<provider>` default, and `router.ParseOptions` resolves the effective model via `provider` + `model_<provider>` while still honouring a bare `model` key for back-compat. `internal/poeproto` grows `Condition`/`ConditionOperand` types and extends `Control` with `condition`/`controls` fields (gated by `omitempty` so the strict additionalProperties:false upstream schema still validates every shape).

### Changed

- **System-prompt transport contract rewritten as a single `[SYS_EXT]` block.** Router now prepends one compact `[SYS_EXT] Relay & Transport Contract` clause that covers both facets of the relay's I/O contract: (1) how `[poe-acp:out-of-band ...]` turns are marked and that replies to them are discarded, and (2) that the user reaches the agent only as the response to one of their turns and may not be co-located on this host (mobile, browser, no shell/tmux/local file access) — so the agent owns the observation loop, manages background work itself, and keeps output mobile-friendly. The `[SYS_EXT]` prefix marks the block as a durable, authoritative extension to fir's system prompt that should survive context compaction. Const name `transportContractClause` retained; tests updated to assert on the new heading.
- **Skills catalog rebuilt per session, not once at startup.** `cmd/poe-acp` now hands the router a `SystemPromptProvider` callback that re-merges built-in and host skills (`<dirname(config)>/skills/`) every time a new conversation/session is created or resumed. Host skills dropped into the directory are picked up by the next new conversation without restarting the relay; built-in extraction stays content-hashed and idempotent, host-overrides-builtin-by-name semantics are preserved. `internal/router.Config.SystemPrompt` (static text) is gone — replaced by `SystemPromptProvider func() string`; internal-only API, no operator-visible flag change.

### Fixed

- **`internal/skills` test isolation.** `TestLoadBuiltin_FSErrorPaths` now swaps `bundleHashFn` alongside `bundleSrc` so the extracted tmp dir is keyed off the fixture FS's content hash instead of the production binary's. Without this, test runs wrote fixture `SKILL.md` files into `$TMPDIR/poe-acp-<production-hash>/skills/`, polluting the same directory the real binary reads from. Added an assertion that every returned `Skill.Path` contains the fixture hash prefix, a `t.Cleanup` that removes the per-test extraction dir, and a `hashPrefixLen` constant in `skills.go` so the test and production code can't drift. Sibling tests `TestLoadBuiltin_WalkReadFileError` and `TestLoadBuiltin_WalkError` also swap `bundleHashFn` for defense-in-depth (they fail at walk before mkdir, so no actual file leak existed, but the swap keeps the policy consistent).

## [0.14.0] - 2026-05-13

### Added

- **`report_reaction` forwarded as out-of-band turn.** Poe reaction events (👍/👎 add or remove on an assistant message) now reach the agent as a synthetic prompt prefixed with `[poe-acp:out-of-band reaction]`, sharing the conversation's ACP session so memory/preference updates stick. Decoder handles two plausible wire shapes — single `reaction` field with optional `+`/`-` prefix, or split `reaction`+`action:added|removed` — and normalises them to `(kind, added|removed)`. Response is discarded (Poe has no SSE channel for the reaction reply); HTTP returns 200 OK as soon as the turn is queued. Raw payloads are logged via debuglog so the actual shape stays visible in prod.

### Changed

- **Per-session turn queue.** Replaced the per-conv `turnMu` + `inUse` counter in `internal/router` with a per-session FIFO queue (`sessionQueue`) consumed by a single `runTurns` goroutine that owns all `Agent.Prompt` calls for the session. Callers submit a `turnReq` and wait on `req.done`. Reactions and user prompts share the queue; on overflow the oldest reaction is shed, never a user prompt; reactions older than 60s at dequeue are dropped as a liveness safeguard. Preserves the existing `endTurn`-ack invariant — the runner only finalises the sink after the chunk drain has processed every chunk emitted before `Agent.Prompt` returned. GC eviction now checks `queue.idle()` instead of an in-use counter.

- **System-prompt out-of-band contract clause.** Router prepends a short clause to the operator's `SystemPrompt` explaining the `[poe-acp:out-of-band ...]` marker so the agent recognises synthetic turns, replies tersely, and doesn't address the user (the reply is discarded).

## [0.13.2] - 2026-05-11


### Changed

- Bumped `github.com/coder/acp-go-sdk` from v0.6.3 to v0.12.2. Upstream now serialises notification dispatch via a sequence-numbered `notificationQueue`, fixing out-of-order `session/update` delivery that previously caused garbled assistant text in Poe (chunk reordering across adjacent text deltas). API renames absorbed: `FileSystemCapability` → `FileSystemCapabilities`; `SetSessionModel{Request,Response}` / `ModelId` (for `session/set_model`) → `UnstableSetSessionModel{Request,Response}` / `UnstableModelId`; `RequestPermissionToolCall` → `ToolCallUpdate`.

## [0.13.1] - 2026-05-10

### Changed

- **Unified heartbeat / spinner.** The animated `> _Thinking._` spinner now runs in BOTH `hide_thinking` modes — there's no separate "invisible heartbeat" code path. Spinner doubles as keepalive, gives the user liveness during the gap between submit and first chunk, and `orderedWriter` clears it the moment the first real chunk lands. Removed `Config.SpinnerInterval` and the `--spinner-interval` flag (collapsed into `HeartbeatInterval` / `--heartbeat-interval`); the heartbeat default dropped from 10s to 1.5s so the spinner animates at a human-readable pace. `hide_thinking` remains a router-level filter on `agent_thought_chunk` content; it no longer affects the spinner.

### Fixed

- **Garbled / out-of-order output** — heartbeat goroutine could race router-side writes (`_(cancelled)_` on `StopReasonCancelled`, `_(response truncated)_` on `MaxTokens`/`MaxTurnRequests`, `_(option not applied)_` on applyOptions failure) when the agent emitted no chunks for `FirstChunk` to fire on. A late tick would clobber the just-written content with `Replace("")` (or a "Thinking…" spinner frame).

  Root cause was structural: the SSE stream had two concurrent writers (heartbeat goroutine + router-driven chunk path) and correctness depended on each call site remembering to stop the heartbeat first — a footgun. Fix introduces `orderedWriter`, which owns the SSEWriter, a `realWritten` flag, and a single mutex; user-visible writes (`userText` / `userReplace` / `userError` / `userDone`) and heartbeat frames (`hbReplace`) all serialise through it, with the gate-check-and-write atomic w.r.t. each other. `hbReplace` becomes a no-op once any user write has landed, and the heartbeat goroutine self-disarms (returns) the first time it observes the closed gate — no caller has to remember anything. Bonus: `userText` / `userError` / `userDone` now auto-clear a visible spinner so the user never sees a frozen "Thinking…" trailing their final content. Regression covered by `TestOrderedWriter_HeartbeatGatedByUserWrite` (structural property), `TestSink_HeartbeatNeverOverwritesUserContent` (end-to-end), and `TestSink_HeartbeatSelfDisarmsViaGate` (goroutine self-disarm).

## [0.13.0] - 2026-05-09

### Added

- `sibling-repos` skill (project-only, not built-in) — points the agent at notes for repo paths.
- `notes` built-in skill — documents `~/.local/state/poe-acp/notes/` as persistent scratch across conversations.
- `deploy` skill: seed `~/.local/state/poe-acp/notes/repos.md` when repo paths are known.
- `AGENTS.md`: documents the notes scratch dir for in-repo agents.

### Changed

- `.covignore`: replaced legacy `unreachable.go` exclusion pattern with `*_must.go` suffix rule. Renamed `cmd/poe-acp/unreachable.go` → `schema_hash_must.go`, `internal/acpclient/unreachable.go` → `spawn_must.go`, `internal/router/unreachable.go` → `attachment_io_must.go` so each file's name reflects what it covers rather than a meta-property ("hard to test").

## [0.12.0] - 2026-05-07

### Added

- **100% unit-test coverage gate** — new `make run-tests` target enforces 100% line coverage (with a small `.covignore` for genuinely unreachable defensive IO branches and the `main()` shim). Wired into `make all` via `test-race`. Mirrors the pattern used in `kfet/skipstone`.
- **Host-supplied skills** — relay now loads skills from `<dirname(config)>/skills/` (default `~/.config/poe-acp/skills/`) and merges them with the embedded built-in bundle. Last-wins by name, so a host SKILL.md with the same `name:` overrides the built-in (the disable mechanism). Required frontmatter fields: `name`, `description`. Missing dir is not an error.
- `--print-catalog` flag prints the merged skills catalog to stdout and exits, for debugging multi-bot deployments.
- `--state-dir` defaults to `<dirname(config)>/state` when `--config` is set explicitly, so multi-bot configs only need one path per bot.

### Changed

- `Makefile`: simplified — bare `make` now runs the full `all` pipeline (was a build-only default); 5 cross-build rules collapsed into one pattern rule with a `FORCE` prereq so every `make` re-runs all checks; `run-tests` helper inlined into `test-race-cover` (renamed from `test-race` to make the coverage gate explicit); `TIDY_DONE` cross-target hack removed (tidy runs once in `all`, standalone elsewhere).
- `.covignore`: migrated from line-number / per-function entries to two file-level patterns (`cmd/<binary>/main.go` and `**/unreachable.go`). Defensive paths previously listed by line number are now either tested directly or isolated in per-package `unreachable.go` helpers that panic instead of returning impossible errors. Coverage gate still enforces 100%.
- `cmd/poe-acp`: `main.go` split into `main.go` (entry-point shim) + `helpers.go` (testable helpers).
- `internal/httpsrv`: `Config.AuthBroker` is now an interface (`AuthBroker`) instead of `*authbroker.Broker` so tests can inject brokers.
- `internal/authbroker`: removed an unreachable `provider == ""` branch in `Handle` (input is trimmed at function entry, so the prior branch was dead code).
- `internal/skills`: `Extract` renamed to `LoadBuiltin`; new `LoadDir(path)` and `Merge(layers, disable)` helpers.

## [0.11.0] - 2026-05-05

### Fixed

- **Scrambled / missing response text** — the ACP SDK dispatches each
  `session/update` notification in its own goroutine (`go c.handleInbound`).
  With the old per-turn channel design, concurrent goroutines raced to call
  `sink.Text()`, which caused chunks to arrive at the SSE stream out of order;
  worse, when the first goroutine's `FirstChunk() → Replace("")` fired *after*
  a later goroutine's `Text()`, the Replace silently erased already-rendered
  content.

  The fix replaces the per-turn channel + `sync.Mutex` + `sync.WaitGroup` with
  a **session-lifetime channel** and a **single drain goroutine** per session.
  `OnUpdate` is now a lock-free two-line channel send; all per-turn state
  (`first`, `chunkMode`, `hideThinking`) lives as local variables in
  `drainChunks` and is only ever touched by that one goroutine — no
  synchronisation required. Begin- and end-of-turn control messages
  (`beginTurn` / `endTurn`) flow on the same FIFO channel so ordering is
  guaranteed by the channel itself. The drain goroutine is stopped via
  `drainStop` when the session is GC'd.

## [0.10.0] - 2026-05-04

### Changed

- Project renamed from `poe-acp-relay` to `poe-acp`. Module path is now `github.com/kfet/poe-acp`, binary is `poe-acp`, GoReleaser project and Homebrew formula are renamed accordingly. Existing installs must reinstall under the new name.

### Added

- Thinking dropdown now offers `xhigh` and `max` levels in addition to `off`/`minimal`/`low`/`medium`/`high`, matching fir's full `ai.ThinkingLevel` set. Config validation and `ParseOptions` accept the new values.

### Changed

- Attachment forwarding pivoted to a file-on-disk + inline hybrid. The relay now downloads every attachment to `<StateDir>/convs/<conv_id>/.poe-attachments/<message_id>/<name>` and emits a `file://` `ResourceLink` as the universal carrier; ACP agents (fir included) handle file:// natively, so HEIC/PDF/video/octet-stream/big images "just work" because the agent reaches for its own tools (`sips`, `pdftotext`, `Read`, …) on the file path. For supported inline formats (PNG/JPEG/GIF/WebP) under `MaxInlineImageBytes` (3 MiB raw default), an `ImageBlock` is *additionally* emitted after the `ResourceLink` so the LLM sees the pixels directly without a tool round-trip. Pre-parsed text from Poe (`parsed_content` + `embeddedContext`) keeps its existing zero-fetch fast path. New `Config.MaxAttachmentBytes` (100 MiB default), `Config.AttachmentTTL` (30 days default, clamped up to `SessionTTL` with a warn log) and `Turn.MessageID`. Hostile filenames (e.g. `../../etc/passwd`) are contained inside the per-message dir via `os.Root` (Go 1.24) plus a hash-derived fallback when the kernel/runtime rejects the supplied name. The GC ticker sweeps stale files past `AttachmentTTL` and removes empty per-message dirs. Fixes images being silently dropped by fir (and most ACP agents) when forwarded as bare `https://` `ResourceLink`.

### Fixed

- Non-reasoning models (e.g. `kimi-k2.6`) that reject `thinking_level` other than `"off"` no longer surface a user-visible "option not applied" notice on every prompt. The router logs the rejection at debug level, marks the level as applied to suppress per-turn retries, and proceeds with the prompt normally.

### Removed

- `Config.MaxInlineTextBytes` (subsumed by the universal file:// `ResourceLink` path; text attachments are now downloaded to disk like everything else).

## [0.9.1] - 2026-05-03

### Changed

- Skill bundling is now opt-in per-skill via a `builtin: true` frontmatter field, mirroring fir's own `pkg/resources/builtin_skills` loader. Every SKILL.md under `internal/skills/bundle/` is still embedded so `.fir/skills` (a symlink into the bundle) stays git-coherent and fir running in this repo picks them all up as project-local skills, but only those marked builtin are surfaced to ACP agents at runtime. The `release` skill is now project-only — it lives in the bundle tree but is no longer announced to deployed agents.

## [0.9.0] - 2026-05-03

### Added

- Skill catalog injection. The relay embeds a small bundle of relay-specific Markdown skills (`deploy`, `update`, `release`) and announces them to every ACP session as a fir-style `<available_skills>` catalog. Bodies are extracted to a per-version tmp dir and read on demand by the agent. When the agent advertises the new `_meta["session.systemPrompt"]` capability (RFD: `acp-spec/rfd-system-prompt.md`), the catalog rides on `session/new._meta` as durable system context; otherwise the relay inlines it as a "preserve verbatim" header on the first `session/prompt` (and re-injects on resume). Relay advertises the matching client capability in `initialize`. New `internal/skills` package; new `acpclient.Caps.SystemPrompt`; new `router.Config.SystemPrompt`.

### Fixed

- Heartbeat keepalive no longer pollutes the rendered response. When `hide_thinking=false` the relay previously sent a zero-width space via Poe's `text` SSE event each tick; those events *append*, so by the time the agent's first chunk arrived the response already began with N invisible characters and Poe's Markdown renderer would silently mis-parse leading headings, lists or fenced code blocks. The keepalive now uses `replace_response` with empty body (matching the `hide_thinking=true` spinner mechanism) so SSE bytes still flow but the rendered response stays empty until the agent emits.
- Debug-log content previews truncate on rune boundaries instead of byte boundaries, so multi-byte UTF-8 sequences are no longer split mid-codepoint in `--debug` output.

## [0.8.0] - 2026-05-03

### Added

- `--debug` CLI flag and `POEACP_DEBUG=1` env var enable verbose debug logging. When on, logs the raw inbound Poe request body (capped 16 KiB), per-turn `parameters` dicts, resolved `opts` vs `Defaults`, and the `getOrCreate`/`applyOptions` paths in the router. Useful for diagnosing options-handling issues on first message and branched conversations.

## [0.7.0] - 2026-05-02

### Changed

- `defaults.hide_thinking` is now `*bool` in config and defaults to `true` when omitted. Operators who want streamed thoughts must set `"hide_thinking": false` explicitly. Previous behaviour: omitted == `false`.

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

- JSON config file at `$XDG_CONFIG_HOME/poe-acp/config.json` (override with `--config /path`). Holds the bot's identity (`bot_name`), per-conversation defaults (`defaults.model`, `defaults.thinking`, `defaults.hide_thinking`), and reserved `agent.profile` field. Unknown keys fail loudly at boot (DisallowUnknownFields). See `docs/config.example.json`. Empty/missing file preserves zero-config behavior.
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

- M0 skeleton: design doc and compiling scaffold for `poe-acp`, an HTTP server that implements Poe's server-bot protocol and relays each conversation to a spawned ACP-speaking agent over stdio.
- Extracted to its own standalone Go module (`github.com/kfet/poe-acp`) so it can be vendored/deployed independently of fir.
- M1 build: per-conversation cwd, heartbeat keep-alive, cancellation, session GC, and unit tests for the HTTP handler and router.
- Capture of `available_commands_update` notifications from the agent; M1 complete.
- Review pass cleanups.
- `--poe-path` flag for deploy-specific path mapping (e.g. Funnel prefix stripping).
- Poe server-bot protocol reference doc.
- Deployment section in the design doc capturing the Funnel prefix-strip gotcha.
- README.
