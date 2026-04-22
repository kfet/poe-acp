# poe-acp-relay

Poe.com server-bot that drives ACP-compliant agents (default: `fir --mode acp`)
as a pure ACP client. One binary, no MCP server surface. Each Poe
`conversation_id` maps 1:1 to an ACP session inside a shared agent process.

See [docs/poe-acp-relay-design.md](docs/poe-acp-relay-design.md) for the full
design, scope, and milestones. For the underlying Poe wire protocol see
[docs/poe-protocol-reference.md](docs/poe-protocol-reference.md).

Module: `github.com/kfet/poe-acp-relay`. Standalone — not linked into
the main fir binary.

**Status:** M1 complete. End-to-end Poe `query` → ACP `session/prompt` →
SSE-streamed assistant response is verified against a real `fir --mode acp`
child. See the "Live test" section below.

## Quick start

```bash
# build
go build -o ./bin/poe-acp-relay ./cmd/poe-acp-relay

# run (requires fir on $PATH, or override via --agent-cmd)
export POEACP_ACCESS_KEY=mysecret        # match the key in your Poe bot dashboard
./bin/poe-acp-relay \
  --http-addr :8080 \
  --agent-cmd "fir --mode acp" \
  --permission allow-all

# smoke test (separate shell)
POEACP_ACCESS_KEY=mysecret ./test/smoke.sh
```

Point your Poe bot at `https://<host>/poe` (with any reverse proxy or
`tailscale funnel` fronting the plain HTTP port).

## Deployment

### Single bot behind Tailscale Funnel (recommended)

The host just needs `tailscale funnel` enabled. The relay listens on a
loopback port; Funnel exposes it publicly with a valid cert.

```bash
# 1. Enable funnel for the default path (if not already).
tailscale funnel --bg 127.0.0.1:8080

# 2. Set the Poe bot's Server URL (in the Poe dashboard) to:
#      https://<host>.<tailnet>.ts.net/poe
#    and copy the generated Access Key.

# 3. Start the relay with that key in env.
export POEACP_ACCESS_KEY=<key-from-poe-dashboard>
./bin/poe-acp-relay --http-addr 127.0.0.1:8080 --agent-cmd "fir --mode acp"
```

### Multiple bots on one host (path-based routing)

Several bots (e.g. the MCP bridge at `external/poe` and this relay) can
share a Tailscale node by mapping each to a distinct path. `tailscale
funnel --set-path=<prefix>` routes a URL prefix to a local port and
**strips the prefix before proxying**. Because of the strip, the Poe
bot's server URL must include a trailing path that the relay actually
registers (`/poe` by default).

Concrete example used in our deployment:

```bash
# Existing MCP bridge on :8080 under /
#   (already set up via `tailscale funnel 127.0.0.1:8080`)

# Add the ACP relay on :8081 under /poe-acp/*
tailscale funnel --bg --set-path=/poe-acp 127.0.0.1:8081

# Start the relay. --poe-path is optional; /poe is always served as a
# fallback so tests keep working regardless.
POEACP_ACCESS_KEY=<key> ./bin/poe-acp-relay \
  --http-addr 127.0.0.1:8081 \
  --poe-path /poe-acp \
  --agent-cmd "fir --mode acp"
```

**Poe dashboard URL** for this bot:

```
https://<host>.<tailnet>.ts.net/poe-acp/poe
```

That resolves, via Funnel, to the relay's `/poe` handler. (Without the
`/poe` suffix the URL would become `/` after the prefix strip, which
the relay doesn't serve and returns 404.)

Verify end-to-end:

```bash
curl -sS -H "Authorization: Bearer $POEACP_ACCESS_KEY" \
     -H 'Content-Type: application/json' \
     -d '{"type":"settings"}' \
     "https://<host>.<tailnet>.ts.net/poe-acp/poe"
# → {"allow_attachments":false,"introduction_message":"..."}

curl -sS -H "Authorization: Bearer $POEACP_ACCESS_KEY" \
     "https://<host>.<tailnet>.ts.net/poe-acp/debug/sessions"
# → {"count":0,"sessions":[]}
```

### Auto-restart on the host (not yet automated)

v1 runs under `nohup` / a tmux window and must be restarted by hand
on host reboot. A launchd / systemd unit is a straightforward M2
follow-up; a template will land alongside the first production deploy.

## Endpoints

