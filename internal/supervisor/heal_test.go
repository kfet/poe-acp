//go:build unix

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeStore is an in-memory Store whose every operation can be made to
// fail, so the Healer's degradation paths are driven deterministically.
type fakeStore struct {
	mu sync.Mutex

	managed  error
	cur      string
	curErr   error
	lastGood string // "" => LastGoodVersion fails (no link yet)
	crashes  []time.Time
	pins     map[string]string
	logs     []string

	failCrashes                                 error
	failSetCrashes                              error
	failSetLastGood, failSwap, failPin, failLog error
}

func newFakeStore(cur, lastGood string) *fakeStore {
	return &fakeStore{cur: cur, lastGood: lastGood, pins: map[string]string{}}
}

func (f *fakeStore) Managed() error { return f.managed }

func (f *fakeStore) CurrentVersion() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur, f.curErr
}

func (f *fakeStore) LastGoodVersion() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastGood == "" {
		return "", errors.New("no last-good link")
	}
	return f.lastGood, nil
}

func (f *fakeStore) SetLastGood(v string) error {
	if f.failSetLastGood != nil {
		return f.failSetLastGood
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastGood = v
	return nil
}

func (f *fakeStore) SwapCurrent(v string) error {
	if f.failSwap != nil {
		return f.failSwap
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cur = v
	return nil
}

func (f *fakeStore) Pin(v, reason string) error {
	if f.failPin != nil {
		return f.failPin
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pins[v] = reason
	return nil
}

func (f *fakeStore) Crashes() ([]time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.crashes...), f.failCrashes
}

func (f *fakeStore) SetCrashes(ts []time.Time) error {
	if f.failSetCrashes != nil {
		return f.failSetCrashes
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.crashes = append([]time.Time(nil), ts...)
	return nil
}

func (f *fakeStore) Logf(format string, a ...any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, fmt.Sprintf(format, a...))
	return f.failLog
}

func (f *fakeStore) logged(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// clock is a manual clock so crash-window logic never depends on wall time.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestHealer(store Store, c *clock) *Healer {
	return NewHealer(HealConfig{Store: store, Limit: 3, Window: time.Minute, Now: c.now})
}

func TestHealerDefaults(t *testing.T) {
	h := NewHealer(HealConfig{})
	if h.limit != DefaultCrashLimit || h.window != DefaultCrashWindow {
		t.Fatalf("defaults not applied: limit=%d window=%s", h.limit, h.window)
	}
	if h.now == nil {
		t.Fatal("clock seam not defaulted")
	}
	// A nil store disables the feature rather than panicking.
	if ok, err := h.Available(); ok || err == nil {
		t.Fatalf("nil store: got ok=%v err=%v", ok, err)
	}
	if out := h.Confirm(); out.Action != HealUnavailable {
		t.Fatalf("Confirm with nil store: %v", out)
	}
	if out := h.Crashed(); out.Action != HealUnavailable {
		t.Fatalf("Crashed with nil store: %v", out)
	}
}

// TestHealerStateMachine drives the substrate's decision table: what a
// sequence of health confirmations and worker crashes does to
// `last-good`, `current` and the pin set.
func TestHealerStateMachine(t *testing.T) {
	type event struct {
		crash   bool          // crash event, else a health confirmation
		advance time.Duration // clock advance BEFORE the event
		want    HealAction
	}
	tests := []struct {
		name         string
		cur          string
		lastGood     string
		events       []event
		wantCurrent  string
		wantLastGood string
		wantPinned   string // version expected in the pin set ("" => none)
		wantLog      string
	}{
		{
			name:         "swap then healthy advances last-good",
			cur:          "0.31.0",
			lastGood:     "0.30.0",
			events:       []event{{want: HealConfirmed}},
			wantCurrent:  "0.31.0",
			wantLastGood: "0.31.0",
			wantLog:      "healthy version=0.31.0",
		},
		{
			name:         "healthy when already last-good is a no-op",
			cur:          "0.31.0",
			lastGood:     "0.31.0",
			events:       []event{{want: HealNone}},
			wantCurrent:  "0.31.0",
			wantLastGood: "0.31.0",
		},
		{
			name:     "three crashes inside the window revert and pin",
			cur:      "0.31.0",
			lastGood: "0.30.0",
			events: []event{
				{crash: true, want: HealArmed},
				{crash: true, advance: 5 * time.Second, want: HealArmed},
				{crash: true, advance: 5 * time.Second, want: HealReverted},
			},
			wantCurrent:  "0.30.0",
			wantLastGood: "0.30.0",
			wantPinned:   "0.31.0",
			wantLog:      "revert version=0.31.0 -> 0.30.0",
		},
		{
			name:     "two crashes then healthy clears the record",
			cur:      "0.31.0",
			lastGood: "0.30.0",
			events: []event{
				{crash: true, want: HealArmed},
				{crash: true, advance: time.Second, want: HealArmed},
				{want: HealConfirmed},
				{crash: true, advance: time.Second, want: HealArmed},
			},
			wantCurrent:  "0.31.0",
			wantLastGood: "0.31.0",
		},
		{
			name:     "crashes spread past the window never loop",
			cur:      "0.31.0",
			lastGood: "0.30.0",
			events: []event{
				{crash: true, want: HealArmed},
				{crash: true, advance: 90 * time.Second, want: HealArmed},
				{crash: true, advance: 90 * time.Second, want: HealArmed},
			},
			wantCurrent:  "0.31.0",
			wantLastGood: "0.30.0",
		},
		{
			name:     "crash loop with no last-good reports and does not revert",
			cur:      "0.31.0",
			lastGood: "",
			events: []event{
				{crash: true, want: HealArmed},
				{crash: true, want: HealArmed},
				{crash: true, want: HealFailed},
			},
			wantCurrent: "0.31.0",
			wantLog:     "revert-failed version=0.31.0",
		},
		{
			name:     "crash loop when last-good IS current reports only",
			cur:      "0.31.0",
			lastGood: "0.31.0",
			events: []event{
				{crash: true, want: HealArmed},
				{crash: true, want: HealArmed},
				{crash: true, want: HealFailed},
			},
			wantCurrent:  "0.31.0",
			wantLastGood: "0.31.0",
			wantLog:      "last-good IS the crashing version",
		},
		{
			name:     "a reverted version that keeps crashing is not reverted again",
			cur:      "0.31.0",
			lastGood: "0.30.0",
			events: []event{
				{crash: true, want: HealArmed},
				{crash: true, want: HealArmed},
				{crash: true, want: HealReverted},
				{crash: true, want: HealArmed},
				{crash: true, want: HealArmed},
				{crash: true, want: HealFailed},
			},
			wantCurrent:  "0.30.0",
			wantLastGood: "0.30.0",
			wantPinned:   "0.31.0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore(tc.cur, tc.lastGood)
			c := &clock{t: time.Unix(1_700_000_000, 0)}
			h := newTestHealer(store, c)
			for i, ev := range tc.events {
				c.add(ev.advance)
				var out HealOutcome
				if ev.crash {
					out = h.Crashed()
				} else {
					out = h.Confirm()
				}
				if out.Action != ev.want {
					t.Fatalf("event %d: got %v (%s), want %v", i, out.Action, out, ev.want)
				}
				// HealFailed always explains itself; every other
				// action must be error-free against a healthy store.
				if (out.Err != nil) != (ev.want == HealFailed) {
					t.Fatalf("event %d: action %v with err %v", i, out.Action, out.Err)
				}
			}
			if got, _ := store.CurrentVersion(); got != tc.wantCurrent {
				t.Errorf("current = %q, want %q", got, tc.wantCurrent)
			}
			if tc.wantLastGood != "" {
				if got, _ := store.LastGoodVersion(); got != tc.wantLastGood {
					t.Errorf("last-good = %q, want %q", got, tc.wantLastGood)
				}
			}
			if tc.wantPinned != "" {
				if _, ok := store.pins[tc.wantPinned]; !ok {
					t.Errorf("version %q not pinned; pins=%v", tc.wantPinned, store.pins)
				}
			} else if len(store.pins) != 0 {
				t.Errorf("unexpected pins: %v", store.pins)
			}
			if tc.wantLog != "" && !store.logged(tc.wantLog) {
				t.Errorf("rollback log missing %q; got %v", tc.wantLog, store.logs)
			}
		})
	}
}

// TestHealerLegacyLayout: a host whose binary is a plain file has no
// versioned layout. Every event must report the feature as unavailable
// and touch nothing.
func TestHealerLegacyLayout(t *testing.T) {
	store := newFakeStore("0.31.0", "0.30.0")
	store.managed = errors.New("not a versioned layout")
	h := newTestHealer(store, &clock{t: time.Unix(1, 0)})

	ok, err := h.Available()
	if ok || err == nil {
		t.Fatalf("Available() = %v, %v", ok, err)
	}
	for i := 0; i < 5; i++ {
		if out := h.Crashed(); out.Action != HealUnavailable {
			t.Fatalf("crash %d: %v", i, out)
		}
	}
	if out := h.Confirm(); out.Action != HealUnavailable {
		t.Fatalf("Confirm: %v", out)
	}
	if got, _ := store.CurrentVersion(); got != "0.31.0" {
		t.Errorf("current changed to %q", got)
	}
	if len(store.logs) != 0 || len(store.pins) != 0 || len(store.crashes) != 0 {
		t.Errorf("legacy layout was written to: logs=%v pins=%v crashes=%v",
			store.logs, store.pins, store.crashes)
	}
}

// TestHealerStoreErrors: every durable-store failure is reported, never
// fatal, and never silently loses the decision.
func TestHealerStoreErrors(t *testing.T) {
	boom := errors.New("boom")

	t.Run("current version unreadable", func(t *testing.T) {
		store := newFakeStore("0.31.0", "0.30.0")
		store.curErr = boom
		h := newTestHealer(store, &clock{})
		if out := h.Confirm(); out.Action != HealUnavailable || !errors.Is(out.Err, boom) {
			t.Fatalf("Confirm: %v", out)
		}
		if out := h.Crashed(); out.Action != HealUnavailable || !errors.Is(out.Err, boom) {
			t.Fatalf("Crashed: %v", out)
		}
	})

	t.Run("set last-good fails", func(t *testing.T) {
		store := newFakeStore("0.31.0", "0.30.0")
		store.failSetLastGood = boom
		h := newTestHealer(store, &clock{})
		out := h.Confirm()
		if out.Action != HealNone || !errors.Is(out.Err, boom) {
			t.Fatalf("Confirm: %v", out)
		}
	})

	t.Run("clearing crashes fails on confirm", func(t *testing.T) {
		store := newFakeStore("0.31.0", "0.30.0")
		store.failSetCrashes = boom
		h := newTestHealer(store, &clock{})
		out := h.Confirm()
		if out.Action != HealConfirmed || !errors.Is(out.Err, boom) {
			t.Fatalf("Confirm: %v", out)
		}
	})

	t.Run("crash record unreadable still counts", func(t *testing.T) {
		store := newFakeStore("0.31.0", "0.30.0")
		store.failCrashes = boom
		h := newTestHealer(store, &clock{})
		out := h.Crashed()
		if out.Action != HealArmed || out.Crashes != 1 || !errors.Is(out.Err, boom) {
			t.Fatalf("Crashed: %v", out)
		}
	})

	t.Run("log write fails", func(t *testing.T) {
		store := newFakeStore("0.31.0", "0.31.0")
		store.failLog = boom
		h := newTestHealer(store, &clock{})
		if out := h.Crashed(); out.Action != HealArmed || !errors.Is(out.Err, boom) {
			t.Fatalf("Crashed: %v", out)
		}
	})

	t.Run("confirm log write fails", func(t *testing.T) {
		store := newFakeStore("0.31.0", "0.30.0")
		store.failLog = boom
		h := newTestHealer(store, &clock{})
		if out := h.Confirm(); out.Action != HealConfirmed || !errors.Is(out.Err, boom) {
			t.Fatalf("Confirm: %v", out)
		}
	})

	t.Run("pin fails: no revert", func(t *testing.T) {
		store := newFakeStore("0.31.0", "0.30.0")
		store.failPin = boom
		h := crashLoop(t, store)
		_ = h
		if got, _ := store.CurrentVersion(); got != "0.31.0" {
			t.Fatalf("current repointed despite failed pin: %q", got)
		}
	})

	t.Run("swap fails: pinned but not reverted", func(t *testing.T) {
		store := newFakeStore("0.31.0", "0.30.0")
		store.failSwap = boom
		crashLoop(t, store)
		if got, _ := store.CurrentVersion(); got != "0.31.0" {
			t.Fatalf("current = %q, want unchanged", got)
		}
		if _, ok := store.pins["0.31.0"]; !ok {
			t.Fatal("bad version not pinned")
		}
	})

	t.Run("clearing crashes fails after revert", func(t *testing.T) {
		store := newFakeStore("0.31.0", "0.30.0")
		c := &clock{t: time.Unix(1, 0)}
		h := newTestHealer(store, c)
		for i := 0; i < 2; i++ {
			if out := h.Crashed(); out.Action != HealArmed {
				t.Fatalf("crash %d: %v", i, out)
			}
		}
		store.failSetCrashes = boom
		out := h.Crashed()
		if out.Action != HealReverted || !errors.Is(out.Err, boom) {
			t.Fatalf("Crashed: %v", out)
		}
	})
}

// crashLoop drives store to the crash threshold and asserts the outcome
// is a reported failure rather than a revert.
func crashLoop(t *testing.T, store *fakeStore) *Healer {
	t.Helper()
	h := newTestHealer(store, &clock{t: time.Unix(1, 0)})
	for i := 0; i < 2; i++ {
		if out := h.Crashed(); out.Action != HealArmed {
			t.Fatalf("crash %d: %v", i, out)
		}
	}
	out := h.Crashed()
	if out.Action != HealFailed || out.Err == nil {
		t.Fatalf("final crash: %v", out)
	}
	return h
}

func TestHealActionString(t *testing.T) {
	for action, want := range map[HealAction]string{
		HealUnavailable: "unavailable",
		HealNone:        "none",
		HealConfirmed:   "confirmed",
		HealArmed:       "armed",
		HealFailed:      "revert-failed",
		HealReverted:    "reverted",
	} {
		if got := action.String(); got != want {
			t.Errorf("HealAction(%d).String() = %q, want %q", action, got, want)
		}
	}
}

func TestHealOutcomeString(t *testing.T) {
	out := HealOutcome{
		Action:  HealReverted,
		Version: "0.31.0",
		Target:  "0.30.0",
		Crashes: 3,
		Err:     errors.New("log write failed"),
	}
	got := out.String()
	for _, want := range []string{"reverted", "version=0.31.0", "-> 0.30.0", "crashes=3", "log write failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing %q", got, want)
		}
	}
	if got := (HealOutcome{Action: HealNone}).String(); got != "rollback: none" {
		t.Errorf("bare outcome = %q", got)
	}
}

func TestSignalSelf(t *testing.T) {
	var gotPid int
	var gotSig syscall.Signal
	restore := kill
	kill = func(pid int, sig syscall.Signal) error {
		gotPid, gotSig = pid, sig
		return nil
	}
	defer func() { kill = restore }()

	if err := SignalSelf(syscall.SIGHUP); err != nil {
		t.Fatalf("SignalSelf: %v", err)
	}
	if gotPid != os.Getpid() || gotSig != syscall.SIGHUP {
		t.Fatalf("kill(%d, %v), want kill(%d, SIGHUP)", gotPid, gotSig, os.Getpid())
	}

	kill = func(int, syscall.Signal) error { return errors.New("EPERM") }
	if err := SignalSelf(syscall.SIGHUP); err == nil {
		t.Fatal("SignalSelf: want error")
	}
}
