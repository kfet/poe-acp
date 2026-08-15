//go:build unix

package supervisor

// Rollback substrate, supervisor half.
//
// The supervisor is never the thing being replaced: systemd/launchd
// babysit IT, and it forks workers from the `current` symlink (see
// internal/install). That makes it the only place a binary swap can be
// judged, and the judgement is identical on both init systems.
//
// Two events drive the state machine:
//
//   - Confirm — a freshly forked worker completed the startup handshake
//     (SIGUSR1 → WaitReady → ReadyOK). That is a POSITIVE health signal
//     from the new binary itself, not an absence of bad news, so it is
//     what advances `last-good` to `current`.
//
//   - Crashed — a worker died: before signalling ready, or after serving.
//     CrashLimit crashes inside CrashWindow means the current version
//     cannot hold a worker up. The supervisor repoints `current` back at
//     `last-good`, PINS the bad version so it is never activated again,
//     and reports HealReverted; the caller re-enters its ordinary
//     worker-swap path, which forks from the (now reverted) symlink.
//
// Every decision is appended to the durable rollback log. On a host that
// does not use the versioned layout the whole machine reports
// HealUnavailable and changes nothing — rollback is a capability, not a
// requirement.

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

// Crash-loop defaults: CrashLimit worker deaths inside CrashWindow are a
// crash loop. Two deaths are noise (a wedged agent, an OOM); three in a
// minute is a binary that cannot serve.
const (
	DefaultCrashLimit  = 3
	DefaultCrashWindow = 60 * time.Second
)

// Store is the durable half of the rollback substrate — implemented by
// install.Layout, stubbed in tests. Every method may fail on a host that
// does not use the versioned layout; the Healer degrades rather than
// propagating.
type Store interface {
	// Managed reports whether the versioned layout is present and usable.
	Managed() error
	// CurrentVersion is the version the `current` symlink names.
	CurrentVersion() (string, error)
	// LastGoodVersion is the version the `last-good` symlink names.
	LastGoodVersion() (string, error)
	// SetLastGood repoints `last-good` at version.
	SetLastGood(version string) error
	// SwapCurrent repoints `current` at version.
	SwapCurrent(version string) error
	// Pin marks version as never-activate-again.
	Pin(version, reason string) error
	// Crashes returns the recorded crash timestamps, oldest first.
	Crashes() ([]time.Time, error)
	// SetCrashes replaces the crash record (nil clears it).
	SetCrashes(ts []time.Time) error
	// Logf appends one line to the durable rollback log.
	Logf(format string, a ...any) error
}

// HealAction is what a Healer decided.
type HealAction int

const (
	// HealUnavailable: this host has no versioned layout, so rollback is
	// not possible. Reported once per event, never fatal.
	HealUnavailable HealAction = iota
	// HealNone: nothing to do (the healthy version is already last-good).
	HealNone
	// HealConfirmed: `last-good` advanced to the running version.
	HealConfirmed
	// HealArmed: a crash was recorded but the loop threshold is not met.
	HealArmed
	// HealFailed: a crash loop was detected but could not be reverted —
	// no last-good, it equals current, or the swap itself failed.
	HealFailed
	// HealReverted: `current` was repointed at `last-good` and the bad
	// version pinned. The caller must now swap workers.
	HealReverted
)

func (a HealAction) String() string {
	switch a {
	case HealNone:
		return "none"
	case HealConfirmed:
		return "confirmed"
	case HealArmed:
		return "armed"
	case HealFailed:
		return "revert-failed"
	case HealReverted:
		return "reverted"
	default:
		return "unavailable"
	}
}

// HealOutcome reports a single decision. Err carries a non-fatal problem
// worth logging (a store that could not be read or written); it never
// means the supervisor should stop.
type HealOutcome struct {
	Action  HealAction
	Version string // `current` version at decision time, when known
	Target  string // version reverted to (HealReverted)
	Crashes int    // crashes inside the window, including this one
	Err     error
}

// String renders the outcome for a log line.
func (o HealOutcome) String() string {
	s := "rollback: " + o.Action.String()
	if o.Version != "" {
		s += " version=" + o.Version
	}
	if o.Target != "" {
		s += " -> " + o.Target
	}
	if o.Crashes > 0 {
		s += fmt.Sprintf(" crashes=%d", o.Crashes)
	}
	if o.Err != nil {
		s += " (" + o.Err.Error() + ")"
	}
	return s
}

// HealConfig configures a Healer.
type HealConfig struct {
	// Store is the durable layout. A nil Store disables the feature
	// (every event reports HealUnavailable).
	Store Store
	// Limit is the crash count that constitutes a loop (default
	// DefaultCrashLimit).
	Limit int
	// Window is the span crashes must fall inside (default
	// DefaultCrashWindow).
	Window time.Duration
	// Now is the clock seam (default time.Now).
	Now func() time.Time
}

// Healer is the supervisor's rollback state machine. It is safe for
// concurrent use: crashes are observed from worker-reaper goroutines.
type Healer struct {
	store  Store
	limit  int
	window time.Duration
	now    func() time.Time

	mu sync.Mutex
}

// NewHealer returns a Healer for cfg.
func NewHealer(cfg HealConfig) *Healer {
	h := &Healer{store: cfg.Store, limit: cfg.Limit, window: cfg.Window, now: cfg.Now}
	if h.limit <= 0 {
		h.limit = DefaultCrashLimit
	}
	if h.window <= 0 {
		h.window = DefaultCrashWindow
	}
	if h.now == nil {
		h.now = time.Now
	}
	return h
}