| Path                   | Auth   | Purpose                                |
|------------------------|--------|----------------------------------------|
| `POST /poe`            | Bearer | Poe protocol: `query`/`settings`/etc.  |
| `GET  /healthz`        | none   | `ok sessions=N`                        |
| `GET  /debug/sessions` | Bearer | JSON dump of conv → session state      |

## Flags

```
--http-addr            HTTP listen address (default :8080)
--poe-path             HTTP path for the Poe protocol endpoint (default /poe)
--agent-cmd            ACP agent command + args (default "fir --mode acp")
--agent-dir            FIR_AGENT_DIR passed to the child (default inherit)
--state-dir            Per-conv state root (default $XDG_STATE_HOME/poe-acp-relay)
--permission           allow-all|read-only|deny-all (default allow-all)
--access-key-env       Env var holding the Poe bearer secret (default POEACP_ACCESS_KEY)
--introduction         Poe introduction message
--session-ttl          Idle TTL for a conv (default 2h)
--gc-interval          GC sweep interval (default 5m)
--heartbeat-interval   SSE heartbeat tick (default 10s, 0 to disable)
--version              Print version and exit
```

## Behaviour notes

- **Per-conv cwd.** Each `conversation_id` gets a dedicated working
  directory under `$STATE/convs/<conv_id>/`, passed to fir via the
  ACP `NewSessionRequest.Cwd`. Fir's session-history, `.fir/settings.json`,
  and (optional) `.fir/mcp.json` are naturally isolated per conv.
- **Heartbeat.** While an agent turn is in flight but before any real
  token has streamed, the relay emits a zero-width-space `text` event
  every `--heartbeat-interval`. This keeps Poe's SSE connection alive
  during slow first-token scenarios (fir cold start is ~50s with a
  full extension set). The heartbeat stops on the first real agent
  chunk.
- **Cancel propagation.** If the Poe HTTP client disconnects
  mid-turn, the relay issues ACP `session/cancel` so fir stops burning
  tokens. Fir's `StopReasonCancelled` is translated to an SSE
  `replace_response` + `done`.
- **Stop reasons.** `end_turn` → clean `done`; `max_tokens` /
  `max_turn_requests` → a "_(truncated)_" suffix + `done`; `refusal` →
  an SSE `error` event + `done`.
- **Commands.** Fir publishes its available slash commands via the
  ACP `available_commands_update` session update on every new session.
  The relay snapshots the latest list and returns the names in the
  Poe `settings.commands` response so they show up in the Poe UI
  autocomplete menu.
- **Permission.** `allow-all` (default), `read-only`, or `deny-all`
  via `--permission`. The relay answers `session/request_permission`
  locally; no prompt to the Poe user in v1.
- **Attachments / thoughts / plans / tool-call updates** are not
  forwarded to the Poe user in v1 — only `AgentMessageChunk` text
  reaches the SSE stream.

## Tests

```
go test ./...
```

- `internal/router` — streaming, session reuse, StopReason translation,
  idle GC with an injectable clock.
- `internal/httpsrv` — `query` round-trip (meta/text/done), `settings`
  JSON, bearer auth pass/fail.
- `test/smoke.sh` — black-box curl SSE smoke test for a running relay
  (no fir patching required).

## Live test log (2026-04-19)

Using `fir 0.30.0-dev` on the host:

1. `POST /healthz` → `ok sessions=0`
2. `POST /poe {type:"settings"}` → JSON with `introduction_message`
3. `POST /poe {type:"query", …}` →
   ```
   event: meta
   event: text (× N zero-width heartbeats, ~50s of fir boot)
   event: text (real assistant message)
   event: done
   ```
   curl exited 0.
4. `GET /debug/sessions` → the conv's session_id + cwd + last_used.

One gotcha captured along the way: an earlier 30-second probe deadline
was too tight for fir's cold start. Production uses the HTTP request
context and is fine.

## Layout

```
poe-acp-relay/
  cmd/poe-acp-relay/       entry point
  docs/                    design doc + Poe protocol reference
  internal/acpclient/      acp.Client impl + stdio agent proc wrapper
  internal/httpsrv/        /poe handler with heartbeat + cancel plumbing
  internal/poeproto/       minimal Poe HTTP+SSE
  internal/policy/         allow-all / read-only / deny-all
  internal/router/         conv_id → ACP session map + GC
  test/smoke.sh            black-box SSE smoke test
```
