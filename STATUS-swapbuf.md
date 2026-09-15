
## work/swapbuf — absorbed-answer survival across worker swap

- [m1] Read brief. Verified diagnosis in code: `Handler.answers` is a
  process-local `*answerBuffer` (`internal/httpsrv/handler.go:165,218`,
  `internal/httpsrv/buffer.go:160-248`). Absorb latch + `answers.put` at
  `handler.go:455-515`; redrive take at `handler.go:429`. No StateDir on
  `httpsrv.Config` (only `router.Config` has one). Diagnosis CONFIRMED as
  far as it goes.
- [m1a] BUT the brief's proposed fix (write-through on turn completion)
  would NOT have prevented this incident — see timeline analysis below.
  Investigating the stronger fix.
- [m2] Escalated design to advisor. Verdict: brief's completion-only
  write-through would NOT have fixed this incident (redrive landed 45s
  before the absorbed turn finished — FRAMESTATS 06:36:57 minus dur=1m31s
  = 06:35:26, i.e. that is the ORIGINAL turn on 469332 finishing, not the
  re-run). Same hole exists single-worker. Fix = disk store + PENDING
  marker published at the absorb latch + redrive waits on it.
  Control-pipe handoff ruled out: at swap time the answer does not exist.
- [m3] Implemented: internal/httpsrv/answerstore.go (+_must.go), buffer.go
  (store-backed put/take, markPending/abandon/waitPending), handler.go
  (Config.StateDir, mid-flight wait on redrive, pending marker at latch),
  cmd/poe-acp/main.go wiring. go test ./... clean.
- [m4] Tests added (see list below); `make all` green (vet, race+cover
  100%, 5 cross-builds, native build, licenses, scripts).
- [m5] SIGHUP coalescing added to the supervisor loop (hygiene only).
- [m6] Checked siblings: no `answerBuffer`/absorb/redrive machinery in
  slack-acp or acp-kit — this is Poe-specific, nothing to push shared.
- [m7] DONE. Branch `work/swapbuf`, 4 commits, NOT merged, NOT released.

### Correction to the brief's diagnosis

The brief's core claim is right — the absorbed-answer buffer is
worker-local process memory and does not survive a swap. But its proposed
fix (write the answer through to disk **at turn completion**) would NOT
have prevented this incident:

    06:36:57.447 FRAMESTATS conv=…rbagv… dur=1m31s
    06:36:57.447 − 1m31s = 06:35:26

i.e. that FRAMESTATS is the ORIGINAL absorbed turn on worker 469332
finishing — 45 seconds AFTER the user's retry had already landed on
worker 469510 and been forced to re-run. At redrive time no completed
answer existed in any worker, on disk or otherwise. The same hole exists
single-worker: a redrive arriving mid-absorbed-turn misses the buffer and
re-prompts.

So the fix publishes TWO states, not one: a **pending marker written at
the absorb latch** (not at completion), and the answer at completion. A
redrive that misses both memory and the answer file sees the live pending
marker and WAITS for the in-flight turn — wherever it is running — rather
than re-prompting.

Also ruled out (per advisor): handing the buffer to the successor over
the supervisor control pipe. At swap time the answer does not exist yet,
so there is nothing to hand over.

### What changed

- `internal/httpsrv/answerstore.go` (new) — on-disk, cross-generation
  store under `<state>/absorbed/`. `<sha256(key)>.a` answer,
  `<sha256(key)>.p` pending marker. Expiry is mtime+AnswerTTL uniformly
  (so the sweep is pure readdir, and tests drive expiry with
  `os.Chtimes`, never `time.Sleep`). Take-once across generations is a
  `rename(2)` claim — exactly one racing worker wins, the loser's ENOENT
  is an ordinary miss. Writes are tmp+rename; torn/foreign payloads are a
  miss, never fatal. Sweep at construction and on every put (the only
  growth point), enforcing `defaultAnswerBufferMax` — no background
  goroutine to leak.
- `internal/httpsrv/answerstore_must.go` (new) — `mustMarshalCalls`.
- `internal/httpsrv/buffer.go` — `answerBuffer` gains `store`;
  `put`/`take` write through / fall back; new `markPending` and
  `waitPending` (bounded by TTL, marker clearance, and the redrive's own
  ctx). Nil store = today's memory-only behaviour exactly.
- `internal/httpsrv/handler.go` — `Config.StateDir`; pending marker
  published at the absorb latch; redrive fast-path now falls through to
  `waitPending` before re-prompting.
