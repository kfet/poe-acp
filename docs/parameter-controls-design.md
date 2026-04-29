# Parameter Controls — design

Status: draft for review. No code written yet.

## Goal

Surface a small, type-safe set of per-conversation knobs in the Poe UI
via Poe's native `parameter_controls` mechanism, and apply user
selections to the underlying ACP agent (fir) on each turn.

This replaces no existing functionality. It adds discoverable options
("change model", "thinking level", etc.) without inventing slash-command
syntax or other side channels.

## Non-goals

- **Permission policy.** Stays a server-side flag (`--permission`).
  Letting Poe users flip permissions per-conversation is a security
  footgun for a server bot — out of scope.
- **Per-turn model-list refresh.** v1 probes the agent once at relay
  startup and caches the result for the relay's lifetime. Auth changes
  during runtime are not reflected until restart.
- **Multi-agent.** v1 assumes a single `--agent-cmd`. Per-agent
  parameter sets are a follow-up.
- **Persisting selections relay-side.** Poe stores parameter values per
  conversation and replays them on every `query`; the relay is
  stateless w.r.t. selections, just diff-and-apply.

## Poe surface used

1. **`SettingsResponse.parameter_controls`** — schema. Returned on
   `type: settings` requests. JSON shape per Poe spec (Sections →
   Controls), see `docs/poe-protocol-reference.md`.
2. **`query[-1].parameters`** — values. `map[string]any` carrying the
   user's current selections on every `query`.
3. **`meta` event with `refetch_settings: true`** — emitted in-band when
   the relay's schema snapshot changes (e.g. dynamic model list arrived
   from the agent). v1 doesn't emit this; reserved for follow-ups.

Untrusted input: per Poe's docs, `parameters` may contain unknown keys
and wrong types (other bots calling ours via the Bot Query API can
inject arbitrary parameters). The relay validates every value against
an allowlist before acting.

## ACP surface used

fir exposes (verified in `~/dev/ai/fir/pkg/modes/acp`):

- **`session/set_model`** (unstable) — `SetSessionModelRequest{SessionId,
  ModelId}` where `ModelId` is `"<provider>/<modelId>"`. Fir parses,
  looks up in `modelRegistry`, calls `session.SetModel(model)`.
- **`session/set_config_option`** — `SetSessionConfigOptionRequest{
  SessionId, ConfigId, Value}`. fir handles `thinking_level` (values:
  `off|minimal|low|medium|high`) and `model` (same as set_model).
- **`SessionModelState` on `NewSessionResponse`** — fir returns the
  available-models list at session creation. The relay can use this to
  populate the model dropdown dynamically (follow-up; v1 is static).

For everything else (steeringMode, serverTools, compaction, etc.) fir
does **not** expose an ACP control RPC. Those would need either (a) a
fir patch to extend `set_config_option`, or (b) injection via
`.fir/settings.json` in the per-conv cwd before agent startup. Out of
scope for v1.

## v1 controls

| Control          | parameter_name      | Type     | Values / range                          | Applied via                 |
|------------------|---------------------|----------|-----------------------------------------|-----------------------------|
| Model            | `model`             | dropdown | dynamic list probed from agent at boot  | `session/set_model`         |
| Thinking         | `thinking`          | dropdown | `off, minimal, low, medium, high`      | `session/set_config_option` (`thinking_level`) |
| Hide thinking    | `hide_thinking`     | toggle   | true/false                              | relay-side: drop thinking ACP updates from SSE |

All controls are optional with safe defaults. Unknown parameter keys
are ignored. Wrong-type values are ignored with a debug log.

### Layout

Flat. All three controls in a single section, always visible. The
`condition` wrapper is deliberately deferred until v1.1 — keeps the
schema simple and easy to validate against Poe's parser on first ship.

### Why these three

- `model` and `thinking` are the canonical model-selection knobs and
  the only ones with first-class ACP RPCs.
- `hide_thinking` is a zero-risk relay-only output that exercises the
  "relay reads parameters but doesn't touch agent" path, validating
  the plumbing on both sides.

