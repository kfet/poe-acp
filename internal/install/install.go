// Package install describes the versioned on-disk layout that makes
// replacing the poe-acp binary REVERSIBLE.
//
// Layout (root defaults to $XDG_STATE_HOME/poe-acp/dist):
//
//	<root>/versions/poe-acp-<version>   real binaries, one per version
//	<root>/current      -> versions/poe-acp-<version>   the supervisor's ExecStart target
//	<root>/last-good    -> versions/poe-acp-<version>   last version that proved itself
//	<root>/pinned.json  versions that crash-looped; never swapped to again
//	<root>/crashes.json recent worker crash timestamps (survives a supervisor restart)
//	<root>/rollback.log append-only, greppable record of every decision
//
// Both links are relative (`versions/poe-acp-<v>`), so the whole tree can
// be moved or built inside a staging dir and renamed into place.
//
// The supervisor's ExecStart MUST target <root>/current, never a real
// file: swapping the binary is then a single atomic symlink repoint
// (write a temp symlink, rename(2) it over the old one) and the next
// fork of a worker — or the next start by systemd/launchd — picks up the
// new version with no unit-file edit. The supervisor process itself is
// never the thing being replaced, so this works identically under
// systemd-user and launchd.
//
// Nothing here fetches anything. The layout is written by whatever
// delivers a binary (today scripts/converge.sh over ssh, later
// `poe-acp reconcile`) and read by the supervisor's rollback state
// machine (internal/supervisor, heal.go).
//
// Degrading gracefully is a hard requirement: a host whose binary is a
// plain file at ~/.local/bin/poe-acp has no <root>/current symlink, so
// every read reports ErrUnmanaged and the supervisor simply announces
// that rollback is unavailable. There is no flag day.
package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Link and file names inside the layout root.
const (
	// CurrentLink is the symlink the supervisor is started from.
	CurrentLink = "current"
	// LastGoodLink names the last version a worker proved healthy.
	LastGoodLink = "last-good"

	versionsDir = "versions"
	binPrefix   = "poe-acp-"
	pinnedFile  = "pinned.json"
	crashesFile = "crashes.json"
	logFile     = "rollback.log"
)

// ErrUnmanaged reports that this host does not use the versioned layout
// (no `current` symlink, or it does not point into versions/). Rollback
// is unavailable; everything else keeps working.
var ErrUnmanaged = errors.New("install: not a versioned layout")

// now is the clock seam (overridden in tests).
var now = time.Now

// Config configures a Layout.
type Config struct {
	// Root is the layout root. Defaults to DefaultRoot().
	Root string
	// Scope separates the crash counters of several supervisors sharing
	// one install root (one unit per bot, one binary per host). Empty
	// means the unscoped counter.
	Scope string
}

// Layout is a handle on a versioned install root. The zero value is not
// usable; construct with New.
type Layout struct {
	root  string
	scope string
}

// New returns the Layout for cfg.
func New(cfg Config) Layout {
	root := cfg.Root
	if root == "" {
		root = DefaultRoot()
	}
	return Layout{root: root, scope: cfg.Scope}
}

// DefaultRoot returns the layout root: $POEACP_INSTALL_ROOT, else
// $XDG_STATE_HOME/poe-acp/dist, else ~/.local/state/poe-acp/dist. macOS
// has no XDG_STATE_HOME by default, so it lands on the same ~/.local
// path — one layout for both supervisors.
func DefaultRoot() string {
	if d := os.Getenv("POEACP_INSTALL_ROOT"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "poe-acp", "dist")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "state", "poe-acp", "dist")
	}
	return filepath.Join(os.TempDir(), "poe-acp", "dist")
}

// Root returns the layout root directory.
func (l Layout) Root() string { return l.root }

// CurrentPath returns the path of the `current` symlink — the path an
// ExecStart / launchd ProgramArguments entry must name.
func (l Layout) CurrentPath() string { return filepath.Join(l.root, CurrentLink) }

// LastGoodPath returns the path of the `last-good` symlink.
func (l Layout) LastGoodPath() string { return filepath.Join(l.root, LastGoodLink) }

// LogPath returns the path of the durable rollback log.
func (l Layout) LogPath() string { return filepath.Join(l.root, logFile) }

// VersionPath returns the real binary path for version.
func (l Layout) VersionPath(version string) string {
	return filepath.Join(l.root, versionsDir, binPrefix+version)
}

