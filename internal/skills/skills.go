// Package skills embeds the poe-acp curated bundle and delegates loading,
// merging, and catalog formatting to acp-kit/skills. Bundle entries marked
// `builtin: true` are extracted to a per-content-hash tmp dir and surfaced
// to ACP agents at runtime.
package skills

import (
	"embed"

	kitskills "github.com/kfet/acp-kit/skills"
)

//go:embed all:bundle
var bundleFS embed.FS

// Skill is one entry in a fir-style skills catalog.
type Skill = kitskills.Skill

// LoadBuiltin walks the embedded poe-acp bundle and extracts builtin skills.
func LoadBuiltin() ([]Skill, error) { return kitskills.LoadBuiltin(bundleFS, "poe-acp") }

// LoadDir walks <path>/*/SKILL.md and returns a fir-style catalog.
func LoadDir(path string) ([]Skill, error) { return kitskills.LoadDir(path) }

// Merge layers skill lists with last-wins-by-name semantics and drops names
// listed in disable.
func Merge(layers [][]Skill, disable []string) []Skill { return kitskills.Merge(layers, disable) }

// FormatCatalog renders a fir-style <available_skills> block.
func FormatCatalog(s []Skill) string { return kitskills.FormatCatalog(s) }
