package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

// TestTurnEndNote_OnlyRelayCutsSpeak locks the rule that decides whether
// a user sees anything when a turn's context ends.
//
// A plain cancel is Poe dropping the bot-facing connection mid-turn (the
// cold-start redrive race): the redrive carries the real answer, and any
// note here is what Poe renders as the spurious "first response failed".
// A relay-initiated cut is the opposite — silence there reads as a lost
// answer, because nothing else ever tells the user their turn was
// abandoned.
func TestTurnEndNote_OnlyRelayCutsSpeak(t *testing.T) {
	if got := turnEndNote(context.Canceled); got != "" {
		t.Fatalf("a plain cancel must stay silent, got %q", got)
	}
	if got := turnEndNote(nil); got != "" {
		t.Fatalf("no cause must stay silent, got %q", got)
	}
	if got := turnEndNote(errors.New("some agent error")); got != "" {
		t.Fatalf("an unrelated cause must stay silent, got %q", got)
	}
	// The cause carries its own sentence — the layer that owns the
	// timeouts is the one that knows the numbers — so the note must
	// RENDER it rather than substitute wording of its own.
	wedged := fmt.Errorf("%w: no output for 2m0s", client.ErrNoProgress)
	if got := turnEndNote(wedged); !strings.Contains(got, "no output for 2m0s") {
		t.Fatalf("the note must carry the cause's own sentence, got %q", got)
	}
	ceiling := fmt.Errorf("%w: hit the 10m0s ceiling", client.ErrTurnCeiling)
	if got := turnEndNote(ceiling); !strings.Contains(got, "hit the 10m0s ceiling") {
		t.Fatalf("the ceiling note must carry the cause's own sentence, got %q", got)
	}
}

// TestRunOneTurn_WedgeCutSpeaksToTheUser is the end-to-end half: a turn
// whose context is cut with an ErrNoProgress cause finalises with a note
// in the answer body, not in silence.
//
// The counterpart — a plain cancel finalising silently — is asserted by
// the existing cancel/drop tests, which would break loudly if this path
// ever started speaking on them.
func TestRunOneTurn_WedgeCutSpeaksToTheUser(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	agent := newFakeAgent(func(ctx context.Context, _ *fakeAgent, _ acp.SessionId, _ string) (acp.StopReason, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return acp.StopReasonCancelled, ctx.Err()
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})

	// The handler cuts a wedged turn exactly like this: cancel the turn
	// context with a cause that unwraps to client.ErrNoProgress and
	// carries the window that expired.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	sink := &captureSink{}
	done := make(chan error, 1)
	go func() {
		done <- r.Prompt(ctx, "c-wedge", "u",
			[]Turn{{Role: "user", Content: "hi", MessageID: "m1"}}, Options{}, sink)
	}()
	<-entered
	cancel(fmt.Errorf("%w: no output for 2m0s", client.ErrNoProgress))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the cut turn never unwound")
	}
	sink.mu.Lock()
	body := sink.text.String()
	sealed := sink.done
	sink.mu.Unlock()
	if !strings.Contains(body, "no output for 2m0s") {
		t.Fatalf("a cut wedged turn must say why, got %q", body)
	}
	if !sealed {
		t.Fatal("the stream must still be sealed")
	}
}
