// Package router maps Poe conversation_ids to ACP sessions and owns their
// lifecycle (lazy create, per-conv cwd, serial-per-conv prompting, idle GC).
package router

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/poe-acp-relay/internal/acpclient"
)

// ChunkSink is the interface the HTTP/SSE layer implements to receive
// assistant output chunks for a single Poe query.
type ChunkSink interface {
	Text(s string) error
	Replace(s string) error
	Error(text, errorType string) error
	Done() error
	// FirstChunk is called by the router once, the first time the sink
	// receives a non-empty update from the agent. Used by the HTTP layer
	// to stop the "still thinking…" heartbeat.
	FirstChunk()
}

// Agent is the subset of acpclient.AgentProc the router needs. Exposed
// as an interface for testability.
type Agent interface {
	NewSession(ctx context.Context, cwd string, sink acpclient.SessionUpdateSink) (acp.SessionId, error)
	Prompt(ctx context.Context, sid acp.SessionId, text string) (acp.StopReason, error)
	Cancel(ctx context.Context, sid acp.SessionId) error
}

// Config configures a Router.
type Config struct {
	// Agent drives sessions. *acpclient.AgentProc satisfies this.
	Agent Agent
	// StateDir is the root for per-conv working dirs. Each conv gets
	// StateDir/convs/<conv_id>/ as its cwd.
	StateDir string
	// SessionTTL: sessions idle longer than this are dropped from the map.
	SessionTTL time.Duration
	// Now overrides the clock for tests. Defaults to time.Now.
	Now func() time.Time
}

// Router is the conv_id → session map.
type Router struct {
	cfg Config

	mu       sync.Mutex
	sessions map[string]*sessionState
}

// sessionState tracks one conv_id.
type sessionState struct {
	convID    string
	userID    string
	sessionID acp.SessionId
	cwd       string

	// turnMu serialises prompts for this conv. Held for the whole turn.
	turnMu sync.Mutex

	// sinkMu guards sink (written by Prompt, read by OnUpdate).
	sinkMu sync.Mutex
	sink   ChunkSink
	first  bool

	lastUsedNs int64 // atomic-ish via sessionState lock in router
}

// New creates a router.
func New(cfg Config) (*Router, error) {
	if cfg.Agent == nil {
		return nil, fmt.Errorf("router: nil Agent")
	}
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("router: empty StateDir")
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 2 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "convs"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir state: %w", err)
	}
	return &Router{cfg: cfg, sessions: make(map[string]*sessionState)}, nil
}

// OnUpdate implements acpclient.SessionUpdateSink; forwards to the current
// sink (if one is attached).
func (s *sessionState) OnUpdate(_ context.Context, n acp.SessionNotification) error {
	s.sinkMu.Lock()
	sink := s.sink
	firstSent := s.first
	s.sinkMu.Unlock()

	if sink == nil {
		return nil
	}
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		if c := u.AgentMessageChunk.Content; c.Text != nil && c.Text.Text != "" {
			if !firstSent {
				sink.FirstChunk()
				s.sinkMu.Lock()
				s.first = true
				s.sinkMu.Unlock()
			}
			return sink.Text(c.Text.Text)
		}
	}
	// Thoughts, tool-calls, plans are suppressed in v1.
	return nil
}

