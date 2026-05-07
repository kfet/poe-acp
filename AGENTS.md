Use idiomatic Go. Keep it simple.

Prefer `sync/atomic`, `sync.Once`, and channels over manual mutex management when appropriate.

Do not ignore any issues, address them promptly, even if preexisting. Do not postpone any work, even if it seems daunting — just break it down into smaller tasks. **Never dismiss a problem as "pre-existing" or "out of scope" — you own this entire codebase. If you see it, you fix it.**

Do not leave incomplete or stubbed code. Ensure all code is functional and tested.

## What this is

`poe-acp` is a standalone HTTP server that implements Poe's server-bot protocol and relays each conversation to a spawned ACP-speaking agent (`fir --mode acp`, Claude Code, etc.) over stdio. One binary, no MCP surface. Each Poe `conversation_id` maps 1:1 to an ACP session inside a shared agent process.

See [docs/poe-acp-design.md](docs/poe-acp-design.md) for the full design, goals, non-goals, and milestones. For the Poe wire protocol see [docs/poe-protocol-reference.md](docs/poe-protocol-reference.md).

## Repository layout

```
cmd/poe-acp/       entry point: flags + server wiring
docs/                    design doc + Poe protocol reference
internal/acpclient/      acp.Client impl + stdio agent process wrapper
internal/config/         JSON config loader (DisallowUnknownFields)
internal/httpsrv/        /poe handler with heartbeat + cancel plumbing
internal/paramctl/       parameter_controls schema builder + Resolve
internal/poeproto/       minimal Poe HTTP+SSE
internal/policy/         allow-all / read-only / deny-all permission gates
internal/router/         conv_id → ACP session map + GC
test/smoke.sh            black-box SSE smoke test
```

The relay owns `conv_id → session` lifecycle. Agents are spawned via `--agent-cmd` (default `fir --mode acp`). Keep the split clean: HTTP + Poe framing in `httpsrv`/`poeproto`, agent + ACP in `acpclient`, session lifecycle in `router`, policy in `policy`.

## Think before you specialise

Before implementing a fix or feature inside a specific package, stop and ask: **is this actually unique to this layer, or does it belong elsewhere?**

- Poe protocol concerns (event shape, SSE framing) → `poeproto`.
- Agent-process concerns (spawn, stdio, ACP framing) → `acpclient`.
- Session lifecycle (cwd, GC, heartbeat, cancel) → `router` + `httpsrv`.
- Policy (tool permission decisions) → `policy`.
- Operator-facing config (defaults, bot identity) → `config` + `paramctl.Resolve`.
- Schema building (parameter_controls) → `paramctl.Build`.
- When fixing a bug, check whether the same bug exists in sibling code paths. Fix it at the root, not per-site.

## Git

Git commands that require an editor (`git rebase --continue`, `git commit`, `git merge --continue`) will open vim non-interactively and hang. Always prefix such commands with `GIT_EDITOR=true`:

```bash
GIT_EDITOR=true git rebase --continue
GIT_EDITOR=true git commit
```

When the user says "rebase to main", they mean local `main`, not `origin/main`.

When merging a feature branch back to main, always use `git merge --ff-only` to keep a linear history. Rebase the branch first if needed.

## Stuck loops

If you have run the same command (`go test`, `go build`) more than 5 times since your last file edit, you are in a stuck loop. Stop. Do not re-read any file you have already read this session. Rewrite the problematic file completely from scratch. If tests are failing due to API changes, the test file itself needs updating — patch it or rewrite it, don't just re-run it.

## Build and test

Run `make test` to verify your changes. Always finish every task with `make all` to confirm the full build and test suite passes (vet, test-race-cover, 5 cross-builds, native build, check-licenses).

When fixing a regression, **write the test first** so it fails before your fix, then make it pass. This confirms the test actually catches the bug.

The live smoke test against a real Poe + agent roundtrip is `test/smoke.sh`; run it manually when changing anything in `httpsrv` or `router`.

## Testing — avoid wall-clock timeouts

Prefer deterministic synchronization over `time.Sleep` and wall-clock polling:

- **Channels over polling** — use `chan struct{}` signals, `sync.WaitGroup`, or callbacks instead of `require.Eventually` with arbitrary timeouts. When testing async behaviour (agent spawn, event delivery, cancel), wire callbacks or subscribe to events and wait on channels.
- **No `time.Sleep` in tests** — sleep-based tests are flaky under CI load and the race detector. If you need to wait for a goroutine, use a channel or sync primitive.
- **`require.Eventually` is a last resort** — only for checking external state you can't subscribe to. Use short poll intervals (10ms) and generous timeouts (3–5s) when unavoidable.
- **Callbacks in Config, not after init** — if a struct spawns goroutines on creation, callbacks must be set via the config/options struct *before* construction, not after. Setting callbacks after init races with the goroutine.

## Agent-process concerns

The relay spawns agents as long-lived child processes and talks ACP over their stdio. A few recurring traps:

- **Cold-start budget** — `fir --mode acp` can take multiple seconds to be ready. Use the HTTP request context for readiness gates; don't bake in a 30-second wall-clock deadline.
- **Per-conversation cwd** — each session runs in its own working directory so `.fir/` state (skills, MCP, auth) stays isolated. Don't share cwds across conversations.
- **Heartbeat + cancel** — the `/poe` handler must keep the SSE connection alive (heartbeat events) and propagate HTTP disconnect to `session/cancel` on the agent side. Don't regress either.
- **GC** — stale sessions get reaped. Make sure anything holding a session reference (router, handler) checks liveness before dispatching.

## Changelog

When making non-trivial changes, add an entry under `## [Unreleased]` in `CHANGELOG.md` using the appropriate subsection (`### Added`, `### Fixed`, `### Changed`, `### Removed`). Keep entries concise — one line per change. Do not bump `VERSION`; that happens during release (see `.fir/skills/release/SKILL.md`).

New entries go at the top of their subsection (most recent first).

## Release

Releasing is driven by `.fir/skills/release/SKILL.md`. `make publish` pushes `main + vVERSION` to `origin`; `release.yml` runs `make all` + `make notices` and then GoReleaser, which publishes the GitHub release and updates `Formula/poe-acp.rb` in the shared `kfet/homebrew-fir` tap. Users install with `brew install kfet/fir/poe-acp`.

## Caveman Mode

Ultra-compressed communication. Slash token usage ~75% by speaking like caveman while keeping full technical accuracy.

### Grammar
- Drop articles (a, an, the)
- Drop filler (just, really, basically, actually, simply)
- Drop pleasantries (sure, certainly, of course, happy to)
- Short synonyms (big not extensive, fix not "implement a solution for")
- No hedging (skip "it might be worth considering")
- Fragments fine. No need full sentence
- Technical terms stay exact
- Code blocks unchanged. Caveman speak around code, not in code
- Error messages quoted exact

### Pattern
`[thing] [action] [reason]. [next step].`

Not: "Sure! I'd be happy to help. The issue is likely caused by..."
Yes: "Bug in auth middleware. Token expiry check use `<` not `<=`. Fix:"

### Boundaries
- Code: write normal. Caveman English only
- Git commits: normal
- PR descriptions: normal
- User say "stop caveman" or "normal mode": revert immediately
