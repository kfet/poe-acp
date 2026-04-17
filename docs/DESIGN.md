# Poe ↔ ACP relay (pure ACP client design)

## Status

Proposal / scaffold. Branch `wt/poe-acp-relay`. Greenfield; does not reuse the
MCP-centric bridge in `wt/poe-integration` (`external/poe`), though it lifts a
few primitives (SSE writer, access-control pairing, optional tsnet Funnel).

## Motivation

The existing Poe bridge (`wt/poe-integration`) glues Poe to fir through **MCP**:
fir is the "host", the bridge is an MCP server process that fir spawns, and the
bridge injects inbound Poe messages into fir via a `notifications/claude/channel/message`
notification while fir calls back via a `reply` MCP tool. Fir also owns the
ACP/REPL session lifecycle — spawning worktree agents, handling slash commands,
etc. Poe is one of several I/O surfaces wired into a user-driven fir process.

This works, but it inverts the natural ownership for a **pure server-side bot**:

- Poe calls the bot; there is no human at a terminal. A long-lived fir process
  is overkill.
- Session lifecycle (create, resume, route-by-conv-id, spawn-on-demand) is
  logic the **relay** should own, not fir. It is the thing that sees every
  inbound request with a stable `conversation_id`.
- MCP is the wrong protocol for the relay's actual need: "drive an AI agent
  and stream its output back." That is exactly what **ACP** is for.

So: build a Poe bot where the **relay is an ACP client** that spawns or
connects to ACP-compliant agents (starting with `fir --mode acp`) and
manages a 1:1 map of Poe `conversation_id` → ACP session. No MCP server. No
`reply` tool. The relay drives sessions directly.

## Goals

1. **Pure ACP client.** The relay uses `acp-go-sdk`'s `ClientSideConnection`
   to speak to any conforming ACP agent. No MCP dependency on the hot path.
2. **Per-conversation ACP sessions.** Each Poe `conversation_id` maps to
   exactly one ACP `SessionId`. Inbound Poe `query` → `session/prompt`.
   ACP `session/update` chunks → SSE `text` / `replace_response` events.
3. **Agent-agnostic.** Default agent is `fir --mode acp`, but the transport
   is a plain stdio ACP connection, so any ACP agent (Zed's, claude-code-acp,
   gemini-acp, …) could be wired in via a config flag.
4. **Zero fir-side changes (initial milestone).** Everything new lives under
   `cmd/poe-acp-relay/` + `internal/poeacp/`. Fir is consumed as a library
   only for the `acp` SDK dependency.
5. **Stable public HTTPS.** Optional `tsnet` Funnel mode so the relay can be
   dropped on any tailnet host and get a public cert without a reverse proxy.

## Non-goals (v1)

- **No multi-host relay** (single process owns Poe + agents). The
  `wt/poe-integration` ws-based relay→agent fanout is explicitly *out*; the
  ACP relay owns its agent processes directly. That distinction can come
  later as "remote ACP over ws" if desired.
- **No MCP server surface.** The relay does not advertise itself as an MCP
  server. If an ACP session wants tools, the agent loads them itself via its
  own config (fir reads MCP from `.fir/mcp.json` in the session cwd).
- **No inbound tool-call surface** for the Poe user. Sessions are one-way
  (user text in, assistant text out). `session/request_permission` is
  auto-approved (or auto-rejected) per a configured policy. Permission
  round-trips back to the Poe user are a future enhancement.
- **No attachments round-trip** initially. Poe attachments arrive as URLs;
  we pass their text representation through as a pre-amble, nothing else.
- **No multi-user auth** initially. Single Poe `access_key` bearer; optional
  allowlist of `user_id`s in a config file.

## Architecture

```
  ┌──────────┐   HTTPS POST (query)     ┌──────────────────────────────┐
  │   Poe    │ ────────────────────────▶│  poe-acp-relay (single bin)  │
  │ servers  │ ◀──────── SSE ───────────│                              │
  └──────────┘                          │  ┌────────────────────────┐  │
                                        │  │  internal/poeacp/http  │  │
                                        │  │  (Poe protocol, SSE)   │  │
                                        │  └──────────┬─────────────┘  │
                                        │             │                │
                                        │  ┌──────────▼─────────────┐  │
                                        │  │  internal/poeacp/router│  │
                                        │  │  conv_id → session     │  │
                                        │  │  spawn / reuse / GC    │  │
                                        │  └──────────┬─────────────┘  │
                                        │             │ ACP (stdio)    │
                                        │  ┌──────────▼─────────────┐  │
                                        │  │  internal/poeacp/acp   │  │
                                        │  │  ClientSideConnection  │  │
                                        │  │  impl of acp.Client    │  │
                                        │  └──────────┬─────────────┘  │
                                        └─────────────┼────────────────┘
                                                      │ stdio
                                  ┌───────────────────┼───────────────────┐
                                  ▼                   ▼                   ▼
                            ┌──────────┐        ┌──────────┐        ┌──────────┐
                            │ fir acp  │        │ fir acp  │        │ fir acp  │
                            │ conv α   │        │ conv β   │        │ conv γ   │
                            └──────────┘        └──────────┘        └──────────┘
```