// versionTarget is the RELATIVE symlink target for version.
func versionTarget(version string) string {
	return filepath.Join(versionsDir, binPrefix+version)
}

// Managed reports whether this host uses the versioned layout: `current`
// is a symlink into versions/ and the binary it names exists. It returns
// an error wrapping ErrUnmanaged otherwise, suitable for logging as the
// reason rollback is unavailable.
func (l Layout) Managed() error {
	v, err := l.CurrentVersion()
	if err != nil {
		return err
	}
	if _, err := os.Stat(l.VersionPath(v)); err != nil {
		return fmt.Errorf("%w: %s: %s", ErrUnmanaged, l.CurrentPath(), err)
	}
	return nil
}

// CurrentVersion returns the version the `current` symlink names.
func (l Layout) CurrentVersion() (string, error) { return l.linkVersion(CurrentLink) }

// LastGoodVersion returns the version the `last-good` symlink names. A
// layout that has never confirmed a healthy worker has no such link and
// reports an error wrapping ErrUnmanaged — rollback is then simply not
// possible yet.
func (l Layout) LastGoodVersion() (string, error) { return l.linkVersion(LastGoodLink) }

// linkVersion reads one of the layout's symlinks and extracts the
// version from its target. A plain file (the legacy layout) fails
// Readlink with EINVAL and is reported as unmanaged, exactly like a
// missing link.
func (l Layout) linkVersion(name string) (string, error) {
	p := filepath.Join(l.root, name)
	target, err := os.Readlink(p)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %s", ErrUnmanaged, p, err)
	}
	base := filepath.Base(target)
	v := strings.TrimPrefix(base, binPrefix)
	if v == base || v == "" {
		return "", fmt.Errorf("%w: %s -> %s is not a versioned binary", ErrUnmanaged, p, target)
	}
	return v, nil
}

// Install writes r as the binary for version, atomically: it is staged
// under versions/ and renamed into place, so a half-written binary is
// never visible under its final name. Re-installing a version replaces
// it. It does not touch any symlink — installing is not activating.
func (l Layout) Install(version string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Join(l.root, versionsDir), 0o755); err != nil {
		return fmt.Errorf("install: versions dir: %w", err)
	}
	if err := atomicWrite(l.VersionPath(version), r, 0o755); err != nil {
		return fmt.Errorf("install: %s: %w", version, err)
	}
	return nil
}

// atomicWrite writes r to dst with the given mode via a temp file in
// dst's directory and a rename(2), so a reader (or an exec) never sees a
// partially written file under its final name. Both the binaries and the
// small JSON state files go through here.
func atomicWrite(dst string, r io.Reader, mode os.FileMode) error {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	staged := tmp.Name()
	defer func() { _ = os.Remove(staged) }() // no-op once renamed
	_, cerr := io.Copy(tmp, r)
	if err := errors.Join(cerr, tmp.Chmod(mode), tmp.Close()); err != nil {
		return fmt.Errorf("write %s: %w", staged, err)
	}
	if err := os.Rename(staged, dst); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	return nil
}

// SwapCurrent repoints `current` at version. This is the binary swap:
// the next worker the supervisor forks — and the next start by the init
// system — runs that version.
func (l Layout) SwapCurrent(version string) error { return l.point(CurrentLink, version) }

// SetLastGood repoints `last-good` at version, recording that it proved
// itself healthy.
func (l Layout) SetLastGood(version string) error { return l.point(LastGoodLink, version) }

