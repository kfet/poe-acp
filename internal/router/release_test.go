package router

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

// notFoundErr is the typed session-not-found error an ACP agent returns when
// it no longer holds a session (released or idle-reaped).
func notFoundErr() error {
	return &acp.RequestError{Code: client.SessionNotFoundCode, Message: "Session not found"}
}

// TestRouter_GCReleasesEvictedSessions verifies gcOnce calls Agent.ReleaseSession
// for each evicted (idle, past-TTL) session — freeing the agent's per-session
// subprocesses — and does NOT release a still-hot session.
func TestRouter_GCReleasesEvictedSessions(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})

	var now int64 = 1_000_000_000_000
	nowFn := func() time.Time { return time.Unix(0, atomic.LoadInt64(&now)) }

	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Minute, Now: nowFn})
	if err != nil {
		t.Fatal(err)
	}

	// Create an idle session (c1) that will age out.
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	// Capture c1's sid before it is evicted.
	r.mu.Lock()
	c1sid := r.sessions["c1"].sessionID
	r.mu.Unlock()

	// Advance past TTL, then create a fresh hot session (c2) at the new time.
	atomic.StoreInt64(&now, now+int64(2*time.Minute))
	if err := r.Prompt(context.Background(), "c2", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}

	r.gcOnce()

	if r.Len() != 1 {
		t.Fatalf("want 1 session (c2) after GC, got %d", r.Len())
	}
	if got := atomic.LoadInt32(&agent.releaseCalls); got != 1 {
		t.Fatalf("ReleaseSession calls = %d, want 1", got)
	}
	agent.mu.Lock()
	released := append([]acp.SessionId(nil), agent.releasedSids...)
	agent.mu.Unlock()
	if len(released) != 1 || released[0] != c1sid {
		t.Fatalf("released = %v, want [%s] (the idle session only)", released, c1sid)
	}
}

// TestRouter_GCReleaseErrorIsBestEffort verifies a failing ReleaseSession does
// not prevent eviction (the map entry is still removed).
func TestRouter_GCReleaseErrorIsBestEffort(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	agent.releaseErr = notFoundErr()

	var now int64 = 1_000_000_000_000
	nowFn := func() time.Time { return time.Unix(0, atomic.LoadInt64(&now)) }
	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Minute, Now: nowFn})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt64(&now, now+int64(2*time.Minute))
	r.gcOnce()
	if r.Len() != 0 {
		t.Fatalf("want 0 sessions after GC despite release error, got %d", r.Len())
	}
	if got := atomic.LoadInt32(&agent.releaseCalls); got != 1 {
		t.Fatalf("ReleaseSession calls = %d, want 1", got)
	}
}

// TestRouter_PromptRecoversOnSessionNotFound is the keystone: the first prompt
// hits a session the agent has forgotten (not-found); the router must evict,
// recreate, and replay the turn once — succeeding transparently.
func TestRouter_PromptRecoversOnSessionNotFound(t *testing.T) {
	var attempts int32
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			// First session: pretend the agent forgot it.
			return "", notFoundErr()
		}
		a.emit(sid, "recovered-ok")
		return acp.StopReasonEndTurn, nil
	})

	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	sink := &captureSink{}
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, sink); err != nil {
		t.Fatalf("Prompt should have recovered, got err: %v", err)
	}
	if sink.errText != "" {
		t.Fatalf("recovery leaked error to sink: %q", sink.errText)
	}
	if got := sink.text.String(); got != "recovered-ok" {
		t.Fatalf("sink text = %q, want %q", got, "recovered-ok")
	}
	if !sink.done {
		t.Fatal("sink.Done not called after recovery")
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("prompt attempts = %d, want 2 (one failed + one replayed)", got)
	}
	if got := atomic.LoadInt32(&agent.newSessCalls); got != 2 {
		t.Fatalf("NewSession calls = %d, want 2 (original + recovery)", got)
	}
	if r.Len() != 1 {
		t.Fatalf("want exactly 1 session after recovery, got %d", r.Len())
	}
}

