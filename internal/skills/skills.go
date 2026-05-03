// Package skills embeds the relay's curated skill bundle, extracts it to a
// per-version dir at startup, and formats a fir-style <available_skills>
// catalog for injection into ACP agents.
//
// Design (see docs/skill-injection-plan.md):
//   - Bundle = a small set of Markdown SKILL.md files describing
//     deploy / update / release flows specific to running an agent under
//     poe-acp-relay. Embedded via go:embed.
//   - At startup the relay extracts the bundle to
//     $TMPDIR/poe-acp-relay-<hash>/skills/<name>/SKILL.md once. The hash
//     covers the embedded content so a new binary uses a new dir and
//     never reads stale skill text.
//   - The relay then formats a fir-style catalog (name + description +
//     absolute path) and hands it to the agent either via the
//     session.systemPrompt _meta capability or as an inline prefix on
//     the first prompt. The agent reads bodies on demand using whatever
//     read tool it has.
package skills

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed all:bundle
var bundleFS embed.FS

// Skill is one entry in the catalog.
type Skill struct {
	Name        string
	Description string
	// Path is the absolute on-disk path to SKILL.md after Extract.
	Path string
}

// Extract writes the embedded bundle into a per-content-hash dir under
// $TMPDIR and returns the parsed catalog. Idempotent: re-running with
// the same binary is a no-op (files already exist).
//
// Returned skills are sorted by name for deterministic catalog output.
func Extract() ([]Skill, error) {
	hash, err := bundleHash()
	if err != nil {
		return nil, fmt.Errorf("hash bundle: %w", err)
	}
	root := filepath.Join(os.TempDir(), "poe-acp-relay-"+hash[:12], "skills")

	var skills []Skill
	err = fs.WalkDir(bundleFS, "bundle", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || filepath.Base(p) != "SKILL.md" {
			return nil
		}
		body, rerr := bundleFS.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		// p is "bundle/<name>/SKILL.md"; strip the bundle/ prefix.
		rel := strings.TrimPrefix(p, "bundle/")
		dst := filepath.Join(root, rel)
		if mkerr := os.MkdirAll(filepath.Dir(dst), 0o755); mkerr != nil {
			return mkerr
		}
		// Best-effort: only write if missing or content differs. A simple
		// stat-and-compare is sufficient for our small bundle.
		if cur, rerr := os.ReadFile(dst); rerr != nil || string(cur) != string(body) {
			if werr := os.WriteFile(dst, body, 0o644); werr != nil {
				return werr
			}
		}
		name, desc := parseFrontmatter(body)
		if name == "" {
			// Fall back to the directory name so the catalog isn't broken
			// by a missing/malformed frontmatter.
			name = filepath.Base(filepath.Dir(rel))
		}
		skills = append(skills, Skill{Name: name, Description: desc, Path: dst})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, nil
}

// FormatCatalog renders the fir-style <available_skills> XML block plus
// a short preamble. The preamble tells the agent that bodies are read
// on demand via the agent's own read tool.
//
// Mirrors fir's pkg/resources/skills.go FormatSkillsForPrompt so any
// agent that already understands the fir block recognises ours.
func FormatCatalog(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The following skills provide specialized instructions for specific tasks.\n")
	b.WriteString("Use your read tool to load a skill's SKILL.md when the task matches its description.\n")
	b.WriteString("Skill body paths are absolute and stable for the lifetime of this session.\n\n")
	b.WriteString("<available_skills>\n")
	for _, s := range skills {
		b.WriteString("  <skill>\n")
		b.WriteString("    <name>")
		b.WriteString(escapeXML(s.Name))
		b.WriteString("</name>\n")
		b.WriteString("    <description>")
		b.WriteString(escapeXML(s.Description))
		b.WriteString("</description>\n")
		b.WriteString("    <location>")
		b.WriteString(escapeXML(s.Path))
		b.WriteString("</location>\n")
		b.WriteString("  </skill>\n")
	}
	b.WriteString("</available_skills>\n")
	return b.String()
}

// parseFrontmatter extracts name and description from a minimal YAML
// frontmatter block (--- ... ---) at the top of a SKILL.md. Only the
// two scalar fields we care about are read; everything else is ignored.
func parseFrontmatter(body []byte) (name, desc string) {
	s := string(body)
	if !strings.HasPrefix(s, "---\n") {
		return "", ""
	}
	end := strings.Index(s[4:], "\n---")
	if end < 0 {
		return "", ""
	}
	for _, line := range strings.Split(s[4:4+end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		switch k {
		case "name":
			name = v
		case "description":
			desc = v
		}
	}
	return name, desc
}

// bundleHash returns a stable hex digest over the embedded bundle's
// file paths and contents. Stable across rebuilds with identical
// content; new content → new hash → new tmp dir.
func bundleHash() (string, error) {
	h := sha256.New()
	err := fs.WalkDir(bundleFS, "bundle", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		b, rerr := bundleFS.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func escapeXML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}
