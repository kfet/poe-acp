package router

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/remotefs"
)

// poolRouter builds a router with a curated host list where `pooled`
// hosts are served by their own agent + provisioner, exactly the way
// cmd/poe-acp wires agentpool in.
func poolRouter(t *testing.T, hosts []string, defaultHost string, pooled map[string]AgentTarget, resolveErr error) (*Router, *fakeAgent) {
	t.Helper()
	echo := func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	}
	def := newFakeAgent(echo)
	r, err := New(Config{
		Agent:      def,
		StateDir:   t.TempDir(),
		SessionTTL: time.Hour,
		Hosts:      hosts,
		Defaults:   Options{Host: defaultHost},
		AgentFor: func(_ context.Context, host string) (AgentTarget, error) {
			if resolveErr != nil {
				return AgentTarget{}, resolveErr
			}
			if tgt, ok := pooled[host]; ok {
				return tgt, nil
			}
			// Not pooled: the default target serves it, which is what
			// the pool reports as ErrNotPooled.
			return AgentTarget{}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, def
}

func promptOn(t *testing.T, r *Router, conv, host string) *captureSink {
	t.Helper()
	sink := &captureSink{}
	opts := ParseOptions(map[string]any{"host": host}, r.Defaults(), nil)
	if err := r.Prompt(context.Background(), conv, "u", []Turn{{Role: "user", Content: "hi"}}, opts, sink); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	return sink
}

// The point of the whole feature: a host with its own agent process is
// served by THAT process, and the default agent never sees the session.
func TestPool_PooledHostRunsOnItsOwnAgent(t *testing.T) {
	t.Parallel()
	hostb := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "from hostb")
		return acp.StopReasonEndTurn, nil
	})
	r, def := poolRouter(t, []string{"local", "hostb"}, "local",
		map[string]AgentTarget{"hostb": {Agent: hostb, Pooled: true}}, nil)

	sink := promptOn(t, r, "c1", "hostb")
	if !strings.Contains(sink.text.String(), "from hostb") {
		t.Fatalf("answer came from the wrong agent: %q", sink.text.String())
	}
	if atomic.LoadInt32(&def.prompts) != 0 {
		t.Fatal("the default agent served a pooled host's turn")
	}
	if atomic.LoadInt32(&hostb.prompts) != 1 {
		t.Fatalf("pooled agent prompts = %d, want 1", hostb.prompts)
	}
	// The process choice IS the placement, so no hint is sent: a
	// pooled fir would reject `_meta.host` anyway.
	if h, ok := hostb.metaHost(); ok {
		t.Fatalf("_meta.host = %q sent to a pooled agent", h)
	}
}

// A host WITHOUT its own agent command keeps the pre-pool contract
// byte for byte: default agent, `_meta.host` hint.
func TestPool_HintHostKeepsTheDefaultAgentAndMeta(t *testing.T) {
	t.Parallel()
	hostb := newFakeAgent(nil)
	r, def := poolRouter(t, []string{"boxy", "hostb"}, "boxy",
		map[string]AgentTarget{"hostb": {Agent: hostb, Pooled: true}}, nil)

	promptOn(t, r, "c1", "boxy")
	if atomic.LoadInt32(&def.prompts) != 1 {
		t.Fatalf("default agent prompts = %d, want 1", def.prompts)
	}
	if h, ok := def.metaHost(); !ok || h != "boxy" {
		t.Fatalf("_meta.host = %q ok=%v, want boxy", h, ok)
	}
	if atomic.LoadInt32(&hostb.prompts) != 0 {
		t.Fatal("an unrelated pooled agent was used")
	}
}

