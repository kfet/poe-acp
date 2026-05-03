package httpsrv

import (
	"bytes"
	"context"
	"encoding/json"
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
	mu         sync.Mutex
	sinks      map[acp.SessionId]acpclient.SessionUpdateSink
	lastPrompt []acp.ContentBlock
	n          int
}

func (f *fakeAgent) Caps() acpclient.Caps { return acpclient.Caps{} }
func (f *fakeAgent) ListSessions(_ context.Context, _ string) ([]acpclient.SessionInfo, error) {
	return nil, nil
}
func (f *fakeAgent) ResumeSession(_ context.Context, _ string, _ acp.SessionId, _ acpclient.SessionUpdateSink) error {
	return nil
}
func (f *fakeAgent) NewSession(_ context.Context, _ string, sink acpclient.SessionUpdateSink, _ []acp.ContentBlock) (acp.SessionId, error) {
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
func (f *fakeAgent) Prompt(_ context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (acp.StopReason, error) {
	f.mu.Lock()
	sink := f.sinks[sid]
	f.lastPrompt = prompt
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
func (f *fakeAgent) SetModel(_ context.Context, _ acp.SessionId, _ string) error {
	return nil
}
func (f *fakeAgent) SetConfigOption(_ context.Context, _ acp.SessionId, _, _ string) error {
	return nil
}

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

func TestHandler_Settings_ParameterControls(t *testing.T) {
	models := []acpclient.ModelInfo{
		{ID: "anthropic/claude-sonnet-4-5", Name: "Sonnet"},
		{ID: "openai/gpt-5", Name: "GPT-5"},
	}
	h := New(Config{
		Settings: poeproto.SettingsResponse{IntroductionMessage: "hi"},
		ParameterControlsProvider: func() *poeproto.ParameterControls {
			opts := make([]poeproto.ValueNamePair, 0, len(models))
			for _, m := range models {
				opts = append(opts, poeproto.ValueNamePair{Value: m.ID, Name: m.Name})
			}
			return &poeproto.ParameterControls{
				Sections: []poeproto.Section{{
					Name: "Options",
					Controls: []poeproto.Control{
						{Control: "drop_down", Label: "Model", ParameterName: "model",
							DefaultValue: "anthropic/claude-sonnet-4-5", Options: opts},
						{Control: "drop_down", Label: "Thinking", ParameterName: "thinking",
							DefaultValue: "medium"},
						{Control: "toggle_switch", Label: "Hide thinking output",
							ParameterName: "hide_thinking", DefaultValue: false},
					},
				}},
			}
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/poe",
		bytes.NewReader(mustJSON(map[string]any{"type": "settings"})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.Bytes()
	// Must be valid JSON.
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("invalid json: %v\nbody: %s", err, raw)
	}
	// parameter_controls present.
	pc, ok := resp["parameter_controls"].(map[string]any)
	if !ok {
		t.Fatalf("parameter_controls absent or wrong type; body=%s", raw)
	}
	// Sections present and non-empty.
	sections, _ := pc["sections"].([]any)
	if len(sections) == 0 {
		t.Fatalf("sections empty; body=%s", raw)
	}
	sec := sections[0].(map[string]any)
	controls, _ := sec["controls"].([]any)
	if len(controls) < 3 {
		t.Fatalf("expected >=3 controls, got %d; body=%s", len(controls), raw)
	}
	// Verify parameter_name fields.
	names := map[string]bool{}
	for _, c := range controls {
		cm := c.(map[string]any)
		if n, ok := cm["parameter_name"].(string); ok {
			names[n] = true
		}
	}
	for _, want := range []string{"model", "thinking", "hide_thinking"} {
		if !names[want] {
			t.Fatalf("missing control %q; got %v\nbody=%s", want, names, raw)
		}
	}
	// Model dropdown options populated from provider.
	var modelCtl map[string]any
	for _, c := range controls {
		cm := c.(map[string]any)
		if cm["parameter_name"] == "model" {
			modelCtl = cm
		}
	}
	if modelCtl == nil {
		t.Fatal("model control missing")
	}
	opts, _ := modelCtl["options"].([]any)
	if len(opts) != 2 {
		t.Fatalf("expected 2 model options, got %d", len(opts))
	}
}

func TestHandler_Query_ParametersForwardedToAgent(t *testing.T) {
	// Track what SetModel and SetConfigOption are called with.
	type call struct{ method, arg string }
	var (
		mu    sync.Mutex
		calls []call
	)

	fa := &trackingAgent{
		fakeAgent: &fakeAgent{},
		onSetModel: func(id string) {
			mu.Lock()
			calls = append(calls, call{"set_model", id})
			mu.Unlock()
		},
		onSetConfig: func(cid, val string) {
			mu.Lock()
			calls = append(calls, call{cid, val})
			mu.Unlock()
		},
	}

	rtr, err := router.New(router.Config{
		Agent: fa, StateDir: t.TempDir(), SessionTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 0})

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "cx", "user_id": "u", "message_id": "m",
		"query": []map[string]any{{
			"role":    "user",
			"content": "hello",
			"parameters": map[string]any{
				"model":    "anthropic/claude-sonnet-4-5",
				"thinking": "high",
			},
		}},
	})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "event: done") {
		t.Fatalf("response incomplete: %s", rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	wantModel := call{"set_model", "anthropic/claude-sonnet-4-5"}
	wantThinking := call{"thinking_level", "high"}
	found := map[call]bool{}
	for _, c := range calls {
		found[c] = true
	}
	if !found[wantModel] {
		t.Errorf("set_model not called; calls=%v", calls)
	}
	if !found[wantThinking] {
		t.Errorf("set_config thinking_level not called; calls=%v", calls)
	}
}

// trackingAgent wraps fakeAgent and records SetModel/SetConfigOption calls.
type trackingAgent struct {
	*fakeAgent
	onSetModel  func(string)
	onSetConfig func(string, string)
}

func (a *trackingAgent) SetModel(_ context.Context, _ acp.SessionId, id string) error {
	if a.onSetModel != nil {
		a.onSetModel(id)
	}
	return nil
}
func (a *trackingAgent) SetConfigOption(_ context.Context, _ acp.SessionId, cid, val string) error {
	if a.onSetConfig != nil {
		a.onSetConfig(cid, val)
	}
	return nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// slowAgent delays Prompt until release is closed, then emits one chunk.
// Used to give the heartbeat goroutine time to tick.
type slowAgent struct {
	*fakeAgent
	release chan struct{}
	chunk   string
	thought bool
}

func (a *slowAgent) Prompt(ctx context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
	select {
	case <-a.release:
	case <-ctx.Done():
		return acp.StopReasonCancelled, ctx.Err()
	}
	a.fakeAgent.mu.Lock()
	sink := a.fakeAgent.sinks[sid]
	a.fakeAgent.mu.Unlock()
	upd := acp.SessionUpdate{}
	if a.thought {
		upd.AgentThoughtChunk = &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock(a.chunk)}
	} else {
		upd.AgentMessageChunk = &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock(a.chunk)}
	}
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{SessionId: sid, Update: upd})
	return acp.StopReasonEndTurn, nil
}

func TestHandler_HideThinkingSpinner(t *testing.T) {
	sa := &slowAgent{fakeAgent: &fakeAgent{}, release: make(chan struct{}), chunk: "answer"}
	rtr, err := router.New(router.Config{Agent: sa, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 5 * time.Millisecond})

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1", "user_id": "u", "message_id": "m",
		"query": []map[string]any{{
			"role": "user", "content": "hi",
			"parameters": map[string]any{"hide_thinking": true},
		}},
	})

	// Run in background so we can release the agent after a few ticks.
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
		h.ServeHTTP(rec, req)
		close(done)
	}()
	time.Sleep(40 * time.Millisecond)
	close(sa.release)
	<-done

	out := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, out)
	}
	if !strings.Contains(out, "event: replace_response") {
		t.Fatalf("missing replace_response (spinner): %s", out)
	}
	if !strings.Contains(out, `"text":"\u003e _Thinking.`) && !strings.Contains(out, `"text":"> _Thinking.`) {
		t.Fatalf("missing Thinking spinner text: %s", out)
	}
	// Spinner must be cleared (empty replace_response) before final answer.
	if !strings.Contains(out, `"text":""`) {
		t.Fatalf("missing spinner clear: %s", out)
	}
	if !strings.Contains(out, `"text":"answer"`) {
		t.Fatalf("missing real answer: %s", out)
	}
	if !strings.Contains(out, "event: done") {
		t.Fatalf("missing done: %s", out)
	}
}

