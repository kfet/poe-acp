// Package router maps Poe conversation_ids to ACP sessions and owns the
// lifecycle of those sessions (create, reuse, GC).
//
// Skeleton: in-memory map, one AgentProc shared across sessions.
package router

import (
	"context"
	"fmt"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/fir/internal/poeacp/acpclient"
)

// ChunkSink is the interface the HTTP/SSE layer implements to receive
// assistant output chunks for a single Poe query.
type ChunkSink interface {
	// Text appends text to the response.
	Text(s string) error
	// Replace replaces the entire response with s.
	Replace(s string) error
	// Error emits an error event.
	Error(text, errorType string) error
	// Done marks the response complete.
	Done() error
}

// sessionState tracks one conv_id → ACP session mapping.
type sessionState struct {
	convID    string
	userID    string
	sessionID acp.SessionId
	agent     *acpclient.AgentProc

	mu       sync.Mutex
	sink     ChunkSink // sink for the currently-open Poe query, if any
	lastUsed time.Time
}

// OnUpdate implements acpclient.SessionUpdateSink; forwards to the current
// sink (if one is attached).
func (s *sessionState) OnUpdate(_ context.Context, n acp.SessionNotification) error {
	s.mu.Lock()
	sink := s.sink
	s.mu.Unlock()
	if sink == nil {
		return nil
	}
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		if c := u.AgentMessageChunk.Content; c.Text != nil {
			return sink.Text(c.Text.Text)
		}
	case u.AgentThoughtChunk != nil:
		// v1: suppress thoughts. Future: forward as dim markdown.
		return nil
	case u.ToolCall != nil:
		// v1: suppress. Future: compact status line.
		return nil
	case u.ToolCallUpdate != nil:
		return nil
	case u.Plan != nil:
		return nil
	}
	return nil
}

// Router owns the conv_id → session map.
type Router struct {
	agent *acpclient.AgentProc
	cwd   string

	mu       sync.Mutex
	sessions map[string]*sessionState
}

// New creates an empty router backed by the given agent process.
func New(agent *acpclient.AgentProc, cwd string) *Router {
	return &Router{
		agent:    agent,
		cwd:      cwd,
		sessions: make(map[string]*sessionState),
	}
}

// Prompt handles one Poe query. It (a) looks up or creates the session for
// convID, (b) attaches sink as the chunk receiver, (c) issues session/prompt
// and streams updates until the turn ends, (d) emits a terminal event on sink.
//
// Prompt is synchronous: it returns when the ACP turn completes. Callers
// typically run it in the request goroutine holding the open SSE stream.
func (r *Router) Prompt(ctx context.Context, convID, userID, text string, sink ChunkSink) error {
	st, err := r.getOrCreate(ctx, convID, userID)
	if err != nil {
		_ = sink.Error(fmt.Sprintf("relay: %v", err), "user_caused_error")
		_ = sink.Done()
		return err
	}

	st.mu.Lock()
	st.sink = sink
	st.lastUsed = time.Now()
	st.mu.Unlock()

	defer func() {
		st.mu.Lock()
		st.sink = nil
		st.mu.Unlock()
	}()

	stop, err := r.agent.Prompt(ctx, st.sessionID, text)
	if err != nil {
		_ = sink.Error(fmt.Sprintf("acp prompt: %v", err), "user_caused_error")
		_ = sink.Done()
		return err
	}

	switch stop {
	case "refusal":
		_ = sink.Error("agent refused the request", "user_caused_error")
	case "max_tokens":
		_ = sink.Text("\n\n_(response truncated: max tokens)_")
	case "cancelled":
		_ = sink.Replace("_(cancelled)_")
	}
	return sink.Done()
}

func (r *Router) getOrCreate(ctx context.Context, convID, userID string) (*sessionState, error) {
	r.mu.Lock()
	st, ok := r.sessions[convID]
	r.mu.Unlock()
	if ok {
		return st, nil
	}

	st = &sessionState{convID: convID, userID: userID, agent: r.agent, lastUsed: time.Now()}
	sid, err := r.agent.NewSession(ctx, r.cwd, st)
	if err != nil {
		return nil, err
	}
	st.sessionID = sid

	r.mu.Lock()
	// Second check in case of concurrent create; last writer wins, but both
	// sessions are valid — worst case one is GC'd later.
	if existing, ok := r.sessions[convID]; ok {
		r.mu.Unlock()
		return existing, nil
	}
	r.sessions[convID] = st
	r.mu.Unlock()
	return st, nil
}

// Cancel requests cancellation of the current prompt for a conv.
func (r *Router) Cancel(ctx context.Context, convID string) error {
	r.mu.Lock()
	st, ok := r.sessions[convID]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	return r.agent.Cancel(ctx, st.sessionID)
}

// Len returns the number of tracked sessions (for diagnostics).
func (r *Router) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}
