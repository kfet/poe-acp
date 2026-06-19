package httpsrv

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/poe-acp/internal/router"
)

// startSignalAgent signals when Prompt is entered, then blocks until the
// test releases it. With the gated turn-decouple a pre-output client
// disconnect is absorbed (not cancelled), so the turn ctx is NOT cancelled
// mid-turn — the test drives completion via the release channel instead.
type startSignalAgent struct {
	*fakeAgent
	started chan struct{}
	release chan struct{}
}

func (a *startSignalAgent) Prompt(_ context.Context, _ acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
	close(a.started)
	<-a.release
	return acp.StopReasonEndTurn, nil
}

func runFastCancel(t *testing.T, threshold time.Duration) string {
	t.Helper()
	orig := fastCancelThreshold
	fastCancelThreshold = threshold
	defer func() { fastCancelThreshold = orig }()

	var buf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(origOut)

	sa := &startSignalAgent{fakeAgent: &fakeAgent{}, started: make(chan struct{}), release: make(chan struct{})}
	rtr, err := router.New(router.Config{Agent: sa, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 0})

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1", "user_id": "u", "message_id": "m",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)).WithContext(ctx)
		h.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	<-sa.started
	cancel()
	// Pre-output disconnect is absorbed: release the agent so the
	// decoupled turn completes and ServeHTTP returns.
	close(sa.release)
	<-done

	// The disconnect-watch goroutine may log after ServeHTTP returns; give
	// it a brief deterministic window to settle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if buf.Len() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return buf.String()
}

func TestHandler_FastCancelLogsWarn(t *testing.T) {
	// Threshold large: any cancel counts as "fast" -> WARN logged.
	out := runFastCancel(t, time.Hour)
	if !strings.Contains(out, "WARN fast client disconnect") {
		t.Fatalf("expected fast-cancel WARN, got: %q", out)
	}
}

func TestHandler_SlowCancelNoWarn(t *testing.T) {
	// Threshold tiny: elapsed >= threshold -> no WARN.
	out := runFastCancel(t, time.Nanosecond)
	if strings.Contains(out, "WARN fast client disconnect") {
		t.Fatalf("did not expect fast-cancel WARN, got: %q", out)
	}
}
