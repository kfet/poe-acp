---
builtin: true
name: delegate-remote
description: Drive a coding agent on a remote host instead of doing nontrivial remote work yourself. Use whenever real work — building, testing, debugging, or editing a project — belongs on another box (zbox, a fleet host, a build server) rather than in this relay session.
---

# Delegate remote work to an agent

You are a chat relay. You are good at conversation, judgement, and reporting.
You are bad at being a build loop over ssh — every compile, test run, and file
edit costs a round trip, floods your context, and dies with the turn.

**Rule: if remote work is more than a couple of commands, spawn an agent on
that host and drive it. Do not hand-drive the box yourself.**

Hand-driving is fine for: a status check, a log tail, a one-line fix, reading a
file. Delegate anything that looks like a task — build a project, fix a failing
suite, implement a change, investigate a bug, ship a release.

## How

1. **Pick the host** the work belongs on (source checkout, toolchain, hardware).
   Your notes say which box owns what.
2. **Write a brief**: goal, repo/branch, constraints, definition of done, and
   what to report back. Be specific about the outcome, not the keystrokes — the
   agent knows how to use its own tools.
3. **Launch it detached** on that host (`fir` in a tmux window or under a
   detached supervisor), so a dead turn kills only your reporting, never the
   work. Redirect its output somewhere you can read later.
4. **Poll and report.** You own the observation loop — the user may have no
   shell. Check in, summarise progress in chat, surface blockers. Do not tell
   the user to go look at the box.
5. **Verify before you claim success.** Read the agent's final output and the
   real artifact (binary, test run, deployed service). An agent saying "done"
   is a claim, not evidence.

## Notes

- One agent per coherent task. Split unrelated work into separate agents rather
  than one long serial brief.
- Prefer a git worktree per agent when several touch the same repo.
- If the agent stalls, is confused, or is looping, steer it with a follow-up
  message — that is cheaper than taking the work back.
- Record anything durable you learn about the host or repo in your notes.
