package skills

import "testing"

func TestWrappersDelegate(t *testing.T) {
	got, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	if len(got) == 0 {
		t.Skip("bundle has no builtin entries; nothing to assert")
	}
	if dir, err := LoadDir(""); err != nil || dir != nil {
		t.Fatalf("LoadDir empty: %v %v", dir, err)
	}
	merged := Merge([][]Skill{got, nil}, nil)
	if len(merged) != len(got) {
		t.Fatalf("Merge len = %d want %d", len(merged), len(got))
	}
	if FormatCatalog(merged) == "" {
		t.Fatal("FormatCatalog empty")
	}
}
