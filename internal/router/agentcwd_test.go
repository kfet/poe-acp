package router

import (
	"context"
	"errors"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

// cwd_mode=agent: every session call carries the opaque per-conv
// placeholder, never the relay's state dir, and nothing is provisioned on
// the agent's host (a failing Mkdir would otherwise abort creation).
func TestRouter_AgentCwdPlaceholder(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	agent.caps = client.Caps{ListSessions: true, ResumeSession: true}
	agent.resumeErr = errors.New("gone")
	agent.listResult = []client.SessionInfo{{SessionId: "old"}}
	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour, AgentCwd: true,
		Provisioner: &fakeProvisioner{mkdirErr: errors.New("must not provision")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Prompt(context.Background(), "conv-a", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	want := AgentCwdPlaceholder + "/conv-a"
	if len(agent.cwds) != 3 {
		t.Fatalf("cwds=%v want list+resume+new", agent.cwds)
	}
	for _, c := range agent.cwds {
		if c != want {
			t.Fatalf("cwd=%q want %q", c, want)
		}
	}
}