// Available reports whether the versioned layout is usable, and why not
// when it is not. Call it once at startup so the operator learns from
// the log that rollback is off, rather than at the moment it is needed.
func (h *Healer) Available() (bool, error) {
	if h.store == nil {
		return false, errors.New("rollback: no install layout configured")
	}
	if err := h.store.Managed(); err != nil {
		return false, err
	}
	return true, nil
}

// Confirm records that a freshly forked worker completed its startup
// handshake: the running version can serve, so `last-good` advances to
// it and the crash record is cleared.
func (h *Healer) Confirm() HealOutcome {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ok, err := h.Available(); !ok {
		return HealOutcome{Action: HealUnavailable, Err: err}
	}
	cur, err := h.store.CurrentVersion()
	if err != nil {
		return HealOutcome{Action: HealUnavailable, Err: err}
	}
	// A worker proved this version: any earlier crash of an older
	// version is history and must not count towards a future loop.
	cerr := h.store.SetCrashes(nil)
	if lg, err := h.store.LastGoodVersion(); err == nil && lg == cur {
		return HealOutcome{Action: HealNone, Version: cur, Err: cerr}
	}
	if err := h.store.SetLastGood(cur); err != nil {
		return HealOutcome{Action: HealNone, Version: cur, Err: errors.Join(cerr, err)}
	}
	logErr := h.store.Logf("healthy version=%s last-good advanced", cur)
	return HealOutcome{Action: HealConfirmed, Version: cur, Err: errors.Join(cerr, logErr)}
}

// Crashed records the death of a worker and decides whether the current
// version is in a crash loop. On HealReverted the `current` symlink has
// already been repointed at `last-good` and the bad version pinned; the
// caller re-enters its worker-swap path to fork from it.
func (h *Healer) Crashed() HealOutcome {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ok, err := h.Available(); !ok {
		return HealOutcome{Action: HealUnavailable, Err: err}
	}
	cur, err := h.store.CurrentVersion()
	if err != nil {
		return HealOutcome{Action: HealUnavailable, Err: err}
	}

	n, rerr := h.recordCrash()
	logErr := h.store.Logf("crash version=%s count=%d/%d window=%s", cur, n, h.limit, h.window)
	if n < h.limit {
		return HealOutcome{Action: HealArmed, Version: cur, Crashes: n, Err: errors.Join(rerr, logErr)}
	}

	out := HealOutcome{Version: cur, Crashes: n, Err: errors.Join(rerr, logErr)}
	lg, err := h.store.LastGoodVersion()
	switch {
	case err != nil:
		out.Action = HealFailed
		out.Err = errors.Join(out.Err, fmt.Errorf("no last-good to revert to: %w", err))
	case lg == cur:
		out.Action = HealFailed
		out.Err = errors.Join(out.Err, fmt.Errorf("last-good IS the crashing version %s", cur))
	default:
		out = h.revert(out, cur, lg)
	}
	if out.Action == HealFailed {
		out.Err = errors.Join(out.Err, h.store.Logf(
			"revert-failed version=%s crashes=%d: %v", cur, n, out.Err))
	}
	return out
}

// revert pins the crashing version and repoints `current` at lg. The pin
// comes first: a supervisor killed between the two steps must not come
// back and re-activate the version it just condemned.
func (h *Healer) revert(out HealOutcome, cur, lg string) HealOutcome {
	reason := fmt.Sprintf("crash-loop: %d worker crashes within %s", out.Crashes, h.window)
	if err := h.store.Pin(cur, reason); err != nil {
		out.Action = HealFailed
		out.Err = errors.Join(out.Err, fmt.Errorf("pin %s: %w", cur, err))
		return out
	}
	if err := h.store.SwapCurrent(lg); err != nil {
		out.Action = HealFailed
		out.Err = errors.Join(out.Err, fmt.Errorf("repoint current at %s: %w", lg, err))
		return out
	}
	// The revert is the fresh start for crash accounting: the reverted
	// version gets the full budget before it is condemned in turn.
	out.Action = HealReverted
	out.Target = lg
	out.Err = errors.Join(out.Err,
		h.store.SetCrashes(nil),
		h.store.Logf("revert version=%s -> %s pinned=%s reason=%q", cur, lg, cur, reason))
	return out
}

// recordCrash appends now to the durable crash record, dropping entries
// older than the window, and returns how many remain. A store that
// cannot be read or written still yields a usable count (this crash),
// with the error reported for logging.
func (h *Healer) recordCrash() (int, error) {
	t := h.now()
	prev, err := h.store.Crashes()
	kept := make([]time.Time, 0, len(prev)+1)
	for _, ts := range prev {
		if t.Sub(ts) < h.window {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, t)
	return len(kept), errors.Join(err, h.store.SetCrashes(kept))
}

// SignalSelf delivers sig to this process. The supervisor uses it to
// SIGHUP ITSELF after a revert: the update path IS the reload path, so a
// rollback re-enters the ordinary worker-swap branch (fork a worker from
// the repointed `current`, retire the old one) instead of growing a
// second, differently-behaved code path.
func SignalSelf(sig syscall.Signal) error {
	if err := kill(os.Getpid(), sig); err != nil {
		return fmt.Errorf("supervisor: signal self %v: %w", sig, err)
	}
	return nil
}
