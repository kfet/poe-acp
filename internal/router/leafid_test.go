package router

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/client"
)

// leafAgent is a forkAgent that also reports per-turn leaf ids
// (client.TurnPrompter): turn n ends at "leaf-n".
type leafAgent struct {
	*forkAgent
	turns int32
}

func (l *leafAgent) PromptTurn(ctx context.Context, sid acp.SessionId, blocks []acp.ContentBlock) (client.TurnResult, error) {
	stop, err := l.Prompt(ctx, sid, blocks)
	if err != nil {
		return client.TurnResult{}, err
	}
	return client.TurnResult{Stop: stop, LeafID: fmt.Sprintf("leaf-%d", atomic.AddInt32(&l.turns, 1))}, nil
}

// The parent runs one turn per Poe query; a branch from the reply to its
// FIRST turn forks at that turn's leaf instead of reseeding.
func TestFork_EarlierBranchForksAtRecordedLeaf(t *testing.T) {
	a := &leafAgent{forkAgent: newForkAgent()}
	a.caps.ForkAt = true
	r := forkRouter(t, a)
	for _, n := range []int{1, 3, 5} {
		if err := r.Prompt(context.Background(), "parent", "u", parentTurns[:n], Options{}, &captureSink{}); err != nil {
			t.Fatal(err)
		}
	}
	r.mu.Lock()
	leaves := r.sessions["parent"].turnLeaves
	r.mu.Unlock()
	if leaves["m1"] != "leaf-1" || leaves["m2"] != "leaf-2" || leaves["m3"] != "leaf-3" {
		t.Fatalf("turnLeaves = %v", leaves)
	}
	if err := r.Prompt(context.Background(), "child", "u", branchQuery(parentTurns[:2]), Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&a.forkCalls) != 1 || a.lastAt != "leaf-1" {
		t.Fatalf("forkCalls=%d at=%q, want a fork at leaf-1", a.forkCalls, a.lastAt)
	}
}

// A failed turn records no leaf.
func TestPromptTurn_FailedTurnRecordsNoLeaf(t *testing.T) {
	a := &leafAgent{forkAgent: newForkAgent()}
	a.onPrompt = func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return "", errors.New("boom")
	}
	r := forkRouter(t, a)
	_ = r.Prompt(context.Background(), "parent", "u", parentTurns[:1], Options{}, &captureSink{})
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.sessions["parent"]; st != nil && len(st.turnLeaves) != 0 {
		t.Fatalf("turnLeaves = %v", st.turnLeaves)
	}
}
