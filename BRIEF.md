# Brief: bot-as-dist-spec + converge + `--tot` (poe-acp)

You are implementing steps 1–3 of an agreed design. Repo: `kfet/poe-acp`
(this worktree). Branch already created for you.

**Use advisor** (your own escalation config) for design judgement calls and for
an adversarial review pass before you declare done. Do not pin a specific
advisor model — just escalate normally.

## Context — why

4 personal Poe server bots, one per host. Today each bot's identity is smeared
across four surfaces: `config.json`, the systemd unit / launchd plist (e.g.
`--heartbeat-interval` lives in the **unit file**), an env file, and the agent
dir. Result: silent drift. A live audit right now found:

| host | bot | poe-acp | fir | notes |
|---|---|---|---|---|
| kopitwo (linux, arm64, systemd --user) | two-fir | 0.51.0 | **0.93.0** | |
| ko1 / kopione (linux, arm64, systemd --user) | kopi-fir | 0.51.0 | **0.94.0** | intro says "default opus-4-8" but config says opus-5 |
| rn / sea-racknerd (linux, x86_64, systemd --user) | sea-fir | 0.51.0 | **0.95.0** | config has extra `"poe_mcp": true` |
| airm1 (macOS, launchd, brew) | fir-air | 0.51.0 | **0.90.1** | config has `"agent": {"profile": "fir"}`; unit lacks `--enable-mcp-attach` and `--session-ttl`; uses DEFAULT config path (no `--config`); poe-path `/poe-acp` not `/fir-air` |

Four different fir versions. That is the bug being killed.

## Design (already decided — do not relitigate)

- fir stays a fat single binary. **poe-acp is the distribution/curation layer.**
  LazyVim-shaped: manifest + converge script, never a fork.
- **A bot IS a dist spec**: one declarative JSON file that fully describes it.
- **Extensions are NOT embedded.** Explicit user decision: "we want ext to grow —
  don't embed it." fir extensions stay an external, independently growing repo
  (`github.com/kfet/fir-exts`), installed via `fir install`, **pinned by rev in
  the lockfile**. Do not `go:embed` any Python. Do not vendor fir-exts.
- **`tot` is a verb, not a state.** No host is ever allowed to be "rolling".
  `--tot` means: resolve latest-of-everything ONCE, write the result into
  `dist.lock`, commit, then converge all hosts from that recorded lock.
  Resolution NEVER happens on a host.
- YAGNI line: specs are static files. No templating, no inheritance, no
  resolver, no registry, no separate repo, no Ansible/Nix.

## Deliverables

### 1. `bots/<name>.json` + `dist.lock`  (pure transcription, zero behaviour change)

Four spec files: `bots/two-fir.json`, `kopi-fir.json`, `sea-fir.json`,
`fir-air.json`, capturing the hosts **exactly as they are now** (see table above;
full current configs and ExecStart lines are in `CURRENT-STATE.md` in this
worktree — read it first).

Suggested shape — refine with advisor, but keep it flat and boring:

```json
{
  "name": "two-fir",
  "host": "kopitwo",
  "platform": "linux/arm64",
  "supervisor": "systemd-user",
  "unit": "poe-acp-two-fir",
  "agent": { "cmd": "fir --mode acp", "kind": "fir" },
  "fir": { "exts": ["github.com/kfet/fir-exts"] },
  "server": {
    "http_addr": "127.0.0.1:8347",
    "poe_path": "/two-fir",
    "heartbeat_interval": "3s",
    "session_ttl": "10m",
    "enable_mcp_attach": true,
    "introduction": "..."
  },
  "config": { "...": "the contents of config.json, verbatim" },
  "credentials": { "env_file": "~/.config/poe-acp/bot-two-fir/env" }
}
```

Rules:
- Top level stays **agent-generic** (`agent.cmd`, `agent.kind`). Agent-specific
  stuff goes in a **bespoke per-agent stanza** (`"fir": {...}`). Do NOT invent a
  generic cross-agent `plugins:` field — fir/claude-code/gemini-cli/codex have
  four incompatible ext systems and one word for them would be a lie.
