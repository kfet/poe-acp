# Poe ↔ ACP relay (pure ACP client design)

## Status

Active build. Lives as a standalone Go module (`github.com/kfet/poe-acp-relay`) —
not linked into the main `fir` binary.

## Motivation

The existing Poe bridge (`wt/poe-integration`, `external/poe/`) glues Poe
to fir through **MCP**: fir is the "host", the bridge is an MCP server that
fir spawns, the bridge injects Poe messages via a custom
`notifications/claude/channel/message` notification, and fir calls back
via a `reply` MCP tool. Session lifecycle (spawn-on-demand, catch-all,
slash commands) lives in **fir + a skill**, coordinated through a ws relay.

This works but inverts the natural ownership for a **headless server bot**:

- Poe calls the bot; no human at a terminal. A long-lived fir is overkill.
- Session lifecycle (create, resume, route-by-conv-id) is logic the
  **relay** should own — it is the thing that sees every inbound request
  with a stable `conversation_id`.
- MCP is the wrong protocol for "drive an AI agent and stream its output
  back". That is exactly what **ACP** is for.

So: the relay is a pure **ACP client** that drives ACP-compliant agents
(default: `fir --mode acp`) and maintains a 1:1 map of Poe
`conversation_id` → ACP `session_id`. No MCP server. No `reply` tool.

## Goals

1. **Pure ACP client.** Relay uses `acp-go-sdk`'s `ClientSideConnection`.
   Any conforming ACP agent can be plugged in via `--agent-cmd`.
2. **Per-conv ACP sessions** in a shared fir process. One fir, N sessions,
   driven through one ACP stdio connection.
3. **Per-conv cwd** so fir's session history, `.fir/settings.json`, and any
   `.fir/mcp.json` are naturally isolated between Poe convs.
4. **Zero fir-side changes.** Consumed via `--mode acp` over stdio.
5. **Own Go module** (`github.com/kfet/poe-acp-relay`). Not linked into the fir binary.

## Non-goals (v1)

- **No multi-agent pool.** One fir process multiplexes all sessions. If it
  crashes, all convs reconnect on the next Poe message. (A pool can be
  added later if contention becomes visible.)
- **No MCP server surface.** If a session wants MCP tools, the *agent*
  loads them from its own config (fir reads `.fir/mcp.json` in the cwd).
- **No user auth / allowlist.** Any request with the correct
  `POEACP_ACCESS_KEY` is accepted. Per-user auth and state are tracked as
  a future item.
- **No Tailscale Funnel mode.** We deploy to nodes already on tailnet; a
  fronting reverse proxy or `tailscale funnel` on the host handles TLS.
  Documented as a future option (see Future section).
- **No attachments round-trip.** Poe attachments arrive as URLs; we don't
  fetch them.
- **No remote ACP transport.** The relay spawns agents locally over stdio.
- **No permission round-trip to the Poe user.** `session/request_permission`
  is handled by a local policy (allow-all / read-only / deny-all).

## fir multi-session facts (as of wt/poe-acp-relay base)

- `firAgent.sessions map[string]*firSession` holds concurrent sessions;
  `Prompt` / `SessionUpdate` are keyed by `SessionId`.
- **Cwd is per-session** (`NewSessionRequest.Cwd`). Fir derives session
  history dir from `(agentDir, cwd)` so each conv's cwd gets its own
  `sessions/` subtree.
- **`config.NewSettingsManager(cwd, agentDir)`** means `.fir/settings.json`
  is resolved from the per-session cwd.
- **`$FIR_AGENT_DIR` is process-wide.** `auth.json`, `models.json`,
  installed extensions, installed skills are **shared** across all
  sessions in one fir proc. For most server-bot deployments that's
  desirable (one provider login serves all convs). Full isolation would
  require a small fir patch to accept a per-session agent dir; future.

**Conclusion:** give each conv a dedicated cwd
(`$STATE_DIR/convs/<conv_id>/`) and fir's native per-cwd mechanics do the
right thing with zero modifications.

## Architecture