- `cmd/poe-acp/main.go` — wires `StateDir`; coalesces redundant SIGHUPs.
- `CHANGELOG.md`.

### Tests

`internal/httpsrv/swapbuf_test.go`
- `TestSwap_RedriveWaitsForInFlightTurnOnRetiringGeneration`  ← the incident
- `TestSwap_RedriveServesCompletedAnswerFromOtherGeneration`
- `TestSwap_PendingMarkerLifecycle`
- `TestSwap_WaitAbortsOnRedriveClientDisconnect`
- `TestSwap_ExpiredPendingMarkerFallsThroughToRerun`
- `TestSwap_PendingClearedMidWaitReleasesWaiter`
- `TestSwap_ExpiredDiskAnswerNotServed`

`internal/httpsrv/answerstore_test.go`
- `TestAnswerStore_UnusableDirDisablesStore`
- `TestAnswerStore_RoundTripsEveryOp` (incl. take-once)
- `TestAnswerStore_TornFileIsIgnored`
- `TestAnswerStore_SweepDropsExpiredAndOrphans`
- `TestAnswerStore_EnforcesEntryBound`
- `TestAnswerStore_SweepSurvivesMissingDir`
- `TestAnswerStore_FailedRenameCleansTmp`
- `TestAnswerStore_PendingLiveFalseWhenAbsent`

`go test ./...` — all packages ok. `make all` — all targets ✓, coverage
gate 100%.

### Commits (branch `work/swapbuf`, not merged, not released)

- `db6d02e` fix(httpsrv): survive absorbed answers across a worker swap
- `0f665d8` test(httpsrv): cover the swap/mid-flight absorbed-answer paths
- `94513a0` feat(supervisor): coalesce redundant SIGHUP swap requests
- `60ed1b2` docs: changelog for absorbed-answer swap survival

## Follow-up (1): pending-marker TTL decoupled from AnswerTTL

- [m8] User is right: `.p` inherited AnswerTTL (2m), so a 10-minute turn's
  marker expired mid-flight and the redrive fell back to a re-run — the
  old bad behaviour on exactly the long turns where it hurts most.
- [m9] Escalated the design. Verdict + build:
  - `answerStore` now carries two TTLs. `ttl` = AnswerTTL bounds a
    completed answer; `pendingTTL` = IdleWriteTimeout bounds a marker.
    Not a coupling of convenience: IdleWriteTimeout is "maximum silence
    before the OWNER declares its turn dead", and the marker's expiry is
    "maximum silence before a WAITER declares the owner dead" — the same
    quantity from two vantage points. `sweep`/`live` are suffix-aware.
  - The owner REPUBLISHES its marker from `watchIdle` on every tick
    (IdleWriteTimeout/4 → four ticks of margin), gated on `absorbed`.
    Reaching the republish already proves not-wedged: the idle check
    above it cancels and returns first. So a wedged turn stops
    refreshing and its marker expires; so does a killed worker's. No new
    goroutine — the wedge backstop and the liveness heartbeat derive
    from the same clock, so they share one ticker.
  - Refresh is implemented as re-calling `markPending` (zero-byte
    tmp+rename resets mtime) rather than `os.Chtimes`: no second code
    path, and a marker swept or deleted underneath the owner is simply
    recreated.
  - Gating on `absorbed` matters: a marker left by a DEAD worker must
    not be kept alive by an unrelated live turn on the same key, which
    would never clear it.
- [m10] Fixed a second defect found while implementing: a redrive whose
  OWN client died mid-wait fell through and re-prompted on the decoupled
  turn ctx — a duplicate agent invocation, with real side effects, of a
  turn already running elsewhere. `waitPending` is now tri-state
  (`waitServed` / `waitAborted` / `waitMiss`). Deliberately narrow:
  `waitAborted` requires a LIVE marker, so "client already gone, nobody
  owns this" still runs and buffers — that is the original absorb
  feature and a blanket early-return would have regressed it.