## Model list discovery

fir already filters its full model registry down to "models with auth
configured" inside `BuildModelState` and returns that on
`NewSessionResponse.SessionModelState`. The relay reuses this:

1. At relay startup, after `AgentProc.Start` succeeds, create one
   short-lived "probe" ACP session in a tmp cwd.
2. Read `SessionModelState.AvailableModels` from the response.
3. Cache the slice on `AgentProc` (mutex-guarded) and immediately close
   the probe session.
4. Real conversations get their model dropdown populated from this
   cache. The cache lives for the agent process's lifetime; if the
   agent restarts (out of scope today) the cache rebuilds on next
   probe.

The default selection is `SessionModelState.CurrentModelId` from the
probe response — i.e. fir's own configured default. No `--models-file`
flag, no hardcoded fallbacks beyond an empty dropdown if probe fails
(in which case the relay falls back to passing prompts straight through
without setting any model — fir uses its default).

`fir --list-models` was considered as an alternative but rejected: it
emits all models regardless of auth, would need separate parsing, and
duplicates work the ACP path already does.

## Schema (concrete JSON)

Returned in `settings` response under `parameter_controls`. Note the
required `api_version: "2"` and `control: "drop_down"` (NOT `"dropdown"`)
— Poe validates with `extra="forbid"` per `fastapi_poe.types` and
silently drops the entire object on any literal mismatch.

```json
{
  "api_version": "2",
  "sections": [
    {
      "name": "Model",
      "controls": [
        {
          "control": "drop_down",
          "label": "Model",
          "parameter_name": "model",
          "default_value": "anthropic/claude-sonnet-4-5",
          "options": [
            {"value": "anthropic/claude-sonnet-4-5", "name": "Claude Sonnet 4.5 / anth"},
            {"value": "anthropic/claude-opus-4-5",   "name": "Claude Opus 4.5 / anth"},
            {"value": "openai/gpt-5",                "name": "GPT-5 / oai"}
          ]
        },
        {
          "control": "drop_down",
          "label": "Thinking",
          "parameter_name": "thinking",
          "default_value": "medium",
          "options": [
            {"value": "off",     "name": "Off"},
            {"value": "minimal", "name": "Minimal"},
            {"value": "low",     "name": "Low"},
            {"value": "medium",  "name": "Medium"},
            {"value": "high",    "name": "High"}
          ]
        },
        {
          "control": "toggle_switch",
          "label": "Hide thinking output",
          "parameter_name": "hide_thinking",
          "default_value": false
        }
      ]
    }
  ]
}
```

The `model` options array is populated dynamically from the boot-time
probe (see "Model list discovery" above).

## Wire-up plan

### `internal/poeproto`

- Add `Parameters map[string]any \`json:"parameters,omitempty"\`` to `Message`.
- Add new types for the schema:
  - `ParameterControls{ Sections []Section }`
  - `Section{ Name string; Controls []json.RawMessage; CollapsedByDefault bool }`
  - Per-control structs (`Dropdown`, `ToggleSwitch`, `Condition`) that
    serialise with the `control` discriminator, mirroring fastapi_poe.
  - Helper `ParameterControls.MarshalJSON` if needed.
- Add `ParameterControls *ParameterControls \`json:"parameter_controls,omitempty"\`` to `SettingsResponse`.
- Add `RefetchSettings bool` field path for the `meta` event (write-only;
  v1 never sets it but the API exists).

### `internal/router`

- New `Options` struct:
  ```go
  type Options struct {
      Model         string // "" = unset
      Thinking      string // "" = unset
      HideThinking  bool
  }
  ```
- `sessionState` gains `applied Options`. On each `Prompt`:
  - Diff incoming `Options` against `applied`.
  - For each changed agent-facing field, call the corresponding
    `acpclient` method **before** issuing the prompt.
  - For relay-side fields (`HideThinking`), record on the sink/SSE
    writer for that turn.
  - Update `applied` only after the agent calls succeed.
