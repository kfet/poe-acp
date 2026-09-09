# Stage 2 assertion-coverage audit

Every behavioural assertion in the gate tests BEFORE the TurnLiveness
adoption, and where it lives AFTER. Nothing may get weaker.

## internal/httpsrv/idle_test.go

| # | Behaviour asserted (before) | After |
|---|---|---|
| 1 | A hung agent (no text AND no tool activity) is cut within `IdleWriteTimeout`, while its heartbeat spinner keeps ticking | `TestHandler_IdleWriteTimeout_CutsWedgedTurn` — unchanged |
| 2 | The cut actually stops the agent (`Prompt` ctx cancelled, prompt returns) | same test — unchanged |
| 3 | With `TurnTimeout==0`, a turn emitting periodic text survives far past `IdleWriteTimeout` | `TestHandler_NoTurnCeiling_ProgressKeepsTurnAlive` — unchanged |
| 4 | Poll cadence is `timeout/4`, floored at 10ms | `TestIdleCheckInterval` — unchanged (function now drives the pending-marker refresh only) |
| 5 | `sink.Text` resets the wedge clock (`TestSink_IdleSince`) | `TestSink_WriteFeedsTheLivenessWatcher` — a real `client.TurnLiveness` is attached and repeated `Text` keeps the turn uncut. Also pins that the nil-watcher (no turn attached) case is safe. **Weaker in FORM:** `lastProgress` is kit-private, so the reset is observed through its effect over eight writes rather than per call — a reset that fired only every other write would pass here. The per-call reset is pinned in acp-kit by `TestTurnLiveness_ProgressResetsTheClock`. |

## internal/httpsrv/midturn_test.go

| # | Behaviour asserted (before) | After |
|---|---|---|
| 6 | Spinner re-arms multiple times mid-turn; exact SSE event sequence; accumulator never dropped | `TestSink_MidTurnSpinnerToggleSSE` — unchanged |
| 7 | A keepalive spinner frame does NOT reset the wedge clock | `TestSink_SpinnerFrameDoesNotKeepAWedgedTurnAlive` — spinner frames are emitted continuously for >3× the no-progress window and the turn is still cut with `client.ErrNoProgress` |
| 8 | A keepalive spinner frame does NOT reset the content-stall clock | same test — `lastContent` unchanged (mechanism untouched by Stage 2) |
| 9 | `ToolActivity` resets the wedge clock | `TestSink_ToolActivityIsLivenessNotContent` — repeated `ToolActivity` over 2× the no-progress window leaves the turn uncut. Weaker in form for the same reason as row 5, covered per-call by the kit's own tests. |
| 10 | `ToolActivity` does NOT reset the content-stall clock | same test — `lastContent` unchanged |
| 11 | `ToolActivity` records the spinner label | same test — unchanged assertion |
| 12 | `ToolActivity` never marks `realWritten` | same test — unchanged assertion |
| 13 | A real content write clears the transient tool label | same test — unchanged assertion |
| 14 | A tool-only turn (no text) far longer than `IdleWriteTimeout` is NOT cut, end to end | `TestHandler_ToolActivityKeepsWedgeAlive` — unchanged |
| 15 | Non-stalled tick emits no frame; a sealed stream stops the heartbeat | `TestSink_EmitSpinnerFrameNotStalled` — unchanged |

## internal/httpsrv/pendingttl_test.go

| # | Behaviour asserted (before) | After |
|---|---|---|
| 16 | `pendingTTL` defaults to the defaulted `IdleWriteTimeout` | unchanged |
| 17 | Marker and answer expire on their own independent TTLs | unchanged |
| 18 | A turn outliving `pendingTTL` keeps its marker live (refresh from the tick loop) and its redrive is SERVED, not re-run | `TestPending_LongTurnKeepsMarkerLiveAndRedriveIsServed` — unchanged; `idleTickHook` / `idleWriteCancelHook` seams both survive on `watchTurn` |
| 19 | A dead owner stops refreshing, its marker expires, the redrive re-runs | `TestPending_DeadOwnersMarkerExpiresAndRedriveReruns` — unchanged |
| 20 | A normally-connected turn publishes no marker | unchanged |
| 21 | A WEDGED turn stops refreshing its marker | now structural and stronger: `watchTurn` returns on `turnCtx.Done()` instead of on the next poll tick, so refreshing stops at the cut rather than up to a quarter-window later. Pinned by `TestWatchTurn_CutStopsTheMarkerRefresh` |

