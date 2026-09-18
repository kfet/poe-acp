package agentpool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/kfet/acp-kit/remotefs"
)

// stubAgent stands in for a *client.AgentProc: the pool only ever asks
// whether it is still alive and tells it to stop.
type stubAgent struct {
	mu     sync.Mutex
	err    error
	closed int
}

func (a *stubAgent) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

func (a *stubAgent) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed++
	return nil
}

func (a *stubAgent) die(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
}

func (a *stubAgent) closes() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed
}

func TestNew_RejectsPooledHostsWithoutStart(t *testing.T) {
	t.Parallel()
	_, err := New(Config[*stubAgent]{Specs: map[string]Spec{"miki": {Argv: []string{"fir"}}}})
	if err == nil || !strings.Contains(err.Error(), "no Start callback") {
		t.Fatalf("err = %v", err)
	}
}

func TestNew_RejectsEmptyCommand(t *testing.T) {
	t.Parallel()
	_, err := New(Config[*stubAgent]{
		Specs: map[string]Spec{"miki": {}},
		Start: func(Spec) (*stubAgent, error) { return &stubAgent{}, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "empty agent command") {
		t.Fatalf("err = %v", err)
	}
}

// An empty pool is the single-agent relay: every host is somebody
// else's business, and nothing is ever started.
func TestGet_EmptyPoolIsNotPooled(t *testing.T) {
	t.Parallel()
	p, err := New(Config[*stubAgent]{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(t.Context(), "miki"); !errors.Is(err, ErrNotPooled) {
		t.Fatalf("err = %v, want ErrNotPooled", err)
	}
	if p.Pooled("miki") || len(p.Hosts()) != 0 || p.Len() != 0 {
		t.Fatal("empty pool reports pooled hosts")
	}
	p.Close() // no-op, but must not panic
}

func newTestPool(t *testing.T, start func(Spec) (*stubAgent, error), hosts ...string) *Pool[*stubAgent] {
	t.Helper()
	specs := make(map[string]Spec, len(hosts))
	for _, h := range hosts {
		specs[h] = Spec{Argv: []string{"fir", "--mode", "acp"}}
	}
	p, err := New(Config[*stubAgent]{Specs: specs, Start: start, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// The whole point: one process per host, started once, reused after.
func TestGet_StartsLazilyAndCaches(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	starts := map[string]int{}
	p := newTestPool(t, func(spec Spec) (*stubAgent, error) {
		mu.Lock()
		starts[spec.Host]++
		mu.Unlock()
		return &stubAgent{}, nil
	}, "miki", "beta")

	if p.Len() != 0 {
		t.Fatal("pool started an agent before anyone asked")
	}
	if got := p.Hosts(); len(got) != 2 || got[0] != "beta" || got[1] != "miki" {
		t.Fatalf("Hosts = %v, want sorted", got)
	}

	first, err := p.Get(t.Context(), "miki")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if first.Spec.Host != "miki" {
		t.Fatalf("entry spec host = %q", first.Spec.Host)
	}
	again, err := p.Get(t.Context(), "miki")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if again != first {
		t.Fatal("second Get started a second process for the same host")
	}
	if p.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (beta was never asked for)", p.Len())
	}
	mu.Lock()
	defer mu.Unlock()
	if starts["miki"] != 1 || starts["beta"] != 0 {
		t.Fatalf("starts = %v", starts)
	}
}

// A dead agent is never handed out: the entry is dropped and the next
// conversation gets a fresh process, so a rebooted box recovers without
// a relay restart.
func TestGet_ReplacesADeadAgent(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var made []*stubAgent
	p := newTestPool(t, func(Spec) (*stubAgent, error) {
		a := &stubAgent{}
		mu.Lock()
		made = append(made, a)
		mu.Unlock()
		return a, nil
	}, "miki")

	first, err := p.Get(t.Context(), "miki")
	if err != nil {
		t.Fatal(err)
	}
	first.Agent.die(errors.New("ssh: connection closed"))
	second, err := p.Get(t.Context(), "miki")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("pool handed out the dead entry")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(made) != 2 {
		t.Fatalf("started %d agents, want 2", len(made))
	}
}

func TestGet_StartFailurePropagates(t *testing.T) {
	t.Parallel()
	p := newTestPool(t, func(Spec) (*stubAgent, error) {
		return nil, errors.New("ssh: host unreachable")
	}, "miki")
	_, err := p.Get(t.Context(), "miki")
	if err == nil || !strings.Contains(err.Error(), "host unreachable") ||
		!strings.Contains(err.Error(), `host "miki"`) {
		t.Fatalf("err = %v", err)
	}
	if p.Len() != 0 {
		t.Fatal("a failed start left an entry behind")
	}
	// The failure is not sticky: the next conversation tries again.
	if _, err := p.Get(t.Context(), "miki"); err == nil {
		t.Fatal("second Get did not retry the start")
	}
}

// Two first turns on the same host must not race two processes into
// existence, and neither must block on the other's unrelated host.
func TestGet_SingleFlightsConcurrentStarts(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	var starts int
	var mu sync.Mutex
	p := newTestPool(t, func(Spec) (*stubAgent, error) {
		mu.Lock()
		starts++
		mu.Unlock()
		entered <- struct{}{}
		<-release
		return &stubAgent{}, nil
	}, "miki")

	const callers = 4
	got := make(chan *Entry[*stubAgent], callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := p.Get(context.Background(), "miki")
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			got <- e
		}()
	}
	<-entered // exactly one spawn is under way
	close(release)
	wg.Wait()
	close(got)
	var first *Entry[*stubAgent]
	for e := range got {
		if first == nil {
			first = e
			continue
		}
		if e != first {
			t.Fatal("concurrent Gets returned different processes")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if starts != 1 {
		t.Fatalf("starts = %d, want 1", starts)
	}
	if len(entered) != 0 {
		t.Fatalf("%d extra spawns entered", len(entered))
	}
}

// A caller that gives up (its turn's deadline) gets an error, but the
// spawn it triggered runs on — and whoever asks next finds it hot.
func TestGet_ContextTimeoutLeavesTheStartRunning(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{})
	p := newTestPool(t, func(Spec) (*stubAgent, error) {
		<-release
		close(started)
		return &stubAgent{}, nil
	}, "miki")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Get(ctx, "miki"); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the caller's context error", err)
	}
	close(release)
	<-started
	e, err := p.Get(context.Background(), "miki")
	if err != nil {
		t.Fatalf("Get after abandoned wait: %v", err)
	}
	if e == nil || p.Len() != 1 {
		t.Fatalf("abandoned start was not installed (len=%d)", p.Len())
	}
}

// Close shuts every live agent down, and a start that lands afterwards
// is closed rather than orphaned — these are ssh pipes.
func TestClose_ShutsDownLiveAndLateAgents(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	late := &stubAgent{}
	p, err := New(Config[*stubAgent]{
		Specs: map[string]Spec{"miki": {Argv: []string{"fir"}}, "beta": {Argv: []string{"fir"}}},
		Start: func(spec Spec) (*stubAgent, error) {
			if spec.Host == "beta" {
				<-release
				return late, nil
			}
			return &stubAgent{}, nil
		},
		Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := p.Get(t.Context(), "miki")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, betaCall := p.startAsync(Spec{Host: "beta", Argv: []string{"fir"}})
	_, _ = p.Get(ctx, "beta") // abandons the wait

	p.Close()
	if live.Agent.closes() != 1 {
		t.Fatalf("live agent closes = %d, want 1", live.Agent.closes())
	}
	close(release)
	<-betaCall.done
	if betaCall.err == nil || !strings.Contains(betaCall.err.Error(), "pool is closed") {
		t.Fatalf("late start err = %v", betaCall.err)
	}
	if late.closes() != 1 {
		t.Fatalf("late agent closes = %d, want 1", late.closes())
	}
	if p.Len() != 0 {
		t.Fatalf("Len after Close = %d", p.Len())
	}
}

// The entry's provisioner is what stages a conversation's cwd and
// attachments, so "no ssh host" must mean the relay's own filesystem
// rather than a nil call.
func TestEntry_ProvisionerDefaultsToLocal(t *testing.T) {
	t.Parallel()
	ssh, err := remotefs.New("miki")
	if err != nil {
		t.Fatal(err)
	}
	local := &Entry[*stubAgent]{}
	if local.Provisioner() != remotefs.Local {
		t.Fatal("nil provisioner did not default to remotefs.Local")
	}
	remote := &Entry[*stubAgent]{Spec: Spec{Prov: ssh}}
	if remote.Provisioner() != ssh {
		t.Fatal("configured provisioner not returned")
	}
}

// The installing goroutine publishes the entry and drops its inflight
// record in ONE critical section, so a caller whose liveness check ran
// just before that lands in startAsync with neither visible. It must
// find the fresh entry there rather than spawn a second process for a
// host that already has one.
func TestStartAsync_FindsAnEntryInstalledDuringTheRace(t *testing.T) {
	t.Parallel()
	var starts int
	var mu sync.Mutex
	p := newTestPool(t, func(Spec) (*stubAgent, error) {
		mu.Lock()
		starts++
		mu.Unlock()
		return &stubAgent{}, nil
	}, "miki")

	first, err := p.Get(t.Context(), "miki")
	if err != nil {
		t.Fatal(err)
	}
	// Simulates the late caller: it has already decided nothing was
	// live and goes straight to startAsync.
	entry, call := p.startAsync(Spec{Host: "miki", Argv: []string{"fir"}})
	if call != nil {
		t.Fatal("startAsync launched a second start for a live host")
	}
	if entry != first {
		t.Fatalf("entry = %p, want the live one %p", entry, first)
	}
	mu.Lock()
	defer mu.Unlock()
	if starts != 1 {
		t.Fatalf("starts = %d, want 1", starts)
	}
}

// Logf is optional; a pool without one must still start and log.
func TestNew_NilLogf(t *testing.T) {
	t.Parallel()
	p, err := New(Config[*stubAgent]{
		Specs: map[string]Spec{"miki": {Argv: []string{"fir"}}},
		Start: func(Spec) (*stubAgent, error) { return &stubAgent{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Get(t.Context(), "miki"); err != nil {
		t.Fatalf("Get: %v", err)
	}
}
