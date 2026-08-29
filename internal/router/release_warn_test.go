package router

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// methodNotFoundErr is what an agent that does not implement session/release
// answers with (JSON-RPC -32601). acp-tmux is one such agent.
func methodNotFoundErr() error {
	return acp.NewMethodNotFound("session/release")
}

// captureLog redirects the standard logger for the duration of the test and
// returns a func yielding everything written so far.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(out)
		log.SetFlags(flags)
	})
	return buf.String
}

const releaseWarnMarker = "does not implement the session/release ACP method"

// gcSessions creates n sessions on r, ages them past the TTL and runs one GC
// pass, returning the (mutated) clock value.
func gcSessions(t *testing.T, r *Router, now *int64, convs ...string) {
	t.Helper()
	for _, c := range convs {
		if err := r.Prompt(context.Background(), c, "u",
			[]Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{}); err != nil {
			t.Fatal(err)
		}
	}
	atomic.AddInt64(now, int64(2*time.Minute))
	r.gcOnce()
}

func newGCRouter(t *testing.T, agent *fakeAgent, now *int64) *Router {
	t.Helper()
	r, err := New(Config{
		Agent:      agent,
		StateDir:   t.TempDir(),
		SessionTTL: time.Minute,
		Now:        func() time.Time { return time.Unix(0, atomic.LoadInt64(now)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newReleaseAgent(releaseErr error) *fakeAgent {
	a := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	a.releaseErr = releaseErr
	return a
}

// TestRouter_ReleaseUnsupportedWarnsOnce: an agent answering session/release
// with method-not-found cannot reclaim sessions at all — that is a standing
// leak, so it must warn, but exactly once per agent no matter how many
// sessions are reaped across how many GC passes.
func TestRouter_ReleaseUnsupportedWarnsOnce(t *testing.T) {
	logged := captureLog(t)
	now := int64(1_000_000_000_000)
	agent := newReleaseAgent(methodNotFoundErr())
	r := newGCRouter(t, agent, &now)

	gcSessions(t, r, &now, "c1", "c2")
	gcSessions(t, r, &now, "c3")
	gcSessions(t, r, &now, "c4")

	if got := atomic.LoadInt32(&agent.releaseCalls); got != 4 {
		t.Fatalf("ReleaseSession calls = %d, want 4", got)
	}
	if got := strings.Count(logged(), releaseWarnMarker); got != 1 {
		t.Fatalf("release-unsupported warnings = %d, want exactly 1\nlog:\n%s", got, logged())
	}
}

// TestRouter_ReleaseQuietCases: session-not-found is normal (the agent already
// dropped the session), an unclassified error stays debug-level, and the happy
// path says nothing. None of them may warn.
func TestRouter_ReleaseQuietCases(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"session_not_found", notFoundErr()},
		{"other_error", acp.NewInternalError(map[string]any{"error": "boom"})},
		{"happy_path", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := captureLog(t)
			now := int64(1_000_000_000_000)
			agent := newReleaseAgent(tc.err)
			r := newGCRouter(t, agent, &now)

			gcSessions(t, r, &now, "c1")

			if got := atomic.LoadInt32(&agent.releaseCalls); got != 1 {
				t.Fatalf("ReleaseSession calls = %d, want 1", got)
			}
			if strings.Contains(logged(), releaseWarnMarker) {
				t.Fatalf("unexpected release-unsupported warning\nlog:\n%s", logged())
			}
		})
	}
}