// The reserved sentinel still means "wherever the relay's own agent
// runs", even with pooled hosts alongside it.
func TestPool_LocalSentinelStaysOnTheDefaultAgent(t *testing.T) {
	t.Parallel()
	hostb := newFakeAgent(nil)
	r, def := poolRouter(t, []string{"local", "hostb"}, "local",
		map[string]AgentTarget{"hostb": {Agent: hostb, Pooled: true}}, nil)

	promptOn(t, r, "c1", "local")
	if atomic.LoadInt32(&def.prompts) != 1 {
		t.Fatalf("default agent prompts = %d, want 1", def.prompts)
	}
	if _, ok := def.metaHost(); ok {
		t.Fatal("the local sentinel must not put a host on the wire")
	}
}

// A target that names no agent or provisioner falls back to the
// default ones — the router never calls a nil agent.
func TestPool_TargetDefaultsAreFilledIn(t *testing.T) {
	t.Parallel()
	r, def := poolRouter(t, []string{"hostb"}, "hostb",
		map[string]AgentTarget{"hostb": {Pooled: true}}, nil)
	promptOn(t, r, "c1", "hostb")
	if atomic.LoadInt32(&def.prompts) != 1 {
		t.Fatalf("default agent prompts = %d, want 1", def.prompts)
	}
	r.mu.Lock()
	st := r.sessions["c1"]
	r.mu.Unlock()
	if st.provisioner() != remotefs.Local {
		t.Fatal("nil target provisioner did not default to remotefs.Local")
	}
}

// Without an AgentFor there is no pool at all: the single-process relay.
func TestPool_NoAgentForUsesTheDefaultTarget(t *testing.T) {
	t.Parallel()
	r, def := hostRouter(t, []string{"boxy"}, "boxy")
	tgt, err := r.targetFor(context.Background(), "boxy")
	if err != nil {
		t.Fatalf("targetFor: %v", err)
	}
	if tgt.Agent != Agent(def) || tgt.Pooled {
		t.Fatalf("target = %#v, want the default agent, unpooled", tgt)
	}
}

// A host whose agent will not start must SAY so, as visible assistant
// text: the user picked that host and is the one who can pick another.
// An `error` event would render as an empty bubble in Poe.
func TestPool_AgentStartFailureIsVisibleText(t *testing.T) {
	t.Parallel()
	r, _ := poolRouter(t, []string{"hostb"}, "hostb", nil, errors.New("ssh: host unreachable"))
	sink := &captureSink{}
	opts := ParseOptions(map[string]any{"host": "hostb"}, r.Defaults(), nil)
	err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, opts, sink)
	if err == nil {
		t.Fatal("Prompt: want an error")
	}
	var ae agentStartError
	if !errors.As(err, &ae) || !strings.Contains(ae.Error(), "hostb") {
		t.Fatalf("err = %v, want an agentStartError naming the host", err)
	}
	if !errors.Is(err, ae.err) {
		t.Fatal("agentStartError does not unwrap to the cause")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !strings.Contains(sink.text.String(), agentStartFailedMsg) || !strings.Contains(sink.text.String(), "host unreachable") {
		t.Fatalf("user saw %q", sink.text.String())
	}
	if sink.errText != "" {
		t.Fatalf("reported as an error event Poe does not render: %q", sink.errText)
	}
	if !sink.done {
		t.Fatal("stream not finalised")
	}
}

