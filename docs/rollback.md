# Rollback substrate

Replacing the poe-acp binary is only safe if it can be *un*-replaced
without a human on the other end of an ssh session. This is the
mechanism that makes a swap reversible. It is step 1 of moving the fleet
from push-based `scripts/converge.sh` to pull-based self-reconciliation;
nothing here fetches anything.

Code: [`internal/install`](../internal/install) (layout on disk),
[`internal/supervisor/heal.go`](../internal/supervisor/heal.go) (the
state machine), `poe-acp dist` ([cmd/poe-acp/dist.go](../cmd/poe-acp/dist.go)).

## Layout

Root defaults to `$POEACP_INSTALL_ROOT`, else `$XDG_STATE_HOME/poe-acp/dist`,
else `~/.local/state/poe-acp/dist` — the same path on Linux and macOS, so
systemd-user and launchd hosts look identical.

```
<root>/versions/poe-acp-<version>   real binaries, one per version
<root>/current      -> versions/poe-acp-<version>   ExecStart target
<root>/last-good    -> versions/poe-acp-<version>   last version a worker confirmed healthy
<root>/pinned.json  versions that crash-looped; never activated again
<root>/crashes.json recent worker-crash timestamps (per bot: crashes-<bot>.json)
<root>/rollback.log append-only, greppable record of every decision
```

Both links are **relative**, so the tree can be moved or staged
elsewhere and renamed into place.

**The supervisor's `ExecStart` must target `<root>/current`, never a real
file.** Swapping the binary is then one atomic symlink repoint (a temp
symlink `rename(2)`d over the old one): the next worker the running
supervisor forks, and the next start by the init system, both pick up the
new version with no unit-file edit.

```ini
# systemd --user
ExecStart=%h/.local/state/poe-acp/dist/current -config %h/.config/poe-acp/<bot>.json
```

```xml
<!-- launchd -->
<key>ProgramArguments</key>
<array><string>/Users/kfet/.local/state/poe-acp/dist/current</string>…</array>
```

## Legacy layout (no flag day)

All four fleet hosts currently run a plain file at `~/.local/bin/poe-acp`.
There is no `current` symlink there, so every layout read reports
`install.ErrUnmanaged`, the supervisor logs

```
supervisor: crash-loop rollback UNAVAILABLE (install: not a versioned layout: …); binary swaps are one-way on this host
```

once at startup, and nothing else changes. Rollback is a capability, not
a requirement. Adopting the layout on a host is:

```sh
poe-acp dist -version 0.31.0 install ./poe-acp-linux-arm64
poe-acp dist activate 0.31.0
# then point ExecStart at <root>/current and restart the unit ONCE
```

## Supervisor state machine

The supervisor is never the thing being replaced — systemd/launchd
babysit *it*, and it forks workers from `current` — so the whole machine
lives there and behaves identically on both init systems.

| event | source | effect |
| --- | --- | --- |
| **Confirm** | the worker's existing startup handshake: `NotifyReady` → `SIGUSR1` → `WaitReady` → `ReadyOK` | `last-good` ← `current`, crash record cleared |
| **Crashed** | a worker died — before signalling ready, or while serving | crash appended to the durable record |
| **Crash loop** | 3 crashes inside 60s (`DefaultCrashLimit` / `DefaultCrashWindow`) | pin `current`, repoint `current` → `last-good`, clear the record, re-enter the worker-swap path |

Health is a *positive* signal from the new binary itself, not an absence
of bad news: only a worker that completed the handshake advances
`last-good`.

Outcomes (`supervisor.HealAction`): `confirmed`, `none`, `armed`,
`reverted`, `revert-failed`, `unavailable`. Nothing here is ever fatal —
a store that cannot be read is reported and the supervisor keeps serving.

### After a revert

The revert repoints the symlink; the *worker swap* is what makes the
reverted binary the one running. Which path does that depends on where
the crash was observed:

- **A crash during a SIGHUP swap** (the old worker is still serving):
  the supervisor `SIGHUP`s **itself** (`supervisor.SignalSelf`) and
  re-enters the same swap branch — fork from the repointed `current`,
  retire the old worker. The update path *is* the reload path.
- **A crash of the serving worker, or of the initial worker**: there is
  no live worker to retire, so the supervisor respawns directly. That is
  the same fork-from-`current` code (`spawnReady`), minus a SIGTERM
  aimed at a process that is already gone.

### Pinning

A version that crash-looped is written to `pinned.json` with its reason
and time, **before** `current` is repointed — a supervisor killed between
the two steps must not come back and re-activate the version it just
condemned. Whatever chooses the desired version skips pinned ones: a pin
is cleared by shipping a *newer* build (step 2's reconcile, or
`poe-acp dist unpin`), never by retrying the same broken one.

### Why crash counts are durable

The decisive crash loop is the one that takes the supervisor down with
it: an in-memory counter resets on every init-system restart and would
never reach the threshold. `crashes.json` is scoped per bot
(`crashes-<bot>.json`) so several units sharing one install root do not
pool each other's crashes — they *do* share `last-good` and pins, which
describe the binary itself.

## Durable record

Every decision appends one line to `<root>/rollback.log`:

```
2026-08-15T09:14:22Z poe-acp-rollback crash version=0.32.0 count=3/3 window=1m0s
2026-08-15T09:14:22Z poe-acp-rollback revert version=0.32.0 -> 0.31.0 pinned=0.32.0 reason="crash-loop: 3 worker crashes within 1m0s"
```

`grep rollback ~/.local/state/poe-acp/dist/rollback.log` answers "what
happened to this host" without journald or a launchd stdout file that
may have already been rotated away.

## CLI

```
poe-acp dist status                 # current, last-good, pins, crash count
poe-acp dist -version V install F   # stage F under versions/ (does not activate)
poe-acp dist activate V             # repoint `current` (refuses a pinned V; -force overrides)
poe-acp dist unpin V                # clear a crash-loop pin
```

## Deliberately not here (steps 2-5)

Fetching a desired version, the `dist.lock` schema, the reconcile loop,
timer/boot reconciliation, and status push. This step only makes the
swap *safe*.
