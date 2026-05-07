# Graceful Restart — design (future)

Status: **deferred.** Not implemented. This document captures the design
so it can be picked up when in-flight stream survival across binary
upgrades becomes a real pain point. Until then, restart is whatever the
supervisor (systemd / launchd) does on `kickstart -k` / `restart`, and
in-flight Poe SSE responses are dropped on the floor.

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
   need it any more, child has its own dup), start a wait-for-active
   loop, `os.Exit(0)` once the active count hits zero or a hard
   timeout (90s default) fires.
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

#### Active-request tracking (parent)

`http.Server` doesn't expose a direct in-flight counter, but we can
wrap the handler:

```go
var inflight atomic.Int64
mux := http.NewServeMux()
// register handlers...
wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    inflight.Add(1)
    defer inflight.Add(-1)
    mux.ServeHTTP(w, r)
})
```

Drain loop:

```go
deadline := time.Now().Add(90 * time.Second)
for inflight.Load() > 0 && time.Now().Before(deadline) {
    time.Sleep(100 * time.Millisecond)
}
// Optional: connection-draining via http.Server.Shutdown(ctx) instead
// of the polling loop above. Shutdown closes the listener (already
// closed) and waits for active conns to finish.
```

Prefer `http.Server.Shutdown(ctx)` with a 90s timeout — it does
exactly the right thing, including respecting hijacked SSE
connections via `ConnState` callbacks if registered.

### SSE-specific gotchas

- SSE handlers in poe-acp run a long-lived `for` loop draining the
  router channel. They must observe `r.Context().Done()` and return
  promptly when the request is cancelled. Verify that
  `internal/httpsrv/handler.go`'s SSE writer respects ctx cancellation
  before relying on `Server.Shutdown(ctx)` — if it doesn't, drain will
  block until the agent finishes the prompt naturally (which is
  actually the desired behaviour for our case, but means the 90s cap
  matters).
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

A user-facing `/reexec` chat command (authbroker-style) is **not**
recommended — it'd let any Poe user trigger relay restarts.

### Failure modes & ops

| Scenario | Behaviour |
|---|---|
| New binary missing/corrupt | Preflight stat fails, parent logs, no fork, keeps serving |
| Child crashes before SIGUSR1 | Parent observes `cmd.Wait()` return, stays in serve mode, logs |
| Child crashes after SIGUSR1 | Parent already in drain mode; once drained it exits, supervisor restarts → temporary outage but matches today's behaviour |
| Drain timeout hits | Parent calls `Server.Close()` (force) and exits; remaining SSE clients see RST |
| Two upgrades in flight | Reject second SIGHUP if a child PID is already tracked |

### Files to touch (when implemented)

- `cmd/poe-acp/main.go` — listener acquisition (cold vs graceful path),
  signal handlers, child fork.
- New `internal/graceful/` package — ~150 LOC: `Listen()`, `Upgrade()`,
  `Drain()`, env-var contract, signal handling. Self-contained, easy
  to delete or replace with tableflip later.
- `internal/httpsrv/handler.go` — wrap `ServeHTTP` with the inflight
  counter (or move to `Server.Shutdown` for drain).
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