// point atomically repoints the named link at version: symlink to a temp
// name in the same directory, then rename(2) over the old link. A reader
// (or an exec) either sees the old target or the new one, never neither.
func (l Layout) point(name, version string) error {
	if _, err := os.Stat(l.VersionPath(version)); err != nil {
		return fmt.Errorf("install: point %s at %s: %w", name, version, err)
	}
	tmp := filepath.Join(l.root, "."+name+".tmp")
	_ = os.Remove(tmp) // leftover from a crashed swap; a fresh symlink needs a free name
	if err := os.Symlink(versionTarget(version), tmp); err != nil {
		return fmt.Errorf("install: stage %s link: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(l.root, name)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install: publish %s link: %w", name, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pins
// ---------------------------------------------------------------------------

// Pin records a version that crash-looped and must not be activated
// again. Whatever chooses the desired version (step 2's reconcile) skips
// pinned versions until the desired version moves PAST them — i.e. a
// pin is cleared by shipping a newer build, never by retrying the same
// broken one.
type Pin struct {
	Version string    `json:"version"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
}

// Pin marks version as bad. Re-pinning refreshes the reason and time.
func (l Layout) Pin(version, reason string) error {
	pins, err := l.Pinned()
	if err != nil {
		return err
	}
	pins[version] = Pin{Version: version, Reason: reason, At: now().UTC()}
	return l.writeJSON(pinnedFile, pins)
}

// Unpin forgets the pin for version (nothing to do if it is not pinned).
func (l Layout) Unpin(version string) error {
	pins, err := l.Pinned()
	if err != nil {
		return err
	}
	if _, ok := pins[version]; !ok {
		return nil
	}
	delete(pins, version)
	return l.writeJSON(pinnedFile, pins)
}

// Pinned returns the pinned versions keyed by version. A layout with no
// pin file returns an empty (non-nil) map.
func (l Layout) Pinned() (map[string]Pin, error) {
	pins := map[string]Pin{}
	if err := l.readJSON(pinnedFile, &pins); err != nil {
		return nil, err
	}
	return pins, nil
}

// IsPinned reports whether version is pinned, and why.
func (l Layout) IsPinned(version string) (bool, Pin, error) {
	pins, err := l.Pinned()
	if err != nil {
		return false, Pin{}, err
	}
	p, ok := pins[version]
	return ok, p, nil
}

// ---------------------------------------------------------------------------
// Crash records
// ---------------------------------------------------------------------------

// Crashes returns the recorded worker-crash timestamps, oldest first.
// The record is durable because the decisive crash loop is the one that
// takes the supervisor down with it: an in-memory counter resets on
// every init-system restart and would never reach the threshold.
func (l Layout) Crashes() ([]time.Time, error) {
	var ts []time.Time
	if err := l.readJSON(l.crashesName(), &ts); err != nil {
		return nil, err
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	return ts, nil
}

// SetCrashes replaces the crash record (nil clears it).
func (l Layout) SetCrashes(ts []time.Time) error { return l.writeJSON(l.crashesName(), ts) }

// crashesName is the crash file for this Layout's scope. Several bots on
// one host share an install root — and therefore last-good and pins,
// which describe the BINARY — but must not share a crash counter, or one
// bot's config problem would revert everyone's binary.
func (l Layout) crashesName() string {
	if l.scope == "" {
		return crashesFile
	}
	return "crashes-" + l.scope + ".json"
}

// ---------------------------------------------------------------------------
// Durable log
// ---------------------------------------------------------------------------

// Logf appends one timestamped line to <root>/rollback.log. This is the
// durable, greppable record of every health confirmation, crash and
// revert — it outlives the process that made the decision, which stderr
// under launchd very often does not.
func (l Layout) Logf(format string, a ...any) error {
	if err := os.MkdirAll(l.root, 0o755); err != nil {
		return fmt.Errorf("install: log dir: %w", err)
	}
	f, err := os.OpenFile(l.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("install: open log: %w", err)
	}
	line := fmt.Sprintf("%s poe-acp-rollback %s\n",
		now().UTC().Format(time.RFC3339), fmt.Sprintf(format, a...))
	_, werr := f.WriteString(line)
	return errors.Join(werr, f.Close())
}

// ---------------------------------------------------------------------------
// JSON state helpers
// ---------------------------------------------------------------------------

// readJSON decodes <root>/<name> into v. A missing file is not an error:
// v is left as the caller initialised it.
func (l Layout) readJSON(name string, v any) error {
	b, err := os.ReadFile(filepath.Join(l.root, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("install: read %s: %w", name, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("install: parse %s: %w", name, err)
	}
	return nil
}

// writeJSON encodes v to <root>/<name> atomically (temp + rename).
func (l Layout) writeJSON(name string, v any) error {
	if err := os.MkdirAll(l.root, 0o755); err != nil {
		return fmt.Errorf("install: state dir: %w", err)
	}
	if err := atomicWrite(filepath.Join(l.root, name), bytes.NewReader(mustMarshalJSON(v)), 0o644); err != nil {
		return fmt.Errorf("install: write %s: %w", name, err)
	}
	return nil
}