## internal/httpsrv/plan_test.go

| # | Behaviour asserted (before) | After |
|---|---|---|
| 22 | `SetPlan` touches neither clock and never marks `realWritten` | `TestSink_PlanIsNotUserVisibleOutput` — `lastContent` unchanged, `realWritten` false, and a plan-only turn is still cut as wedged |

## One assertion that has no successor, deliberately

`TestSink_IdleSince` also implicitly asserted that the wedge clock starts
fresh at SINK construction (`newSinkOpts` stamped `lastWrite`). That is
no longer true and should not be: the window now starts at
`StartTurnLiveness`, which the handler calls after the preamble and
after the redrive's wait for an in-flight turn have both had their
chance to return early. Preamble and redrive-wait time therefore no
longer count against the wedge window — a strictly more lenient start
point, and one the kit pins itself (`lastProgress: time.Now()` inside
`StartTurnLiveness`). It is called out in the CHANGELOG.

## Stage 1 tests that must still pass

- `TestHandler_HiddenThinkingIsNotAWedge` (turncause_test.go) — unchanged.
- `internal/router/turnendnote_test.go` — unchanged; the cause types still
  wrap the kit sentinels and still carry the numbers.
- `internal/httpsrv/cancelproof_test.go` — unchanged.

## New assertions Stage 2 adds

- `TestHandler_TurnCeilingCutsAndSaysSo` — the opt-in `TurnTimeout`
  ceiling actually fires, cuts the turn, and the user is told, with the
  ceiling's duration in the sentence. Previously only the *construction*
  of the ceiling branch was exercised (`TestHandler_TurnTimeoutOptIn`
  used a one-hour ceiling that never fired).
- acp-kit `TestTurnLiveness_MisWrappedCauseIsIgnored` — a custom cause
  that does not wrap the sentinel cannot break cross-relay classification.

## Did the liveness code get smaller?

Counting only the wedge/ceiling MECHANISM — not the pending-marker tick
loop, which survives either way and is not liveness — in non-blank,
non-comment lines of `internal/httpsrv/handler.go`:

| | before | after |
|---|---|---|
| turn ctx + ceiling plumbing | 9 | 8 (`StartTurnLiveness` + attach) |
| the wedge clock (`lastWrite` field/init, `touchTool`, `idleSince`, `touch`'s wedge half) | 11 | 7 (`live` field, `attachLiveness`, `progress`) |
| the cut itself (poll+compare+cancel / settled-report) | 9 | 11 |
| **total** | **29** | **26** |

So: smaller, but only by three lines — a **marginal** pass of the "must
be meaningfully smaller" test at the relay level, and it would be
dishonest to call that a win on its own.

The real reduction is one level up. The ~90 lines of timer, re-arm,
race-resolution and cause-dispatch logic that this relay used to own a
variant of now exist ONCE, in `acp-kit/client/liveness.go`, shared by
poe-acp, slack-acp and zulip-acp. What is left here is glue: hand the
watcher the two operator numbers, feed it the progress signal the router
already classifies, report the cut. That is the shape the adoption was
for, and it is what makes the next liveness bug a one-repo fix.

## Why `Wrap` was not used

`TurnLiveness.Wrap` decorates a per-TURN `client.SessionUpdateSink`.
poe-acp's session sink is `*router.sessionState` — session-lifetime,
installed once at `NewSessionWithMeta`, never reassigned. It enqueues
every `session/update` onto one channel drained by a single goroutine,
with turn boundaries carried IN-BAND as `beginTurn`/`endTurn` control
messages on that same channel. That is precisely what makes
chunk-to-turn attribution race-free here.

Installing `Wrap` would mean making the session sink swappable per turn,
reintroducing the race the drain queue exists to eliminate (an update
landing on the old wrapper after the swap, or on the new one before
`beginTurn`) — in exchange for a classifier the router ALREADY applies
on the raw update. So the seam is not `Wrap`; it is `ChunkSink.Progress`,
which Stage 1 already put in the right place and which is already
handler-owned. There was no ownership move to make.

What was missing was the kit's watcher being reachable without the
decorator. acp-kit v0.17.0 exports `TurnLiveness.Progress` (`Wrap` calls
it, so there is still one implementation) and lets the caller supply the
causes, so the relay keeps its own user-facing sentence and its numbers.
