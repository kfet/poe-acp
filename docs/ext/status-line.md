# ACP Extension — `dev.acp-kit.status-line/v1`

A compact one-line status line that poe-acp appends as an italic
FOOTER under assistant responses, and shows live in the "Thinking…"
indicator, so users on mobile chat surfaces see fir-style model / mood
/ plan signals they'd otherwise miss without a TUI.

It is a footer, not a header: `mood` and `plan` are agent-supplied and
usually arrive mid-turn, so a line rendered before the first token
would show a status the agent had not published yet. Rendered at the
end, it always carries the final snapshot.

This document is the wire spec. The relay-side renderer lives in
[`internal/statusline`](../../internal/statusline/), the parser feeds
through [`internal/router`](../../internal/router/), and the SSE
emitter is in [`internal/httpsrv`](../../internal/httpsrv/).

## Format

Example footer appended to an assistant message:

```
…the agent's actual reply ends here…

_🏛️ opus-4.5 • steady • 2/5_
```

The footer is preceded by a blank line and wrapped in `_…_`, so it
reads as a signature under the answer rather than as body text.

While the agent is thinking, the relay's heartbeat spinner carries the
same line with an animated `Thinking.`/`Thinking..`/`Thinking...`
suffix, rendered inside Poe's blockquote/italic style:

```
> _🏛️ opus-4.5 • steady • 2/5 • Thinking..._
```

The line has three segments, in fixed order:

1. **Model identity** — relay-resolved, never supplied by the agent.
   The provider emoji (Anthropic 🏛️, OpenAI 🌐, Google ✨, etc.) and the
   short model name, joined by a **single space** and NOT by a bullet:
   they name one thing. Either half alone degrades to just that half.
2. **Mood** — agent-supplied opaque string (e.g. `steady`, `curious`,
   `frayed`). Length-capped at 12 runes by the renderer.
3. **Plan** — agent-supplied opaque string (e.g. `2/5`, `step 3`).
   Format is the agent's choice; the relay never parses it. Same 12-rune
   cap.

Segments with empty values are dropped. The remaining non-empty
segments are joined with ` • ` (space–bullet–space). If all three
would be empty, no footer is emitted on the final message; the spinner
falls back to the bare `> _Thinking..._` frame for liveness.

### Short model name

The model name is derived by the relay from the dispatched model id,
never sent by the agent. `acp-kit/statusline.ShortModelName` applies,
in order, to the part after `<provider>/`:

1. drop the `<provider>/` prefix (no `/` → use the whole string);
2. drop a trailing date stamp `-YYYYMMDD` and a trailing `-latest` /
   `-preview` (repeatedly, so `-preview-20251101` unwinds fully);
3. drop a leading vendor echo the emoji already carries — `claude-`,
   `anthropic-`. Family prefixes that carry meaning (`gpt-`, `gemini-`,
   `grok-`, `llama-`, `deepseek-`) are **kept**;
4. rewrite a dash BETWEEN TWO DIGITS as a dot (`4-5` → `4.5`), leaving
   name dashes (`gpt-5-codex`) alone;
5. lowercase and cap to 12 runes.

| Model id                             | Rendered     |
| ------------------------------------ | ------------ |
| `anthropic/claude-opus-4-5-20251001` | `opus-4.5`   |
| `anthropic/claude-sonnet-4-5`        | `sonnet-4.5` |
| `openai/gpt-5-codex`                 | `gpt-5-codex`|
| `google/gemini-3-pro-preview`        | `gemini-3-pro`|
| `poe/Claude-Opus-4.5`                | `opus-4.5`   |

The result is lossy by design — it is a display label and must never be
fed back onto the wire as a model id.

## Negotiation

Both sides advertise support via their `_meta` map in the ACP
`initialize` handshake.

**Client → agent**, in `clientCapabilities._meta`:

```json
{
  "_meta": {
    "dev.acp-kit.status-line/v1": { "version": 1 }
  }
}
```

**Agent → client**, in `agentCapabilities._meta`:

```json
{
  "_meta": {
    "dev.acp-kit.status-line/v1": { "version": 1 }
  }
}
```

`version` is an integer. Future major-version breaking changes will
use a new key (e.g. `.../v2`).

Negotiation is informational: the renderer does not gate on the
agent's advertisement. Agents that don't emit `_meta` still get a
model-identity-only footer (or none if the provider and model are
both unknown).
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
      "dev.acp-kit.status-line/v1": {
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
chunk. Because the status line is rendered as a footer at the end of
the turn, a late update still lands in it — which is exactly why the
line moved to the bottom.

## Provider emoji

The provider emoji — like the short model name — is **always** chosen
by poe-acp from the model id it dispatched the turn to
(`<provider>/<model>` convention). The agent must not include either in
`_meta`; doing so has no effect.

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
  latest snapshot of `(emoji, model, mood, plan)`, animating the dot
  count for liveness.
- The footer is emitted exactly once per turn, as the last `text`
  event before the terminal `done`, from the LATEST status snapshot.
  It survives the transient keepalive region for free: the footer goes
  out through the ordinary text path, which strips a visible spinner
  first, and `done` seals the stream so no later heartbeat frame can
  replace it.
- The footer is suppressed when the turn produced no user-visible
  content, and on **error turns** (a Poe `error` event is followed by a
  mandatory `done`, so the sink latches the failure to tell the two
  apart).
- If the rendered line is empty (unknown provider + no model + no agent
  `_meta`), nothing is appended — not even the blank line.
- The footer is not part of the recorded answer used for redrive
  replay: it is produced inside the sink's `Done`, below the recorder,
  and a replay regenerates it from the replayed status instead of
  emitting it twice.
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
- Spinner + footer append on the SSE sink:
  [`internal/httpsrv/handler.go`](../../internal/httpsrv/handler.go)
  (search for `statusline.Spinner` and `maybeAppendFooter`).
- Redrive record/replay of the status calls:
  [`internal/httpsrv/buffer.go`](../../internal/httpsrv/buffer.go)
  (search for `opSetModelInfo`).
- Capability advertisement: `client.Config.ClientMeta` in
  [`cmd/poe-acp/main.go`](../../cmd/poe-acp/main.go).
