# Graceful Restart — design

Status: **implemented** as a master/worker supervisor shim (v0.36.0;
supersedes the v0.34.0 two-process re-exec and the v0.35.0 systemd
MAINPID handshake). In-flight Poe SSE streams survive a binary upgrade,
and the model is now **structurally identical and safe on both systemd
and launchd**.

## The master/worker supervisor model (v0.36.0)

The same binary runs in one of two modes, selected at startup:

- **Supervisor S** is the process the init system launches and tracks
  (systemd `MainPID` / launchd job PID). It binds the listen socket
  **once** (`net.Listen`) and **never rebinds or exits during an
  upgrade**. It forks worker processes, handing each the listener fd
  (cleared of `O_CLOEXEC`, delivered as `POE_ACP_WORKER_FD=3`) plus the
  read end of a parent-liveness pipe (`POE_ACP_DEATH_FD=4`). S is tiny
  and rarely changes. Implementation: `internal/supervisor/`.
- **Worker W** is detected by `POE_ACP_WORKER_FD` being set. It recovers
  the listener via `net.FileListener` and runs ALL the relay logic
  (`httpsrv`/`router`/agent). This is where churn lives.

**Why this unifies launchd + systemd.** The v0.35.0 model was "the
server IS the supervised PID, so on upgrade it must exit." That is
unfixable-with-drain under launchd: launchd tracks the spawned PID and
`KeepAlive`-relaunches on its exit; the relaunched instance cold-binds
the port and hits `EADDRINUSE` because the surviving drainer still holds
the socket → crash-loop. With a stable supervisor S that never exits
during a worker swap, the init system never observes the tracked PID
vanish — so **`EADDRINUSE`-on-relaunch is structurally impossible on any
OS**, and the systemd-specific MAINPID re-point handshake is no longer
needed.

### Hot upgrade — the common path (worker swap)

Trigger: `SIGHUP` to S (systemd `ExecReload=/bin/kill -HUP $MAINPID`;
launchd `launchctl kill SIGHUP`), or `POST /admin/reexec` (the worker
relays this to S as a `SIGHUP`).

1. S forks a NEW worker W2 with the SAME fd (W2 = the new on-disk
   binary). W2 recovers the listener and starts accepting; the kernel
   accept queue covers any gap, so no connection is refused.
2. W2 signals S it is ready (`SIGUSR1`).
3. S tells W1 to stop accepting and **drain** its in-flight SSE to
   natural completion / caller-cancel (`SIGTERM`, which W maps to
   `http.Server.Shutdown`; the per-stream idle-write backstop in
   `internal/httpsrv` is the only force-kill).
4. W1 exits; S reaps it.

Full drain, zero refusal, identical on launchd and systemd. S stays on
its old in-memory code throughout — intended and fine, because S is tiny
and the relay logic lives entirely in the worker.

### Parent-death detection

Each worker holds the read end of a pipe whose write end S keeps open.
If S dies, the read end EOFs and the worker exits, freeing the socket so
the init system relaunches S cleanly (a brief cold blip) rather than
orphan-locking the port.

### Shim self-upgrade — the rare path

S's own code seldom changes. When it must, S re-execs ITSELF in place
via `syscall.Exec` (same PID, inheriting the listen fd via
`POE_ACP_SUPERVISOR_FD` so it does not rebind) — but **only when
quiescent**: the trigger (`SIGUSR2` to S, distinct from the worker-swap
`SIGHUP`; or `POST /admin/reexec?scope=supervisor`) first drains and
reaps the current worker, so nothing in-flight is dropped, then re-execs.

### Readiness (systemd)

`internal/sdnotify/` still sends `READY=1` (Type=notify ordering) — but
now from **S**, once its first worker is serving. The v0.35.0
`MAINPID=<pid>` re-point datagram and the `NotifyAccess=all` unit
requirement are **retired**: S is a stable `MainPID`, so systemd never
needs to adopt a successor.

**Unit requirements** (see deploy SKILL): `Type=notify`,
`ExecReload=/bin/kill -HUP $MAINPID`. Do **not** add `NotifyAccess=all`.
launchd: `KeepAlive=true` + `RunAtLoad` (S owns the socket; no `Sockets`
key needed).

### First-cutover caveat

