package httpsrv

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kfet/poe-acp/internal/router"
)

// TestHandler_TurnTimeoutOptIn covers the opt-in absolute turn ceiling
// branch: when Config.TurnTimeout > 0 the handler wraps the decoupled
// turn context in context.WithTimeout (rather than the default
// progress-bounded WithCancel). A generous ceiling never fires here —
// the turn completes normally — but the WithTimeout branch is exercised.
func TestHandler_TurnTimeoutOptIn(t *testing.T) {
	rtr, err := router.New(router.Config{Agent: &fakeAgent{}, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 0, TurnTimeout: time.Hour})

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c-tt",
		"query": []map[string]any{{"role": "user", "content": "ping"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	out := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, out)
	}
	if !strings.Contains(out, `"text":"pong"`) || !strings.Contains(out, "event: done") {
		t.Fatalf("turn did not complete cleanly under opt-in ceiling:\n%s", out)
	}
}

// TestHandler_TurnCeilingCutsAndSaysSo is the other half of the opt-in
// ceiling: previously only its CONSTRUCTION was exercised (a one-hour
// cap that never fired). Here it fires on a hung agent, cuts the turn,
// and the user is told — with the operator's ceiling in the sentence,
// which is the whole reason the relay supplies its own cause rather than
// letting acp-kit's bare sentinel through.
func TestHandler_TurnCeilingCutsAndSaysSo(t *testing.T) {
	a := &wedgeAgent{fakeAgent: &fakeAgent{}, returned: make(chan struct{})}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// Ceiling far shorter than the no-progress window, so the ceiling is
	// unambiguously what fired.
	h := New(Config{Router: rtr, TurnTimeout: 60 * time.Millisecond, IdleWriteTimeout: time.Hour})

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c-ceiling",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))

	out := rec.Body.String()
	if !strings.Contains(out, "hit the relay's 60ms limit") {
		t.Fatalf("ceiling cut did not name its limit to the user:\n%s", out)
	}
	if !strings.Contains(out, "event: done") {
		t.Fatalf("ceiling cut did not seal the stream:\n%s", out)
	}
	select {
	case <-a.returned:
	case <-time.After(3 * time.Second):
		t.Fatal("ceiling cut never reached the agent's prompt context")
	}
}