```
  ┌──────────┐   HTTPS POST (query)     ┌──────────────────────────────┐
  │   Poe    │ ────────────────────────▶│  poe-acp-relay (single bin)  │
  │ servers  │ ◀──────── SSE ───────────│                              │
  └──────────┘                          │  ┌────────────────────────┐  │
                                        │  │ internal/httpsrv       │  │
                                        │  │  + internal/poeproto   │  │
                                        │  │  (HTTP + SSE)          │  │
                                        │  └──────────┬─────────────┘  │
                                        │             │                │
                                        │  ┌──────────▼─────────────┐  │
                                        │  │ internal/router        │  │
                                        │  │  conv_id → session     │  │
                                        │  │  per-conv cwd          │  │
                                        │  │  idle GC               │  │
                                        │  └──────────┬─────────────┘  │
                                        │             │                │
                                        │  ┌──────────▼─────────────┐  │
                                        │  │ internal/acpclient     │  │
                                        │  │  ClientSideConnection  │  │
                                        │  │  impl of acp.Client    │  │
                                        │  └──────────┬─────────────┘  │
                                        └─────────────┼────────────────┘
                                                      │ stdio
                                                      ▼
                                              ┌──────────────┐
                                              │ fir --mode   │
                                              │    acp       │
                                              │ N sessions   │
                                              └──────────────┘
```

## Components

### `internal/poeproto` — Poe protocol

- `Request` (type, query[], conv_id, user_id, message_id)
- `SSEWriter` (Meta, Text, Replace, Error, Done)
- `SettingsResponse`
- `BearerAuth` middleware

### `internal/acpclient` — ACP agent wrapper

- `AgentProc` = exec.Cmd + `acp.ClientSideConnection`; implements
  `acp.Client` (SessionUpdate fan-out, RequestPermission → policy,
  ReadTextFile / WriteTextFile, terminal no-ops)
- `Start` launches child, calls `Initialize`
- `NewSession(ctx, cwd, sink)` creates ACP session, registers sink
- `Prompt(ctx, sid, text) → StopReason`
- `Cancel(ctx, sid)`

### `internal/router` — conv_id router

- `sessionState{convID, userID, sessionID, cwd, sink, lastUsed}`
- Lazy-create session on first query; pick cwd = `$STATE/convs/<conv_id>/`
- `Prompt(convID, text, sink)`: attach sink, issue ACP prompt, handle
  stop reason, emit terminal SSE event.
- Serial per conv: while a prompt is in flight for conv X, new inbound
  for X waits (mutex on `sessionState`). New Poe queries for **different**
  convs proceed concurrently.
- Idle GC: sessions idle > `SESSION_TTL` removed from map. Agent proc
  itself runs until relay shutdown (no pool).

### `internal/policy` — permission policy

`allow-all` / `read-only` / `deny-all`. Selected via `--permission`.

### `internal/httpsrv` — HTTP layer

- `/poe` POST (bearer-gated): dispatches `query` / `settings` / `report_*`
- `/healthz` (public): `ok sessions=N`
- `/debug/sessions` (bearer-gated): JSON dump for triage

### `cmd/poe-acp-relay` — entry point

Flags:

```
--http-addr            :8080
--agent-cmd            "fir --mode acp"
--agent-dir            $FIR_AGENT_DIR   (env override for the spawned fir)
--state-dir            $XDG_STATE_HOME/poe-acp-relay
--permission           allow-all|read-only|deny-all
--access-key-env       POEACP_ACCESS_KEY
--session-ttl          2h
--heartbeat-interval   10s
```

## Request flow

### `query`

```
1. POST /poe; bearer checked by middleware.
2. Decode Request → conv_id, user_id, latest user text.
3. Open SSEWriter → emit `meta` event.
4. Start heartbeat goroutine: every Nsec, if no chunks yet, emit a
   no-op "\u200b" (zero-width space) text event. Cancel on first real
   chunk or Done.
5. Router.Prompt(conv_id, text, sseSink):
     a. Look up/create sessionState for conv_id. Fresh session ⇒
        mkdir -p $STATE/convs/<conv_id>/; agent.NewSession(cwd).
     b. Lock sessionState.mu for the duration of the turn.
     c. Attach sseSink. Stop heartbeat on first OnUpdate.
     d. agent.Prompt(sid, text) → StopReason.
     e. Translate stop reason to terminal SSE event:
        - end_turn    → Done
        - max_tokens  → Text("\n\n_(truncated)_") + Done
        - refusal     → Error + Done
        - cancelled   → Replace("_(cancelled)_") + Done
6. Request ctx cancellation (Poe client disconnect) → Router.Cancel(conv)
   → ACP session/cancel to the agent.
```

