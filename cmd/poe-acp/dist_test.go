package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kfet/poe-acp/internal/install"
)

// distRun drives runDist against root and returns (code, stdout, stderr).
func distRun(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runDist(append([]string{"-root", root}, args...), &out, &errOut)
	return code, out.String(), errOut.String()
}

// TestDistLifecycle walks the operator path: an unmanaged host, staging
// a version, activating it, and the pin that a crash-loop rollback
// leaves behind.
func TestDistLifecycle(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(t.TempDir(), "poe-acp")
	if err := os.WriteFile(bin, []byte("#!/bin/true"), 0o755); err != nil {
		t.Fatal(err)
	}

	code, out, _ := distRun(t, root, "status")
	if code != 0 || !strings.Contains(out, "UNMANAGED") {
		t.Fatalf("status on a fresh root: code=%d out=%q", code, out)
	}

	if code, _, errOut := distRun(t, root, "-version", "0.31.0", "install", bin); code != 0 {
		t.Fatalf("install: code=%d err=%q", code, errOut)
	}
	if code, _, errOut := distRun(t, root, "activate", "0.31.0"); code != 0 {
		t.Fatalf("activate: code=%d err=%q", code, errOut)
	}
	code, out, _ = distRun(t, root, "status")
	if code != 0 || !strings.Contains(out, "current:   0.31.0") {
		t.Fatalf("status: code=%d out=%q", code, out)
	}
	if !strings.Contains(out, "last-good: (none yet") {
		t.Errorf("status should say last-good is unset: %q", out)
	}

	// Simulate what a crash-loop revert leaves behind.
	l := install.New(install.Config{Root: root})
	if err := l.SetLastGood("0.31.0"); err != nil {
		t.Fatal(err)
	}
	if err := l.Pin("0.32.0", "crash-loop: 3 worker crashes within 1m0s"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetCrashes(nil); err != nil {
		t.Fatal(err)
	}
	code, out, _ = distRun(t, root, "status")
	if code != 0 || !strings.Contains(out, "pinned:    0.32.0 (crash-loop") || !strings.Contains(out, "last-good: 0.31.0") {
		t.Fatalf("status: code=%d out=%q", code, out)
	}

	// A pinned version is refused, forced through with -force, and
	// released by unpin.
	if code, _, errOut := distRun(t, root, "-version", "0.32.0", "install", bin); code != 0 {
		t.Fatalf("install 0.32.0: %q", errOut)
	}
	code, _, errOut := distRun(t, root, "activate", "0.32.0")
	if code != 1 || !strings.Contains(errOut, "is pinned") {
		t.Fatalf("activate pinned: code=%d err=%q", code, errOut)
	}
	if code, _, errOut := distRun(t, root, "-force", "activate", "0.32.0"); code != 0 {
		t.Fatalf("activate -force: code=%d err=%q", code, errOut)
	}
	if code, _, errOut := distRun(t, root, "unpin", "0.32.0"); code != 0 {
		t.Fatalf("unpin: code=%d err=%q", code, errOut)
	}
	if ok, _, _ := l.IsPinned("0.32.0"); ok {
		t.Error("still pinned after unpin")
	}
	if code, _, errOut := distRun(t, root, "activate", "0.32.0"); code != 0 {
		t.Fatalf("activate after unpin: code=%d err=%q", code, errOut)
	}
	// Every mutation is recorded in the durable log.
	b, err := os.ReadFile(l.LogPath())
	if err != nil || !strings.Contains(string(b), "activate version=0.32.0") {
		t.Fatalf("rollback log = %q (%v)", b, err)
	}
}

// TestDistInstallStageFailure: a versions/ dir that cannot be written is
// reported, not swallowed.
func TestDistInstallStageFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "versions"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "versions"), 0o755) })
	bin := filepath.Join(t.TempDir(), "poe-acp")
	if err := os.WriteFile(bin, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := distRun(t, root, "-version", "0.31.0", "install", bin)
	if code != 1 || !strings.Contains(errOut, "dist install:") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

func TestDistUsageErrors(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"no command", nil, 2, "unknown command"},
		{"unknown command", []string{"frobnicate"}, 2, "unknown command"},
		{"bad flag", []string{"-nope"}, 2, "flag provided but not defined"},
		{"install without file", []string{"-version", "0.1.0", "install"}, 2, "need a file argument"},
		{"install without version", []string{"install", "/dev/null"}, 2, "-version"},
		{"install missing file", []string{"-version", "0.1.0", "install", filepath.Join(root, "absent")}, 1, "no such file"},
		{"activate without version", []string{"activate"}, 2, "need a version argument"},
		{"activate uninstalled", []string{"activate", "9.9.9"}, 1, "dist activate"},
		{"unpin without version", []string{"unpin"}, 2, "need a version argument"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errOut := distRun(t, root, tc.args...)
			if code != tc.wantCode || !strings.Contains(errOut, tc.wantErr) {
				t.Fatalf("code=%d err=%q, want code=%d containing %q", code, errOut, tc.wantCode, tc.wantErr)
			}
		})
	}
}

// TestDistCorruptState: unreadable state is reported with a non-zero
// exit rather than a misleading "all clear".
func TestDistCorruptState(t *testing.T) {
	root := t.TempDir()
	l := install.New(install.Config{Root: root})
	if err := l.Install("0.31.0", strings.NewReader("bin")); err != nil {
		t.Fatal(err)
	}
	if err := l.SwapCurrent("0.31.0"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pinned.json"), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := distRun(t, root, "status"); code != 1 || !strings.Contains(out, "pinned:    ERROR") {
		t.Fatalf("status with a corrupt pin file: code=%d out=%q", code, out)
	}
	if code, _, errOut := distRun(t, root, "activate", "0.31.0"); code != 1 || !strings.Contains(errOut, "parse") {
		t.Fatalf("activate with a corrupt pin file: code=%d err=%q", code, errOut)
	}
	if code, _, errOut := distRun(t, root, "unpin", "0.31.0"); code != 1 || !strings.Contains(errOut, "parse") {
		t.Fatalf("unpin with a corrupt pin file: code=%d err=%q", code, errOut)
	}
	if err := os.Remove(filepath.Join(root, "pinned.json")); err != nil {
		t.Fatal(err)
	}
	// A crash record that cannot be read is equally loud.
	if err := os.MkdirAll(filepath.Join(root, "crashes.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := distRun(t, root, "status"); code != 1 || !strings.Contains(out, "crashes:   ERROR") {
		t.Fatalf("status with an unreadable crash file: code=%d out=%q", code, out)
	}
}

// TestDistActivateLogFailure: a log the CLI cannot append to is
// reported, but the swap itself still succeeds — the symlink is the
// truth, the log is the record.
func TestDistActivateLogFailure(t *testing.T) {
	root := t.TempDir()
	l := install.New(install.Config{Root: root})
	if err := l.Install("0.31.0", strings.NewReader("bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.LogPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := distRun(t, root, "activate", "0.31.0")
	if code != 0 || !strings.Contains(out, "current -> 0.31.0") {
		t.Fatalf("activate: code=%d out=%q", code, out)
	}
	if !strings.Contains(errOut, "log:") {
		t.Errorf("log failure not reported: %q", errOut)
	}
	if v, err := l.CurrentVersion(); err != nil || v != "0.31.0" {
		t.Fatalf("current = %q (%v)", v, err)
	}
}