### Components

#### `internal/poeacp/poeproto` — Poe protocol

Trimmed copy of `external/poe/internal/poe` from the MCP bridge:

- `SSEWriter` (text / replace_response / error / done events)
- Request decoder for `query` / `settings` / `report_*`
- Settings response builder (introduction_message, commands list)
- Bearer auth middleware

Zero changes from the existing impl — this layer is agnostic about what's
behind it.

#### `internal/poeacp/acpclient` — ACP client wrapper

A small wrapper around `acp.ClientSideConnection`. Key pieces:

- **`type AgentProc`** — one stdio child (`fir --mode acp` by default). Owns
  the `*exec.Cmd`, the `ClientSideConnection`, and a map of live session IDs.
- **`type Client`** — implements `acp.Client` (server-initiated methods):
  - `RequestPermission` → policy engine (v1: auto-allow all tool calls; later:
    prompt the Poe user via a pending-reply side channel).
  - `SessionUpdate` → fan out to the per-session chunk stream currently
    attached to an open Poe SSE response.
  - `ReadTextFile` / `WriteTextFile` → implemented against the agent's cwd
    (same as the SDK example client).
  - Terminal methods → no-op stubs (fir agents don't require them).
- **Initialize once, NewSession per Poe conv**: the relay calls `Initialize`
  on process start, then `NewSession` (or `LoadSession` when resuming) for
  each new `conversation_id`.

#### `internal/poeacp/router` — conversation router

The brains. Maps `conversation_id → sessionState`.

```go
type sessionState struct {
    convID    string
    userID    string
    sessionID acpsdk.SessionId      // ACP session id
    agent     *acpclient.AgentProc
    pending   chan chunk            // live inbound Poe request's SSE stream
    lastUsed  time.Time
}
```

Policies (v1):

- **One ACP session per Poe conv.** Created lazily on the first query.
- **One agent process per N sessions** (configurable; default 1). This lets
  fir's in-memory session registry serve many convs from one fir process,
  which is cheaper than one process per conv.
- **Idle GC.** Sessions idle > `SESSION_TTL` (default 2h) are dropped from
  the map; underlying ACP session is allowed to age out on the agent side.
  Agents with zero live sessions and zero inflight prompts are `SIGTERM`ed
  after `AGENT_IDLE_TTL` (default 10m).
- **Concurrency.** A single Poe conv serialises prompts: while a prompt is
  inflight, new inbound queries for the same conv either (a) wait (default)
  or (b) cancel-and-restart (configurable, mirrors `session/cancel`).

#### `internal/poeacp/policy` — permission policy

v1 options:

- `allow-all` (default) — auto-approve every `session/request_permission`.
  Fine for read-only coding-assist sessions where fir's own tool surface is
  the limit.
- `read-only` — approve reads / greps / `ls`; deny writes and bash.
- `deny-all` — reject every permission request. For demo / exploration.

Controlled by `POEACP_PERMISSION_POLICY`.

#### `cmd/poe-acp-relay/main.go` — entry point

```
Usage: poe-acp-relay [flags]

Transport:
  --http-addr     :8080       Poe HTTP listen address
  --funnel                    Use tsnet Funnel (public HTTPS :443)
  --funnel-name   poe-relay   Funnel hostname under tailnet

Agent:
  --agent-cmd     "fir --mode acp"   ACP agent command (stdio)
  --agent-cwd     $HOME              Working dir for spawned agent(s)
  --sessions-per-agent  8            Sessions multiplexed per agent proc

Access:
  --access-key-env  POEACP_ACCESS_KEY  Poe bearer secret
  --allow-user-ids  u-abc,u-def         Optional allowlist
  --permission      allow-all           allow-all|read-only|deny-all

State:
  --state-dir     $XDG_STATE_HOME/poe-acp-relay
```

## Request flow

### Inbound `query`

```
1. POST /poe arrives; bearer checked.
2. Parse request; extract conv_id, message_id, user_id, latest user msg.
3. Allowlist check (user_id).
4. SSE writer opens: emit `meta` event immediately (5s rule).
5. Router.Handle(conv_id, user_id, msg, sse):
     a. Look up session; if none, AgentProc.NewSession(cwd) → session_id.
     b. Register sse as current chunk sink for session_id.
     c. Call acp.Prompt(session_id, TextBlock(msg)) asynchronously.
6. acp.Client.SessionUpdate callbacks fire:
     - AgentMessageChunk   → sse.WriteText(chunk)
     - AgentThoughtChunk   → optional: suppressed or forwarded as dim text
     - ToolCall / Update   → collapsed into a compact status line (opt-in)
     - Plan                → rendered as a short markdown block (opt-in)
7. acp.Prompt returns (StopReason):
     - end_turn   → sse.WriteEvent("done", {})
     - max_tokens → append "(truncated)" + done
     - refusal    → emit SSE `error` + done
     - cancelled  → emit `replace_response` + done
```

### Inbound `settings`

Return static JSON: bot description, optional `introduction_message`,
`commands[]` derived from fir's built-in slash list (reused from
`resources.BuiltinSlashCommands`, same as the MCP bridge).

