package install

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newLayout returns a Layout rooted in a fresh temp dir with a versions/
// dir containing a fake binary for each of versions.
func newLayout(t *testing.T, versions ...string) Layout {
	t.Helper()
	l := New(Config{Root: t.TempDir()})
	for _, v := range versions {
		install(t, l, v)
	}
	return l
}

func install(t *testing.T, l Layout, version string) {
	t.Helper()
	if err := l.Install(version, strings.NewReader("#!/bin/true\n"+version)); err != nil {
		t.Fatalf("Install %s: %v", version, err)
	}
}

func TestDefaultRoot(t *testing.T) {
	t.Setenv("POEACP_INSTALL_ROOT", "/tmp/explicit")
	if got := DefaultRoot(); got != "/tmp/explicit" {
		t.Errorf("explicit root = %q", got)
	}
	t.Setenv("POEACP_INSTALL_ROOT", "")
	t.Setenv("XDG_STATE_HOME", "/tmp/state")
	if got, want := DefaultRoot(), filepath.Join("/tmp/state", "poe-acp", "dist"); got != want {
		t.Errorf("xdg root = %q, want %q", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/tmp/home")
	if got, want := DefaultRoot(), filepath.Join("/tmp/home", ".local", "state", "poe-acp", "dist"); got != want {
		t.Errorf("home root = %q, want %q", got, want)
	}
	// No HOME at all (a bare launchd job): fall back to a temp path
	// rather than an empty, relative root.
	t.Setenv("HOME", "")
	if got := DefaultRoot(); !strings.HasSuffix(got, filepath.Join("poe-acp", "dist")) || !filepath.IsAbs(got) {
		t.Errorf("fallback root = %q", got)
	}
	// New(Config{}) adopts DefaultRoot.
	if got := New(Config{}).Root(); got != DefaultRoot() {
		t.Errorf("New(Config{}).Root() = %q, want %q", got, DefaultRoot())
	}
}

func TestPaths(t *testing.T) {
	l := New(Config{Root: "/r"})
	for _, tc := range []struct{ got, want string }{
		{l.Root(), "/r"},
		{l.CurrentPath(), "/r/current"},
		{l.LastGoodPath(), "/r/last-good"},
		{l.LogPath(), "/r/rollback.log"},
		{l.VersionPath("0.31.0"), "/r/versions/poe-acp-0.31.0"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestSwapCycle is the happy path the supervisor drives: install a
// version, activate it, confirm it, swap to the next, roll back.
func TestSwapCycle(t *testing.T) {
	l := newLayout(t, "0.30.0", "0.31.0")

	if err := l.Managed(); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("no current link: err = %v, want ErrUnmanaged", err)
	}
	if err := l.SwapCurrent("0.30.0"); err != nil {
		t.Fatalf("SwapCurrent: %v", err)
	}
	if err := l.SetLastGood("0.30.0"); err != nil {
		t.Fatalf("SetLastGood: %v", err)
	}
	if err := l.Managed(); err != nil {
		t.Fatalf("Managed: %v", err)
	}
	// The links must be relative, so the whole tree stays relocatable.
	target, err := os.Readlink(l.CurrentPath())
	if err != nil || target != filepath.Join("versions", "poe-acp-0.30.0") {
		t.Fatalf("current -> %q (%v), want a relative versions/ target", target, err)
	}
	// ...and the symlink must resolve to a runnable binary.
	fi, err := os.Stat(l.CurrentPath())
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("current is not an executable file: %v %v", fi, err)
	}

	if err := l.SwapCurrent("0.31.0"); err != nil {
		t.Fatalf("SwapCurrent: %v", err)
	}
	cur, err := l.CurrentVersion()
	if err != nil || cur != "0.31.0" {
		t.Fatalf("CurrentVersion = %q, %v", cur, err)
	}
	lg, err := l.LastGoodVersion()
	if err != nil || lg != "0.30.0" {
		t.Fatalf("LastGoodVersion = %q, %v", lg, err)
	}
	// Rollback: repoint current back at last-good.
	if err := l.SwapCurrent(lg); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if cur, _ := l.CurrentVersion(); cur != "0.30.0" {
		t.Fatalf("after rollback current = %q", cur)
	}
}

// TestInstallReplacesAndIsAtomic: re-installing a version overwrites it,
// and a failed copy never publishes a half-written binary.
func TestInstallReplacesAndIsAtomic(t *testing.T) {
	l := newLayout(t, "0.31.0")
	if err := l.Install("0.31.0", strings.NewReader("second")); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	b, err := os.ReadFile(l.VersionPath("0.31.0"))
	if err != nil || string(b) != "second" {
		t.Fatalf("reinstall content = %q (%v)", b, err)
	}

	if err := l.Install("0.32.0", errReader{}); err == nil {
		t.Fatal("Install with a failing reader: want error")
	}
	if _, err := os.Stat(l.VersionPath("0.32.0")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("half-written binary was published: %v", err)
	}
	// No staging leftovers.
	entries, err := os.ReadDir(filepath.Join(l.Root(), "versions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover staging file %s", e.Name())
		}
	}
}

func TestInstallErrors(t *testing.T) {
	// A root that is a FILE: neither the versions dir nor the staging
	// file can be created.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := New(Config{Root: f}).Install("0.1.0", strings.NewReader("x")); err == nil {
		t.Fatal("Install under a file root: want error")
	}

	// versions/ exists but is not writable: staging fails.
	l := newLayout(t)
	dir := filepath.Join(l.Root(), "versions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	chmod(t, dir, 0o555)
	if err := l.Install("0.1.0", strings.NewReader("x")); err == nil {
		t.Fatal("Install into a read-only versions dir: want error")
	}
	chmod(t, dir, 0o755)

	// The final rename fails when the destination name is a non-empty
	// directory.
	if err := os.MkdirAll(filepath.Join(dir, binPrefix+"0.1.0", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.Install("0.1.0", strings.NewReader("x")); err == nil {
		t.Fatal("Install over a directory: want error")
	}
}

// TestLegacyLayout: the four hosts today have a plain file, not a
// symlink. Every read must report ErrUnmanaged and nothing must panic.
func TestLegacyLayout(t *testing.T) {
	l := New(Config{Root: t.TempDir()})
	if err := os.WriteFile(l.CurrentPath(), []byte("a real binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func() (string, error){
		"CurrentVersion":  l.CurrentVersion,
		"LastGoodVersion": l.LastGoodVersion,
	} {
		if _, err := fn(); !errors.Is(err, ErrUnmanaged) {
			t.Errorf("%s: err = %v, want ErrUnmanaged", name, err)
		}
	}
	if err := l.Managed(); !errors.Is(err, ErrUnmanaged) {
		t.Errorf("Managed: err = %v, want ErrUnmanaged", err)
	}

	// A symlink that does not point into versions/ is equally unmanaged.
	other := filepath.Join(l.Root(), "elsewhere")
	if err := os.WriteFile(other, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(l.Root(), CurrentLink)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if err := l.Managed(); !errors.Is(err, ErrUnmanaged) {
		t.Errorf("foreign symlink: err = %v, want ErrUnmanaged", err)
	}

	// A DANGLING versioned symlink (version dir wiped) is unmanaged too:
	// the version parses, but there is nothing to exec.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(versionTarget("9.9.9"), link); err != nil {
		t.Fatal(err)
	}
	if v, err := l.CurrentVersion(); err != nil || v != "9.9.9" {
		t.Fatalf("CurrentVersion = %q, %v", v, err)
	}
	if err := l.Managed(); !errors.Is(err, ErrUnmanaged) {
		t.Errorf("dangling link: err = %v, want ErrUnmanaged", err)
	}
}

func TestPointErrors(t *testing.T) {
	l := newLayout(t, "0.31.0")

	if err := l.SwapCurrent("nope"); err == nil {
		t.Fatal("SwapCurrent to an uninstalled version: want error")
	}

	// Read-only root: the staging symlink cannot be created.
	chmod(t, l.Root(), 0o555)
	if err := l.SetLastGood("0.31.0"); err == nil {
		t.Fatal("SetLastGood into a read-only root: want error")
	}
	chmod(t, l.Root(), 0o755)

	// A leftover staging symlink from a crashed swap is replaced, not
	// fatal.
	if err := os.Symlink("stale", filepath.Join(l.Root(), "."+CurrentLink+".tmp")); err != nil {
		t.Fatal(err)
	}
	if err := l.SwapCurrent("0.31.0"); err != nil {
		t.Fatalf("SwapCurrent over a stale staging link: %v", err)
	}

	// The publishing rename fails when the link name is a non-empty dir.
	if err := os.Remove(l.LastGoodPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(l.LastGoodPath(), "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.SetLastGood("0.31.0"); err == nil {
		t.Fatal("SetLastGood over a directory: want error")
	}
	if _, err := os.Lstat(filepath.Join(l.Root(), "."+LastGoodLink+".tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("failed publish left its staging link behind: %v", err)
	}
}

func TestPins(t *testing.T) {
	l := newLayout(t)
	restore := now
	now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	defer func() { now = restore }()

	pins, err := l.Pinned()
	if err != nil || len(pins) != 0 {
		t.Fatalf("fresh layout: pins=%v err=%v", pins, err)
	}
	if err := l.Pin("0.31.0", "crash-loop"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if err := l.Pin("0.32.0", "crash-loop"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	// Re-pinning refreshes rather than duplicating.
	if err := l.Pin("0.31.0", "crash-loop again"); err != nil {
		t.Fatalf("re-Pin: %v", err)
	}
	pins, err = l.Pinned()
	if err != nil || len(pins) != 2 {
		t.Fatalf("pins=%v err=%v", pins, err)
	}
	if pins["0.31.0"].Reason != "crash-loop again" || pins["0.31.0"].At.IsZero() {
		t.Errorf("pin not refreshed: %+v", pins["0.31.0"])
	}
	ok, p, err := l.IsPinned("0.31.0")
	if err != nil || !ok || p.Version != "0.31.0" {
		t.Fatalf("IsPinned = %v, %+v, %v", ok, p, err)
	}
	if ok, _, err := l.IsPinned("0.30.0"); err != nil || ok {
		t.Fatalf("IsPinned(unpinned) = %v, %v", ok, err)
	}

	// Unpinning is how a newer build supersedes a condemned one.
	if err := l.Unpin("0.31.0"); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	if err := l.Unpin("0.31.0"); err != nil {
		t.Fatalf("Unpin of an unpinned version must be a no-op: %v", err)
	}
	if ok, _, _ := l.IsPinned("0.31.0"); ok {
		t.Error("still pinned after Unpin")
	}
}

func TestCrashRecord(t *testing.T) {
	l := New(Config{Root: t.TempDir()})
	ts, err := l.Crashes()
	if err != nil || len(ts) != 0 {
		t.Fatalf("fresh layout: %v %v", ts, err)
	}
	t0 := time.Unix(1_700_000_000, 0).UTC()
	// Written out of order; read back oldest first.
	if err := l.SetCrashes([]time.Time{t0.Add(time.Minute), t0}); err != nil {
		t.Fatalf("SetCrashes: %v", err)
	}
	ts, err = l.Crashes()
	if err != nil || len(ts) != 2 || !ts[0].Equal(t0) || !ts[1].Equal(t0.Add(time.Minute)) {
		t.Fatalf("Crashes = %v, %v", ts, err)
	}
	if err := l.SetCrashes(nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if ts, _ := l.Crashes(); len(ts) != 0 {
		t.Fatalf("after clear: %v", ts)
	}

	// Scoped layouts (one unit per bot, one binary per host) keep
	// separate counters but share the same root.
	a := New(Config{Root: l.Root(), Scope: "sea-fir"})
	b := New(Config{Root: l.Root(), Scope: "ko-claude"})
	if err := a.SetCrashes([]time.Time{t0}); err != nil {
		t.Fatal(err)
	}
	if ts, _ := b.Crashes(); len(ts) != 0 {
		t.Errorf("scope leak: %v", ts)
	}
	if ts, _ := a.Crashes(); len(ts) != 1 {
		t.Errorf("scoped record lost: %v", ts)
	}
	if _, err := os.Stat(filepath.Join(l.Root(), "crashes-sea-fir.json")); err != nil {
		t.Errorf("scoped crash file: %v", err)
	}
}

func TestStateFileErrors(t *testing.T) {
	l := New(Config{Root: t.TempDir()})

	// Corrupt state is reported, not silently dropped.
	if err := os.WriteFile(filepath.Join(l.Root(), pinnedFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Pinned(); err == nil {
		t.Error("corrupt pin file: want error")
	}
	if err := l.Pin("0.1.0", "x"); err == nil {
		t.Error("Pin over a corrupt file: want error")
	}
	if err := l.Unpin("0.1.0"); err == nil {
		t.Error("Unpin over a corrupt file: want error")
	}
	if _, _, err := l.IsPinned("0.1.0"); err == nil {
		t.Error("IsPinned over a corrupt file: want error")
	}

	// A state path that is a directory fails the READ.
	if err := os.MkdirAll(filepath.Join(l.Root(), crashesFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Crashes(); err == nil {
		t.Error("crash file as a directory: want error")
	}

	// A read-only root fails the WRITE (staging).
	ro := New(Config{Root: t.TempDir()})
	chmod(t, ro.Root(), 0o555)
	if err := ro.SetCrashes(nil); err == nil {
		t.Error("write into a read-only root: want error")
	}
	chmod(t, ro.Root(), 0o755)

	// A root that is a file fails the MkdirAll.
	f := filepath.Join(t.TempDir(), "file-root")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := New(Config{Root: f}).SetCrashes(nil); err == nil {
		t.Error("write under a file root: want error")
	}
	if err := New(Config{Root: f}).Logf("x"); err == nil {
		t.Error("log under a file root: want error")
	}

	// Publishing the state file fails when its name is a non-empty dir.
	l2 := New(Config{Root: t.TempDir()})
	if err := os.MkdirAll(filepath.Join(l2.Root(), crashesFile, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l2.SetCrashes(nil); err == nil {
		t.Error("publish over a directory: want error")
	}
}

func TestLogf(t *testing.T) {
	l := New(Config{Root: t.TempDir()})
	restore := now
	now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	defer func() { now = restore }()

	if err := l.Logf("revert version=%s -> %s", "0.31.0", "0.30.0"); err != nil {
		t.Fatalf("Logf: %v", err)
	}
	if err := l.Logf("healthy version=%s", "0.30.0"); err != nil {
		t.Fatalf("Logf: %v", err)
	}
	b, err := os.ReadFile(l.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("log has %d lines: %q", len(lines), b)
	}
	want := "2023-11-14T22:13:20Z poe-acp-rollback revert version=0.31.0 -> 0.30.0"
	if lines[0] != want {
		t.Errorf("line 0 = %q, want %q", lines[0], want)
	}

	// The log path being a directory is reported, never fatal.
	l2 := New(Config{Root: t.TempDir()})
	if err := os.MkdirAll(l2.LogPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l2.Logf("x"); err == nil {
		t.Error("log path as a directory: want error")
	}
}

// errReader fails every Read, standing in for a truncated download.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

var _ io.Reader = errReader{}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
