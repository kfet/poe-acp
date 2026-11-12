package router

// Mid-turn quick replies (acp-kit/interject).
//
// When a user message arrives while the conversation's main turn is
// still running, the relay forks the main session, asks the fork, posts
// its short answer as this message's reply, and lets the fork's verdict
// decide what happens to the main turn. Any failure falls back to the
// ordinary path: the message is queued behind the running turn.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/interject"
)

// interjectTimeout bounds one fork Q&A.
const interjectTimeout = 60 * time.Second

// agentCtxKey carries the session's Agent to agentDispatch.
type agentCtxKey struct{}

// agentDispatch is the interject.Agent the Interjector is built on. A
// conversation may run on a pooled agent, so every call is routed to
// the Agent carried in ctx (set by interjectTurn).
type agentDispatch struct{}

// forker is the part of interject.Agent every forking agent has;
// ExtMethod is probed separately.
type forker interface {
	ForkSession(ctx context.Context, cwd string, parent acp.SessionId, at string, sink client.SessionUpdateSink) (acp.SessionId, error)
	Prompt(ctx context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (acp.StopReason, error)
	CloseSession(ctx context.Context, sid acp.SessionId) error
	Caps() client.Caps
}

func ctxForkAgent(ctx context.Context) (forker, error) {
	if a, ok := ctx.Value(agentCtxKey{}).(forker); ok {
		return a, nil
	}
	return nil, client.ErrForkUnsupported
}

func (agentDispatch) ForkSession(ctx context.Context, cwd string, parent acp.SessionId, at string, sink client.SessionUpdateSink) (acp.SessionId, error) {
	a, err := ctxForkAgent(ctx)
	if err != nil {
		return "", err
	}
	return a.ForkSession(ctx, cwd, parent, at, sink)
}

// Prompt and CloseSession are only reached after ForkSession succeeded,
// i.e. with the same ctx values, so the assertion always holds.
func (agentDispatch) Prompt(ctx context.Context, sid acp.SessionId, p []acp.ContentBlock) (acp.StopReason, error) {
	a, _ := ctxForkAgent(ctx)
	return a.Prompt(ctx, sid, p)
}

func (agentDispatch) CloseSession(ctx context.Context, sid acp.SessionId) error {
	a, _ := ctxForkAgent(ctx)
	return a.CloseSession(ctx, sid)
}

// Caps has no ctx to route by, so it advertises the steer extension
// and ExtMethod checks the routed agent's real caps: a refusal there is
// "not delivered" to the Interjector, which then takes the old path.
func (agentDispatch) Caps() client.Caps {
	return client.Caps{Extensions: map[string]json.RawMessage{interject.SteerCap: nil}}
}

func (agentDispatch) ExtMethod(ctx context.Context, sid acp.SessionId, method string, p map[string]any) (json.RawMessage, error) {
	a, _ := ctxForkAgent(ctx)
	x, ok := a.(interface {
		ExtMethod(context.Context, acp.SessionId, string, map[string]any) (json.RawMessage, error)
	})
	if _, adv := a.Caps().Extensions[interject.SteerCap]; !ok || !adv {
		return nil, errNoSteer
	}
	return x.ExtMethod(ctx, sid, method, p)
}

var errNoSteer = errors.New("interject: agent does not advertise " + interject.SteerCap)

// interjectOutcome tells Prompt how to continue after interjectTurn.
type interjectOutcome int

const (
	ijFallback interjectOutcome = iota // not handled: ordinary turn
	ijHandled                          // reply posted, sink finalised
)

// interjectTurn handles a message that arrived during a running main
// turn. See the package comment above.
func (r *Router) interjectTurn(ctx context.Context, st *sessionState, msgID, text string, sink ChunkSink) interjectOutcome {
	if r.ij == nil || (msgID != "" && st.liveUserMsg.Load() == msgID) {
		return ijFallback
	}
	if _, live := r.active.Get(st.convID); !live {
		return ijFallback
	}
	actx, cancel := context.WithTimeout(context.WithValue(ctx, agentCtxKey{}, st.agent()), r.interjectTimeout)
	rep, err := r.ij.Ask(actx, st.convID, st.cwd, st.sessionID, text)
	cancel()
	if err != nil {
		log.Printf("router: interject conv=%s: %v — falling back", st.convID, err)
		return ijFallback
	}
	sink.FirstChunk()
	if rep.Text != "" {
		_ = sink.Text(rep.Text + "\n\n")
	}
	_ = sink.Text(rep.Tag() + "\n\n")
	switch {
	case rep.Action == interject.Inform && !rep.Delivered:
		// The agent could not take it mid-turn: supersede, as before.
		_ = r.Cancel(ctx, st.convID)
		return ijFallback
	case rep.Action == interject.Abort:
		_ = r.Cancel(ctx, st.convID)
	}
	_ = sink.Done()
	return ijHandled
}

// withSidePrefix prepends the side Q&A the agent could not take
// mid-turn to a main prompt.
func (r *Router) withSidePrefix(convID string, blocks []acp.ContentBlock) []acp.ContentBlock {
	prefix := r.ij.Take(convID)
	if prefix == "" {
		return blocks
	}
	out := append([]acp.ContentBlock(nil), blocks...)
	if len(out) > 0 && out[0].Text != nil {
		out[0] = acp.TextBlock(prefix + out[0].Text.Text)
		return out
	}
	return append([]acp.ContentBlock{acp.TextBlock(prefix)}, out...)
}
