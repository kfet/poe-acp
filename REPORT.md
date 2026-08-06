# REPORT — bot-as-dist-spec + converge + `--tot`

Branch: `work/dist-spec`. All work committed; nothing pushed/merged/tagged.

## What shipped

1. **`bots/*.json`** — four declarative specs (`two-fir`, `kopi-fir`,
   `sea-fir`, `fir-air`) transcribed faithfully from `CURRENT-STATE.md`,
   quirks preserved (see below).
2. **`dist.lock`** — fleet-wide target: poe-acp `0.51.0`, fir `0.95.0`
   (from `kfet/fir-dist`), fir-exts pinned at
   `2b2caa7a8504bc5c41aa88b17c3cbe8389884ee0`. Verified against live
   remotes: a `--tot` run resolved exactly these values ("nothing moved"),
   which also confirms fir-exts HEAD still equals the pinned rev.
3. **`scripts/converge.sh`** — the only sanctioned way to touch a host.
   Dry-run by default (full diffs, touches nothing), `--apply` to act,
   idempotent (second run: "already converged"), `.bak-<timestamp>` backups
   before every overwrite, ssh by host alias, `jq` only dependency.
4. **`--tot`** — resolves latest poe-acp tag, latest fir-dist tag, and
   fir-exts `HEAD` via `git ls-remote` (peeled tags stripped, strict
   `vX.Y.Z` filter, `sort -V`), prints old→new per component, rewrites
   `dist.lock`, and **stops**. It refuses any other argument
   (`--tot --apply` is an error): lock, review, commit, converge are
   separate deliberate steps. Resolution never happens on a host.
5. **`test/converge_render.sh`** — 30 offline assertions: golden renders
   for all four bots (config / execstart / unit / plist), boolean-flag edge
   cases, intro-content guard, `--tot` arg refusal, and a full fake-target
   cycle (dry-run writes nothing → apply writes → second apply reports
   converged → backup on overwrite → fir-air lands at the *default* config
   path). `REGEN=1` regenerates goldens.
6. **Docs/skills** — README "Fleet: bot specs + converge" section, layout +
   tests sections updated; `deploy` / `update` / `custom-bots` skills now
   open with "converge is the only sanctioned way to touch a host";
   CHANGELOG entry under `[Unreleased]`.

Quality bar: `make all` green (no Go code added, coverage gate untouched);
`shellcheck` 0.10.0 clean on both scripts; no existing tests modified.

## Spec schema

Flat, static, agent-generic at top level; fir-specifics in a bespoke `fir`
stanza; supervisor-specifics in `systemd` / `launchd` stanzas.

```jsonc
{
  "name": "two-fir",
  "host": "kopitwo",                 // ssh alias
  "platform": "linux/arm64",         // selects the release asset
  "supervisor": "systemd-user",      // or "launchd"
  "unit": "poe-acp-two-fir",         // unit name / launchd label
  "binary": "~/.local/bin/poe-acp",
  "agent": { "cmd": "fir --mode acp", "kind": "fir" },
  "fir": { "exts": ["github.com/kfet/fir-exts"] },   // pinned via dist.lock
  "server": { /* relay flags; ABSENT (or false/null) key = NO flag */
    "http_addr": "...", "poe_path": "...",
    "config_path": "...",            // absent => no --config => default path
    "heartbeat_interval": "3s", "session_ttl": "10m",
    "enable_mcp_attach": true,       // booleans: present-and-true emits flag
    "introduction": "..."
  },
  "systemd": { "env_path": "...", "restart": "...", "restart_sec": "..." },  // optional
  "launchd": { "plist": "...", "path_env": "...", "log_out": "...", "log_err": "..." },
  "config": { /* config.json contents, verbatim */ },
  "credentials": { "env_file": "~/.config/poe-acp/bot-two-fir/env" }  // reference only
}
```

Conventions:
- **Absent = no flag.** `"enable_mcp_attach": false` renders identically to
  the key being absent (tested).
- Spec paths use `~/`; rendered as `%h/` in units, `"$HOME"/` inside the
  plist `sh -c` wrapper. launchd `Standard*Path` must be absolute in the
  spec (launchd does not expand `$HOME` there).