### `settings`

Static JSON. Introduction message set via `--introduction`. Optional
commands[] populated from a small hand-written list (no fir coupling; the
relay doesn't forward slash commands to fir v1).

### `report_*`

Accept and drop.

## Test strategy

HTTP is trivial to test: a `curl | head` pointed at `http://localhost:8080/poe`
with a crafted JSON body and a bearer header streams SSE to stdout. We'll
script a `test/smoke.sh` that sends a minimal `query` request and asserts
the response contains `event: text` and `event: done`.

Unit tests use the `acp-go-sdk`'s in-memory pair: spin up a fake agent in
the same process that emits scripted `SessionUpdate` chunks; drive
`router.Prompt` against it; assert the `ChunkSink` captured the expected
text and stop-reason translation.

## Deployment

Single binary on a tailnet node, fronted by either:
- an existing reverse proxy (Caddy / nginx) terminating TLS, OR
- `tailscale funnel` on the host for zero-config HTTPS.

The Poe bot dashboard URL points at `https://<node>.<tailnet>.ts.net/poe`.
Bearer secret lives in the host environment.

**Funnel gotcha:** `tailscale funnel --set-path=/prefix` **strips the
prefix** before proxying to the backend. If multiple bots share a node
via path routing, the Poe URL must still include a path the relay
actually registers. Given the default `--poe-path /poe`, a bot mounted
under `/poe-acp` needs its Poe URL set to `/poe-acp/poe` (not bare
`/poe-acp`). See `README.md` → "Deployment" for a concrete example.

## Future

- **Per-user auth/state config.** Map Poe `user_id` to a scoped config
  blob (which provider, which agent dir, which default cwd). Replaces
  the simple allowlist gate.
- **Per-session fir agent dir.** Small fir patch to accept an
  `agent_dir` on `NewSessionRequest` so auth/models/extensions can be
  isolated per conv, not just per cwd. Needed for multi-tenant hosting.
- **Embedded Tailscale Funnel mode.** `tsnet.Server` + `ListenFunnel`
  to get public HTTPS from the binary itself, no reverse proxy. Not
  needed for our deployment (we sit behind a node already on tailnet).
- **Multi-agent pool.** Shard conv_ids across N fir processes for
  resilience and isolation. Only if one process becomes a bottleneck.
- **Remote ACP transport.** "ACP over ws" so agents can live on a
  different host than the relay.
- **Permission round-trip to the Poe user.** Turn
  `session/request_permission` into an interstitial Poe message with
  an inline allow/deny; requires a pending-continuation mechanism.
- **Attachments.** Download Poe attachment URLs and inject as
  `EmbeddedResource` content blocks in the ACP prompt.
- **Slash commands.** Forward a command list (from fir's
  `BuiltinSlashCommands` + extensions) into Poe settings. First version
  can be a static hand-written list.

## Milestones

- **M0** — scaffold compiles; `--version` works. ✅
- **M1** — end-to-end over HTTP: real `fir --mode acp` child, Poe-shaped
  `curl` request in, assistant text streams out on SSE. Heartbeat,
  cancellation, stop-reason translation, per-conv cwd, idle GC. ✅
- **M2** — cleanup + docs + smoke test script. Ship.

## Live test (2026-04-19)

First real end-to-end run: relay spawned `fir --mode acp`, a curl against
`/poe` produced `meta` → zero-width-space heartbeats during fir boot →
real assistant text chunk (a rate-limit error surfaced from Anthropic) →
clean `done`. `/debug/sessions` showed the conv_id mapped to an ACP
session_id in its per-conv cwd. The 429 was surfaced in-band as agent
text, not as a relay error, which is the correct behaviour (the agent is
responsible for describing provider failures to the user).

Notes from the run:

- Fir sends `available_commands_update` on session start. `AgentProc`
  now snapshots it and `httpsrv.Config.CommandsProvider` exposes the
  names in the Poe `settings.commands` response.
- Cold-start cost: ~1s to spawn+Initialize, another ~50–55s for the
  first prompt (extension boot + first API call). Heartbeats handled it.
- Earlier confusion about the prompt hanging turned out to be a
  30-second deadline on a standalone probe — the production HTTP path
  uses the request context and works fine.
