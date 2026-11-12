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

func ctxForkAgent(ctx context.Context) (interject.Agent, error) {
	if a, ok := ctx.Value(agentCtxKey{}).(interject.Agent); ok {
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

// interjectOutcome tells Prompt how to continue after interjectTurn.
type interjectOutcome int

const (
	ijFallback   interjectOutcome = iota // not handled: ordinary turn
	ijHandled                            // reply posted, sink finalised
	ijQueuedTurn                         // run a turn carrying only the queued prefix
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
	switch rep.Action {
	case interject.Steer:
		_ = r.Cancel(ctx, st.convID)
		return ijFallback
	case interject.Abort:
		_ = r.Cancel(ctx, st.convID)
	case interject.Queue:
		if _, live := r.active.Get(st.convID); !live && r.ij.Queued(st.convID) {
			return ijQueuedTurn
		}
	}
	_ = sink.Done()
	return ijHandled
}

// withSidePrefix prepends the interjector's pending side Q&A and queued
// messages to a main prompt.
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
