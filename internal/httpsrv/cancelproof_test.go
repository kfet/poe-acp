package httpsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/poe-acp/internal/router"
)

// stubAgentEnv makes a child invocation of this test binary run as a tiny
// ACP agent over stdin/stdout instead of running the test suite.
// stubCancelFileEnv names the file the stub touches when it receives a
// session/cancel notification — the whole point of the exercise.
const (
	stubAgentEnv      = "POE_ACP_STUB_AGENT"
	stubCancelFileEnv = "POE_ACP_STUB_CANCEL_FILE"
)

func TestMain(m *testing.M) {
	if os.Getenv(stubAgentEnv) == "1" {
		runStubAgent()
		return
	}
	os.Exit(m.Run())
}

// runStubAgent serves ACP on stdio as a WEDGED agent: it accepts a
// session, then blocks on session/prompt emitting nothing at all. It
// answers session/cancel by touching the witness file and releasing the
// hung prompt. This is the real agent-side observer for the idle-cut
// path: nothing but an actual `session/cancel` on the wire can create
// that file.
func runStubAgent() {
	var once sync.Once
	cancelled := make(chan struct{})
	handler := func(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{
				"protocolVersion":   acp.ProtocolVersionNumber,
				"agentCapabilities": map[string]any{},
			}, nil
		case acp.AgentMethodSessionNew:
			return map[string]any{"sessionId": "stub-sess"}, nil
		case acp.AgentMethodSessionPrompt:
			<-cancelled
			return map[string]any{"stopReason": "cancelled"}, nil
		case acp.AgentMethodSessionCancel:
			once.Do(func() {
				if f := os.Getenv(stubCancelFileEnv); f != "" {
					_ = os.WriteFile(f, []byte("cancelled"), 0o600)
				}
				close(cancelled)
			})
			return nil, nil
		}
		return nil, acp.NewMethodNotFound(method)
	}
	_ = acp.NewConnection(handler, os.Stdout, os.Stdin)
	select {} // block until the parent kills us
}

// TestIdleCut_SendsSessionCancelToRealAgent is the Stage 0 proof.
//
// Before acp-kit v0.16.2 the idle-write backstop cancelled the turn
// context and `AgentProc.Prompt` merely ABANDONED the JSON-RPC request:
// the client stopped waiting, and the agent — never told anything — went
// on running its tool. Measured in the wild on a sibling relay: a file
// written a minute after the relay had given up.
//
// This drives the WHOLE poe-acp stack (Handler → Router → real
// *client.AgentProc → a stub agent subprocess) and asserts a real
// `session/cancel` reaches the agent process when the idle cut fires. It
// is deliberately not a fake-Agent test: the fix lives inside
// AgentProc.Prompt, so only a real one proves poe-acp is on that path.
func TestIdleCut_SendsSessionCancelToRealAgent(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	witness := filepath.Join(t.TempDir(), "cancel-witness")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agent, err := client.Start(ctx, client.Config{
		Command: []string{exe},
		Env: append(os.Environ(),
			stubAgentEnv+"=1",
			stubCancelFileEnv+"="+witness,
		),
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("start stub agent: %v", err)
	}
	defer func() { _ = agent.Close() }()

	rtr, err := router.New(router.Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, IdleWriteTimeout: 150 * time.Millisecond})
	fired := make(chan struct{})
	h.idleWriteCancelHook = func() { close(fired) }

	srv := httptest.NewServer(h)
	defer srv.Close()

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c-cancelproof",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
		resp, derr := http.DefaultClient.Do(req)
		if derr == nil {
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-fired:
	case <-time.After(20 * time.Second):
		t.Fatal("idle-write backstop never fired")
	}
	// The witness can only appear via a session/cancel on the wire.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, statErr := os.Stat(witness); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent was never told to cancel — its tool would run on")
		}
		time.Sleep(10 * time.Millisecond)
	}
	<-done
}
