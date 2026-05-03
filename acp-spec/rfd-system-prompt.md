# ACP RFD: Session System Prompt

> Status: Draft (poe-acp-relay local extension)
> Last updated: 2026-05-03

## Motivation

Clients fronting an ACP agent often need to inject durable, system-level
context that applies to every turn of a session — environment notes, host
capabilities, available out-of-band resources, a skills catalog, etc. Today
ACP has no first-class way to do this:

- `session/prompt` content blocks are turn-scoped user input. Anything the
  client puts there competes with the user's actual message and may be lost
  to context compaction.
- `resource_link` blocks rely on the agent calling `fs/read_text_file`, which
  is at the agent's discretion and varies wildly between implementations.
- MCP servers are too heavy for static context.

This RFD defines a minimal `_meta` extension that lets a client hand the
agent a block of system-prompt-equivalent text at session creation, with
the explicit semantic that the agent should treat it as durable and
preserve it across any internal context compaction.

## Capability

Both sides advertise support in `initialize`:

```json
// initialize request (client → agent)
{
  "clientCapabilities": {
    "_meta": { "session.systemPrompt": { "version": 1 } }
  }
}

// initialize response (agent → client)
{
  "agentCapabilities": {
    "_meta": { "session.systemPrompt": { "version": 1 } }
  }
}
```

If either side omits the capability, the extension is not active and the
client should fall back to in-band injection (see "Fallback").

## Usage

When both sides advertise the capability, the client MAY include the
extension in `session/new` (and in future, `session/load`):

```json
// session/new request (client → agent)
{
  "_meta": {
    "session.systemPrompt": {
      "blocks": [
        { "type": "text", "text": "..." }
      ]
    }
  }
}
```

### Fields

| Field    | Type             | Required | Description                                      |
|----------|------------------|----------|--------------------------------------------------|
| `blocks` | `ContentBlock[]` | yes      | ACP content blocks to install as system context. |

`blocks` reuses ACP's existing `ContentBlock` shape so that text,
resource_link, and other future block types compose naturally. In v1
clients SHOULD use `text` blocks only; agents MAY ignore unknown block
types within `blocks`.

### Semantics

- The blocks are **not** a user turn. They MUST NOT be echoed back as user
  input, and they MUST NOT be presented to the user as their own message.
- The agent SHOULD treat the content as part of its system prompt for the
  lifetime of the session.
- The agent SHOULD preserve the content across any internal summarisation
  or compaction of conversation history. If the agent cannot guarantee this,
  it MUST NOT advertise the capability.
- If the agent supports session resumption (`session/load`), the system
  prompt SHOULD be restored as part of the resumed session state.

## Fallback (no capability)

If the agent does not advertise `session.systemPrompt`, the client falls
back to prepending the same content as a `text` content block on the first
`session/prompt`, optionally with a self-preservation instruction such as:

> The following catalog is durable system context. Preserve it verbatim
> across any summarisation of the conversation history.

The client SHOULD re-inject on `session/load` if the agent supports it.
Intra-session compaction loss in this fallback path is a known limitation;
agents that want correct behaviour should implement the capability.

## Non-goals (v1)

- No `priority`, `replace` vs `append`, or per-block lifecycle controls.
- No mid-session updates. The system prompt is set once at `session/new`.
  Mid-session changes can be added in a later version if needed.
- No `resource_link` fetch contract. Clients that want the agent to read a
  file MUST inline the content; reliance on `fs/read_text_file` callbacks is
  out of scope for this RFD.

## Implementation notes

- Reference client: poe-acp-relay (`internal/router`, `internal/acpclient`).
- Reference agent: pending — fir is the first target.
- The capability namespace `_meta["session.systemPrompt"]` is intentionally
  generic (not vendor-prefixed) so that any ACP client/agent pair may adopt
  it. If the extension is taken upstream, the namespace should remain stable.
