// Package dist owns poe-acp's binary-distribution identity: the single
// distkit.Config that both the `poe-acp update` subcommand and the
// generated root install.sh derive from.
//
// Keeping it in one place means the release asset naming
// ("poe-acp-<os>-<arch>", raw binaries — goreleaser archives
// formats=[binary], with GOARCH=arm published as "armv6") has exactly one
// definition, and a drift test proves install.sh.json still agrees with it.
//
// Everything else — the GitHub API download with token discovery, sha256
// verification against checksums.txt, the ETXTBSY-safe atomic swap, the
// Homebrew keg upgrade, and the refusal on an install we do not own —
// lives in github.com/kfet/distkit.
package dist

import "github.com/kfet/distkit"

// Repo is the GitHub "owner/name" releases are taken from by default.
const Repo = "kfet/poe-acp"

// Binary is the installed executable name, and the asset stem.
const Binary = "poe-acp"

// RestartHint is printed after a successful binary swap. A swapped binary
// is inert until the process re-execs, and for a binary-only change the
// GRACEFUL recycle is the right one: `reload` makes the running supervisor
// fork a new worker and drain the old, where a plain restart drops every
// in-flight conversation.
const RestartHint = "systemctl --user reload poe-acp-<bot>"

// ArmSuffix is the asset token for the 32-bit ARM build, where GOARCH is
// just "arm" but goreleaser appends the GOARM level.
const ArmSuffix = "armv6"

// Config returns the distkit configuration for this build. version is the
// compiled-in version string (main.version, set via -ldflags).
func Config(version string) distkit.Config {
	return distkit.Config{
		Repo:      Repo,
		Binary:    Binary,
		AssetStem: Binary,
		// Stated rather than defaulted, so the goreleaser contract is
		// visible here and a drift test can assert it:
		// archives.name_template is
		// "poe-acp-{{.Os}}-{{.Arch}}{{if .Arm}}v{{.Arm}}{{end}}", i.e.
		// raw binaries with the GOARM=6 build published as "armv6".
		AssetTemplate: distkit.DefaultAssetTemplate,
		ArmSuffix:     ArmSuffix,
		Version:       version,
		RestartHint:   RestartHint,
	}
}