- [m11] Tests added in `internal/httpsrv/pendingttl_test.go`:
  - `TestPending_ZeroConfigStillYieldsALiveMarker` (guards the silent-
    disable trap: a zero pendingTTL would make every marker instantly
    dead with nothing in the log)
  - `TestPending_RemarkRefreshesAndRecreates`
  - `TestPending_SweepIsSuffixAware` (both directions)
  - `TestPending_LongTurnKeepsMarkerLiveAndRedriveIsServed`  ← the fix
  - `TestPending_DeadOwnersMarkerExpiresAndRedriveReruns`    ← the bound
  - `TestPending_ConnectedTurnPublishesNoMarker`
  - `TestPending_WedgedAbsorbedTurnReleasesWaiterWithItsErrorCard`
  - `TestPending_AbortedWaitDoesNotDuplicateTheTurn`
  - `TestPending_DeadClientWithNoOwnerStillRunsAndBuffers` (narrowness
    guard for the tri-state)
  All expiry driven by `os.Chtimes` backdating and hook-signalled
  channels; no `time.Sleep`, no wall-clock polling.
- [m12] `make all` green; `go test ./...` clean; httpsrv re-run 4× under
  `-race -shuffle=on` with no flakes; coverage gate 100%.

## Ship: review-and-fix loop

- [m13] Cycle 1 — 3 issues, all fixed:
  - **urgent**: take-once violated across the memory/disk pair. `put`
    writes both; a redrive served from memory left the disk copy
    claimable, so a later redrive replayed the same answer twice.
    Failing-first test `TestAnswerStore_MemoryHitRetiresTheDiskCopy`.
  - **urgent**: the entry bound counted only `.a` files, so a flood of
    distinct absorbed keys inside one marker TTL grew the directory
    without limit through pending markers alone. Failing-first test
    `TestAnswerStore_BoundCountsPendingMarkersToo`.
  - docs: `<state>/absorbed/` layout + the two-bounds rationale written
    into `docs/poe-acp-design.md`; stale `abandon()` doc trimmed;
    concurrent-generation sweep interleaving documented.
- [m14] Cycle 2 — 2 issues, fixed: `writeAtomic` logged both failure
  branches at debug only, so an unwritable state dir degraded every put
  and every refresh silently — now WARNs once per process naming the
  consequence (`TestAnswerStore_FirstWriteFailureWarnsOnce`); stale
  `sufAnswer` doc claiming answers were the only bounded suffix.
- [m15] Cycle 3 — 3 test-hygiene issues, fixed: long-turn test could hang
  instead of failing on a slow box; a no-op zero backdate; `newGen`
  duplicating `newGenOpts`.
- [m16] Cycle 4 — 1 issue: `TestPending_DeadClientWithNoOwnerStillRuns`
  pre-released its agent, racing the router round-trip against the
  disconnect watcher (a router win flips the latch to CancelTurn). Gated
  on `absorbDecidedHook` like every other absorb test.
- [m17] Cycle 5 — 1 issue: the long-turn test consumed one buffered
  `idleTickHook` signal and assumed that tick's refresh postdated the
  backdate. Waits on the observable state instead, bounded by the idle
  backstop.
- [m18] Chased a one-off 600s test hang seen during cycle 5 rather than
  writing it off as a flake. It was a REAL deadlock: the long-turn test
  left generation A blocked while B's redrive waited, putting the test
  tail inside A's 2s idle window. A backstop cut there means A never
  publishes (`absorbAgent` ignores ctx), the marker expires, B falls
  through to a re-run, and agentB — release channel never closed —
  blocks forever. Fixed by releasing A before issuing B's redrive, and by
  pre-closing every generation-B agent's release channel so a regression
  fails by name instead of deadlocking. Verified 4× `-count=5 -race
  -shuffle=on` full-package runs clean.
- [m19] Cycle 6 — ZERO new issues. Loop exits.

## Ship

- [m20] Squashed 13 commits to 1, rebased onto local main (which had moved
  to v0.62.0 — CHANGELOG conflict resolved by collapsing the
  intermediate "fixes to my own unreleased work" entries into one
  coherent Unreleased section). Verified `git diff main..HEAD` removes no
  main content. `make all` green on the branch and again on main after
  ff-merge.
- [m21] Released **v0.62.1** (patch: Unreleased held only Fixed/Changed).
  `make publish` pushed main + tag; `ci` and `release` workflows both
  completed successfully; GitHub release has 8 assets; the Homebrew tap
  formula `kfet/homebrew-ai` now pins 0.62.1.
- [m22] Worktree ~/src/poe-acp-wt-swapbuf and branch work/swapbuf removed.
  NOT deployed anywhere — kopione rollout is the user's to do.

### Merge commit

- `7c20551` fix(httpsrv): make absorbed answers survive a worker swap
- `5d9277e` release: v0.62.1  (tag `v0.62.1`)
