# STATUS — work/reactions

End-to-end Poe `report_reaction` support, plus the per-session turn-queue
refactor the spec called for. Branch is build-clean (`make all` ✓; tests +
race + cross-builds + license check + 100% coverage gate). Not committed —
left for review-and-fix.

## What landed

### 1. `internal/poeproto` — reaction decode

- `Request` gained `Reaction` + `ReactionAction` (typed `ReactionAdded`/`ReactionRemoved`).
- `Decode()` runs `normaliseReaction()` for `report_reaction` payloads, collapsing both
  observed wire shapes into `(kind, added|removed)`:
  - single field with prefix: `+👍` / `-👍` / `like` / `dislike` (no prefix → added);
  - split fields: `{"reaction":"👍","action":"added"|"removed"}`.
- Raw bodies stay logged via `debuglog` (cap 16 KiB tee), so production traces still show
  the true Poe shape even after normalisation.
- Tests: `TestRequest_DecodesReaction` (split-added/removed, ±prefix, bare like/dislike).

### 2. `internal/router` — per-session turn queue

- Removed `sessionState.turnMu` + `inUse`. Added `queue *sessionQueue` (mutex-guarded slice
  + buffered `notify chan`) and `runStop chan`.
- New single-goroutine owner of `Agent.Prompt` per session: `runTurns(st)`, spawned alongside
  `drainChunks` in `getOrCreate`.
- `turnReq` carries `kind` (`turnUser`/`turnReaction`), ctx, sink, opts, blocks, hideThinking,
  applySystemPromptInline flag, enqueuedAt, done, err, shed.
- Overflow policy (`sessionQueueCap = 32`):
  - Drop the **oldest reaction** first; never shed a user prompt (queue grows past the cap if
    no reaction is available to shed).
  - If a new reaction arrives with no older reaction to shed → drop the **new** reaction.
- Liveness: reactions older than `reactionMaxAge = 60s` at dequeue are skipped.
- `endTurn`-ack invariant preserved: runner sends `endTurn` after `Agent.Prompt` returns,
  waits for the drain to ack, then calls `sink.Done()` / `Error` / `Replace`. Sink writes never
  race late chunk delivery.
- `Prompt()` waits **unconditionally** on `req.done` (even after ctx fires) — guarantees the
  HTTP handler doesn't return until the shared sink has been finalised, fixing the SSE
  use-after-free panic that an early naive impl produced. Caller still gets `ctx.Err()` if the
  runner finished cleanly.
- `gcOnce` now evicts on `queue.idle()` (queue empty AND no in-flight turn).
- New router API: `Router.ReportReaction(ctx, convID, userID, messageID, kind, action)`.
  Builds the synthetic prompt, queues a `turnReaction` with `discardSink`, returns as soon as
  the queue accepts (or immediately if dropped).

### 3. `internal/router` — system-prompt clause

`reactionContractClause` is prepended to the operator's `SystemPrompt` in `New()`. Explains the
`[poe-acp:out-of-band ...]` marker contract: don't address the user, keep the reply terse, update
memory silently. Future out-of-band kinds (feedback?) can reuse the same prefix.

### 4. `internal/httpsrv` — wire-up

`ServeHTTP` splits `report_reaction` out of the `accept+drop` group; it calls
`Handler.handleReaction` which logs the decoded fields (debuglog), drops malformed payloads
with no `reaction` kind, and invokes `Router.ReportReaction`. HTTP returns 200 OK regardless
of queue acceptance — Poe has no SSE channel for the reaction reply, so dropped reactions
are logged but never user-visible.

### 5. Tests added

- `internal/poeproto/poeproto_test.go` — `TestRequest_DecodesReaction` (6 subtests).
- `internal/router/reactions_test.go` — FIFO ordering, ctx-cancel waits for runner, oldest-reaction
  shed, new-reaction drop on user-saturated queue, age drop on dequeue, `endTurn`-ack-before-sink-Done
  invariant, `ReportReaction` fire-and-forget, queue.stop drains pending, torn-down session, default
  action="added", getOrCreate-error wrap, ctx+runner-err precedence.
- `internal/httpsrv/branches_test.go` — `report_reaction` forwards a synthetic turn with the marker
  prefix, `-emoji` prefix maps to `removed`, debuglog branch, router-error log branch.

### 6. Docs & changelog

- `docs/poe-protocol-reference.md` — `report_reaction` row now says "out-of-band turn" and describes
  the decode + forward path.
- `CHANGELOG.md` — Unreleased: Added (out-of-band turn delivery) + Changed (queue refactor, system-prompt clause).

## Open questions

1. **`reactionMaxAge` and `sessionQueueCap`.** Picked 60s and 32 by feel. No config knobs yet — tune
   once we see the reaction-burst shape in production.
2. **GC-while-queued race.** Right now a session created by `ReportReaction` will live at least until
   `SessionTTL` elapses since the last `touch`. A reaction-only conversation (no user query ever)
   touches `lastUsed` only at turn-completion via `runOneTurn`'s `defer r.touch`. Reasonable, but if
   we worry about *very* idle reaction streams keeping a session alive, revisit.
3. **Synthetic prompt wording.** The prompt currently says `Acknowledge silently — your reply will NOT
   be shown to the user.` Some agents may still produce a chatty reply and waste tokens. Worth A/B'ing
   the wording once we have live runs.
4. **Wire shape unknowns.** The two shapes documented in the spec are speculative. The raw-body debuglog
   means we'll see the real shape on first production hit; if Poe ships a third variant,
   `normaliseReaction` is a one-line addition.
5. **Tests use a small amount of polling** (`for atomic.Load… && time.Now().Before(deadline)`) to wait
   for the runner to enter `Agent.Prompt`. Channel-based handoff would be cleaner but requires touching
   `fakeAgent.Prompt`. Left as-is to keep the diff focused; happy to tighten if reviewer prefers.

## Verification

```
make all   # vet, test-race-cover (100%), 5 cross-builds, native build, check-licenses — all ✓
```

`test/smoke.sh` was **not** run (requires a live Poe + agent). Recommend running it before merge
since this touches `router` + `httpsrv`.
