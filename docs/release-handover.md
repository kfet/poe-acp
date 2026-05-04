# Handover: poe-acp

## Current release state

- Standalone repo path: `/Users/kfet/dev/ai/poe-acp`
- Branch: `main`
- Latest local release commit: `da79a39c29948fb088a6c1eaec920b35347eff7f`
- Release tag: `v0.2.0`
- VERSION: `0.2.0`

## Release workflow status (completed)

### Done
- `make all` executed and passed (vet, test-race, native build, 5 cross-builds, check-licenses).
- `CHANGELOG.md` updated with:
  - fresh `## [Unreleased]` section at top
  - release section `## [0.2.0] - 2026-04-22`
- `VERSION` updated to `0.2.0`.
- Changes committed:
  - `git commit -m "release: v0.2.0"`
- Annotated tag created:
  - `git tag -a v0.2.0 -m "release: v0.2.0"`
- Local install + verification:
  - `make install`
  - `poe-acp --version` prints `0.2.0`
- Working tree clean after these steps.

### Remaining (requires explicit user confirmation)
- Publish release artifacts and Homebrew tap update:
  - `make publish` (pushes main + tag, triggers GitHub release workflow)
  - poll release runs via `gh run list` by `headSha`
  - verify tap update via `gh api repos/kfet/homebrew-fir/contents/Formula/poe-acp.rb`

## Notes
- Do not push or publish until user explicitly confirms.