// The conversation's cwd must be created on the machine ITS agent runs
// on — not on the default agent's host.
func TestPool_ProvisionsOnThePooledHost(t *testing.T) {
	t.Parallel()
	hostbFS := &fakeProvisioner{}
	defFS := &fakeProvisioner{}
	hostb := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	echo := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	r, err := New(Config{
		Agent:       echo,
		Provisioner: defFS,
		StateDir:    t.TempDir(),
		SessionTTL:  time.Hour,
		Hosts:       []string{"local", "hostb"},
		Defaults:    Options{Host: "local"},
		AgentFor: func(_ context.Context, host string) (AgentTarget, error) {
			if host == "hostb" {
				return AgentTarget{Agent: hostb, Prov: hostbFS, Pooled: true}, nil
			}
			return AgentTarget{}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	promptOn(t, r, "c-hostb", "hostb")
	promptOn(t, r, "c-local", "local")

	mkdirs, _ := hostbFS.snapshot()
	if len(mkdirs) != 1 || !strings.HasSuffix(mkdirs[0], "c-hostb") {
		t.Fatalf("pooled host mkdirs = %v", mkdirs)
	}
	defMkdirs, _ := defFS.snapshot()
	if len(defMkdirs) != 1 || !strings.HasSuffix(defMkdirs[0], "c-local") {
		t.Fatalf("default host mkdirs = %v", defMkdirs)
	}
}

// A failure to prepare the POOLED host's filesystem fails that
// conversation, exactly as it does for the default agent's host.
func TestPool_ProvisionFailureOnPooledHost(t *testing.T) {
	t.Parallel()
	hostb := newFakeAgent(nil)
	r, _ := poolRouter(t, []string{"hostb"}, "hostb", map[string]AgentTarget{
		"hostb": {Agent: hostb, Prov: &fakeProvisioner{mkdirErr: errors.New("no route to host")}, Pooled: true},
	}, nil)
	sink := &captureSink{}
	opts := ParseOptions(map[string]any{"host": "hostb"}, r.Defaults(), nil)
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, opts, sink); err == nil {
		t.Fatal("want an error")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !strings.Contains(sink.text.String(), provisionFailedMsg) {
		t.Fatalf("user saw %q", sink.text.String())
	}
}

// A GC'd session is released on ITS OWN agent: a session id only means
// something to the process that issued it.
func TestPool_SessionIsReleasedOnItsOwnAgent(t *testing.T) {
	t.Parallel()
	hostb := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	now := time.Now()
	def := newFakeAgent(nil)
	r, err := New(Config{
		Agent:      def,
		StateDir:   t.TempDir(),
		SessionTTL: time.Minute,
		Hosts:      []string{"hostb"},
		Defaults:   Options{Host: "hostb"},
		Now:        func() time.Time { return now },
		AgentFor: func(_ context.Context, host string) (AgentTarget, error) {
			return AgentTarget{Agent: hostb, Pooled: true}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	promptOn(t, r, "c1", "hostb")

	now = now.Add(time.Hour) // the session is now idle past its TTL
	r.gcOnce()
	if got := atomic.LoadInt32(&hostb.releaseCalls); got != 1 {
		t.Fatalf("pooled agent releases = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&def.releaseCalls); got != 0 {
		t.Fatalf("default agent releases = %d, want 0", got)
	}
}

// A pooled agent's death is reported to ITS conversations — the router
// must consult the session's own process, not the default one.
func TestPool_DeadPooledAgentIsReportedToItsConversation(t *testing.T) {
	t.Parallel()
	hostb := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return "", errors.New("write |1: broken pipe")
	})
	r, _ := poolRouter(t, []string{"hostb"}, "hostb",
		map[string]AgentTarget{"hostb": {Agent: hostb, Pooled: true}}, nil)
	r.cfg.AgentDeathGrace = time.Millisecond

	hostb.die(errors.New("ssh: connection closed"))
	sink := &captureSink{}
	opts := ParseOptions(map[string]any{"host": "hostb"}, r.Defaults(), nil)
	_ = r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, opts, sink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !strings.Contains(sink.text.String(), agentDownMsg) {
		t.Fatalf("user saw %q, want the agent-down notice", sink.text.String())
	}
}

// A pooled agent's death does NOT recycle the worker (only the default
// agent's does), so the session map keeps a conversation pinned to a
// process that is gone. The next turn must rebuild it on the pool's
// replacement instead of failing until the idle GC notices.
func TestPool_DeadPooledAgentIsRebuiltOnItsReplacement(t *testing.T) {
	t.Parallel()
	echo := func(reply string) *fakeAgent {
		return newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
			a.emit(sid, reply)
			return acp.StopReasonEndTurn, nil
		})
	}
	first, second := echo("from the first process"), echo("from the replacement")
	// The pool hands out the live one: exactly what agentpool.Get does
	// when it finds a dead entry.
	agentFor := func(context.Context, string) (AgentTarget, error) {
		if first.Err() == nil {
			return AgentTarget{Agent: first, Pooled: true}, nil
		}
		return AgentTarget{Agent: second, Pooled: true}, nil
	}
	r, def := poolRouter(t, []string{"hostb"}, "hostb", nil, nil)
	r.cfg.AgentFor = agentFor
	r.cfg.AgentDeathGrace = time.Millisecond

	if got := promptOn(t, r, "c1", "hostb").text.String(); !strings.Contains(got, "from the first process") {
		t.Fatalf("first turn answered %q", got)
	}
	first.die(errors.New("ssh: connection closed"))

	sink := promptOn(t, r, "c1", "hostb")
	if got := sink.text.String(); !strings.Contains(got, "from the replacement") {
		t.Fatalf("second turn answered %q, want the replacement process", got)
	}
	if atomic.LoadInt32(&second.newSessCalls) == 0 {
		t.Fatal("no session was acquired on the replacement process")
	}
	// The dead process is never asked to release the session id it can
	// no longer resolve, and the default agent is not involved at all.
	if got := atomic.LoadInt32(&first.releaseCalls); got != 0 {
		t.Fatalf("dead agent releases = %d, want 0", got)
	}
	if atomic.LoadInt32(&def.prompts) != 0 {
		t.Fatal("the default agent served a pooled conversation")
	}
}

// Session acquisition is the one failure with no session to consult, so
// an outage there used to be judged by the DEFAULT agent's liveness and
// reported as an `error` event — which Poe renders as an empty bubble.
// The error carries its agent, so the user gets the outage notice.
func TestPool_DeadPooledAgentDuringAcquisitionIsReportedAsText(t *testing.T) {
	t.Parallel()
	hostb := newFakeAgent(nil)
	hostb.newSessErr = errors.New("write |1: broken pipe")
	hostb.die(errors.New("ssh: connection closed"))
	r, _ := poolRouter(t, []string{"hostb"}, "hostb",
		map[string]AgentTarget{"hostb": {Agent: hostb, Pooled: true}}, nil)
	r.cfg.AgentDeathGrace = time.Millisecond

	sink := &captureSink{}
	opts := ParseOptions(map[string]any{"host": "hostb"}, r.Defaults(), nil)
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, opts, sink); err == nil {
		t.Fatal("want an error")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !strings.Contains(sink.text.String(), agentDownMsg) {
		t.Fatalf("user saw %q, want the agent-down notice", sink.text.String())
	}
	if sink.errText != "" {
		t.Fatalf("reported as an error event Poe does not render: %q", sink.errText)
	}
}

// The provisioning failure of a LIVE pooled agent must still read as a
// provisioning failure, not as an outage — the error is agent-bound now
// and the two must not blur.
func TestPool_ProvisionFailureOnALiveAgentIsNotAnOutage(t *testing.T) {
	t.Parallel()
	hostb := newFakeAgent(nil)
	r, _ := poolRouter(t, []string{"hostb"}, "hostb", map[string]AgentTarget{
		"hostb": {Agent: hostb, Prov: &fakeProvisioner{mkdirErr: errors.New("no route to host")}, Pooled: true},
	}, nil)
	r.cfg.AgentDeathGrace = time.Millisecond
	sink := &captureSink{}
	opts := ParseOptions(map[string]any{"host": "hostb"}, r.Defaults(), nil)
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, opts, sink); err == nil {
		t.Fatal("want an error")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if got := sink.text.String(); !strings.Contains(got, provisionFailedMsg) || strings.Contains(got, agentDownMsg) {
		t.Fatalf("user saw %q", got)
	}
}
