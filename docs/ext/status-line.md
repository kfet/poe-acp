# ACP Extension — `dev.poe-acp.status-line/v1`

A compact one-line status header that poe-acp prepends to assistant
responses and to the live "Thinking…" indicator, so users on mobile
chat surfaces see fir-style mood / plan signals they'd otherwise miss
without a TUI.

This document is the wire spec. The relay-side renderer lives in
[`internal/statusline`](../../internal/statusline/), the parser feeds
through [`internal/router`](../../internal/router/), and the SSE
emitter is in [`internal/httpsrv`](../../internal/httpsrv/).

## Format

Example final header prepended to an assistant message:

```
🏛️ • steady • 2/5

…the agent's actual reply continues here…
```

While the agent is thinking, the relay's heartbeat spinner carries the
same header with an animated `Thinking.`/`Thinking..`/`Thinking...`
suffix, rendered inside Poe's blockquote/italic style:

```
> _🏛️ • steady • 2/5 • Thinking..._
```

The header has three segments, in fixed order:

1. **Provider emoji** — relay-resolved. Identifies the model provider
   (Anthropic 🏛️, OpenAI 🌐, Google ✨, etc.). Never supplied by the
   agent.
2. **Mood** — agent-supplied opaque string (e.g. `steady`, `curious`,
   `frayed`). Length-capped at 12 runes by the renderer.
3. **Plan** — agent-supplied opaque string (e.g. `2/5`, `step 3`).
   Format is the agent's choice; the relay never parses it. Same 12-rune
   cap.

Segments with empty values are dropped. The remaining non-empty
segments are joined with ` • ` (space–bullet–space). If all three
would be empty, no header is emitted on the final message; the spinner
falls back to the bare `> _Thinking..._` frame for liveness.

## Negotiation

Both sides advertise support via their `_meta` map in the ACP
`initialize` handshake.

**Client → agent**, in `clientCapabilities._meta`:

```json
{
  "_meta": {
    "dev.poe-acp.status-line/v1": { "version": 1 }
  }
}
```

**Agent → client**, in `agentCapabilities._meta`:

```json
{
  "_meta": {
    "dev.poe-acp.status-line/v1": { "version": 1 }
  }
}
```

`version` is an integer. Future major-version breaking changes will
use a new key (e.g. `.../v2`).

Negotiation is informational: the renderer does not gate on the
agent's advertisement. Agents that don't emit `_meta` still get a
provider-emoji-only header (or no header if the provider is unknown).
The advertisement just lets each side log the other's support for
diagnostics.

## Streaming payload

The agent reports the latest mood and plan by attaching `_meta` to any
`session/update` notification — typically the same frame that carries
an `AgentMessageChunk`, an `AgentThoughtChunk`, or a plan update.

```jsonc
{
  "jsonrpc": "2.0",
  "method": "session/update",
  "params": {
    "sessionId": "sess-…",
    "_meta": {
      "dev.poe-acp.status-line/v1": {
        "mood": "steady",
        "plan": "2/5"
      }
    },
    "update": { /* whatever update kind the agent is sending */ }
  }
}
```

Field semantics:

- `mood` (string, optional) — opaque label. The relay trims whitespace,
  caps to 12 runes, and renders as-is.
- `plan` (string, optional) — opaque label. Same treatment.

Both fields are optional and may be omitted independently. To clear a
previously-set field the agent emits an empty string.

Updates may be sent as often as the agent likes; the renderer keeps
the latest values and re-renders on every heartbeat tick. A typical
emitter sends two frames per turn: one shortly after `session/prompt`
acks (to populate the spinner), and one with the final assistant
chunk (to lock in the final header).

## Provider emoji

The provider emoji is **always** chosen by poe-acp from the model id
it dispatched the turn to (`<provider>/<model>` convention). The agent
must not include it in `_meta`; doing so has no effect.

| Provider slug                                   | Emoji |
| ----------------------------------------------- | ----- |
| `anthropic`, `claude`                           | 🏛️    |
| `openai`, `codex`                               | 🌐    |
| `poe`                                           | 👻    |
| `google`, `gemini`, `google-antigravity`        | ✨    |
| `copilot`, `github-copilot`, `github`           | 🐙    |
| `sakana`                                        | 🐡    |
| `xai`, `grok`                                   | ✖️    |
| `mistral`, `mistralai`                          | 🌪️    |
| `meta`, `meta-llama`, `llama`                   | 🦙    |
| `openrouter`                                    | 🔀    |
| `groq`                                          | ⚡    |
| `deepseek`                                      | 🐋    |
| `cohere`                                        | 🔗    |

Slug match is case-insensitive on the part of the model id before the
first `/`. Unknown providers (and model ids with no `/`) produce no
emoji — the segment is dropped.

## Renderer rules

- Spinner frames are emitted by the SSE heartbeat as
  `replace_response` events. Each tick rebuilds the frame from the
  latest snapshot of `(emoji, mood, plan)`, animating the dot count
  for liveness.
- The final header is emitted exactly once, as the first portion of
  the very first `text` event for the turn. It is **not** prepended on
  `replace_response` paths (e.g. `_(cancelled)_`) — those overwrite
  the body, so a header there would be erased anyway.
- If the rendered header is empty (unknown provider + no agent
  `_meta`), the first `text` event passes through unchanged.
- Mood and plan are length-capped at 12 runes (not bytes) — emoji and
  non-ASCII strings count by rune, never split a UTF-8 sequence. No
  ellipsis is appended; the cap is tight enough that an ellipsis would
  cost meaningful characters.
- Non-string `mood` / `plan` values are ignored (treated as absent),
  not rejected, for forward compatibility.

## Out of scope

- The fir-side emitter that populates `_meta` on each
  `session/update` — tracked separately.
- Any reverse-direction signal (client → agent) carrying status. The
  extension is one-way: agent → relay.

## Code anchors

- Renderer + slug map: [`internal/statusline/statusline.go`](../../internal/statusline/statusline.go)
- `_meta` parsing in the router's chunk drain: search
  `drainProcessChunk` in [`internal/router/router.go`](../../internal/router/router.go)
- Spinner + header prepend on the SSE sink:
  [`internal/httpsrv/handler.go`](../../internal/httpsrv/handler.go)
  (search for `statusline.Spinner` and `maybePrependHeader`).
- Capability advertisement: `client.Config.ClientMeta` in
  [`cmd/poe-acp/main.go`](../../cmd/poe-acp/main.go).
