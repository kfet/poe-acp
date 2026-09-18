// Package agentpool keeps one ACP agent process per selectable host.
//
// The relay's baseline is a single agent child: one `--agent-cmd`, one
// process, one ACP handshake. That is enough when the user only picks
// WHERE the agent should place a session (the `_meta.host` hint an
// acp-tmux-style agent understands), but it cannot satisfy "run the
// agent itself over there": `fir --mode acp` cannot relocate a running
// process. So a host that declares its own `agent_cmd` gets its own
// child here, started lazily on the first conversation that resolves to
// it and kept for the worker's lifetime.
//
// The pool deliberately owns very little: it holds the host→spec table,
// serialises starts, caches live entries, and forgets an entry whose
// agent has died so the next conversation gets a fresh process. Spawning
// (and whatever probing/watching belongs with it) is the caller's Start
// callback, which keeps this package free of the relay's agent wiring
// and trivially testable.
package agentpool

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/kfet/acp-kit/remotefs"
)

// Agent is the little of an agent process the pool itself touches: is it
// still alive, and shut it down. Pool is generic over it so the relay can
// hold concrete *client.AgentProc values (which the router needs in
// full) while tests can drive the cache with a stub, without a cast at
// either end.
type Agent interface {
	// Err reports why the process exited, nil while it runs.
	Err() error
	// Close shuts the process down.
	Close() error
}

// Spec is the immutable description of one pooled host.
type Spec struct {
	// Host is the `hosts[i].value` this spec serves. Informational for
	// the Start callback and logs; the pool keys on it.
	Host string
	// Argv is the agent command, already split into argv.
	Argv []string
	// Prov makes the paths the relay puts on the ACP wire exist on the
	// machine this host's agent runs on. Nil means remotefs.Local.
	Prov remotefs.Provisioner
}

// Entry is a live pooled agent.
type Entry[A Agent] struct {
	// Spec is the host this entry serves.
	Spec Spec
	// Agent is the running child.
	Agent A
}

// Provisioner is the entry's filesystem provisioner, never nil.
func (e *Entry[A]) Provisioner() remotefs.Provisioner {
	if e.Spec.Prov == nil {
		return remotefs.Local
	}
	return e.Spec.Prov
}

// Config configures a Pool.
type Config[A Agent] struct {
	// Specs are the pooled hosts, keyed by host value. A host absent
	// from this map is not pooled: it is served by the relay's default
	// agent, and Get reports ErrNotPooled for it.
	Specs map[string]Spec
	// Start spawns (and handshakes, probes — the caller's business) the
	// agent for one spec. Required when Specs is non-empty.
	//
	// It takes NO context, deliberately: the child's lifetime belongs
	// to the caller's own root context (acp-kit's client.Start binds the
	// process to the ctx it is given), and a per-conversation deadline
	// must never be what kills a pooled agent. The pool bounds the
	// WAIT for a start, not the start itself — a caller that gives up
	// leaves the spawn running, and the agent it produces is installed
	// for whoever asks next. Never called concurrently for one host.
	Start func(spec Spec) (A, error)
	// Logf receives one line per start and per dropped dead entry. Nil
	// discards.
	Logf func(format string, args ...any)
}

// ErrNotPooled is returned by Get for a host that has no agent command
// configured. It is not a failure: the caller serves that host with the
// relay's default agent, which is exactly the pre-pool behaviour.
var ErrNotPooled = fmt.Errorf("host is not pooled")

// Pool is a lazily-populated host→agent map. Safe for concurrent use.
type Pool[A Agent] struct {
	specs map[string]Spec
	start func(spec Spec) (A, error)
	logf  func(format string, args ...any)

	mu      sync.Mutex
	closed  bool
	entries map[string]*Entry[A]
	// inflight single-flights starts per host, so two first-turn
	// conversations cannot race two agent processes into existence, and
	// a caller whose context expired does not waste the spawn it
	// started. Per-host, so an unrelated host's cold start is never
	// blocked behind a slow ssh handshake.
	inflight map[string]*startCall[A]
}

// startCall is one in-flight agent start. done is closed once entry/err
// are final.
type startCall[A Agent] struct {
	done  chan struct{}
	entry *Entry[A]
	err   error
}

