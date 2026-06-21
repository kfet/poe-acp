package httpsrv

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/poe-acp/internal/router"
)

// blockAgent delays its first (and only) content chunk until release is
// closed, so no agent output lands during the first-frame measurement
// window. This isolates the heartbeat/spinner path: the only way a
// content SSE event can reach the client before the agent speaks is the
// heartbeat emitting a `replace_response` spinner frame.
type blockAgent struct {
	*fakeAgent
	release chan struct{}
}

func (a *blockAgent) Prompt(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
	a.fakeAgent.mu.Lock()
	sink := a.fakeAgent.sinks[sid]
	a.fakeAgent.mu.Unlock()
	<-a.release
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

// TestHandler_FirstContentFrameIsPrompt is the regression guard for the
// pre-output content-starvation bug. Poe drops a new-conversation bot
// connection if it sees only the SSE preamble + `meta` (a non-content
// event) and no real *content* event within its tolerance window. The
// first content event is a heartbeat-driven `replace_response` spinner
// frame — which, on the buggy code, is not emitted until the heartbeat
// ticker's FIRST tick at t = HeartbeatInterval (default 1500ms).
//
// This test configures a measurable HeartbeatInterval (1s) and a
// blocked agent (no agent output during the window), then asserts the
// first `replace_response` frame arrives FAR below one heartbeat
// interval. On current code the first frame lands at ~1000ms → RED;
// after the fix (emit tick #0 immediately) it lands at ~0ms → GREEN.
func TestHandler_FirstContentFrameIsPrompt(t *testing.T) {
	const hb = 1 * time.Second
	const bound = 250 * time.Millisecond

	a := &blockAgent{fakeAgent: &fakeAgent{}, release: make(chan struct{})}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: hb})

	srv := httptest.NewServer(h)
	defer srv.Close()

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c-ff",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	// Read the raw SSE stream frame-by-frame, timestamping the first
	// `event: meta` and the first heartbeat-shaped `event:
	// replace_response` relative to it.
	var metaAt, firstContentAt time.Time
	r := bufio.NewReader(resp.Body)
	var cur strings.Builder
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		line, rerr := r.ReadString('\n')
		cur.WriteString(line)
		if line == "\n" { // frame boundary
			frame := cur.String()
			cur.Reset()
			now := time.Now()
			if strings.Contains(frame, "event: meta") {
				metaAt = now
			}
			if strings.Contains(frame, "event: replace_response") {
				firstContentAt = now
				break
			}
			continue
		}
		if rerr != nil {
			break
		}
	}
	// Release the blocked agent so the turn finishes cleanly (no leaked
	// goroutine) regardless of assertion outcome.
	close(a.release)

	if metaAt.IsZero() {
		t.Fatal("never received meta event")
	}
	if firstContentAt.IsZero() {
		t.Fatal("never received a content (replace_response) frame")
	}
	delta := firstContentAt.Sub(metaAt)
	if delta >= bound {
		t.Fatalf("first content frame arrived %s after meta; want < %s (heartbeat interval is %s) — content-starvation window open",
			delta.Round(time.Millisecond), bound, hb)
	}

	// Drain the rest so the server-side handler completes.
	_, _ = r.ReadString(0)
}
