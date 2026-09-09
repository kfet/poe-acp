package httpsrv

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/poe-acp/internal/router"
)

// TestCauses_WrapTheKitSentinels locks the cross-relay contract: poe-acp
// classifies a cut turn with the SAME sentinels slack-acp and zulip-acp
// do, so `errors.Is(context.Cause(ctx), client.ErrNoProgress)` means the
// same thing in all three. The wrapper exists only to carry the window,
// which the relay layer knows and the router does not.
func TestCauses_WrapTheKitSentinels(t *testing.T) {
	np := noProgressCause{window: 2 * time.Minute}
	if !errors.Is(np, client.ErrNoProgress) {
		t.Fatal("noProgressCause must satisfy errors.Is(client.ErrNoProgress)")
	}
	if !strings.Contains(np.Error(), "2m0s") {
		t.Fatalf("the note must name the window that expired: %q", np.Error())
	}
	tc := turnCeilingCause{ceiling: 10 * time.Minute}
	if !errors.Is(tc, client.ErrTurnCeiling) {
		t.Fatal("turnCeilingCause must satisfy errors.Is(client.ErrTurnCeiling)")
	}
	if !strings.Contains(tc.Error(), "10m0s") {
		t.Fatalf("the note must name the ceiling: %q", tc.Error())
	}
	// The two must stay distinguishable: a wedged agent and an operator
	// cap are different things to say to a user.
	if errors.Is(np, client.ErrTurnCeiling) || errors.Is(tc, client.ErrNoProgress) {
		t.Fatal("the two causes must not alias each other")
	}
}

// TestHandler_WedgedTurnTellsTheUserWhy is the Stage 1 user-visible
// upgrade. Before this the idle cut was a bare cancel: the router could
// not tell it from Poe's mid-turn transport drop, so it finalised
// SILENTLY and the user watched their answer simply stop. Now the cut
// carries a cause and the stream says what happened.
func TestHandler_WedgedTurnTellsTheUserWhy(t *testing.T) {
	a := &wedgeAgent{fakeAgent: &fakeAgent{}, returned: make(chan struct{})}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, IdleWriteTimeout: 40 * time.Millisecond})

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c-wedge-note",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	out := rec.Body.String()
	if !strings.Contains(out, "it looks wedged") {
		t.Fatalf("a cut wedged turn must say why:\n%s", out)
	}
	if !strings.Contains(out, "40ms") {
		t.Fatalf("the note must name the window that expired:\n%s", out)
	}
	if !strings.Contains(out, "event: done") {
		t.Fatalf("the stream must still be sealed:\n%s", out)
	}
	<-a.returned
}

// thoughtAgent emits ONLY agent_thought chunks — no message text, no
// tool calls — then ends the turn. An agent reasoning at length looks
// exactly like this.
type thoughtAgent struct {
	*fakeAgent
	gap       time.Duration
	count     int
	completed chan struct{}
	once      sync.Once
}

func (a *thoughtAgent) Prompt(ctx context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
	a.fakeAgent.mu.Lock()
	sink := a.fakeAgent.sinks[sid]
	a.fakeAgent.mu.Unlock()
	for i := 0; i < a.count; i++ {
		select {
		case <-ctx.Done():
			return acp.StopReasonCancelled, ctx.Err()
		case <-time.After(a.gap):
		}
		_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
			SessionId: sid,
			Update: acp.SessionUpdate{
				AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
					Content: acp.TextBlock("thinking\n"),
				},
			},
		})
	}
	a.once.Do(func() { close(a.completed) })
	return acp.StopReasonEndTurn, nil
}

// TestHandler_HiddenThinkingIsNotAWedge is the regression guard for a
// real bug on the load-bearing idle path.
//
// The wedge clock used to be reset only as a SIDE EFFECT of what the
// router happened to render. A user with show_thinking off gets nothing
// rendered for a thought chunk — so an agent that reasons for longer
// than IdleWriteTimeout before answering was indistinguishable from a
// hung one and got cut, while the same agent with show_thinking ON
// survived. Whether the user wants to SEE the thoughts is a display
// choice; it is not evidence about the agent.
//
// The classifier is now client.IsProgress on the raw session/update, so
// the two cases behave identically. Note show_thinking is deliberately
// left OFF here: with it on this test passes even without the fix.
func TestHandler_HiddenThinkingIsNotAWedge(t *testing.T) {
	a := &thoughtAgent{
		fakeAgent: &fakeAgent{},
		gap:       20 * time.Millisecond,
		count:     12, // ~240ms of thinking, far beyond the 50ms window
		completed: make(chan struct{}),
	}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, IdleWriteTimeout: 50 * time.Millisecond})
	idleFired := make(chan struct{})
	h.idleWriteCancelHook = func() { close(idleFired) }

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c-thinking",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
		resp, derr := http.DefaultClient.Do(req)
		if derr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-a.completed:
	case <-idleFired:
		t.Fatal("a thinking agent was cut as wedged because its thoughts were hidden")
	case <-time.After(5 * time.Second):
		t.Fatal("thinking turn never completed")
	}
	<-done
}

// TestHandler_AbsorbedWedgeNoteSurvivesTheRedrive covers the awkward
// intersection of the two mechanisms.
//
// A pre-output transport drop is ABSORBED: the turn is decoupled, keeps
// running with no client attached, and its answer is buffered for Poe's
// redrive. If that decoupled turn then WEDGES, the cut's note is written
// into the buffer like any other output — so the redrive serves the
// explanation rather than an empty answer. Without the note this is the
// worst case in the system: the user's question silently produces
// nothing at all, twice.
func TestHandler_AbsorbedWedgeNoteSurvivesTheRedrive(t *testing.T) {
	a := &wedgeAgent{fakeAgent: &fakeAgent{}, returned: make(chan struct{})}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, IdleWriteTimeout: 60 * time.Millisecond})
	decided := make(chan struct{})
	h.absorbDecidedHook = func() { close(decided) }

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c-absorb-wedge", "user_id": "u", "message_id": "req",
		"query": []map[string]any{{"role": "user", "content": "hi", "message_id": "m1"}},
	})

	// Request 1: drop the connection before any output → absorbed.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)).WithContext(ctx)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	cancel()
	<-decided
	// The agent is wedged, so the idle backstop — not the agent — ends
	// the turn, and only then is the answer buffered.
	<-a.returned
	<-done

	// Request 2: Poe's redrive must be told what happened.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))
	out := rec.Body.String()
	if !strings.Contains(out, "it looks wedged") {
		t.Fatalf("the redrive must serve the wedge note, not an empty answer:\n%s", out)
	}
	if !strings.Contains(out, "event: done") {
		t.Fatalf("redrive missing done event:\n%s", out)
	}
}