// New builds a Pool. An empty Specs map yields a pool that reports
// ErrNotPooled for everything — the single-agent baseline.
func New[A Agent](cfg Config[A]) (*Pool[A], error) {
	if len(cfg.Specs) > 0 && cfg.Start == nil {
		return nil, fmt.Errorf("agentpool: %d pooled host(s) but no Start callback", len(cfg.Specs))
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	specs := make(map[string]Spec, len(cfg.Specs))
	for host, spec := range cfg.Specs {
		if len(spec.Argv) == 0 {
			return nil, fmt.Errorf("agentpool: host %q has an empty agent command", host)
		}
		spec.Host = host
		specs[host] = spec
	}
	return &Pool[A]{
		specs:    specs,
		start:    cfg.Start,
		logf:     logf,
		entries:  make(map[string]*Entry[A]),
		inflight: make(map[string]*startCall[A]),
	}, nil
}

// Pooled reports whether host runs its own agent process.
func (p *Pool[A]) Pooled(host string) bool {
	_, ok := p.specs[host]
	return ok
}

// Hosts lists the pooled host values, sorted, for logging.
func (p *Pool[A]) Hosts() []string {
	out := make([]string, 0, len(p.specs))
	for h := range p.specs {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// Get returns the live agent for host, starting it on first use.
// ErrNotPooled means "not ours — use the default agent".
//
// Entries are never evicted while their agent lives — a pooled agent holds live ACP
// sessions the relay has no way to re-home.
func (p *Pool[A]) Get(ctx context.Context, host string) (*Entry[A], error) {
	spec, ok := p.specs[host]
	if !ok {
		return nil, ErrNotPooled
	}
	// One locked decision: hand back the live agent, join an in-flight
	// start, or begin one. Doing the liveness check anywhere but under
	// the lock that installs entries is what lets two conversations
	// race two processes onto one host.
	entry, call := p.startAsync(spec)
	if entry != nil {
		return entry, nil
	}
	select {
	case <-call.done:
		if call.err != nil {
			return nil, fmt.Errorf("start agent for host %q: %w", host, call.err)
		}
		return call.entry, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("start agent for host %q: %w", host, ctx.Err())
	}
}

// startAsync returns the in-flight start for spec.Host, launching one if
// none is running. The spawn runs to completion regardless of who is
// still waiting, and installs its agent into the pool on success.
//
// It returns a live ENTRY instead whenever the host already has one.
// A cached entry whose agent has exited is dropped here and replaced,
// so the pool never hands out a dead process and a rebooted box
// recovers on the next conversation without a relay restart.
func (p *Pool[A]) startAsync(spec Spec) (*Entry[A], *startCall[A]) {
	p.mu.Lock()
	var dead error
	if e, ok := p.entries[spec.Host]; ok {
		if dead = e.Agent.Err(); dead == nil {
			p.mu.Unlock()
			return e, nil
		}
		delete(p.entries, spec.Host)
	}
	if call, ok := p.inflight[spec.Host]; ok {
		p.mu.Unlock()
		return nil, call
	}
	call := &startCall[A]{done: make(chan struct{})}
	p.inflight[spec.Host] = call
	p.mu.Unlock()
	if dead != nil {
		p.logf("agentpool: agent for host %q had exited (%v); starting a fresh one", spec.Host, dead)
	}

	go func() {
		agent, err := p.start(spec)
		p.mu.Lock()
		delete(p.inflight, spec.Host)
		// A start that lands after Close has nowhere to live: the
		// worker is going away, so shut the child down rather than
		// orphan it (it is an ssh pipe, and orphans hold terminals).
		late := p.closed && err == nil
		if err == nil && !late {
			call.entry = &Entry[A]{Spec: spec, Agent: agent}
			p.entries[spec.Host] = call.entry
		}
		n := len(p.entries)
		p.mu.Unlock()
		if late {
			_ = agent.Close()
			err = fmt.Errorf("pool is closed")
		}
		call.err = err
		close(call.done)
		if err != nil {
			p.logf("agentpool: WARN agent for host %q failed to start: %v", spec.Host, err)
			return
		}
		p.logf("agentpool: started agent for host %q (%d live)", spec.Host, n)
	}()
	return nil, call
}

// Len is the number of live pooled agents.
func (p *Pool[A]) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// Close shuts every pooled agent down. Called on worker exit; the
// default agent is closed by its own owner.
func (p *Pool[A]) Close() {
	p.mu.Lock()
	p.closed = true
	entries := make([]*Entry[A], 0, len(p.entries))
	for host, e := range p.entries {
		entries = append(entries, e)
		delete(p.entries, host)
	}
	p.mu.Unlock()
	for _, e := range entries {
		_ = e.Agent.Close()
	}
}