- New helper `parseOptions(params map[string]any) Options` with strict
  allowlist validation. Unknown keys / wrong types → silently dropped
  (logged at debug).

### `internal/acpclient`

- `SetModel(ctx, sid, providerSlashID string) error` — wraps unstable
  `session/set_model`. ACP-go-sdk exposes this via
  `RawRequest("session/set_model", …)`; check the SDK for an idiomatic
  helper, otherwise raw.
- `SetConfigOption(ctx, sid, configID, value string) error` — wraps
  `session/set_config_option`.
- `ProbeModels(ctx) ([]ModelInfo, defaultID string, err error)` —
  creates a throwaway session in a tmp cwd, reads
  `SessionModelState` from the response, closes the session. Called
  once from `AgentProc.Start` after `Initialize`. Result cached on
  `AgentProc`; exposed via `Models()` snapshot accessor.
- Both setters are idempotent and return errors verbatim; router
  decides how to surface (probably a one-line `text` event then
  continue).

### `internal/httpsrv`

- `settings` handler: build `parameter_controls` from the cached model
  list on `AgentProc.Models()`. If probe failed (empty list), omit the
  `model` dropdown entirely so users aren't shown an empty selector.
- `query` handler: extract `req.Query[-1].Parameters`, parse to
  `Options`, pass into `router.Prompt(convID, text, opts, sink)`.
- Sink wrapper: applies `HideThinking` (drop thinking blocks) before
  forwarding to the SSE writer.

### `cmd/poe-acp-relay`

No new flags. The model list is discovered automatically.

## Tests

- `poeproto`: round-trip `SettingsResponse` with `parameter_controls`
  through `json.Marshal/Unmarshal`; check field names match Poe spec.
- `router`: table test driving `parseOptions` with valid/invalid/unknown
  inputs; verify allowlist rejects bad values.
- `router`: scripted ACP fake (existing pattern) verifying that:
  - First prompt with `model=X` calls `SetModel(X)` once.
  - Second prompt with same `model` does NOT call `SetModel` again.
  - Changing `thinking` on the third prompt calls `SetConfigOption`.
  - `hide_thinking=true` filters thinking-block updates from the sink.
- `httpsrv`: settings request returns valid JSON; query request with
  `parameters` threads them into the router (mock router).
- `smoke.sh`: extend with a `query` carrying `parameters: {model: ...}`
  and assert the SSE stream still terminates cleanly.

## Open questions

1. **Probe cost.** Boot-time probe adds one ACP `session/new` +
   `session/cancel` to relay startup. Fir's cold-start is ~1s for
   Initialize and ~50s for the first prompt — but the probe doesn't
   prompt, just creates a session and reads the response. Expected
   cost: ~1–2s. Acceptable.

2. **Probe failure.** If the probe fails (auth not configured, agent
   crash, etc.), the relay logs a warning and starts up anyway with
   no `model` dropdown. Users can still chat; the agent picks its own
   default. Verify this degraded mode is acceptable.

3. **Schema discriminator.** fastapi_poe uses a `control` string field
   to tag union members. Confirm Poe's parser is happy with our Go
   struct emitting that exactly (camel/snake case sensitivity).

4. **`hide_thinking` granularity.** ACP `SessionUpdate` has both
   `agent_thought_chunk` and the regular `agent_message_chunk`. The
   toggle should drop the former and keep the latter. Confirm by
   reading `acpclient`'s update fan-out.

## Decision points for the user (you)

Before I start coding, please confirm:

- [ ] v1 control set: `model`, `thinking`, `hide_thinking` — good?
- [ ] Boot-time probe approach for model discovery — good?
- [ ] OK to ship a `condition` block (more complex schema) for
      `hide_thinking`, or flatten and always show it?
- [ ] Default `thinking` value: `medium` (matches fir default), or
      `none` (cheapest, opt-in)?
- [ ] Anything from Tier 2/3 of the earlier discussion (steering,
      server_tools, reset, project preset) you want pulled into v1?
