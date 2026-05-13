# Poe Server Bot Protocol — Reference

> Research summary of the Poe custom server-bot API, kept locally so the
> `poeacp` relay's behaviour can be cross-referenced without leaving the
> repo. Originally lifted from `external/poe/docs/poe/protocol-reference.md`
> (the MCP bridge's reference) and re-grounded against the upstream sources
> listed below.
>
> Sources:
> - [Poe Protocol Specification](https://creator.poe.com/docs/poe-protocol-specification)
> - [`fastapi_poe` type definitions](https://github.com/poe-platform/fastapi_poe)
> - [Poe server-bot tutorial](https://creator.poe.com/docs/quick-start)

## What the relay implements today

The `poeacp` relay speaks the protocol described below, but only the
minimum needed to make a single-user chat bot work. Specifically:

| Feature                     | Supported by relay | Notes                                               |
|-----------------------------|--------------------|-----------------------------------------------------|
| `query` → SSE response      | ✅                 | Streams `meta` + `text*` + `done`.                  |
| `settings` JSON response    | ✅                 | Static config + dynamic `parameter_controls`.       |
| `report_feedback`           | ✅ (accept+drop)   | Returns 200 OK, no-op.                              |
| `report_reaction`           | ✅ (out-of-band turn) | Decoded (both single-`reaction` and split `reaction`+`action` shapes), normalised to (kind, added/removed), forwarded to the agent as a synthetic prompt prefixed with `[poe-acp:out-of-band reaction]`. Response discarded. HTTP returns 200 OK as soon as the turn is queued. |
| `report_error`              | ✅ (accept+drop)   | Ditto; logged by the relay.                         |
| Bearer auth                 | ✅                 | `Authorization: Bearer $POEACP_ACCESS_KEY`.         |
| `replace_response`          | ✅                 | Emitted on `StopReasonCancelled`.                   |
| `error` event               | ✅                 | Emitted on agent refusal / internal errors.         |
| Tool calling                | ❌                 | Tools live on the ACP side (fir does its own).      |
| Attachments                 | ✅                 | Pre-parsed text → inline `Resource` (when agent supports `embeddedContext`). Otherwise downloaded to `<cwd>/.poe-attachments/<message_id>/<name>` and forwarded as `file://` `ResourceLink`; for jpeg/png/gif/webp ≤ 3 MiB an additive `ImageBlock` follows. See `internal/router` design notes. |
| Parameter controls          | ✅                 | `model` + `thinking` dropdowns + `hide_thinking` toggle. |
| Monetisation                | ❌                 | Not used.                                           |
| `suggested_reply`           | ❌                 | Not emitted; easy to add if useful.                 |
| `file` / `json` / `data`    | ❌                 | Not emitted.                                        |

Anything in the rest of this document that isn't in the ticked rows above
is reference material for future extension.

## Overview

Poe server bots receive HTTP POST requests from Poe and respond via
**Server-Sent Events (SSE)**. The protocol is request–response: Poe sends
a query, the bot streams back events. No persistent connection from bot to
Poe exists — Poe holds the SSE stream open until the bot emits `done`.

## Request Types

| Type | Purpose |
|---|---|
| `query` | User sent a message; bot must respond via SSE |
| `settings` | Poe asks the bot for its configuration (JSON response) |
| `report_feedback` | User liked/disliked a message |
| `report_reaction` | User reacted to a message |
| `report_error` | Poe reports a protocol error back to the bot |

### Query Request Fields

```
query[]              — conversation history (role, content, content_type, timestamp, message_id, attachments)
message_id           — identifier for the response message
user_id              — anonymised user identifier
conversation_id      — conversation identifier (resets on context clear)
temperature          — optional model temperature
tools[]              — OpenAI-compatible tool definitions
tool_calls[]         — tool calls from a previous assistant turn
tool_results[]       — tool execution results
language_code        — BCP 47 language code (default "en")
```

Each message in `query[]` has:
- `role`: `system` | `user` | `bot` | `tool`
- `content`: message text
- `content_type`: `text/plain` | `text/markdown`
- `attachments[]`: file attachments with `url`, `content_type`, `name`, `parsed_content`
- `sender`: `{id, name}` — useful in multi-user chats
- `message_type`: `"function_call"` for tool call messages
- `parameters`: user-set parameter values from custom UI controls

## SSE Response Events

All events carry JSON `data`. Poe concatenates all `text` event payloads
to form the final visible response.

| Event | Data | Purpose |
|---|---|---|
| `meta` | `{content_type, linkify, suggested_replies, refetch_settings}` | Must be first event. Sets rendering mode. |
| `text` | `{text}` | Append a text chunk (streaming) |
| `replace_response` | `{text}` | Discard all prior text, replace with this |
| `suggested_reply` | `{text}` | Add a clickable follow-up button |
| `file` | `{url, content_type, name, inline_ref?}` | Attach a file |
| `json` | arbitrary JSON | Send structured data |
| `data` | `{metadata}` | Attach metadata to the response (retrievable in later requests) |
| `error` | `{text?, allow_retry?, error_type?}` | Signal an error |
| `done` | `{}` | **Must** be the last event — closes the stream |

### Error Types

`user_message_too_long` · `insufficient_fund` · `user_caused_error` · `privacy_authorization_error`

## Settings Response

Returned as JSON (not SSE) for `type: settings` requests.

| Field | Default | Purpose |
|---|---|---|
| `server_bot_dependencies` | `{}` | Bots this bot calls (for Bot Query API) |
| `allow_attachments` | `true` | Allow file uploads |
| `introduction_message` | `""` | Greeting shown on first visit |
| `expand_text_attachments` | `true` | Auto-parse text files into `parsed_content` |
| `enable_image_comprehension` | `false` | Auto-describe images |
| `enforce_author_role_alternation` | `false` | Merge consecutive same-role messages |
| `enable_multi_entity_prompting` | `true` | Combine bot messages in multi-entity chats |
| `parameter_controls` | `null` | Custom UI controls (sliders, dropdowns, etc.) |
| `response_version` | `0` (!) | Selects which protocol version's defaults Poe applies |
| `context_clear_window_secs` | server-decided | Auto-clear context after inactivity |
| `allow_user_context_clear` | `true` | Let users manually clear context |

### Gotchas (learned the hard way)

Poe validates the settings response with **Pydantic + `extra="forbid"`**
per `fastapi_poe.types`. Any of the following silently drops the
entire `parameter_controls` object — Poe replies 200 to
`/bot/fetch_settings` and reports it as success, but the bot UI
shows no Options panel:

1. **Missing `response_version: 2`.** When omitted, Poe applies
   *response version 0* defaults under which `parameter_controls`
   is not honoured. Always emit `2`.
2. **Missing `parameter_controls.api_version: "2"`.** Required by
   the `ParameterControls` model.
3. **Wrong control literal.** `dropdown` is NOT valid; the literal
   is `drop_down`. Likewise `toggle_switch`, `text_field`,
   `text_area`, `slider`, `aspect_ratio`, `divider`, `condition`.
4. **Unknown fields.** `extra="forbid"` rejects anything not in
   the upstream Pydantic model.

Validate offline against the vendored JSON Schemas in
`internal/poeproto/testdata/` (regenerated by
`scripts/regen-poe-schema.sh` from a pinned `fastapi-poe`).

### Triggering a refetch

Poe caches the settings response per bot. Force a refetch with:

```
POST https://api.poe.com/bot/fetch_settings/<bot-name>/<access-key>/1.1
```

200 with `'parameter_controls': {...}` in the echo body confirms the
schema was accepted; `'parameter_controls': None` means it was
silently dropped (see gotchas above).

## Tool Calling (OpenAI-compatible)

### Defining Tools

```json
{
  "type": "function",
  "function": {
    "name": "get_weather",
    "description": "Get current weather for a location",
    "parameters": {
      "type": "object",
      "properties": { "location": { "type": "string" } },
      "required": ["location"]
    }
  }
}
```

### Tool Call Flow

1. Bot receives `query` with `tools[]` definitions
2. Bot responds with `tool_calls[]` in the message
3. Poe executes tools (or sends back to bot with `tool_results[]`)
4. Bot receives follow-up query with `tool_results[]` and generates final response

Streaming tool calls use `ToolCallDefinitionDelta` with `index`, `id`, `type`, and partial `function.arguments`.

## Parameter Controls

Bots can define interactive UI controls in settings:

- **TextField** / **TextArea** — text input
- **DropDown** — select from options
- **ToggleSwitch** — boolean
- **Slider** — numeric range with step
- **AspectRatio** — image dimension picker
- **Divider** — visual separator
- **ConditionallyRenderControls** — show/hide based on other parameter values

Controls are grouped into **Sections** (optionally collapsible) with optional **Tabs**.
User selections arrive in `query[-1].parameters`.

## Monetisation

Bots can authorise and capture costs:
- `authorize_cost(amount_usd_milli_cents, description)` — pre-auth before expensive work
- `capture_cost(amount_usd_milli_cents, description)` — charge after completion

## Limits

| Limit | Value |
|---|---|
| First event | Within 5 seconds |
| Total response | Within 120 seconds (spec; bridge uses 50 min) |
| Response length | 10,000 characters |
| Event count | 1,000 events |

## What's NOT in the Protocol

- **Thinking/reasoning blocks** — no native event type for collapsible thinking
- **Typing indicators** — no way to signal "bot is thinking" beyond the meta event
- **Reactions from bot** — bots receive reactions but cannot send them
- **Edit/delete** — bots cannot edit or delete previous messages
- **Push messages** — bots cannot initiate messages; they only respond to queries