### Agent side

`fir --mode acp` is launched once per `sessions-per-agent` budget. On boot
the relay calls `Initialize` with full client capabilities:

```go
acp.ClientCapabilities{
    Fs:       acp.FileSystemCapability{ReadTextFile: true, WriteTextFile: true},
    Terminal: false,
}
```

then reuses the connection for every `NewSession` / `Prompt`. The `firSession`
registry inside `pkg/modes/acp/acp.go` already supports multiple concurrent
sessions per process — no fir-side changes needed.

## Comparison to the MCP bridge (`wt/poe-integration`)

| Aspect                       | MCP bridge                              | ACP relay                               |
|------------------------------|-----------------------------------------|-----------------------------------------|
| Fir relationship             | Fir spawns bridge as its MCP server      | Relay spawns fir as its ACP agent        |
| Conv → session mapping       | Fir owns; bridge just injects messages   | Relay owns; drives `session/prompt`      |
| Assistant output path        | Fir calls `reply` MCP tool → SSE         | ACP `session/update` chunks → SSE        |
| Multi-conv fanout            | ws relay + catch-all agent + spawn skill | Router in-process, spawns ACP agents     |
| Tool permission              | Fir's own UX (interactive prompt)        | Policy engine (v1: allow-all)            |
| Slash commands               | MCP `claude/commands` notification       | Poe settings reuses `BuiltinSlashCommands` |
| Required in fir              | `claude/channel` MCP notifications       | Nothing — just `--mode acp`              |
| Complexity                   | High (fir + bridge + relay + ws + skill) | Medium (relay + stdio ACP children)      |

The MCP bridge's strength is that a human can also be driving the same fir
session interactively while Poe messages are interleaved. The ACP relay is
deliberately **headless** — no human at the fir end — which is the right fit
for a server bot.

## Open questions

1. **Session persistence across relay restarts.** Fir's ACP mode supports
   `LoadSession` with a `session_id`. Can we persist `conv_id → session_id`
   to disk and rehydrate on restart? The session store should already handle
   this, but needs end-to-end testing.
2. **Long prompts.** Poe's 5-second "must emit something" rule: the relay
   emits `meta` immediately, then a `text` space, but if fir takes > 25s to
   produce the first chunk Poe may close. Solution: emit an ACP
   `session/update` heartbeat in fir, or insert a relay-side "thinking…"
   dot-stream on a 10s timer.
3. **Pairing / access control.** The MCP bridge has an elaborate pairing
   flow (slash-command in the terminal to approve). The relay is headless;
   closest analogue is env-var allowlist + a "pending" SSE reply that says
   "your user_id `u-xxx` is not authorised — ask the operator to add it."
4. **Per-conv cwd.** Do we want each Poe conv to get a scratch dir
   (`$state/convs/<conv_id>/`) so fir has a sandbox with `.fir/` config? v1
   says yes — isolates sessions and makes MCP config scoping trivial.
5. **Remote agents.** The MCP bridge supports distributing agents across
   the tailnet. For ACP we'd need an "ACP over ws" transport. Punted to v2.

## Milestones

- **M0 (this branch):** design doc + skeleton that compiles. No Poe I/O yet.
  `poe-acp-relay --version` prints and exits.
- **M1:** stdio ACP client working end-to-end against `fir --mode acp`.
  A CLI test harness (`poe-acp-relay test-prompt "hello"`) spins up fir and
  streams a response to stdout.
- **M2:** Poe HTTP/SSE front-end, in-memory `conv_id → session` router,
  `allow-all` permission policy. Deployable as a single binary behind a
  reverse proxy.
- **M3:** Tailscale Funnel mode, persistent conv-id state, allowlist,
  read-only / deny-all policies, idle GC.
- **M4:** Slash commands in Poe settings, plan / tool-call update rendering,
  resume / cancel handling.
- **M5 (stretch):** permission round-trip to the Poe user via an
  interstitial SSE reply; remote ACP transport over ws.
