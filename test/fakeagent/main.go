// Command fakeagent is a minimal ACP agent used by the graceful-restart
// integration test. It streams text slowly so a turn can be held open
// across a SIGHUP. Behaviour is driven by the latest prompt text:
//
//	contains "wedge"  -> stream nothing, block until cancelled (wedged turn)
//	otherwise         -> stream FAKE_CHUNKS chunks, FAKE_DELAY apart
//
// Env: FAKE_CHUNKS (default 20), FAKE_DELAY (default 1s).
package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

type agent struct {
	conn   *acp.AgentSideConnection
	chunks int
	delay  time.Duration
	mu     sync.Mutex
	n      int
}

func (a *agent) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: 1,
		AgentInfo:       &acp.Implementation{Name: "fakeagent", Version: "0"},
	}, nil
}

func (a *agent) NewSession(_ context.Context, _ acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	a.mu.Lock()
	a.n++
	id := "fake-" + strconv.Itoa(a.n)
	a.mu.Unlock()
	return acp.NewSessionResponse{SessionId: acp.SessionId(id)}, nil
}

func (a *agent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	var text string
	for _, b := range p.Prompt {
		if b.Text != nil {
			text += b.Text.Text
		}
	}
	if strings.Contains(strings.ToLower(text), "wedge") {
		// Wedged turn: emit nothing, block until cancelled.
		<-ctx.Done()
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, ctx.Err()
	}
	for i := 0; i < a.chunks; i++ {
		select {
		case <-ctx.Done():
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, ctx.Err()
		case <-time.After(a.delay):
		}
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: p.SessionId,
			Update: acp.SessionUpdate{
				AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
					Content: acp.TextBlock("chunk-" + strconv.Itoa(i) + " "),
				},
			},
		})
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *agent) Cancel(_ context.Context, _ acp.CancelNotification) error { return nil }
func (a *agent) Authenticate(_ context.Context, _ acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}
func (a *agent) CloseSession(_ context.Context, _ acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}
func (a *agent) ListSessions(_ context.Context, _ acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, nil
}
func (a *agent) ResumeSession(_ context.Context, _ acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, nil
}
func (a *agent) SetSessionConfigOption(_ context.Context, _ acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}
func (a *agent) SetSessionMode(_ context.Context, _ acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}

func main() {
	chunks := 20
	if v, err := strconv.Atoi(os.Getenv("FAKE_CHUNKS")); err == nil && v > 0 {
		chunks = v
	}
	delay := time.Second
	if v, err := time.ParseDuration(os.Getenv("FAKE_DELAY")); err == nil && v > 0 {
		delay = v
	}
	a := &agent{chunks: chunks, delay: delay}
	a.conn = acp.NewAgentSideConnection(a, os.Stdout, os.Stdin)
	<-a.conn.Done()
}
