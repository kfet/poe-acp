package skills

import (
	"strings"
	"testing"
)

func TestExtractAndFormat(t *testing.T) {
	list, err := Extract()
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

func TestExtractIdempotent(t *testing.T) {
	a, err := Extract()
	if err != nil {
		t.Fatalf("Extract #1: %v", err)
	}
	b, err := Extract()
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