The swap is driven by the **currently running** binary. Upgrading from
≤ 0.35.0 (server-is-PID model) to 0.36.0 (shim) cannot be a seamless
reload — the running old binary does not speak the shim protocol. So the
FIRST cutover to 0.36.0 is a plain restart (`systemctl --user restart` /
`launchctl kickstart -k`); only AFTER 0.36.0 is running do
`reload`/`SIGHUP` become seamless worker swaps. (Worse: under the
pre-0.36.0 launchd path, `SIGHUP` self-exit raced `KeepAlive` into the
`EADDRINUSE` crash-loop described above — which is why the old skill
claim that "launchd SIGHUP re-exec is always safe" was wrong and has
been removed.)

---

*The sections below are retained as the original rationale of record for
the drain semantics, which the worker still implements unchanged.*



## Problem

A `poe-acp` upgrade flow (binary swap → supervisor restart) currently
has two visible failure modes:

1. **Refused connections during the restart window.** New Poe `query`
   POSTs that arrive while the listening socket is between processes
   get `ECONNREFUSED`. Poe will surface this as a transient error.
2. **Aborted in-flight SSE streams.** Any Poe conversation mid-reply
   when the old process exits sees its TCP connection RST'd. The user
   sees a truncated reply; their next turn replays history so the model
   "continues" but it's a visible glitch.

Goal of this design: zero of both, without adding library dependencies.

**Cancellation semantics (confirmed).** Poe has no app-level cancel
event — `fastapi_poe`'s `BaseRequest` has five types, none of them
cancel. A user pressing **Stop** is simply Poe **closing the HTTP
request**, which `net/http` surfaces as `r.Context()` cancellation. So
drain needs no cancel protocol: a stream ends either by completing
naturally or by the caller's connection dropping. (Source: redrive
research, poe-acp v0.32.0.)

## Why not tableflip / endless / grace / overseer

Considered and rejected for v1:

- **`cloudflare/tableflip`** — 1.2k LOC dep, last code change 2022
  (release) / 2024 (cosmetic). Solid but quiet. Adds `golang.org/x/sys`
  as a transitive. Reasonable but goes against poe-acp's two-direct-deps
  ethos.
- **`facebookarchive/grace`** — archived by Meta.
- **`fvbock/endless`** — last meaningful activity ~2015.
- **`rcrowley/goagain`** — older than endless; SIGUSR2 trick only.
- **`jpillora/overseer`** — different model (supervisor process), more
  surface than we need.

The kernel primitives we rely on (`syscall.Exec`, `O_CLOEXEC`,
`net.FileListener`, `ExtraFiles` at fork) are stable across Go versions
and OS releases. Rolling ~150 lines is cheaper long-term than carrying a
dep we don't otherwise need.

## What socket activation alone gets us

systemd `.socket` units (Linux) and launchd `Sockets` plist keys (macOS)
solve **#1 only**. The kernel holds the listening socket; new SYNs queue
in the accept backlog while the service flips. Inherited via `LISTEN_FDS`
env (Linux) or `launch_activate_socket(3)` (macOS).

They do **not** solve #2: an in-flight `accept()`-ed TCP connection
belongs to the dying process and dies with it.

For the v1 "good enough" baseline, socket activation + drain-on-SIGTERM
is the pragmatic stopping point. This design covers what to do *beyond*
that, when in-flight stream survival becomes worth the effort.

## Design — DIY graceful restart

Pattern: **two overlapping processes**, parent hands the listener fd to
child, parent keeps serving its already-`accept()`-ed connections until
they finish, child takes new connections.

### Wire protocol between parent and child

Single env var, set by parent before forking child:

```
POE_ACP_GRACEFUL_FD=3
```

Listener fd is passed as `ExtraFiles[0]` (which becomes fd 3 in the
child, since 0/1/2 are stdio). The presence of `POE_ACP_GRACEFUL_FD`
also serves as the "you are a graceful restart child" signal.

A second env var carries the parent PID so the child can signal
readiness:

```
POE_ACP_GRACEFUL_PARENT_PID=12345
```

Child signals "ready, you can drain now" by sending `SIGUSR1` to the
parent PID. (Choosing SIGUSR1 over a pipe avoids a third fd to manage;
the kernel guarantees pid-targeted signals can't be misrouted across
exec because we capture the pid before fork.)

### Lifecycle

#### Cold start (no `POE_ACP_GRACEFUL_FD` env)