func TestHandler_NoSpinnerWhenThinkingVisible(t *testing.T) {
	sa := &slowAgent{fakeAgent: &fakeAgent{}, release: make(chan struct{}), chunk: "answer"}
	rtr, err := router.New(router.Config{Agent: sa, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 5 * time.Millisecond})

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1", "user_id": "u", "message_id": "m",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
		h.ServeHTTP(rec, req)
		close(done)
	}()
	time.Sleep(40 * time.Millisecond)
	close(sa.release)
	<-done

	out := rec.Body.String()
	if strings.Contains(out, "Thinking") {
		t.Fatalf("unexpected Thinking text when hide_thinking=false: %s", out)
	}
	// Heartbeat should still tick — now as empty replace_response
	// events rather than zero-width-space text appends, so the keep-
	// alive bytes never accumulate in the rendered response.
	if !strings.Contains(out, "event: replace_response") || !strings.Contains(out, `"text":""`) {
		t.Fatalf("missing replace_response heartbeat: %s", out)
	}
	if strings.Contains(out, "\u200b") {
		t.Fatalf("zero-width heartbeat must not appear in rendered output: %s", out)
	}
}

func TestHandler_StripsAttachmentsWhenDisallowed(t *testing.T) {
	mkRequest := func() []byte {
		return mustJSON(map[string]any{
			"type":            "query",
			"conversation_id": "c-att",
			"user_id":         "u",
			"message_id":      "m",
			"query": []map[string]any{
				{"role": "user", "content": "hi", "attachments": []map[string]any{
					{"url": "https://poe.example/a.png", "name": "a.png", "content_type": "image/png"},
				}},
			},
		})
	}

	for _, tc := range []struct {
		name      string
		allow     bool
		wantLinks int
	}{
		{"allowed", true, 1},
		{"disallowed", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := &fakeAgent{sinks: make(map[acp.SessionId]acpclient.SessionUpdateSink)}
			rtr, err := router.New(router.Config{
				Agent:      fa,
				StateDir:   t.TempDir(),
				SessionTTL: time.Hour,
			})
			if err != nil {
				t.Fatal(err)
			}
			h := New(Config{
				Router:            rtr,
				HeartbeatInterval: 0,
				Settings:          poeproto.SettingsResponse{AllowAttachments: tc.allow},
			})
			req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(mkRequest()))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			fa.mu.Lock()
			defer fa.mu.Unlock()
			var links int
			for _, b := range fa.lastPrompt {
				if b.ResourceLink != nil || b.Resource != nil {
					links++
				}
			}
			if links != tc.wantLinks {
				t.Fatalf("links=%d want %d (allow=%v)", links, tc.wantLinks, tc.allow)
			}
		})
	}
}