// Prompt handles one Poe query end-to-end. Serialises per-conv.
func (r *Router) Prompt(ctx context.Context, convID, userID, text string, sink ChunkSink) error {
	if convID == "" {
		convID = "default"
	}
	st, err := r.getOrCreate(ctx, convID, userID)
	if err != nil {
		_ = sink.Error(fmt.Sprintf("relay: %v", err), "user_caused_error")
		_ = sink.Done()
		return err
	}

	st.turnMu.Lock()
	defer st.turnMu.Unlock()

	st.sinkMu.Lock()
	st.sink = sink
	st.first = false
	st.sinkMu.Unlock()

	defer func() {
		st.sinkMu.Lock()
		st.sink = nil
		st.sinkMu.Unlock()
		r.touch(convID)
	}()

	stop, err := r.cfg.Agent.Prompt(ctx, st.sessionID, text)
	if err != nil {
		_ = sink.Error(fmt.Sprintf("acp prompt: %v", err), "user_caused_error")
		_ = sink.Done()
		return err
	}

	switch stop {
	case acp.StopReasonEndTurn:
		// normal
	case acp.StopReasonMaxTokens:
		_ = sink.Text("\n\n_(response truncated: max tokens)_")
	case acp.StopReasonMaxTurnRequests:
		_ = sink.Text("\n\n_(response truncated: max turns)_")
	case acp.StopReasonRefusal:
		_ = sink.Error("agent refused the request", "user_caused_error")
	case acp.StopReasonCancelled:
		_ = sink.Replace("_(cancelled)_")
	}
	return sink.Done()
}

// Cancel asks the agent to cancel the current prompt for a conv.
func (r *Router) Cancel(ctx context.Context, convID string) error {
	r.mu.Lock()
	st, ok := r.sessions[convID]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	return r.cfg.Agent.Cancel(ctx, st.sessionID)
}

func (r *Router) getOrCreate(ctx context.Context, convID, userID string) (*sessionState, error) {
	r.mu.Lock()
	if st, ok := r.sessions[convID]; ok {
		r.mu.Unlock()
		return st, nil
	}
	r.mu.Unlock()

	cwd := filepath.Join(r.cfg.StateDir, "convs", convID)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir conv dir: %w", err)
	}

	st := &sessionState{
		convID:     convID,
		userID:     userID,
		cwd:        cwd,
		lastUsedNs: r.cfg.Now().UnixNano(),
	}
	sid, err := r.cfg.Agent.NewSession(ctx, cwd, st)
	if err != nil {
		return nil, fmt.Errorf("acp new session: %w", err)
	}
	st.sessionID = sid

	r.mu.Lock()
	if existing, ok := r.sessions[convID]; ok {
		r.mu.Unlock()
		// Lost the race; the session we just created leaks on the agent
		// side but that is cheap.
		return existing, nil
	}
	r.sessions[convID] = st
	r.mu.Unlock()
	return st, nil
}

func (r *Router) touch(convID string) {
	r.mu.Lock()
	if st, ok := r.sessions[convID]; ok {
		st.lastUsedNs = r.cfg.Now().UnixNano()
	}
	r.mu.Unlock()
}

// RunGC runs a background goroutine that drops idle sessions. Returns a
// stop function.
func (r *Router) RunGC(ctx context.Context, every time.Duration) (stop func()) {
	ctx2, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx2.Done():
				return
			case <-t.C:
				r.gcOnce()
			}
		}
	}()
	return cancel
}

func (r *Router) gcOnce() {
	cutoff := r.cfg.Now().Add(-r.cfg.SessionTTL).UnixNano()
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, st := range r.sessions {
		if st.lastUsedNs < cutoff {
			delete(r.sessions, id)
		}
	}
}

// Len returns the number of tracked sessions.
func (r *Router) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// DebugInfo is a snapshot of session state for /debug/sessions.
type DebugInfo struct {
	ConvID    string    `json:"conv_id"`
	UserID    string    `json:"user_id"`
	SessionID string    `json:"session_id"`
	Cwd       string    `json:"cwd"`
	LastUsed  time.Time `json:"last_used"`
}

// Debug returns a snapshot of all tracked sessions, sorted by conv_id.
func (r *Router) Debug() []DebugInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]DebugInfo, 0, len(r.sessions))
	for _, st := range r.sessions {
		out = append(out, DebugInfo{
			ConvID:    st.convID,
			UserID:    st.userID,
			SessionID: string(st.sessionID),
			Cwd:       st.cwd,
			LastUsed:  time.Unix(0, st.lastUsedNs),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConvID < out[j].ConvID })
	return out
}