- Credentials are **references only**, never contents. No secrets in the repo.
- Preserve the per-bot quirks faithfully (sea-fir's `poe_mcp`, fir-air's
  `agent.profile`, fir-air's different poe_path / missing flags). The spec must
  reproduce today's fleet byte-for-byte where it matters. Flag the quirks in
  the report as candidates for later normalisation — do not normalise now.

`dist.lock` (JSON, top of repo):
```json
{
  "poe_acp": "0.51.0",
  "fir": "0.95.0",
  "exts": { "github.com/kfet/fir-exts": "<git rev>" },
  "resolved_at": "2026-08-06T..."
}
```
fir is released from `github.com/kfet/fir-dist` (releases tagged `v0.95.0`);
that is the source to check/fetch. Record the fir-exts rev by asking git.

### 2. `converge.sh <bot>` — the only sanctioned way to touch a host

Plain POSIX-ish bash, ~40–120 lines, lives in `scripts/converge.sh`. It reads
`bots/<bot>.json` + `dist.lock` and makes the target host match. Steps:

1. resolve host + supervisor from the spec
2. ensure poe-acp at locked version (compare `--version`; fetch the release
   asset for the right platform if not)
3. ensure fir at locked version (compare `fir --version`; `fir update`, or fetch
   from fir-dist releases)
4. ensure each `fir.exts` entry installed at the locked rev (`fir install` /
   `fir packages update`; check the rev)
5. render `config.json` from `spec.config` and write it
6. ensure the unit/plist ExecStart matches `spec.server` (systemd unit or
   launchd plist — **macOS needs `launchctl bootout` + `bootstrap`, NOT
   `kickstart -k`**, which re-runs the cached job definition and ignores an
   edited plist)
7. recycle the worker, verify it comes back active and reports the right version

Requirements:
- **`--dry-run` must be the default-safe path**: print the diff of what would
  change, touch nothing. `--apply` to act.
- **Idempotent**: a second run must report "already converged" and do nothing.
- Backups before overwriting anything (`.bak-<timestamp>`).
- `jq` for JSON. Assume ssh works by host alias.
- You (on miki) may NOT have ssh to the four bot hosts. Do not block on that:
  make the script testable via a `--dry-run` against a fake/local target, and
  test the JSON→config/ExecStart rendering with unit tests. The real
  apply-run will be done by the operator from kopitwo.

### 3. `--tot`

`scripts/converge.sh --tot` (or a sibling `scripts/tot.sh`, your call):
resolve latest poe-acp release, latest fir-dist release, latest fir-exts rev →
rewrite `dist.lock` → **stop** (leave the commit to the operator, or commit with
a clear message). Then converging all bots uses that lock. Emit a clear summary
of what moved. Must never write a lock and converge in the same breath without
showing the diff.

## Quality bar

- `make all` green: cross-builds, vet, race+shuffle tests, coverage gate,
  licenses. This repo enforces **100% coverage** — if you add Go code, it needs
  tests. Prefer keeping converge logic in shell + a small tested Go/CI-checkable
  renderer only if it genuinely helps; **shell is the sanctioned choice here**,
  do not build a spec system in Go.
- Do not modify existing tests.
- Shellcheck-clean if shellcheck is available.
- Update `README.md` and the bundled `deploy` / `update` / `custom-bots` skills
  (in `skills/`) to route through converge: "converge is the only sanctioned way
  to touch a host". Add a CHANGELOG entry.
- Adversarial review pass with advisor before declaring done. Then fix findings.

## Do NOT build

spec templating/inheritance/resolver · a generic cross-agent `plugins:` field ·
per-host rolling ("tot" as a host state) · a separate bots repo · Ansible/Nix ·
a registry or marketplace · `go:embed` of extensions · non-fir agent support
beyond simply not blocking `agent.cmd`.

## When done

Write `REPORT.md` in the worktree root: what shipped, the spec schema, how to
run converge, drift the specs revealed, quirks flagged for normalisation,
anything you disagreed with in this brief and why. Commit everything on the
branch. **Do not push, do not merge, do not tag** — the operator reviews first.
