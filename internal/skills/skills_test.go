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
	if len(list) < 3 {
		t.Fatalf("expected ≥3 skills, got %d: %+v", len(list), list)
	}

	// Bundle ships deploy/update/release; verify they're all present
	// and have non-empty descriptions + absolute paths.
	want := map[string]bool{"deploy": false, "update": false, "release": false}
	for _, s := range list {
		if s.Name == "" || s.Description == "" || s.Path == "" {
			t.Errorf("malformed skill: %+v", s)
		}
		if s.Path[0] != '/' {
			t.Errorf("path not absolute: %s", s.Path)
		}
		want[s.Name] = true
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
		"<name>release</name>",
		"Use your read tool",
	} {
		if !strings.Contains(cat, sub) {
			t.Errorf("catalog missing %q. Got:\n%s", sub, cat)
		}
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
	body := []byte("---\nname: foo\ndescription: bar baz\nextra: ignored\n---\n\n# Body\n")
	name, desc := parseFrontmatter(body)
	if name != "foo" || desc != "bar baz" {
		t.Errorf("got name=%q desc=%q", name, desc)
	}
}

func TestFormatCatalogEmpty(t *testing.T) {
	if FormatCatalog(nil) != "" {
		t.Error("expected empty string for empty catalog")
	}
}
