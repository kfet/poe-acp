# Report: battery-aware SSE flush coalescing in poe-acp

Branch `work/coalesce-flush` in `~/wt/coalesce`, 3 commits on top of
`v0.49.0`. **Not pushed, not merged.** `make all` is green (vet, race +
shuffle + **100% coverage gate**, 5 cross-builds, native build,
license check).

Every pre-existing test passes **unmodified** — including all five
cadence tests the brief pinned (`firstframe_test.go`, `idle_test.go`,
`midturn_test.go`, `plan_test.go`, `finalize_test.go`). Nothing was
relaxed, nothing was deleted.

---

## The number

Synthetic long answer: **2000 chunks over a simulated 60s**, arriving in
100 bursts of 20 (a real agent streams in bursts, not on a metronome).
The timeline is compressed 100× (60s → 600ms) so it runs in the unit
suite; *every* interval — chunk spacing, heartbeat, coalescing period —
is scaled by the same factor, so the frame ratio is exactly what the
uncompressed run produces. Test:
`internal/httpsrv/coalesce_test.go:TestCoalesce_SyntheticLongAnswer`.

| | text | replace | other | **total frames** | bytes_out |
|---|---|---|---|---|---|
| **before** (`coalesce_ms: 0`, animated spinner — today) | 2000 | 2 | 1 | **2003** | 16013 |
| **after** (`coalesce_ms: 3000`, grid, static spinner) | 51 | 2 | 1 | **54** | 16015 |

**37.1× fewer wire frames**, with a byte-identical rendered body (the
test asserts that, it is not an eyeball check).

Frames-per-turn is the direct proxy for modem wakeups, so 37× fewer
frames is 37× fewer radio wakes — and, more importantly, the gaps
between them go from ~30ms (cDRX can never engage) to ~3s (it always
can).

Where the 51 text frames come from: **30** are the deliberate eager
first-flush window (see §3), **~20** are grid ticks (600ms / 30ms), and
**1** is the buffer drain on `done`. On a real 60s answer that is
30 eager + ~20 grid = ~50 frames, matching the brief's "roughly 20"
expectation for the steady-state portion.

`bytes_out` went *up* by 2 bytes. That is the point: bytes were never
the problem, wakeups were. Coalescing moves the same payload in fewer,
larger frames.

---

## What changed

### Config (`internal/config/config.go`)

Three knobs in the `defaults{}` block, alongside `show_plans` /
`show_tools`, all defaulting to today's behaviour byte-for-byte:

- `coalesce_ms` (int, **default 0 = off**) — `config.go:98`
- `coalesce_grid` (bool, **default true** when coalescing is on) — `config.go:107`
- `spinner_animate` (bool, **default true** = today) — `config.go:114`

`Defaults.Stream()` (`config.go:150`) resolves the nil-means-default
cases in one tested place; `Validate` rejects a negative `coalesce_ms`
at boot (`config.go:205`) rather than silently disabling the feature.

**No CLI flags.** The existing pattern for `defaults{}` knobs is
config-only — `show_plans`, `show_tools`, `hide_thinking`,
`hide_thinking` have no flags; only ops concerns (addresses, timeouts,
state dir) do. README says this explicitly: "CLI flags only cover ops
concerns … anything that's 'what kind of bot is this' goes in the
config file." These are bot-shape knobs, so they follow that rule.

**No Poe Options toggle** either, deliberately. It was tempting for
A/B — `router.ParseOptions` runs *before* `newSink`, so per-request
plumbing is mechanically possible. Against it: adding a
`parameter_controls` entry changes the settings schema hash, which
forces a Poe settings refetch for *every* bot and re-pins
`paramctl`'s golden tests, and the user already runs 3–4 separate bots
with separate configs — which is a cleaner A/B (whole-bot, stable
across a conversation) than a per-turn dropdown that would let a
conversation flip modes mid-stream. Recommend revisiting only if
per-conversation control turns out to be wanted.

### Coalescing (`internal/httpsrv/coalesce.go`, `handler.go`)

The buffer lives in `orderedWriter`, not in `sink.Text`. `sink.Text`
does `touch()` (liveness clocks) + header prepend and then delegates;
`orderedWriter` is where the mutex, the accumulator `acc`, the spinner
strip and the wire live, so it is the only place a buffered chunk can
be ordered against a competing writer. Buffering at `sink.Text` would
have left the heartbeat goroutine able to compose a keepalive from a
stale `acc`.

- `orderedWriter.userText` (`handler.go:751`) — buffers when
  `coalesce > 0`, else the original immediate path, unchanged.
- `orderedWriter.flushLocked` (`handler.go:694`) — drains the buffer as
  one `text` frame through the *same* code path a direct write takes
  (spinner strip → `acc` append → `realWritten`), which is why the
  bodies are identical.
- `orderedWriter.shouldFlushLocked` (`handler.go:730`) — eager window
  and paragraph rule.