- **Canonical rendering**: fixed flag order, `--double-dash` everywhere.
  fir-air's live plist uses single-dash flags — identical to Go's flag
  parser, so the first `--apply` will normalise dash style with zero
  behavioural change. Expect a "normalisation-only" unit diff on the first
  dry-run of every host.
- Introductions / agent.cmd containing `"` or `%` are rejected at render
  time (quote/systemd-specifier corruption guard).

## How to run

```bash
scripts/converge.sh two-fir                 # dry run — review the diffs
scripts/converge.sh two-fir --apply         # converge kopitwo
scripts/converge.sh --tot                   # advance dist.lock; review, commit, then converge all
scripts/converge.sh render fir-air plist    # inspect any rendered artefact
scripts/converge.sh two-fir --target-root /tmp/fake --apply   # local fake host (tests)
test/converge_render.sh                     # offline test suite
```

Converge order per host: poe-acp version (fetch release asset per
`platform` if stale) → fir version (`fir update`, then verify equals lock)
→ fir-exts rev (`fir packages list` path + `git rev-parse HEAD`) → rendered
`config.json` → rendered unit/plist → recycle → verify active + version.
macOS recycle: `bootout` + `bootstrap` when the plist changed (never
`kickstart -k`, which re-runs the cached job definition); `kickstart -k`
when only config/binary moved.

## Drift the specs revealed

- **fir version smear is the headline**: 0.90.1 / 0.93.0 / 0.94.0 / 0.95.0
  across four hosts. Lock targets 0.95.0; first `--apply` per host erases it.
- poe-acp uniform at 0.51.0 — no drift.
- fir-exts rev uniform on the three captured hosts (ko1 assumed same —
  converge verifies it concretely at apply time).

## Quirks flagged for later normalisation (transcribed as-is, NOT fixed)

1. **kopi-fir**: introduction says "default opus-4-8" but config default is
   `anthropic/claude-opus-5`. Stale prose.
2. **sea-fir**: same stale intro ("opus-4-8/low"); has `"poe_mcp": true` in
   config while the unit lacks `--enable-mcp-attach`. Functionally
   equivalent — the flag is a **deprecated alias** for the config knob
   (per `cmd/poe-acp/main.go`), so sea-fir is actually the *modern* form;
   candidates two-fir/kopi-fir could migrate flag→config later.
3. **fir-air**: poe_path `/poe-acp` (not `/fir-air`); no `--config` (default
   path); no `--session-ttl`; no mcp-attach; extra `agent.profile: "fir"`
   config key; extra package `github.com/anthropics/claude-plugins-official`
   recorded as `fir.extra_packages` (informational — converge does not
   manage or pin it).
4. **fir-air is brew-managed** (`/opt/homebrew/bin/poe-acp`): converge's
   binary fetch writes there directly, which will fight a later
   `brew upgrade`. Acceptable for now; normalising to `~/.local/bin` (or
   converging via brew) is a candidate.

## Caveats for the first real apply (operator, please read)

- **Captures were excerpts.** `render_unit` emits a canonical full unit and
  now includes `Type=notify` + `ExecReload=/bin/kill -HUP $MAINPID`
  unconditionally (required for the ≥0.36 supervisor handshake and graceful
  reload — correct for the locked 0.51.0). If the live units carry anything
  else uncaptured, the first dry-run diff will surface it — **read those
  diffs before `--apply`**. Same on airm1: the rendered plist carries only
  `PATH` in `EnvironmentVariables` (as captured); if the live plist also
  sets `HOME`, the diff will show it — add it to the spec then.
- ext rev pinning is best-effort enforcement: `fir` has no
  "install at rev X" — converge installs/updates then **verifies** the rev
  against the lock and warns loudly on mismatch (e.g. upstream moved past
  the lock). Re-run `--tot` in that case.
- The `fir packages list` column parse (substring match on SOURCE, `$NF` as
  path) is verified against local fir 0.95.0 output format; eyeball it on
  the first real host converge.

## Disagreements with the brief

None of substance. Two interpretations worth stating: (a) "byte-for-byte
where it matters" was read as *semantics* (flag values, config contents,
paths) not whitespace/dash-style — canonical rendering deliberately rewrites
units into one shape on first apply; (b) the brief's ~40–120 line estimate
for converge.sh was not achievable with plist rendering, diff-based
idempotence, backups, and per-supervisor recycle logic done properly — it
landed at ~470 lines, still plain bash + jq with zero cleverness.
