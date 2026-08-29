# BRIEF: stop silently swallowing "this agent leaks sessions"

## Why (verified)

poe-acp's idle GC does the right thing: gcOnce (internal/router/router.go:2945)
calls Agent.ReleaseSession, which sends the `session/release` ACP RPC.

But some agents do not implement it. acp-tmux (github.com/kfet/acp-tmux) does
not — it answers -32601 method-not-found. poe-acp logs that at **Debugf**
(router.go:2946) and moves on.

Result: the relay believes it reaped the session, the agent keeps it forever.
On zboxserver this leaked ~26 sessions/day at ~465 MB each until 31 GB RAM and
8 GB swap were exhausted and the host live-locked off the network. The failure
was completely invisible in normal logs.

Same silent path exists at router.go:2740 (getOrCreate releasing a stale sid).

## Goal

Make "the agent does not support session release" a VISIBLE, one-time warning
rather than debug noise.

- On ReleaseSession failure, distinguish the cases. A method-not-found /
  unimplemented response means the agent cannot reclaim sessions at all — that
  is a standing resource leak and deserves `log.Printf` at WARN, once per
  agent (not once per session — the whole point is not to spam).
- A session-not-found error (client.IsSessionNotFound, -32001) is NORMAL and
  must stay quiet — it just means the agent already dropped it.
- Any other error stays Debugf as today.
- Wording should tell the operator what it means and what it costs, e.g. that
  idle sessions will accumulate in that agent until it is restarted.

## Definition of done
- `make` passes clean, tests included.
- A test proves: -32601 warns exactly once across several GC passes; -32001
  never warns; the happy path never warns.
- No behaviour change other than logging. Do NOT make release failure fatal,
  and do not add retries.

## Constraints
- Branch `warn-release-nosupport` in this worktree. Small, surgical change.
- Do not change the wire protocol or acp-kit.

## When done
Run the `ship-it` skill: review-and-fix loop until clean, ff-merge, clean up the
worktree. Report what landed.
