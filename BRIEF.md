# Brief: make the absorbed-turn path diagnosable, then converge the fleet

Repo: `~/src/poe-acp` on miki (this box). Work on a worktree per convention:
`~/src/poe-acp-wt-absorb-logging`, branch `work/absorb-logging`.

## Background — why this exists

A pre-output client transport drop is ABSORBED by `internal/httpsrv/handler.go`:
the turn is NOT cancelled, it runs on a decoupled context to completion, and its
recorded output is buffered so Poe's automatic redrive replays it instead of
re-prompting the agent (which would repeat side effects). See commit `f8af9ec`
for the original design and `7c20551` for the on-disk store added in 0.62.1.

**The problem: this entire path is invisible in production.**

An incident on kopitwo/two-fir on 2026-08-28 took four rounds of investigation and
STILL could not be settled from logs. A turn received at 06:55:42 ran 54m59s to
completion (FRAMESTATS 07:50:41) after the user's phone dropped. It is impossible
to tell from the logs whether that turn was absorbed and buffered, or whether Poe
held the bot connection the whole time and only the phone's leg died. Those are
different bugs.

Two concrete causes:

1. The absorb latch logs at `kitlog.Debugf("absorbed pre-output drop: buffered
   answer conv=%s msg=%s", ...)` — debug is off in production, so nothing is
   emitted. The always-on `WARN fast client disconnect` only fires when elapsed is
   under `defaultFastCancelThreshold` (2s), so it misses every realistic drop.

2. **Two different id namespaces, and the one that matters is never logged.**
   - `handler.go:273` logs `RECV conv=%s msg=%s` using `req.MessageID` — the
     per-HTTP-query id, unique on every request. Useless for correlating a redrive.
   - The answer buffer keys on `answerKey(req.ConversationID, latestUserMessageID(turns))`
     — the last `role=="user"` turn's `MessageID`. **This value is logged nowhere.**

   Consequence: you cannot tell from logs whether two queries are a redrive pair.
   During the investigation I "measured" redrive frequency by counting duplicate
   `msg=` in RECV, got zero, and drew a wrong conclusion — the field is unique by
   construction. Do not let the next person make that mistake.

## Goal

Make an absorbed turn and its redrive legible in a default-verbosity production log.

## Required changes

1. **Log the buffer key.** Add the latest-user-message id to the `RECV` line as a
   distinct field — e.g. `RECV conv=… msg=… umsg=… bytes=…`. Keep `msg=` as-is;
   do not repurpose or rename it, other tooling and my notes reference it. Pick a
   field name that cannot be confused with `msg=`.

2. **Promote the absorb latch to an always-on log line.** It should fire when the
   disconnect watcher latches the absorb decision, and carry at minimum:
   conversation id, the buffer key's user-message id, and elapsed time since the
   turn started. This is a rare event (roughly 4/week on a busy bot) so it does not
   need rate limiting. Choose the level sensibly — it is a real user-visible
   failure, so WARN is defensible, but it is also fully handled, so INFO-style
   always-on is fine if that fits the codebase's conventions better. Your call;
   justify it in the commit message.

3. **Make the completion of an absorbed turn visible too.** When an absorbed turn
   finishes and its answer is buffered (`put`), emit an always-on line with the key
   and the turn duration. Without this you can see a turn was absorbed but not
   whether its answer ever landed. Keep it to one line.

4. Check the redrive-serve paths (`redrive served from buffer` is currently
   `Debugf`; `redrive served from absorbed in-flight turn` is already `log.Printf`).
   Make them consistent — both are rare and both are exactly what an investigator
   needs. Promote the Debugf one.

## Explicitly OUT of scope

Do NOT change `--answer-ttl`, `AnswerTTL`, `pendingTTL`, `IdleWriteTimeout`,
`StallThreshold`, or any timeout default. The 2m AnswerTTL is CORRECT — it is
sized for Poe's automatic seconds-scale redrive, per `f8af9ec`. An earlier analysis
of mine proposed raising it to 24h and then replacing it with an event-driven
"supersession" scheme; both were based on an unverified assumption about Poe's
retry semantics and are **withdrawn**. This task is observability only. If you
find yourself editing a duration, stop and re-read this paragraph.

## Definition of done

- `make all` green (the repo has a 100% coverage gate — expect to add tests).
- Tests cover the new lines actually being emitted at default verbosity. The bug
  being fixed is "nothing is logged", so a test asserting presence at default
  level is the point, not an afterthought.
- Reviewed and merged to main via the `ship-it` skill (review-and-fix loop,
  ff-merge, cut a release). This will be 0.63.0 or 0.62.2 — your judgement.
- Fleet converged to the new version. **kopitwo/two-fir is the priority** — it is
  currently on 0.62.0, which has the MEMORY-ONLY answer buffer. That buffer is void
  across every SIGHUP worker swap, and a worker swap is itself a bot-side failure —
  i.e. the feature is dead in exactly its primary use case. 0.62.1's on-disk store
  fixes that and has never been deployed anywhere. Converging the whole fleet is
  in scope; two-fir is the one that must work.

## Gotchas (learned the hard way, do not rediscover)

- `./scripts/converge.sh <bot> --apply </dev/null` — converge.sh runs ssh, which
  eats the rest of the script from stdin when a script is piped to `bash -l -s`.
- converge.sh short-circuits on `changes=0` WITHOUT comparing the RUNNING worker
  version, so a bot sharing a binary with an already-converged bot can silently
  keep running the old worker. Verify the running worker's version, not just the
  on-disk file.
- kopitwo refuses zbox's ssh key. Drive it from miki.
- All source and git work happens on miki. Fleet hosts carry the static binary
  only — never build or push from one.

## Report back

Write a short report to `~/ops/absorb-logging-REPORT.md` covering: what you
changed and why, the exact new log lines with a real example of each, the released
version, per-host converge status with the RUNNING worker version confirmed, and
anything you found that contradicts this brief. I will read that file.

If something here is wrong or you disagree with the approach, say so in the report
rather than silently doing something else.
