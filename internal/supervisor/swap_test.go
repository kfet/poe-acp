//go:build unix

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageBin lays out a live binary (and optionally a staged one) in a
// temp dir and returns the live path.
func stageBin(t *testing.T, live, staged string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "poe-acp")
	if live != "" {
		if err := os.WriteFile(path, []byte(live), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if staged != "" {
		if err := os.WriteFile(path+StagedSuffix, []byte(staged), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// noSidecars asserts the swap window is closed: exactly one file, the
// binary itself, is left in the directory.
func noSidecars(t *testing.T, s *Swapper) {
	t.Helper()
	for _, p := range []string{s.Staged(), s.Prev()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still on disk (err=%v)", p, err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(s.Path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(s.Path) {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want just %q", names, filepath.Base(s.Path))
	}
}

func TestSwapperCleanupIsIdempotent(t *testing.T) {
	s := &Swapper{Path: stageBin(t, "live", "staged")}
	if err := os.WriteFile(s.Prev(), []byte("prev"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.Cleanup()
	noSidecars(t, s)
	s.Cleanup() // second call must not care that both are already gone
	noSidecars(t, s)
}

func TestSwapperInstallAndRevert(t *testing.T) {
	path := stageBin(t, "old", "new")
	s := &Swapper{Path: path}
	if got, want := s.Staged(), path+".new"; got != want {
		t.Fatalf("Staged()=%q want %q", got, want)
	}
	if err := s.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := read(t, path); got != "new" {
		t.Fatalf("after install binary=%q want %q", got, "new")
	}
	if got := read(t, s.Prev()); got != "old" {
		t.Fatalf("after install prev=%q want %q", got, "old")
	}
	if err := s.Revert(); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := read(t, path); got != "old" {
		t.Fatalf("after revert binary=%q want %q", got, "old")
	}
}

func TestSwapperInstallErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		// fail reports whether the nth rename call fails.
		fail    func(n int) bool
		wantErr string
	}{
		{"keep previous fails", func(n int) bool { return n == 1 }, "keep previous binary"},
		{"install fails, restore ok", func(n int) bool { return n == 2 }, "install staged binary: boom"},
		{"install fails, restore fails", func(n int) bool { return n >= 2 }, "restore failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := 0
			s := &Swapper{Path: "/nowhere/poe-acp", Rename: func(_, _ string) error {
				n++
				if tc.fail(n) {
					return boom
				}
				return nil
			}}
			err := s.Install()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Install err=%v want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestSwapperRevertError(t *testing.T) {
	s := &Swapper{Path: filepath.Join(t.TempDir(), "poe-acp")}
	err := s.Revert()
	if err == nil || !strings.Contains(err.Error(), "revert to") {
		t.Fatalf("Revert err=%v want revert-to error", err)
	}
}

// TestSwapAndVerify drives the whole state machine: whichever binary can
// bring a worker to READY is the one left live, and a swap that cannot be
// completed is reported rather than silently left half-done.
func TestSwapAndVerify(t *testing.T) {
	boom := errors.New("not ready")
	tests := []struct {
		name       string
		live       string // "" => no live binary (Install must fail)
		bring      []error
		wantRevert bool
		wantBin    string
		wantErr    string
		wantLogged string
	}{
		{
			name: "new binary comes ready", live: "old", bring: []error{nil},
			wantBin: "new",
		},
		{
			name: "new binary never ready, revert serves old", live: "old", bring: []error{boom, nil},
			wantRevert: true, wantBin: "old", wantLogged: "REVERTED to the previous binary",
		},
		{
			name: "neither comes ready", live: "old", bring: []error{boom, boom},
			wantRevert: true, wantBin: "old", wantErr: "still not ready",
		},
		{
			name: "install impossible", live: "", bring: nil,
			wantErr: "keep previous binary",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := stageBin(t, tc.live, "new")
			s := &Swapper{Path: path}
			calls := 0
			bring := func() error {
				defer func() { calls++ }()
				if calls < len(tc.bring) {
					return tc.bring[calls]
				}
				return fmt.Errorf("unexpected bring call %d", calls)
			}
			var logged strings.Builder
			reverted, err := SwapAndVerify(s, bring, func(f string, a ...any) {
				fmt.Fprintf(&logged, f+"\n", a...)
			})
			if reverted != tc.wantRevert {
				t.Fatalf("reverted=%v want %v (err=%v)", reverted, tc.wantRevert, err)
			}
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err=%v want containing %q", err, tc.wantErr)
			}
			if tc.wantBin != "" {
				if got := read(t, path); got != tc.wantBin {
					t.Fatalf("live binary=%q want %q", got, tc.wantBin)
				}
			}
			// However it ended, the swap window is closed: no .new, no
			// .prev, just the binary.
			if tc.live != "" {
				noSidecars(t, s)
			}
			if tc.wantLogged != "" && !strings.Contains(logged.String(), tc.wantLogged) {
				t.Fatalf("log=%q want containing %q", logged.String(), tc.wantLogged)
			}
			if calls != len(tc.bring) {
				t.Fatalf("bring called %d times, want %d", calls, len(tc.bring))
			}
		})
	}
}

// TestSwapAndVerifyRevertFails covers the case where the rollback rename
// itself cannot be done: the caller is told, loudly, rather than left
// believing the old binary is back.
func TestSwapAndVerifyRevertFails(t *testing.T) {
	path := stageBin(t, "old", "new")
	n := 0
	s := &Swapper{Path: path, Rename: func(oldpath, newpath string) error {
		n++
		if n > 2 { // the revert
			return errors.New("read-only")
		}
		return os.Rename(oldpath, newpath)
	}}
	reverted, err := SwapAndVerify(s, func() error { return errors.New("not ready") }, func(string, ...any) {})
	if !reverted || err == nil || !strings.Contains(err.Error(), "revert to") {
		t.Fatalf("reverted=%v err=%v want a failed revert", reverted, err)
	}
	// Even when the rollback rename fails, nothing is left staged: the
	// binary that could not be moved back is unlinked rather than parked
	// next to the live one forever.
	noSidecars(t, s)
}

// TestSwapAndVerifyPanicCleansUp: a bring that panics still leaves the
// directory with exactly one binary — the cleanup is deferred, not tied
// to the return paths.
func TestSwapAndVerifyPanicCleansUp(t *testing.T) {
	s := &Swapper{Path: stageBin(t, "old", "new")}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("want the panic to propagate")
			}
		}()
		_, _ = SwapAndVerify(s, func() error { panic("worker exploded") }, func(string, ...any) {})
	}()
	noSidecars(t, s)
}
