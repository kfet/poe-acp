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
