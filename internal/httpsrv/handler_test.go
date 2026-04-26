package httpsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/poe-acp-relay/internal/acpclient"
	"github.com/kfet/poe-acp-relay/internal/poeproto"
	"github.com/kfet/poe-acp-relay/internal/router"
)

type fakeAgent struct {
	mu    sync.Mutex
	sinks map[acp.SessionId]acpclient.SessionUpdateSink
	n     int
}

func (f *fakeAgent) Caps() acpclient.Caps { return acpclient.Caps{} }
func (f *fakeAgent) ListSessions(_ context.Context, _ string) ([]acpclient.SessionInfo, error) {
	return nil, nil
}
func (f *fakeAgent) ResumeSession(_ context.Context, _ string, _ acp.SessionId, _ acpclient.SessionUpdateSink) error {
	return nil
}
func (f *fakeAgent) NewSession(_ context.Context, _ string, sink acpclient.SessionUpdateSink) (acp.SessionId, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.sinks == nil {
		f.sinks = make(map[acp.SessionId]acpclient.SessionUpdateSink)
	}
	id := acp.SessionId("s-1")
	f.sinks[id] = sink
	return id, nil
}
func (f *fakeAgent) Prompt(_ context.Context, sid acp.SessionId, _ string) (acp.StopReason, error) {
	f.mu.Lock()
	sink := f.sinks[sid]
	f.mu.Unlock()
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		SessionId: sid,
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.TextBlock("pong"),
			},
		},
	})
	return acp.StopReasonEndTurn, nil
}
func (f *fakeAgent) Cancel(_ context.Context, _ acp.SessionId) error { return nil }

func TestHandler_Query(t *testing.T) {
	rtr, err := router.New(router.Config{
		Agent:      &fakeAgent{},
		StateDir:   t.TempDir(),
		SessionTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 0}) // disable heartbeat for determinism

	body := mustJSON(map[string]any{
		"type":            "query",
		"conversation_id": "c1",
		"user_id":         "u1",
		"message_id":      "m1",
		"query": []map[string]any{
			{"role": "user", "content": "ping"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	out := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, out)
	}
	if !strings.Contains(out, "event: meta") {
		t.Fatalf("missing meta event: %s", out)
	}
	if !strings.Contains(out, `"text":"pong"`) {
		t.Fatalf("missing pong text: %s", out)
	}
	if !strings.Contains(out, "event: done") {
		t.Fatalf("missing done event: %s", out)
	}
}

func TestHandler_Settings(t *testing.T) {
	h := New(Config{
		Settings: poeproto.SettingsResponse{
			AllowAttachments:    false,
			IntroductionMessage: "hi",
		},
	})
	body := mustJSON(map[string]any{"type": "settings"})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	var s poeproto.SettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if s.IntroductionMessage != "hi" {
		t.Fatalf("intro=%q", s.IntroductionMessage)
	}
}

func TestHandler_BearerAuth(t *testing.T) {
	inner := New(Config{HeartbeatInterval: 0})
	gated := poeproto.BearerAuth("secret", inner)
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(mustJSON(map[string]any{"type": "settings"})))
	// No Authorization header → 401.
	rec := httptest.NewRecorder()
	gated.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	// Correct bearer → pass through.
	req2 := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(mustJSON(map[string]any{"type": "settings"})))
	req2.Header.Set("Authorization", "Bearer secret")
	rec2 := httptest.NewRecorder()
	gated.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Ensure io import used (silence unused in some toolchains).
var _ = io.Discard
