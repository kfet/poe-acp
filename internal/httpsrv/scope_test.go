package httpsrv

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/origin"
	"github.com/kfet/poe-acp/internal/router"
)

// originAgent records the origin each session/new and session/prompt ctx
// carried, and gives every session a distinct id.
type originAgent struct {
	fakeAgent
	omu     sync.Mutex
	news    []origin.Origin
	prompts []origin.Origin
	texts   []string
}

func (f *originAgent) NewSessionWithMeta(ctx context.Context, _ string, sink client.SessionUpdateSink, _ []acp.ContentBlock, _ map[string]any) (acp.SessionId, error) {
	o, _ := origin.From(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.sinks == nil {
		f.sinks = make(map[acp.SessionId]client.SessionUpdateSink)
	}
	id := acp.SessionId("s-" + string(rune('0'+f.n)))
	f.sinks[id] = sink
	f.omu.Lock()
	f.news = append(f.news, o)
	f.omu.Unlock()
	return id, nil
}

func (f *originAgent) Prompt(ctx context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (acp.StopReason, error) {
	o, _ := origin.From(ctx)
	f.omu.Lock()
	f.prompts = append(f.prompts, o)
	if len(prompt) > 0 && prompt[0].Text != nil {
		f.texts = append(f.texts, prompt[0].Text.Text)
	}
	f.omu.Unlock()
	return f.fakeAgent.Prompt(ctx, sid, prompt)
}

func scopeHandler(t *testing.T, cfg Config) (*Handler, *originAgent, *router.Router) {
	t.Helper()
	a := &originAgent{}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour, Broker: command.New(noAuth{})})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Router = rtr
	cfg.Commands = rtr.Convo()
	return New(cfg), a, rtr
}

func postQuery(t *testing.T, h *Handler, conv, user, text string) string {
	t.Helper()
	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": conv, "user_id": user, "message_id": "bm-" + user,
		"query": []map[string]any{{"role": "user", "content": text, "message_id": "um-" + user + "-" + text}},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func TestHandler_OriginOnSessionCalls(t *testing.T) {
	h, a, rtr := scopeHandler(t, Config{})
	postQuery(t, h, "c1", "u1", "ping")
	postQuery(t, h, "c1", "u2", "ping")
	want := origin.Origin{Platform: "poe", UserID: "u1", ThreadID: "c1", MessageID: "um-u1-ping", IsDM: true}
	if len(a.news) != 1 || a.news[0] != want {
		t.Fatalf("new origins=%+v want one %+v", a.news, want)
	}
	if len(a.prompts) != 2 || a.prompts[0] != want || a.prompts[1].UserID != "u2" {
		t.Fatalf("prompt origins=%+v", a.prompts)
	}
	if rtr.Len() != 1 {
		t.Fatalf("thread scope: sessions=%d want 1", rtr.Len())
	}
}

func TestHandler_ThreadUserScope(t *testing.T) {
	h, a, rtr := scopeHandler(t, Config{SessionScope: SessionScopeThreadUser})
	postQuery(t, h, "c1", "u1", "ping")
	postQuery(t, h, "c1", "u2", "ping")
	postQuery(t, h, "c1", "u1", "again")
	if rtr.Len() != 2 || len(a.news) != 2 {
		t.Fatalf("sessions=%d news=%d want 2 each", rtr.Len(), len(a.news))
	}
	for i, u := range []string{"u1", "u2"} {
		if a.news[i].UserID != u || a.news[i].ThreadID != "c1" {
			t.Fatalf("news[%d]=%+v", i, a.news[i])
		}
	}
	// A reaction is routed to its sender's session too.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(mustJSON(map[string]any{
		"type": "report_reaction", "conversation_id": "c1", "user_id": "u3", "message_id": "m9", "reaction": "like",
	}))))
	if rtr.Len() != 3 {
		t.Fatalf("reaction: sessions=%d want 3", rtr.Len())
	}
}

func TestSessionKey(t *testing.T) {
	for _, tc := range []struct{ scope, conv, user, want string }{
		{"", "c", "u", "c"},
		{"thread", "c", "u", "c"},
		{SessionScopeThreadUser, "c", "u", "c/u"},
		{SessionScopeThreadUser, "a/b", "c", "a%2Fb/c"},
		{SessionScopeThreadUser, "c", "", "c"},
	} {
		if got := SessionKey(tc.scope, tc.conv, tc.user); got != tc.want {
			t.Errorf("SessionKey(%q,%q,%q)=%q want %q", tc.scope, tc.conv, tc.user, got, tc.want)
		}
	}
}

func TestHandler_AgentCwdLoginGoesToSenderSession(t *testing.T) {
	h, a, _ := scopeHandler(t, Config{AgentCwd: true})
	postQuery(t, h, "c1", "u1", "!login")
	if len(a.texts) != 1 || a.texts[0] != "/login" || a.prompts[0].UserID != "u1" {
		t.Fatalf("texts=%q prompts=%+v", a.texts, a.prompts)
	}
	for in, want := range map[string]string{"!cancel-login": "/login cancel", " .logins": "/logins", "/login x": "/login x"} {
		if got := agentLoginCommand(in); got != want {
			t.Errorf("agentLoginCommand(%q)=%q want %q", in, got, want)
		}
	}
	// Without cwd_mode=agent the relay broker answers it; no prompt.
	h2, a2, _ := scopeHandler(t, Config{})
	postQuery(t, h2, "c1", "u1", "!login")
	if len(a2.texts) != 0 {
		t.Fatalf("local mode forwarded login: %q", a2.texts)
	}
}

type noAuth struct{}

func (noAuth) AuthMethods() []client.AuthMethod { return nil }
func (noAuth) Authenticate(context.Context, string, string, string, bool) (client.AuthResult, error) {
	return client.AuthResult{}, nil
}
