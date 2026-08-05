---
builtin: true
name: delegate
description: Hand nontrivial work to an independent agent and steer it, instead of doing it inline yourself. Applies anywhere — on this host or a remote box (a build server, a fleet host). Use whenever real work — building, testing, debugging, implementing, investigating — is more than a couple of commands.
---

# Delegate the work, steer the agent

You are a chat relay. You are good at conversation, judgement, steering, and
reporting. You are bad at being the work loop — every command, edit, and test
run costs a round trip, floods your context, and dies with the turn.

**Rule: if a task is more than a couple of commands, spawn an agent to do it
and steer that agent. Do not do it inline.**

This is not a remote-work trick. It holds on this host too. A fresh agent
brings its own context and can run a different model; you bring judgement and
correction. In practice that pairing beats the same model grinding the task out
inline — the diversity is doing real work, not just the parallelism.

Do it inline: a status check, a log tail, a one-line fix, reading a file,
answering from what you already know.

Delegate: build a project, fix a failing suite, implement a change,
investigate a bug, ship a release, anything open-ended.

## How

1. **Pick where it runs.** The work goes where the source, toolchain, or
   hardware is. That may be this host. Your notes say which box owns what.
2. **Write a brief**: goal, repo/branch, constraints, definition of done, what
   to report back. Specify the outcome, not the keystrokes — the agent knows
   how to use its own tools.
3. **Let the agent run on its own config.** It brings its own provider, model
   and advisor. When the task needs a stronger or different mind — review,
   second opinions, a stall — say so in the brief and tell it to escalate to
   its advisor, rather than forcing a model on it. See spend, below.
4. **Launch it detached**, so a dead turn kills only your reporting, never the
   work. Put its output somewhere you can read later.
5. **Steer it.** This is the part that earns the delegation. Check in, read what
   it actually did, correct course early, answer its questions, push back when
   it declares victory too soon. A launched-and-ignored agent is worse than
   doing it yourself.
6. **Poll and report in chat.** You own the observation loop — the user may
   have no shell. Summarise progress, surface blockers. Never tell the user to
   go look at the box.
7. **Verify before you claim success.** Read the final output *and* the real
   artifact — binary, test run, deployed service. An agent saying "done" is a
   claim, not evidence.

## Spending is a deliberate choice, never a default

Paid credit is a genuinely useful resource and it is **finite and shared**.
Spending it is fine. Spending it *by accident* is not.

Before you launch an agent, make the choice out loud — one line — on three axes:

- **Provider.** Which pool pays for this, and is that the right pool? A flat
  subscription is free at the margin; reach for it first. Metered pools
  (per-token API keys, Poe points) are for when you actually need something
  the subscription cannot give you.
- **Tier.** Never hand an open-ended, multi-turn, autonomous task to a
  top-tier reasoning model (`-pro`, `-max`, and friends). Those cost dollars
  *per turn* and an unattended agent will happily take fifty of them. They
  are for a single, tightly-framed question — not for a work loop.
- **Blast radius.** Long-running + unattended + metered + top tier is the
  failure mode. Break at least one of those four.

**Do not force a model on the agent you spawn.** Launch it on its own
default — the agent you spawn has its own provider and its own configured
advisor, and that configuration is deliberate. When the task genuinely needs
a stronger or different mind — adversarial review, a second opinion, an
architectural call, a stall — say so *in the brief*: tell it to **escalate to
its advisor** (`aside` with `escalate=true`) for that specific judgement.
That gets you the model diversity you were after, scoped to the question that
needs it, and priced accordingly — instead of paying premium rates for the
agent's every `ls`.

Override the model only when you have a concrete reason the default cannot
work, and say what that reason is.

**Precedent:** wanting a non-Claude adversarial reviewer, a relay spawned a
top-tier `-pro` model as the *whole reviewer agent* and left it unattended on
a poll loop for ~30 minutes. It burned ~$15 and drained the account to
`402 insufficient_quota` before finishing. The cross-family instinct was
right; buying it by the turn, for every turn, was not.

Practical rules:

- **Check in early** on anything metered — first check-in within a few
  minutes, not after a 25-poll `wait`. An agent that is looping is burning.
- If a provider key is missing, do **not** silently fall through to the most
  expensive available one. Say what is missing and choose consciously.
- **Say what you are spending.** When you do launch on a metered provider,
  tell the user in the same message: provider, model, why not the default.
  One line. If you cannot justify it in one line, it is the wrong choice.

## Notes

- One agent per coherent task. Split unrelated work rather than writing one
  long serial brief.
- Prefer a git worktree per agent when several touch the same repo.
- If an agent stalls, loops, or is confused, steer it with a follow-up — that
  is cheaper than taking the work back.
- Record anything durable you learn about the host, repo, or task in your notes.