// TestRouter_PromptRecoversOnApplyOptionsNotFound covers the case where the
// agent has forgotten the session and the FIRST call to surface it is
// applyOptions' set_model (not the prompt). The router must still recover.
func TestRouter_PromptRecoversOnApplyOptionsNotFound(t *testing.T) {
	var prompts int32
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		atomic.AddInt32(&prompts, 1)
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	// set_model always reports the session is gone. On the first turn this is
	// the recoverable trigger; after recovery (noRecover) it degrades to a
	// non-fatal "_(option not applied)_" notice and the prompt proceeds.
	agent.setModelErr = notFoundErr()

	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	if err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi"}}, Options{Model: "m2"}, sink); err != nil {
		t.Fatalf("Prompt should recover, got: %v", err)
	}
	if got := atomic.LoadInt32(&agent.newSessCalls); got != 2 {
		t.Fatalf("NewSession calls = %d, want 2 (recovery)", got)
	}
	if got := atomic.LoadInt32(&prompts); got != 1 {
		t.Fatalf("prompt attempts = %d, want 1 (first turn failed in applyOptions, replay prompted once)", got)
	}
	if got := sink.text.String(); got == "" {
		t.Fatal("expected streamed text after recovery")
	}
	if !sink.done {
		t.Fatal("sink.Done not called")
	}
}

// TestRouter_RecoverGetOrCreateFailurePropagates covers the branch where the
// post-eviction getOrCreate fails: the error is surfaced to the sink.
func TestRouter_RecoverGetOrCreateFailurePropagates(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, _ *fakeAgent, _ acp.SessionId, _ string) (acp.StopReason, error) {
		return "", notFoundErr()
	})
	// First NewSession (initial session) succeeds; the second (recovery) fails.
	agent.newSessErrOnCall = 2

	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	gotErr := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, sink)
	if gotErr == nil {
		t.Fatal("expected error when recovery getOrCreate fails")
	}
	if sink.errText == "" {
		t.Fatal("expected error surfaced to sink")
	}
	if !sink.done {
		t.Fatal("sink.Done not called")
	}
}

// TestRouter_EvictSessionStaleNoop verifies evictSession is a no-op when the
// given session is no longer the current entry for the conv (lost the delete
// race to gcOnce or a prior evict) — it must not double-close channels.
func TestRouter_EvictSessionStaleNoop(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	st := r.sessions["c1"]
	r.mu.Unlock()

	r.evictSession("c1", st) // first eviction: removes + closes
	if r.Len() != 0 {
		t.Fatalf("want 0 sessions after evict, got %d", r.Len())
	}
	// Second eviction with the same (now-stale) st must be a no-op — no panic,
	// no double-close.
	r.evictSession("c1", st)
}

// is surfaced to the sink unchanged and triggers no recovery.
func TestRouter_PromptNonRecoverableErrorPropagates(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, _ *fakeAgent, _ acp.SessionId, _ string) (acp.StopReason, error) {
		return "", acp.NewInternalError(map[string]any{"error": "boom"})
	})
	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	_ = r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, sink)
	if sink.errText == "" {
		t.Fatal("expected error surfaced to sink for non-recoverable error")
	}
	if !sink.done {
		t.Fatal("sink.Done not called")
	}
	if got := atomic.LoadInt32(&agent.newSessCalls); got != 1 {
		t.Fatalf("NewSession calls = %d, want 1 (no recovery on non-not-found)", got)
	}
}

// TestRouter_PromptRecoveryBounded verifies recovery retries exactly once: if
// the replayed turn ALSO returns not-found, the error is finalised on the sink
// (no infinite loop, no hung sink).
func TestRouter_PromptRecoveryBounded(t *testing.T) {
	var attempts int32
	agent := newFakeAgent(func(_ context.Context, _ *fakeAgent, _ acp.SessionId, _ string) (acp.StopReason, error) {
		atomic.AddInt32(&attempts, 1)
		return "", notFoundErr() // always forgotten
	})
	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	gotErr := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, sink)
	if gotErr == nil {
		t.Fatal("expected error after bounded recovery exhausted")
	}
	if !client.IsSessionNotFound(gotErr) {
		t.Fatalf("expected session-not-found error to propagate, got %v", gotErr)
	}
	if sink.errText == "" {
		t.Fatal("sink.Error not called on exhausted recovery (sink would hang)")
	}
	if !sink.done {
		t.Fatal("sink.Done not called on exhausted recovery")
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("prompt attempts = %d, want exactly 2 (original + one bounded retry)", got)
	}
}
