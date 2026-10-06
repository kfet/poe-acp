package router

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/client"
)

// forkAgent is a fakeAgent that also implements Forker.
type forkAgent struct {
	*fakeAgent
	forkErr    error
	onFork     func()
	forkCalls  int32
	lastParent acp.SessionId
	lastAt     string
}

func (f *forkAgent) ForkSession(_ context.Context, _ string, parent acp.SessionId, at string, sink client.SessionUpdateSink) (acp.SessionId, error) {
	atomic.AddInt32(&f.forkCalls, 1)
	if f.onFork != nil {
		f.onFork()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastParent, f.lastAt = parent, at
	if f.forkErr != nil {
		return "", f.forkErr
	}
	f.nextID++
	id := acp.SessionId("fork-" + itoa(f.nextID))
	f.sinks[id] = sink
	return id, nil
}

func newForkAgent() *forkAgent {
	return &forkAgent{fakeAgent: newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})}
}

func forkRouter(t *testing.T, a Agent) *Router {
	t.Helper()
	r, err := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// parentTurns is the transcript of the parent's latest query (its reply to
// q3 not yet recorded).
var parentTurns = []Turn{
	{Role: "user", Content: "q1", MessageID: "m1"},
	{Role: "bot", Content: "a1", MessageID: "b1"},
	{Role: "user", Content: "q2", MessageID: "m2"},
	{Role: "bot", Content: "a2", MessageID: "b2"},
	{Role: "user", Content: "q3", MessageID: "m3"},
}

func seedParent(t *testing.T, r *Router) acp.SessionId {
	t.Helper()
	if err := r.Prompt(context.Background(), "parent", "u", parentTurns, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions["parent"].sessionID
}

// renamed returns turns with fresh message_ids, modelling Poe minting new
// ids for the copied turns of a branch.
func renamed(turns []Turn) []Turn { return renamedAs("x", turns) }

func renamedAs(tag string, turns []Turn) []Turn {
	out := make([]Turn, len(turns))
	for i, t := range turns {
		t.MessageID = fmt.Sprintf("%s%d", tag, i)
		out[i] = t
	}
	return out
}

func branchQuery(prefix []Turn) []Turn {
	return append(append([]Turn{}, prefix...), Turn{Role: "user", Content: "new q", MessageID: "mnew"})
}

func leafPrefix() []Turn {
	return append(append([]Turn{}, parentTurns...), Turn{Role: "bot", Content: "a3", MessageID: "b3"})
}

func TestFork_LeafBranchByID(t *testing.T) {
	a := newForkAgent()
	r := forkRouter(t, a)
	psid := seedParent(t, r)

	if err := r.Prompt(context.Background(), "child", "u", branchQuery(leafPrefix()), Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&a.forkCalls); got != 1 {
		t.Fatalf("forkCalls=%d, want 1", got)
	}
	if a.lastParent != psid || a.lastAt != "" {
		t.Fatalf("fork parent=%q at=%q, want %q at leaf", a.lastParent, a.lastAt, psid)
	}
	if got := atomic.LoadInt32(&a.newSessCalls); got != 1 {
		t.Fatalf("branch must not create a new session: newSessCalls=%d", got)
	}
	// Not reseeded: the agent only sees the new message.
	a.mu.Lock()
	txt := a.lastPromptTxt
	a.mu.Unlock()
	if txt != "new q" {
		t.Fatalf("prompt=%q, want only the new message", txt)
	}
	r.mu.Lock()
	child := r.sessions["child"]
	r.mu.Unlock()
	if child.sessionID == psid || child.sessionID == "" {
		t.Fatalf("child sid=%q", child.sessionID)
	}
}

func TestFork_LeafBranchByContentHash(t *testing.T) {
	a := newForkAgent()
	r := forkRouter(t, a)
	psid := seedParent(t, r)
	if err := r.Prompt(context.Background(), "child", "u", branchQuery(renamed(leafPrefix())), Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&a.forkCalls) != 1 || a.lastParent != psid {
		t.Fatalf("expected hash-matched fork of %q, got calls=%d parent=%q", psid, a.forkCalls, a.lastParent)
	}
}

func TestFork_EarlierBranchPointReseeds(t *testing.T) {
	a := newForkAgent()
	r := forkRouter(t, a)
	seedParent(t, r)
	// Branch from b2 (reply to m2): earlier than the parent's last turn.
	if err := r.Prompt(context.Background(), "child", "u", branchQuery(parentTurns[:4]), Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&a.forkCalls); got != 0 {
		t.Fatalf("earlier branch without a turn leaf must not fork: forkCalls=%d", got)
	}
	if got := atomic.LoadInt32(&a.newSessCalls); got != 2 {
		t.Fatalf("expected reseed via new session: newSessCalls=%d", got)
	}
}

func TestFork_EarlierBranchPointUsesTurnLeaf(t *testing.T) {
	a := newForkAgent()
	r := forkRouter(t, a)
	seedParent(t, r)
	r.mu.Lock()
	parent := r.sessions["parent"]
	r.mu.Unlock()
	r.recordTurnLeaf(parent, "", "x")  // ignored
	r.recordTurnLeaf(parent, "m2", "") // ignored
	r.recordTurnLeaf(parent, "m2", "leaf-m2")
	r.recordTurnLeaf(parent, "m1", "leaf-m1")

	// Prefix ends on an unrecorded bot reply to m1: trailing reply → keyed
	// by m1. Run first: once c2 exists it shares m1 and the id match ties.
	q := []Turn{parentTurns[0], {Role: "bot", Content: "other", MessageID: "bz"}}
	if err := r.Prompt(context.Background(), "c3", "u", branchQuery(q), Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if a.lastAt != "leaf-m1" {
		t.Fatalf("at=%q, want leaf-m1", a.lastAt)
	}
	// Prefix ends on b2, a bot turn the parent recorded → reply to m2.
	// (c3 shares only m1, so the parent's longer overlap wins.)
	if err := r.Prompt(context.Background(), "c2", "u", branchQuery(parentTurns[:4]), Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if a.lastAt != "leaf-m2" {
		t.Fatalf("at=%q, want leaf-m2", a.lastAt)
	}
}

func TestFork_Fallbacks(t *testing.T) {
	cases := []struct {
		name  string
		setup func(a *forkAgent)
	}{
		{"unsupported", func(a *forkAgent) { a.forkErr = fmt.Errorf("wrap: %w", client.ErrForkUnsupported) }},
		{"error", func(a *forkAgent) { a.forkErr = errors.New("boom") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newForkAgent()
			r := forkRouter(t, a)
			seedParent(t, r)
			tc.setup(a)
			if err := r.Prompt(context.Background(), "child", "u", branchQuery(leafPrefix()), Options{}, &captureSink{}); err != nil {
				t.Fatal(err)
			}
			if atomic.LoadInt32(&a.forkCalls) != 1 || atomic.LoadInt32(&a.newSessCalls) != 2 {
				t.Fatalf("want fork attempt then reseed: fork=%d new=%d", a.forkCalls, a.newSessCalls)
			}
		})
	}
}

func TestFork_AgentWithoutForker(t *testing.T) {
	a := newForkAgent().fakeAgent
	r := forkRouter(t, a)
	seedParent(t, r)
	if err := r.Prompt(context.Background(), "child", "u", branchQuery(leafPrefix()), Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&a.newSessCalls); got != 2 {
		t.Fatalf("newSessCalls=%d, want reseed", got)
	}
}

func TestFork_TryForkGuards(t *testing.T) {
	a := newForkAgent()
	r := forkRouter(t, a)
	seedParent(t, r)
	r.mu.Lock()
	parent := r.sessions["parent"]
	r.mu.Unlock()
	ctx := context.Background()
	mk := func(user string) *sessionState {
		return &sessionState{userID: user, target: AgentTarget{Agent: a}, cwd: t.TempDir()}
	}
	q := branchQuery(leafPrefix())

	if r.tryFork(ctx, "c", q[:1], mk("u")) {
		t.Fatal("single-turn query must not fork")
	}
	if r.tryFork(ctx, "c", leafPrefix()[:2], mk("u")) {
		t.Fatal("query ending on a bot turn must not fork")
	}
	if r.tryFork(ctx, "c", q, mk("other")) {
		t.Fatal("other user's session must not match")
	}
	other := mk("u")
	other.target = AgentTarget{Agent: newForkAgent()}
	if r.tryFork(ctx, "c", q, other) {
		t.Fatal("parent on another agent must not fork")
	}
	elsewhere := mk("u")
	elsewhere.host = "host-b"
	if r.tryFork(ctx, "c", q, elsewhere) {
		t.Fatal("parent on another host must not fork")
	}
	// Parent mid-turn: its leaf is not the branch point. Swap in a copy
	// whose queue no runner drains, so the pushed item stays pending.
	busy := &sessionState{convID: "parent", userID: "u", target: parent.target,
		sessionID: parent.sessionID, seenTurns: parent.seenTurns, queue: newSessionQueue()}
	busy.queue.Push(&turnReq{})
	r.mu.Lock()
	r.sessions["parent"] = busy
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.sessions["parent"] = parent
		r.mu.Unlock()
	}()
	if r.tryFork(ctx, "c", q, mk("u")) {
		t.Fatal("busy parent must not fork at leaf")
	}
	if atomic.LoadInt32(&a.forkCalls) != 0 {
		t.Fatalf("forkCalls=%d", a.forkCalls)
	}
}

func TestFindBranchParent(t *testing.T) {
	r := forkRouter(t, newForkAgent())
	fp := turnFingerprints(parentTurns)
	r.sessions["p1"] = &sessionState{userID: "u", seenTurns: fp}

	if _, ok := r.findBranchParent("u", []Turn{{Role: "user", Content: "q1"}}); ok {
		t.Fatal("id-less prefix must not match")
	}
	// Hash match below the overlap floor.
	if _, ok := r.findBranchParent("u", renamed(parentTurns[:2])); ok {
		t.Fatal("short hash overlap must not match")
	}
	// Hash match long enough.
	if m, ok := r.findBranchParent("u", renamed(parentTurns[:3])); !ok || m.overlap != 3 || m.leaf {
		t.Fatalf("hash match: %+v %v", m, ok)
	}
	// Two sessions equally matching by hash → ambiguous.
	r.sessions["p2"] = &sessionState{userID: "u", seenTurns: turnFingerprints(renamedAs("y", parentTurns))}
	if _, ok := r.findBranchParent("u", renamed(parentTurns[:4])); ok {
		t.Fatal("ambiguous hash match must not fork")
	}
	// A longer overlap wins over a shorter one.
	r.sessions["p3"] = &sessionState{userID: "u", seenTurns: fp[2:]}
	if m, ok := r.findBranchParent("u", parentTurns); !ok || m.parent != r.sessions["p1"] {
		t.Fatalf("longest overlap: %+v %v", m, ok)
	}
}

func TestAlignBranch(t *testing.T) {
	s := turnFingerprints(parentTurns)
	byID := func(a, b turnFP) bool { return a.id == b.id }
	if _, _, _, ok := alignBranch(nil, s, false, byID); ok {
		t.Fatal("empty s")
	}
	if _, _, _, ok := alignBranch(s, nil, false, byID); ok {
		t.Fatal("empty prefix")
	}
	// A lone unknown bot turn: nothing left to align after stripping it.
	lone := turnFingerprints([]Turn{{Role: "bot", Content: "z", MessageID: "bz"}})
	if _, _, _, ok := alignBranch(s, lone, true, byID); ok {
		t.Fatal("lone trailing bot")
	}
	// Unknown last turn that is not a bot.
	unk := turnFingerprints([]Turn{{Role: "user", Content: "z", MessageID: "mz"}})
	if _, _, _, ok := alignBranch(s, unk, false, byID); ok {
		t.Fatal("unknown user turn")
	}
	// Last turn matches but an earlier one disagrees.
	bad := turnFingerprints([]Turn{{Role: "user", Content: "z", MessageID: "mz"}, parentTurns[1]})
	if _, _, _, ok := alignBranch(s, bad, false, byID); ok {
		t.Fatal("misaligned prefix")
	}
	// Prefix ends on a user turn: no reply to key on.
	if n, leaf, replyTo, ok := alignBranch(s, s[:3], false, byID); !ok || n != 3 || leaf || replyTo != "" {
		t.Fatalf("user-ended prefix: %d %v %q %v", n, leaf, replyTo, ok)
	}
	// Prefix front-truncated relative to s still aligns.
	if n, _, replyTo, ok := alignBranch(s, s[2:4], false, byID); !ok || n != 2 || replyTo != "m2" {
		t.Fatalf("truncated prefix: %d %q %v", n, replyTo, ok)
	}
}

// A second branch from the parent's leaf, after a first branch forked a
// child that shares the parent's ids, must still fork the parent.
func TestFork_SecondBranchPrefersLeafParent(t *testing.T) {
	a := newForkAgent()
	r := forkRouter(t, a)
	psid := seedParent(t, r)
	for _, conv := range []string{"child1", "child2"} {
		if err := r.Prompt(context.Background(), conv, "u", branchQuery(leafPrefix()), Options{}, &captureSink{}); err != nil {
			t.Fatal(err)
		}
		if a.lastParent != psid || a.lastAt != "" {
			t.Fatalf("%s: forked parent=%q at=%q, want %q at leaf", conv, a.lastParent, a.lastAt, psid)
		}
	}
	if got := atomic.LoadInt32(&a.forkCalls); got != 2 {
		t.Fatalf("forkCalls=%d, want 2", got)
	}
}

// A parent turn that lands while the leaf fork is in flight moves the leaf
// past the branch point: the child is released and reseeded.
func TestFork_ParentMovedDuringFork(t *testing.T) {
	a := newForkAgent()
	r := forkRouter(t, a)
	seedParent(t, r)
	r.mu.Lock()
	parent := r.sessions["parent"]
	r.mu.Unlock()
	a.releaseErr = errors.New("release boom")
	a.onFork = func() {
		r.mu.Lock()
		parent.seenTurns = append([]turnFP(nil), parent.seenTurns...)
		r.mu.Unlock()
	}
	if err := r.Prompt(context.Background(), "child", "u", branchQuery(leafPrefix()), Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&a.newSessCalls); got != 2 {
		t.Fatalf("newSessCalls=%d, want reseed", got)
	}
	if got := atomic.LoadInt32(&a.releaseCalls); got != 1 {
		t.Fatalf("releaseCalls=%d, want 1", got)
	}
}

// A recorded per-turn leaf the agent cannot fork at is dropped when the
// branch point is the leaf anyway.
func TestFork_LeafAtWithoutForkAtCap(t *testing.T) {
	a := newForkAgent()
	r := forkRouter(t, a)
	seedParent(t, r)
	r.mu.Lock()
	parent := r.sessions["parent"]
	r.mu.Unlock()
	r.recordTurnLeaf(parent, "m3", "leaf-m3")
	if err := r.Prompt(context.Background(), "child", "u", branchQuery(leafPrefix()), Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if a.lastAt != "" || atomic.LoadInt32(&a.newSessCalls) != 1 {
		t.Fatalf("at=%q new=%d, want leaf fork", a.lastAt, a.newSessCalls)
	}
	a.caps.ForkAt = true
	if err := r.Prompt(context.Background(), "child2", "u", branchQuery(leafPrefix()), Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if a.lastAt != "leaf-m3" {
		t.Fatalf("at=%q, want leaf-m3", a.lastAt)
	}
}