- `nextFlushDelay` (`coalesce.go:68`) — the wall-clock grid.
- `sink.coalesceLoop` (`handler.go:1148`) — the only *periodic*
  flusher; everything else flushes synchronously because it is about to
  write an event that must not overtake buffered text.

Flush triggers, per the brief: grid tick; paragraph boundary (`\n\n`)
in a buffer over 200 bytes; the eager first-flush window; any
ordering-forcing event.

`realWritten` still flips only on an **actual wire write**. This
matters: it is the discriminator the handler's absorb/cancel path uses
to tell a user Stop from a pre-output transport drop, and the question
it asks is "did the client see anything?" — buffered text has not been
seen. So a drop during the buffer window is still correctly absorbed
and redriven.

### Grid alignment (§2)

`nextFlushDelay(now, period, grid=true)` returns the delay to the next
absolute instant where `UnixMilli % period == 0`. Landing exactly on a
grid instant yields a full period, never a zero-length timer.
`TestNextFlushDelay` pins that three arrival times spread across one
period all resolve to the *same* absolute deadline — which is the
property that makes four bots on four hosts share one wake window with
no coordination protocol.

### Ordering safety (§4)

`userReplace`, `userFile`, `userError` and `hbFrame` all call
`flushLocked()` immediately after taking `writeMu`, before touching
anything else.

`userDone` is the special case and was the one real trap: it takes `mu`
*first* and sets `closed = true` before acquiring `writeMu`, so a
`flushLocked()` there would find `closed` already set and **silently
drop the tail of the answer**. It therefore drains `pending` inside the
same `mu` section that seals the stream (`handler.go:842`) and writes
the tail before `done`. `TestCoalesce_OrderingSafety` is table-driven
over all five forcing events and asserts the buffered text is
`events[1]` and the forcing event `events[2]`.

### Static spinner + keepalive dedupe (§5)

- `sink.dots` (`handler.go:1256`) returns a constant `"..."` when
  `spinner_animate` is false. I did **not** put this inside
  `statusline.Spinner` as the brief suggested: `Spinner` is a pure
  renderer that already takes `dots` as an argument, and threading a
  mode flag into it would give it hidden state for no gain. Same
  observable result, less coupling.
- `orderedWriter.hbFrame` (`handler.go:914`) skips a `replace_response`
  whose body is byte-identical to the last one emitted.

Two guards on the dedupe, both non-obvious and both tested:

1. Suppression requires `spinnerVisible`. If a user write stripped the
   spinner in between, the identical frame **must** be re-sent or the
   status line silently vanishes for the rest of the turn.
   (`TestHeartbeatDedupe_NotSuppressedAfterStrip`.)
2. The liveness floor, below.

### Liveness-floor decision (§5) — what I chose and why

**Floor = `2 × StallThreshold`, never below 10s** (`hbDedupeFloor`,
`handler.go:1099`). With the shipped default `StallThreshold = 8s` that
is **16s**.

Reasoning, and I want to be explicit that this is a judgement under
uncertainty:

- The heartbeat is not decoration; it is what stops Poe
  content-starvation-dropping a long tool-heavy turn. Full suppression
  is therefore genuinely unsafe, so a floor is mandatory — I did not
  treat "may default on" as licence to suppress indefinitely.
