package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadBuiltinAndFormat(t *testing.T) {
	list, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(list) < 2 {
		t.Fatalf("expected ≥2 skills, got %d: %+v", len(list), list)
	}

	// Bundle ships deploy/update as builtin; verify they're present
	// and have non-empty descriptions + absolute paths. The release
	// skill is in the bundle tree but NOT marked builtin, so it must
	// NOT appear in the catalog.
	want := map[string]bool{"deploy": false, "update": false}
	for _, s := range list {
		if s.Name == "release" {
			t.Errorf("release skill must not be in catalog (not builtin): %+v", s)
		}
		if s.Name == "" || s.Description == "" || s.Path == "" {
			t.Errorf("malformed skill: %+v", s)
		}
		if s.Path[0] != '/' {
			t.Errorf("path not absolute: %s", s.Path)
		}
		if _, ok := want[s.Name]; ok {
			want[s.Name] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("skill %q missing from catalog", k)
		}
	}

	cat := FormatCatalog(list)
	for _, sub := range []string{
		"<available_skills>",
		"</available_skills>",
		"<name>deploy</name>",
		"<name>update</name>",
		"Use your read tool",
	} {
		if !strings.Contains(cat, sub) {
			t.Errorf("catalog missing %q. Got:\n%s", sub, cat)
		}
	}
	if strings.Contains(cat, "<name>release</name>") {
		t.Errorf("catalog must not contain release skill. Got:\n%s", cat)
	}
}

func TestLoadBuiltinIdempotent(t *testing.T) {
	a, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("Extract #1: %v", err)
	}
	b, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("Extract #2: %v", err)
	}
	if len(a) != len(b) {
		t.Fatalf("len mismatch: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Path != b[i].Path {
			t.Errorf("path[%d] differs: %s vs %s", i, a[i].Path, b[i].Path)
		}
	}
}

func TestParseFrontmatter(t *testing.T) {
	body := []byte("---\nbuiltin: true\nname: foo\ndescription: bar baz\nextra: ignored\n---\n\n# Body\n")
	name, desc, builtin := parseFrontmatter(body)
	if name != "foo" || desc != "bar baz" || !builtin {
		t.Errorf("got name=%q desc=%q builtin=%v", name, desc, builtin)
	}

	body2 := []byte("---\nname: proj\ndescription: project only\n---\n\nbody\n")
	_, _, b2 := parseFrontmatter(body2)
	if b2 {
		t.Errorf("expected builtin=false when not declared, got true")
	}
}

func TestFormatCatalogEmpty(t *testing.T) {
	if FormatCatalog(nil) != "" {
		t.Error("expected empty string for empty catalog")
	}
}

func TestLoadDirMissing(t *testing.T) {
	got, err := LoadDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
	if g, err := LoadDir(""); err != nil || g != nil {
		t.Errorf("empty path: got %+v err=%v", g, err)
	}
}

func TestLoadDirHappy(t *testing.T) {
	root := t.TempDir()
	mk := func(name, body string) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("alpha", "---\nname: alpha\ndescription: first one\n---\n\nbody\n")
	mk("beta", "---\nname: beta\ndescription: second one\n---\n\nbody\n")
	// Missing description → skipped.
	mk("gamma", "---\nname: gamma\n---\n\nbody\n")
	// Malformed frontmatter (no closing ---) → skipped.
	mk("delta", "---\nname: delta\ndescription: never closes\n")
	// Bare directory without SKILL.md → silently skipped.
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := LoadDir(root)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 skills, got %d: %+v", len(got), got)
	}
	if got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Errorf("unexpected order: %+v", got)
	}
	for _, s := range got {
		if !filepath.IsAbs(s.Path) {
			t.Errorf("path not absolute: %s", s.Path)
		}
	}
}

func TestMerge(t *testing.T) {
	builtin := []Skill{
		{Name: "deploy", Description: "builtin deploy", Path: "/b/deploy"},
		{Name: "update", Description: "builtin update", Path: "/b/update"},
	}
	host := []Skill{
		{Name: "deploy", Description: "host deploy override", Path: "/h/deploy"},
		{Name: "custom", Description: "host only", Path: "/h/custom"},
	}
	got := Merge([][]Skill{builtin, host}, nil)
	if len(got) != 3 {
		t.Fatalf("want 3, got %d: %+v", len(got), got)
	}
	// sorted: custom, deploy, update
	if got[0].Name != "custom" || got[1].Name != "deploy" || got[2].Name != "update" {
		t.Errorf("sort order: %+v", got)
	}
	if got[1].Path != "/h/deploy" || got[1].Description != "host deploy override" {
		t.Errorf("host did not override: %+v", got[1])
	}

	// Disable drops by name regardless of layer.
	got2 := Merge([][]Skill{builtin, host}, []string{"update", "custom"})
	if len(got2) != 1 || got2[0].Name != "deploy" {
		t.Errorf("disable: got %+v", got2)
	}
}
