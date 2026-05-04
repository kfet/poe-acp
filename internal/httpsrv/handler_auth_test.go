package httpsrv

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kfet/poe-acp/internal/acpclient"
	"github.com/kfet/poe-acp/internal/authbroker"
	"github.com/kfet/poe-acp/internal/router"
)

type stubAuth struct {
	methods []acpclient.AuthMethod
	res     acpclient.AuthResult
}

func (s *stubAuth) AuthMethods() []acpclient.AuthMethod { return s.methods }
func (s *stubAuth) Authenticate(_ context.Context, _, _, _ string, _ bool) (acpclient.AuthResult, error) {
	return s.res, nil
}

func TestHandler_LoginIntercept(t *testing.T) {
	stub := &stubAuth{
		methods: []acpclient.AuthMethod{{ID: "oauth-anthropic", Name: "Anthropic", Type: "agent"}},
		res:     acpclient.AuthResult{State: "needs_redirect", URL: "https://example/auth"},
	}
	broker := authbroker.New(stub)

	rtr, err := router.New(router.Config{
		Agent:      &fakeAgent{},
		StateDir:   t.TempDir(),
		SessionTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, AuthBroker: broker, HeartbeatInterval: 0})

	body := mustJSON(map[string]any{
		"type":            "query",
		"conversation_id": "c1",
		"user_id":         "u1",
		"message_id":      "m1",
		"query": []map[string]any{
			{"role": "user", "content": "/login anthropic"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "https://example/auth") {
		t.Fatalf("URL not in response body: %s", out)
	}
	if !strings.Contains(out, "event: done") {
		t.Fatalf("missing done event: %s", out)
	}
	// Router must NOT have created a session for /login.
	if got := rtr.Len(); got != 0 {
		t.Errorf("expected 0 router sessions, got %d", got)
	}
	if !broker.HasPending("c1") {
		t.Errorf("expected pending login for c1")
	}
}

func TestHandler_LoginPaste(t *testing.T) {
	stub := &stubAuth{
		methods: []acpclient.AuthMethod{{ID: "oauth-anthropic", Name: "Anthropic", Type: "agent"}},
		res:     acpclient.AuthResult{State: "needs_redirect", URL: "https://example/auth"},
	}
	broker := authbroker.New(stub)
	// Prime with a /login.
	if _, err := broker.Handle(context.Background(), "c1", "/login anthropic"); err != nil {
		t.Fatal(err)
	}
	if !broker.HasPending("c1") {
		t.Fatal("setup: expected pending")
	}
	stub.res = acpclient.AuthResult{State: "ok"}

	rtr, err := router.New(router.Config{
		Agent:      &fakeAgent{},
		StateDir:   t.TempDir(),
		SessionTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, AuthBroker: broker, HeartbeatInterval: 0})

	body := mustJSON(map[string]any{
		"type":            "query",
		"conversation_id": "c1",
		"user_id":         "u1",
		"message_id":      "m2",
		"query": []map[string]any{
			{"role": "user", "content": "https://localhost/cb?code=abc"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "Authenticated") {
		t.Fatalf("expected success text, body=%s", rec.Body.String())
	}
	if broker.HasPending("c1") {
		t.Error("pending should be cleared")
	}
}

func TestHandler_NormalPromptUnaffectedByAuthBroker(t *testing.T) {
	// Wire a broker but send a non-auth prompt; must reach the agent.
	stub := &stubAuth{methods: []acpclient.AuthMethod{{ID: "oauth-anthropic", Type: "agent"}}}
	broker := authbroker.New(stub)
	rtr, err := router.New(router.Config{
		Agent:      &fakeAgent{},
		StateDir:   t.TempDir(),
		SessionTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, AuthBroker: broker, HeartbeatInterval: 0})

	body := mustJSON(map[string]any{
		"type":            "query",
		"conversation_id": "c2",
		"user_id":         "u1",
		"message_id":      "m1",
		"query": []map[string]any{
			{"role": "user", "content": "ping"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), `"text":"pong"`) {
		t.Fatalf("router not invoked, body=%s", rec.Body.String())
	}
}
