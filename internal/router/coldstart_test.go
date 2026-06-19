package router

import (
	"context"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

// ctxCheckingAgent wraps fakeAgent and fails NewSession if the context it
// receives is already canceled. Proves getOrCreate uses the decoupled
// acquisition context, not the (possibly canceled) caller ctx.
type ctxCheckingAgent struct {
	*fakeAgent
}

func (a *ctxCheckingAgent) NewSession(ctx context.Context, cwd string, sink client.SessionUpdateSink, sysBlocks []acp.ContentBlock) (acp.SessionId, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return a.fakeAgent.NewSession(ctx, cwd, sink, sysBlocks)
}

func TestRouter_GetOrCreate_NewSessionSurvivesCallerCancel(t *testing.T) {
	fa := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: fa, StateDir: t.TempDir(), SessionTTL: time.Hour})
	r.cfg.Agent = &ctxCheckingAgent{fakeAgent: fa}

	// Caller ctx is already canceled, mimicking Poe dropping the bot-facing
	// HTTP connection during a cold start. Acquisition must still succeed
	// because getOrCreate derives a WithoutCancel acquisition context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	st, _, err := r.getOrCreate(ctx, "c1", "u", []Turn{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("cold start must survive caller cancellation: %v", err)
	}
	if st.sessionID == "" {
		t.Fatalf("expected a session to be created")
	}
}

func TestRouter_SessionCreateTimeoutDefault(t *testing.T) {
	fa := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: fa, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if r.cfg.SessionCreateTimeout != defaultSessionCreateTimeout {
		t.Fatalf("want default %s, got %s", defaultSessionCreateTimeout, r.cfg.SessionCreateTimeout)
	}
}
