package skills

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func swap[T any](dst *T, v T) func() {
	old := *dst
	*dst = v
	return func() { *dst = old }
}

func TestParseFrontmatter_NoLeader(t *testing.T) {
	if n, d, b := parseFrontmatter([]byte("# no front")); n != "" || d != "" || b {
		t.Fatalf("expected zero, got %q %q %v", n, d, b)
	}
}

func TestParseFrontmatter_NoCloser(t *testing.T) {
	if n, d, b := parseFrontmatter([]byte("---\nname: x\nbody never closes\n")); n != "" || d != "" || b {
		t.Fatalf("expected zero, got %q %q %v", n, d, b)
	}
}

func TestParseFrontmatter_QuotedAndIgnoredKeys(t *testing.T) {
	body := []byte("---\nname: \"q\"\ndescription: 'desc'\nbuiltin: false\nweird: true\nno-colon\n---\n")
	n, d, b := parseFrontmatter(body)
	if n != "q" || d != "desc" || b {
		t.Fatalf("got %q %q %v", n, d, b)
	}
}

func TestLoadBuiltin_BundleHashError(t *testing.T) {
	// bundleHashFS exposes the real walk paths against a custom FS.
	if _, err := bundleHashFS(fstest.MapFS{}); err == nil {
		t.Fatal("expected walk error from missing bundle dir")
	}
	if _, err := bundleHashFS(brokenFS{}); err == nil {
		t.Fatal("expected hash error")
	}
	// File-with-ReadFile-error scenario: covers the rerr branch.
	if _, err := bundleHashFS(rfErrFS{}); err == nil {
		t.Fatal("expected ReadFile error")
	}

	// Force LoadBuiltin to fail at hash by swapping bundleHash.
	defer swap(&bundleHashFn, func() (string, error) { return "", errors.New("hash-fail") })()
	if _, err := LoadBuiltin(); err == nil || !strings.Contains(err.Error(), "hash-fail") {
		t.Fatalf("LoadBuiltin: expected hash error, got %v", err)
	}
}

// rfErrFS: bundle dir lists one regular file whose Open errors.
type rfErrFS struct{}

func (rfErrFS) Open(name string) (fs.File, error) {
	switch name {
	case "bundle":
		return &fileListDir{entries: []fs.DirEntry{fileEntry{name: "x.txt"}}}, nil
	}
	return nil, errors.New("file-open-err")
}

type fileListDir struct {
	entries []fs.DirEntry
	read    bool
}

func (f *fileListDir) Stat() (fs.FileInfo, error) { return brokenInfo{name: "bundle", dir: true}, nil }
func (*fileListDir) Read([]byte) (int, error)     { return 0, errors.New("nope") }
func (*fileListDir) Close() error                 { return nil }
func (f *fileListDir) ReadDir(int) ([]fs.DirEntry, error) {
	if f.read {
		return nil, nil
	}
	f.read = true
	return f.entries, nil
}

type fileEntry struct{ name string }

func (f fileEntry) Name() string    { return f.name }
func (fileEntry) IsDir() bool       { return false }
func (fileEntry) Type() fs.FileMode { return 0 }
func (f fileEntry) Info() (fs.FileInfo, error) {
	return brokenInfo{name: f.name, dir: false}, nil
}

// readFileErrFS lays out bundle/x/SKILL.md as a non-dir entry whose
// Open fails — exercises the rerr-from-ReadFile branch in LoadBuiltin's
// walk (where the d.IsDir()==false path is taken first).
type readFileErrFS struct{}

func (readFileErrFS) Open(name string) (fs.File, error) {
	switch name {
	case "bundle":
		return &fileListDir{entries: []fs.DirEntry{dirEntry{name: "x"}}}, nil
	case "bundle/x":
		return &fileListDir{entries: []fs.DirEntry{fileEntry{name: "SKILL.md"}}}, nil
	}
	return nil, errors.New("readfile-fail")
}

type dirEntry struct{ name string }

func (d dirEntry) Name() string    { return d.name }
func (dirEntry) IsDir() bool       { return true }
func (dirEntry) Type() fs.FileMode { return fs.ModeDir }
func (d dirEntry) Info() (fs.FileInfo, error) {
	return brokenInfo{name: d.name, dir: true}, nil
}

func TestLoadBuiltin_WalkReadFileError(t *testing.T) {
	defer swap[fs.FS](&bundleSrc, readFileErrFS{})()
	if _, err := LoadBuiltin(); err == nil {
		t.Fatal("expected ReadFile error")
	}
}

// brokenFS returns an fs.File whose ReadDir succeeds once with an entry
// whose subsequent Open errors — driving fs.WalkDir to surface that
// error to the walk callback.
type brokenFS struct{}

func (brokenFS) Open(name string) (fs.File, error) {
	if name == "bundle" {
		return &brokenDir{}, nil
	}
	return nil, errors.New("boom")
}

type brokenDir struct{ done bool }

