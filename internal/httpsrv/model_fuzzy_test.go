package httpsrv

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/command"

	"github.com/kfet/poe-acp/internal/router"
)

// modelAgent is a fakeAgent with a model catalogue that records SetModel.
type modelAgent struct {
	fakeAgent
	mu  sync.Mutex
	set []string
}

func (m *modelAgent) Models() ([]client.ModelInfo, string) {
	return []client.ModelInfo{
		{ID: "anthropic/claude-opus-5-5"},
		{ID: "anthropic/claude-sonnet-5"},
		{ID: "openai/gpt-6"},
		{ID: "openai/gpt-6-mini"},
	}, "openai/gpt-6"
}

func (m *modelAgent) SetModel(_ context.Context, _ acp.SessionId, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.set = append(m.set, id)
	return nil
}

func (m *modelAgent) sets() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.set...)
}

func sendCmd(t *testing.T, h http.Handler, conv, text string) string {
	t.Helper()
	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": conv, "user_id": "u1", "message_id": "m1",
		"query": []map[string]any{{"role": "user", "content": text}},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func newModelHandler(t *testing.T) (http.Handler, *modelAgent) {
	t.Helper()
	ag := &modelAgent{}
	rtr, err := router.New(router.Config{
		Broker: command.New(nil), Agent: ag, StateDir: t.TempDir(), SessionTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{Router: rtr, Commands: rtr.Convo()}), ag
}

func TestHandler_ModelFuzzyResolve(t *testing.T) {
	cases := []struct {
		name, text, wantSet string
		wantOut             []string
	}{
		{"fuzzy alias", "!m anth/opus55", "anthropic/claude-opus-5-5", []string{"`anth/opus55` → `anthropic/claude-opus-5-5`", "Model set to `anthropic/claude-opus-5-5`"}},
		{"exact id", "!model openai/gpt-6-mini", "openai/gpt-6-mini", []string{"openai/gpt-6-mini"}},
		{"ambiguous", "!m gpt", "", []string{"openai/gpt-6", "openai/gpt-6-mini"}},
		{"bare alias lists", "!m", "", []string{"anthropic/claude-sonnet-5", "openai/gpt-6-mini"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, ag := newModelHandler(t)
			out := sendCmd(t, h, "c-"+tc.name, tc.text)
			for _, w := range tc.wantOut {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q: %s", w, out)
				}
			}
			// The override applies from the next prompt.
			sendCmd(t, h, "c-"+tc.name, "hi")
			got := ag.sets()
			if tc.wantSet == "" {
				if len(got) != 0 {
					t.Errorf("unexpected SetModel %v", got)
				}
			} else if len(got) == 0 || got[len(got)-1] != tc.wantSet {
				t.Errorf("SetModel = %v, want %q", got, tc.wantSet)
			}
		})
	}
}

func TestHandler_ModelAliasNotPrefix(t *testing.T) {
	for _, text := range []string{"!me opus", "!msg opus"} {
		h, ag := newModelHandler(t)
		sendCmd(t, h, "c", text)
		if got := ag.sets(); len(got) != 0 {
			t.Errorf("%s switched model: %v", text, got)
		}
	}
}

func TestHandler_ModelQueryLogged(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	h, _ := newModelHandler(t)
	sendCmd(t, h, "c", "!m anth/opus55")
	sendCmd(t, h, "c", "!model openai/gpt-6")
	sendCmd(t, h, "c", "!m gpt")
	sendCmd(t, h, "c", "!status")
	out := buf.String()
	for _, w := range []string{
		`query="anth/opus55" resolved="anthropic/claude-opus-5-5" exact=false`,
		`query="openai/gpt-6" resolved="openai/gpt-6" exact=true`,
		`query="gpt" resolved="" candidates=2`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("log missing %q:\n%s", w, out)
		}
	}
	if strings.Count(out, "MODEL ") != 3 {
		t.Errorf("want 3 MODEL lines:\n%s", out)
	}
}