- Poe's *mid-turn* tolerance is **not documented and this codebase has
  never measured it**. The only hard datum in the repo is the
  cold-start one: a new-conversation connection is dropped within
  milliseconds if it sees no content event (that is why tick #0 exists).
- What we do have is the operator's own declared safety margin:
  `StallThreshold`, documented in-tree as "conservatively under Poe's
  drop tolerance". Deriving the floor from that number rather than
  inventing an absolute means the floor is anchored to a belief someone
  already committed to, and an operator who lengthens `StallThreshold`
  because their bot demonstrably survives longer gaps automatically
  gets a longer floor.
- I chose this over the brief's suggested flat `N ~30s` because 30s is
  ~4× a window the codebase already calls "conservative", with no
  evidence behind the 4×. 16s still buys ~10× fewer keepalive wakes
  (1.5s → 16s cadence during a stall), which captures nearly all of the
  available win at materially less risk.
- If the floor turns out to be over-cautious, raising it is a one-line
  change to a single derived constant with a test on it.

### Instrumentation (§6)

`logFrameStats` (`coalesce.go:98`), emitted once per turn from a defer
registered *before* the sealing defers so it runs after them (LIFO) and
therefore counts the terminal `done`:

```
FRAMESTATS conv=%s text_frames=%d replace_frames=%d bytes_out=%d dur=%s
```

Always on, no debug flag — the before/after ratio is the deliverable.
`bytes_out` counts payload text bytes (`text` / `replace_response`
body), not SSE framing overhead; `other_frames` (file/error/done) is
tracked internally and asserted in tests but kept out of the log line
to match the brief's format exactly.

---

## Where the brief's analysis is wrong

The brief is a hypothesis from reading the code, and one part of it does
not survive contact with the running system. Stating it plainly:

**The 1.5s heartbeat does NOT re-transmit the whole answer for the whole
turn.** The brief says the heartbeat "emits a `replace_response`
carrying the whole accumulated answer plus a spinner whose dots animate
every tick … on a 4KB answer it re-transmits ~2.5KB/s for the whole
turn." `sink.emitSpinnerFrame` only emits when
`planChanged || !hasOutput() || contentIdleSince() >= stall`. On a
normally-streaming turn *none* of those hold and the tick is a
deliberate no-op — the code even documents keeping it allocation-free
for that reason. The measurement confirms it: the un-coalesced 2000-chunk
run produced **2 replace frames, not ~40**.

What is true is the narrower claim: during a **stall** (a long tool
call, no tokens for minutes) the animated spinner does re-send `acc`
every 1.5s and the animation does defeat any downstream dedupe. So the
"accidental anti-coalescing device" reading is correct in the regime it
actually applies to — tool-heavy turns — and that is precisely the
regime the static spinner + dedupe fix targets. The fix is right; the
magnitude claimed for it was overstated, and the real bulk win is the
2000 `text` frames, not the heartbeat.

Two smaller notes:

- The brief's §1 offers a choice between `sink.Text` and
  `orderedWriter.userText` as the ordering boundary. Only
  `orderedWriter` works — see "Coalescing" above.
- The brief's §4 lists the ordering-forcing events but not `userDone`'s
  inverted lock order, which is where the one genuine data-loss bug
  would have been.

## Tests modified

**None.** No existing assertion was touched, relaxed, or deleted. This
was achievable because `newSink(w, hb, stall, tickHook)` kept its exact
signature (it now delegates to `newSinkOpts` with zero-value shaping),
so all 22 existing call sites compile and behave identically.

New tests, all in `internal/httpsrv/coalesce_test.go` unless noted:

| test | covers |
|---|---|
| `TestNextFlushDelay` | grid alignment, on-grid instant, grid-off, sub-ms period |
| `TestCoalesce_FinalTextIdentical` | on/off/grid/no-grid equivalence of final text over 300 chunks |
| `TestCoalesce_EagerFirstFlush` | first chunk on the wire immediately; budget exhaustion; buffered tail survives `done` |
| `TestCoalesce_EagerWindowExpiresOnTime` | the *time* half of "30 chunks or 1.5s, whichever first" |
| `TestCoalesce_ParagraphFlush` | paragraph rule + size floor + boundary-free buffer keeps waiting |
| `TestCoalesce_OrderingSafety` | §4, table-driven over replace / file / error / done / heartbeat |
| `TestCoalesce_GridFlush` | the real flusher goroutine drains with no forcing event |
| `TestCoalesce_ClosedStreamDropsBuffer` | sealed-stream guards |
| `TestCoalesce_ConcurrentWritesAreOrdered` | 500 concurrent writes vs the flusher under `-race` |
| `TestHeartbeatDedupe` | suppression, state-change break, liveness-floor re-emit |
| `TestHeartbeatDedupe_NotSuppressedAfterStrip` | re-arm after a strip |
| `TestSpinnerDots`, `TestHBDedupeFloor`, `TestFrameStats` | static/animated dots, floor derivation, accounting |
| `TestCoalesce_SyntheticLongAnswer` | the measurement above |
| `config`: `TestDefaults_Stream`, `TestLoad_StreamKnobs`, `TestValidate_NegativeCoalesceMs` | knob resolution + validation |

No `time.Sleep` in any assertion path; the flusher is observed through a
`flushHook` channel seam, and the eager window is escaped by backdating
`turnStart` rather than waiting 1.5s.

---

## Recommended defaults for a first rollout

Ship the code with **all three knobs at their current defaults** — i.e.
disabled — so the release is a no-op for every existing bot. Then, per
bot:

```json
"defaults": {
  "coalesce_ms": 3000,
  "coalesce_grid": true,
  "spinner_animate": false
}
```

- **`coalesce_ms: 3000`** — 3s is comfortably above the ~100–320ms cDRX
  needs, and the eager window means the answer still *starts*
  instantly, which is what perceived latency is actually made of. If 3s
  feels laggy in use, 1500 still gives ~20× and stays well clear of the
  cDRX threshold.
- **`coalesce_grid: true`** — non-negotiable with 3–4 bots; without it
  the streams interleave and most of the benefit evaporates.
- **`spinner_animate: false`** — the animation exists only to look
  alive, and it is what blocks the dedupe. During long tool calls this
  is the whole second-order win.

Suggested rollout: enable on **one** bot (kopi-fir), leave the others at
defaults, and diff the `FRAMESTATS` lines — the instrumentation makes
that a grep, and the A/B is free because the knobs are per-bot.

One thing worth watching in the first days: the heartbeat dedupe is on
by default (it is behaviourally invisible), so if Poe *does* drop a long
tool-call turn that it previously survived, the 16s liveness floor is
the first suspect — lower `hbDedupeFloorMin` / the `2×` multiplier, or
raise `StallThreshold`, and it moves together.