func (b *brokenDir) Stat() (fs.FileInfo, error) { return brokenInfo{name: "bundle", dir: true}, nil }
func (*brokenDir) Read([]byte) (int, error)     { return 0, errors.New("not file") }
func (*brokenDir) Close() error                 { return nil }
func (b *brokenDir) ReadDir(int) ([]fs.DirEntry, error) {
	if b.done {
		return nil, nil
	}
	b.done = true
	return []fs.DirEntry{brokenEntry{name: "child"}}, nil
}

type brokenInfo struct {
	name string
	dir  bool
}

func (b brokenInfo) Name() string { return b.name }
func (brokenInfo) Size() int64    { return 0 }
func (b brokenInfo) Mode() fs.FileMode {
	if b.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (brokenInfo) ModTime() time.Time { return time.Time{} }
func (b brokenInfo) IsDir() bool      { return b.dir }
func (brokenInfo) Sys() any           { return nil }

type brokenEntry struct{ name string }

func (b brokenEntry) Name() string    { return b.name }
func (brokenEntry) IsDir() bool       { return true } // descend
func (brokenEntry) Type() fs.FileMode { return fs.ModeDir }
func (b brokenEntry) Info() (fs.FileInfo, error) {
	return brokenInfo{name: b.name, dir: true}, nil
}

func TestLoadBuiltin_WalkError(t *testing.T) {
	defer swap[fs.FS](&bundleSrc, brokenFS{})()
	_, err := LoadBuiltin()
	if err == nil {
		t.Fatal("expected walk error")
	}
	t.Logf("got err: %v", err)
}

func TestLoadBuiltin_FSErrorPaths(t *testing.T) {
	mfs := fstest.MapFS{
		"bundle/x/SKILL.md":    &fstest.MapFile{Data: []byte("---\nname: x\ndescription: d\nbuiltin: true\n---\n")},
		"bundle/proj/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: p\ndescription: d\n---\n")}, // not builtin
		"bundle/none/SKILL.md": &fstest.MapFile{Data: []byte("---\nbuiltin: true\n---\n")},           // empty name → fallback
		"bundle/other/foo.txt": &fstest.MapFile{Data: []byte("nope")},                                // not a SKILL.md
	}
	defer swap[fs.FS](&bundleSrc, mfs)()

	// MkdirAll fails first.
	restore := swap(&osMkdirAll, func(string, os.FileMode) error { return errors.New("mkdir-fail") })
	if _, err := LoadBuiltin(); err == nil || !strings.Contains(err.Error(), "mkdir-fail") {
		t.Fatalf("mkdir: got %v", err)
	}
	restore()

	// WriteFile fails.
	restoreR := swap(&osReadFile, func(string) ([]byte, error) { return nil, errors.New("nope") })
	restoreW := swap(&osWriteFile, func(string, []byte, os.FileMode) error { return errors.New("write-fail") })
	if _, err := LoadBuiltin(); err == nil || !strings.Contains(err.Error(), "write-fail") {
		t.Fatalf("write: got %v", err)
	}
	restoreW()
	restoreR()

	// Happy path with stub FS.
	got, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	if !contains(got, "x") || !contains(got, "none") {
		t.Errorf("names = %+v", got)
	}
	// Run again — exercises the "content matches → skip write" branch.
	if _, err := LoadBuiltin(); err != nil {
		t.Fatalf("idempotent: %v", err)
	}
}

func contains(skills []Skill, name string) bool {
	for _, s := range skills {
		if s.Name == name {
			return true
		}
	}
	return false
}

func TestLoadDir_ReadDirError(t *testing.T) {
	// Path under a non-directory — ReadDir returns non-NotExist error.
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(filepath.Join(f, "child")); err == nil {
		t.Fatal("expected ReadDir error")
	}
}

func TestLoadDir_AbsError(t *testing.T) {
	defer swap(&filepathAbs, func(string) (string, error) { return "", errors.New("abs-fail") })()
	if _, err := LoadDir("/x"); err == nil {
		t.Fatal("expected abs error")
	}
}

func TestLoadDir_ReadDirAbsErrInjected(t *testing.T) {
	defer swap(&osReadDir, func(string) ([]os.DirEntry, error) {
		return nil, errors.New("readdir-boom")
	})()
	if _, err := LoadDir("/whatever"); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadDir_ReadFilePermError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "skill")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer swap(&osReadFile, func(s string) ([]byte, error) {
		if strings.HasSuffix(s, "SKILL.md") {
			return nil, errors.New("perm denied")
		}
		return os.ReadFile(s)
	})()
	got, err := LoadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0, got %+v", got)
	}
}

func TestLoadDir_FallbackNameAndNonDirEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "loose.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "thing")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("---\ndescription: stuff\n---\n")
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "thing" {
		t.Fatalf("got %+v", got)
	}
}

func TestEscapeXML(t *testing.T) {
	if escapeXML("a&b<c>d") != "a&amp;b&lt;c&gt;d" {
		t.Fail()
	}
}