1. `net.Listen("tcp", addr)` as today.
2. Serve normally.
3. On `SIGHUP` (or admin command — see below), trigger upgrade.

#### Upgrade trigger (in parent)

1. Re-exec preflight: stat the binary, refuse if missing/non-executable.
   (Fail loud rather than spawn a broken child.)
2. Take the listener's underlying `*os.File` via
   `listener.(*net.TCPListener).File()`. **Important:** `File()` returns
   a *dup* of the fd with `O_CLOEXEC` cleared, which is what we want to
   pass across exec. The original `net.TCPListener` keeps working in
   the parent until we explicitly stop using it.
3. `cmd := exec.Command(os.Args[0], os.Args[1:]...)`
4. `cmd.ExtraFiles = []*os.File{listenerFile}`
5. `cmd.Env = append(os.Environ(), "POE_ACP_GRACEFUL_FD=3",
   fmt.Sprintf("POE_ACP_GRACEFUL_PARENT_PID=%d", os.Getpid()))`
6. `cmd.Stdin/out/err` = parent's. (No daemonisation — supervisor owns
   us.)
7. `cmd.Start()` and remember `cmd.Process.Pid`.
8. Install a SIGUSR1 handler (one-shot) that flips parent into "drain
   mode": stop calling `Accept()` (close the listener — parent doesn't
   need it any more, child has its own dup), then drain in-flight
   streams to their natural end (see *Drain semantics* below) and
   `os.Exit(0)` once the active count hits zero. No wall-clock cap on a
   live stream — the only backstop is a per-stream idle-write timeout
   for a wedged turn.
9. If `cmd.Process.Wait()` returns *before* SIGUSR1 — child died during
   startup. Parent stays running, logs the failure, and the upgrade is
   considered failed. Operator retries. (No automatic rollback needed
   because parent never stopped serving.)

#### Cold-start of child (`POE_ACP_GRACEFUL_FD=3` set)

1. `f := os.NewFile(3, "graceful-listener")`
2. `ln, err := net.FileListener(f)` — recovers a `net.Listener` from
   the inherited fd. (`f.Close()` after; `FileListener` dups it again
   internally.)
3. Wire ln into `http.Server` exactly as in the cold-start path.
4. After `http.Server.Serve(ln)` is *running* (not blocked-waiting),
   send SIGUSR1 to parent PID from env. The "running" check can be:
   - Spawn `Serve` in a goroutine, do a small self-`http.Get` against
     `/healthz` (or whatever the relay exposes) to confirm the listener
     is live, *then* SIGUSR1; or
   - Cheaper: SIGUSR1 immediately after `Serve` is launched in a
     goroutine — racy in theory, fine in practice because the kernel
     accept queue absorbs the gap.
5. Continue serving. The parent will exit on its own once drained.

#### Drain semantics (parent)

A draining stream ends exactly two legitimate ways, neither of which is
a wall clock:

1. **Natural completion** — the agent emits `agent_end`, the final SSE
   flushes, the handler returns.
2. **Caller cancel** — the user presses **Stop** in Poe. Poe has *no*
   app-level cancel event (`fastapi_poe` `BaseRequest` has 5 types,
   none cancel); a Stop is simply Poe **closing the HTTP request**.
   `net/http` cancels `r.Context()` on that disconnect, the SSE loop
   observes `Done()` and returns. (A dead client is also surfaced by
   the next heartbeat write failing — so the heartbeat goroutine is
   load-bearing for prompt cancel detection.)

