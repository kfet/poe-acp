// Package skills owns the poe-acp embedded skill bundle and re-exports
// the catalog primitives from acp-kit/skills. The wrapper lives here so
// the rest of the relay only depends on `internal/skills` regardless of
// where the implementation moves; it also pins the `"poe-acp"` extraction
// prefix used by LoadBuiltin so multiple relays sharing a host never
// collide.
package skills

import (
	"embed"

	kitskills "github.com/kfet/acp-kit/skills"
)

//go:embed all:bundle
var bundleFS embed.FS

// Skill is one entry in a fir-style skills catalog.
type Skill = kitskills.Skill

// LoadBuiltin walks the embedded poe-acp bundle and extracts builtin
// SKILL.md files to a per-content-hash dir under base — pass the relay's
// state dir, which the app owns and which outlives a process.
//
// Extraction also garbage collects: OTHER generations of this bundle,
// both under base and in the legacy $TMPDIR location, are removed.
// Without that the relay leaked one directory per released version
// forever — 11 of them on one live host.
func LoadBuiltin(base string) ([]Skill, error) {
	return kitskills.LoadBuiltinIn(base, bundleFS, "poe-acp")
}

// LoadDir walks <path>/*/SKILL.md and returns a fir-style catalog.
func LoadDir(path string) ([]Skill, error) { return kitskills.LoadDir(path) }

// Merge layers skill lists with last-wins-by-name semantics and drops
// names listed in disable.
func Merge(layers [][]Skill, disable []string) []Skill { return kitskills.Merge(layers, disable) }

// FormatCatalog renders a fir-style <available_skills> block ready for
// system-prompt injection.
func FormatCatalog(s []Skill) string { return kitskills.FormatCatalog(s) }
