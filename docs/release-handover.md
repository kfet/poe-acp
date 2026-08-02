# Handover: poe-acp

## Current release state

- Branch: `main`
- Latest release commit: `2ae400a` (`release: v0.48.1`)
- Release tag: `v0.48.1` (pushed)
- VERSION: `0.48.1`

## Release workflow status (completed and published)

- `make all` passed (vet, test-race-cover, native build, 5 cross-builds, check-licenses).
- `CHANGELOG.md`: `## [0.48.1] - 2026-08-02` released section, fresh `## [Unreleased]` on top.
- `VERSION` = `0.48.1`; committed as `release: v0.48.1`; annotated tag `v0.48.1`.
- `make install` + `poe-acp --version` → `0.48.1`.
- `make publish` pushed `main` + `v0.48.1`.
- GitHub Actions for the release commit: `release` and `ci` both **success**
  (release run <https://github.com/kfet/poe-acp/actions/runs/30747830254>).
- Release published with binaries, `checksums.txt`, `LICENSE`,
  `THIRD_PARTY_NOTICES.md`: <https://github.com/kfet/poe-acp/releases/tag/v0.48.1>
- Homebrew tap `kfet/homebrew-ai` `Formula/poe-acp.rb` updated to `0.48.1`.

## Notes

- The canonical release procedure lives in `.fir/skills/release/SKILL.md`.
- Do not push or publish a new version until the user explicitly confirms.