So drain is just "wait until every accepted request has returned":

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()
// Optional idle-stream backstop only — NOT a global cap.
_ = srv.Shutdown(ctx)
```

Use `http.Server.Shutdown(context.Background())` (unbounded). It closes
the listener (already closed) and waits for active conns to finish,
respecting hijacked SSE connections. A turn legitimately streaming for
ten minutes survives; a Stopped turn ends the instant the connection
drops.

The **only** force-kill path is a *wedged* turn — fir hung, no tokens,
and the client never disconnected. Bound that with a per-stream
**idle-write timeout** ("no SSE byte written in N s"), enforced inside
the SSE loop, not with a restart-wide deadline. When it fires, that one
handler returns; everyone else keeps draining.

If you want an explicit in-flight gauge for logging/health, wrap the
handler:

```go
var inflight atomic.Int64
wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    inflight.Add(1)
    defer inflight.Add(-1)
    mux.ServeHTTP(w, r)
})
```

### SSE-specific gotchas

- SSE handlers in poe-acp run a long-lived `for` loop draining the
  router channel. They **must** observe `r.Context().Done()` and return
  promptly — this is what makes caller-cancel (Poe closing the request)
  work, and what lets `Server.Shutdown` complete. Confirm
  `internal/httpsrv/handler.go`'s SSE writer respects ctx cancellation;
  if it doesn't, a Stopped turn would keep streaming into a dead socket
  until a write fails.
- Heartbeat goroutines started by `newSink` must shut down when the
  request ends. They already do (`s.stop()` on context end).
- The agent (fir) child of poe-acp is **not** restarted by this flow.
  The child poe-acp inherits a *new* AgentProc (fresh fir spawn)
  because `Start()` runs in `cmd/poe-acp/main.go` startup. The parent's
  AgentProc keeps running until parent exits. This is fine — each Poe
  conv is bound to one process for its current turn; new turns after
  the restart go to the new fir. Document this in operator notes.

### Trigger surfaces

Two ways to initiate an upgrade:

1. **`SIGHUP`** — `kill -HUP $MAINPID`. Compatible with systemd's
   `ExecReload=/bin/kill -HUP $MAINPID`. Operator-friendly.
2. **Admin HTTP endpoint** — `POST /admin/reexec` gated by an
   `ADMIN_TOKEN` env var (same pattern as the existing bearer auth in
   `internal/poeproto/poeproto.go`). Useful for automated update
   flows that don't have shell access.

A user-facing `!reexec` chat command (command-broker-style) is **not**
recommended — it'd let any Poe user trigger relay restarts.

### Failure modes & ops

| Scenario | Behaviour |
|---|---|
| New binary missing/corrupt | Preflight stat fails, parent logs, no fork, keeps serving |
| Child crashes before SIGUSR1 | Parent observes `cmd.Wait()` return, stays in serve mode, logs |
| Child crashes after SIGUSR1 | Parent already in drain mode; once drained it exits, supervisor restarts → temporary outage but matches today's behaviour |
| Wedged turn (idle-write timeout) | That one handler's idle backstop fires, its stream gets RST; all other streams keep draining |
| Parent never drains (all turns wedged) | Supervisor's own stop timeout eventually `Server.Close()`s the parent; matches today's behaviour |
| Two upgrades in flight | Reject second SIGHUP if a child PID is already tracked |

### Files to touch (when implemented)

- `cmd/poe-acp/main.go` — listener acquisition (cold vs graceful path),
  signal handlers, child fork.
- New `internal/graceful/` package — ~150 LOC: `Listen()`, `Upgrade()`,
  `Drain()`, env-var contract, signal handling. Self-contained, easy
  to delete or replace with tableflip later.
- `internal/httpsrv/handler.go` — ensure the SSE loop honours
  `r.Context().Done()` (caller-cancel + `Shutdown`), and add a
  per-stream idle-write timeout as the only force-kill backstop.
- Supervisor docs (`internal/skills/bundle/{deploy,update}/SKILL.md`):
  add `ExecReload=/bin/kill -HUP $MAINPID` to the systemd unit and the
  equivalent launchd note (launchd doesn't have a built-in reload —
  operators send SIGHUP via `kill` or `launchctl kill SIGHUP …`).

## Out of scope

- **Mid-stream resume of a single SSE response across the binary swap.**
  Not feasible without protocol changes on the Poe side. The user-visible
  behaviour with this design is: in-flight reply continues to completion
  on the *old* binary; new turns hit the *new* binary. Acceptable.
- **fir-side reexec.** The shared `fir --mode acp` child of poe-acp
  serves all sessions; reexecing it would yank every conversation. Out
  of scope here; if needed, do it via poe-acp upgrading and re-spawning
  fir.
- **Windows.** poe-acp is unix-only by deployment target.

## When to do this

Defer until at least one of:

- Operator complaints about dropped Poe replies during updates become
  routine.
- Update cadence rises to where the "retry your last message" UX is
  visibly bad.
- A Poe protocol change makes mid-stream reconnection feasible (then
  this becomes the substrate for it).

Until then: socket activation (if even that's added) + drain-on-SIGTERM
is enough.
