# Brief: poe-acp self-heal — MINIMAL rebuild

You are on **miki**, the build/git box. Source lives in `~/src/<repo>` — never write to `~`.
Work in this worktree: `~/src/poe-acp-selfheal2`, branch `feat/selfheal-min`.
Do NOT ssh to or modify any fleet host (kopitwo, ko1, rn, airm1). Local code + tests only.

## Read this first

A previous attempt (PR #3, now closed) built this and was rejected as **overengineered**:
versions/ directory, current/last-good symlinks, ExecStart retargeting, pinned.json,
durable per-bot crash counters, rollback.log, a `poe-acp dist` subcommand, a docs page,
and a legacy-layout fallback. **2315 lines.** Do not resurrect any of it. If you find
yourself adding a state file, a subcommand, or a migration path, you are off track.

**LINE BUDGET: ~450 lines of Go for the whole thing, excluding tests.
If you pass 600, STOP and explain why in the PR description instead of continuing.**

## What we are building

poe-acp must keep itself and its agent up to date by **pulling**, because the fleet is
intermittently online and ssh-push never reaches a sleeping laptop. Four hosts, one
operator, project in construction. Favour deletion over generality throughout.

### 1. Binary swap + one-deep rollback (~80 lines, in the supervisor)

- Update = download to `poe-acp.new` (same dir as the running binary), fsync, then
  `rename` the current file to `poe-acp.prev`, then `rename` `.new` over `poe-acp`.
  No symlinks, no versions dir, no ExecStart change. Renaming a running binary is safe
  (open inode survives) and sidesteps ETXTBSY.
- The supervisor already forks a new worker and **waits for the ready handshake** before
  draining the old one. That gate is the entire safety property — keep it exactly.
- If the new worker fails to come ready, or dies in that window: rename `.prev` back over
  `poe-acp`, fork again, log loudly. The old worker has not been drained, so service never
  breaks.
- That is the whole rollback story. **No crash counters, no durable state, no pins, no
  rollback.log.** The rare "came ready fine, crash-loops an hour later" case is recovered
  manually with `mv poe-acp.prev poe-acp && restart` — the `.prev` file is what makes that
  one command.
- Add in-memory respawn backoff only if the supervisor doesn't already have it (~10 lines).

### 2. `poe-acp reconcile` (~250 lines)

- **Dry-run by default**; `--apply` to act. Non-negotiable.
- Fetch `dist.lock` (conditional GET, cache with ETag; atomic rename on write).
  For now read it from the repo's raw URL or a release asset — pick the simplest that
  needs no git on the host, and say which you chose.
- Compare, then act per artefact:
  - **poe_acp — policy `pin`**: exact match. Download, verify checksum, swap (part 1).
  - **fir — policy `floor`**: do NOT manage fir's binary. fir has its own self-updater —
    shell out to `fir update`, then verify `fir --version >= lock.fir` and report if not.
  - **exts**: `fir packages update`, then verify.
- Lock schema gains a per-artefact policy, defaulting to `pin` so existing locks still
  parse:
  ```json
  { "poe_acp": "0.55.0",
    "fir": { "version": "0.98.1", "policy": "floor" } }
  ```
  Semantics: `pin` = converge exactly, including downgrade. `floor` = upgrade if below,
  leave alone and report `ahead` if above.
- Log one greppable sentence per action to the existing journal/log output — e.g.
  `swapped 0.55.0 -> 0.56.0`, `REVERTED to .prev: worker never came ready`,
  `fir ahead of lock (0.98.2 > 0.98.1), leaving`.

### 3. Timer (~30 lines)

Supervisor runs reconcile on boot and on a jittered ~15m timer. Off by default behind one
config flag so it can be enabled host by host.

### 4. Status (~60 lines)

Write `status.json` next to the config: versions running, lock revision applied, drift
flags, timestamp. **No endpoint, no push, no server.** `poe-acp fleet` ssh-cats it from
wherever it's run; sleeping nodes show as unreachable, which is true.

## Explicit punts — do not build these

Signing, canary/staged rollout, delta downloads, secrets management, unit-file or plist
rewriting (report drift only, never write), fresh-host bootstrap, emergency fir downgrade
(manual ssh), rolling back more than one version deep.

## Constraints

- Read `cmd/poe-acp/` and `internal/` first; match existing style.
- Every write (binary, lock cache, status.json) uses temp + atomic rename.
- Do not touch `scripts/converge.sh` or `bots/*.json`.
- Table tests for the swap/rollback state machine and the pin/floor comparison. Don't
  shell out in tests.
- `go vet` clean, full test suite green.

## When done

Push and open a PR with `gh`. In the description: total non-test line count, what you
chose for the lock source, and every deliberate punt. Print the PR URL.

If you hit a design fork you can't resolve from the code, STOP and append the question to
`NOTES.md` in the worktree rather than guessing.
