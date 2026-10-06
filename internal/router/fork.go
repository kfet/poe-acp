package router

import (
	"context"
	"errors"
	"log"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/client"
	kitlog "github.com/kfet/acp-kit/log"
)

// Poe conversation branching ("Branch" / "new chat from here" in a message's
// follow-up actions on web, macOS and Windows) opens a NEW chat that copies
// the conversation up to the chosen message. For a server bot it arrives as
// a query on a brand-new conversation_id whose `query` already holds the
// copied turns plus the user's new message.
//
// Whether the copied turns keep their original message_ids is NOT
// documented: the protocol spec only says identifiers are globally unique
// and that conversation_id "resets when context is cleared", and no captured
// branch request exists in this repo or the relay's logs (checked
// 2026-10-06). Global uniqueness suggests Poe mints fresh ids for the copies,
// but that is unverified. Detection therefore tries both: an exact
// message_id match first, then a role+content hash match. The hash path
// demands a longer overlap and a unique winner, because unlike ids, content
// can collide across unrelated chats.

// Forker is implemented by agents that can fork a session
// (client.AgentProc.ForkSession, acp-kit's unstable session/fork). Kept off
// the Agent interface so agents without fork support need not stub it.
type Forker interface {
	ForkSession(ctx context.Context, cwd string, parent acp.SessionId, at string, sink client.SessionUpdateSink) (acp.SessionId, error)
}

// minHashOverlap is how many consecutive turns a content-hash match must
// cover before it is trusted: one "hi"/"Hello!" pair is not enough to
// claim two chats share a history.
const minHashOverlap = 3

// branchMatch is the outcome of matching a branch prefix against a live
// session's incorporated transcript.
type branchMatch struct {
	parent  *sessionState
	overlap int
	// leaf: the branch point is the parent's last turn (its latest user
	// turn plus that turn's reply), so forking at the parent's leaf
	// carries exactly the copied history.
	leaf bool
	// replyTo is the message_id of the parent user turn whose reply ends
	// the copied prefix ("" if the prefix ends on a user turn). It keys
	// sessionState.turnLeaves for an earlier-than-leaf fork point.
	replyTo string
}

// alignBranch matches prefix (the copied turns, oldest first) against s, a
// parent's seenTurns. s never holds the reply to its own latest user turn —
// that reply is streamed after seenTurns is recorded — so a prefix that
// ends on one extra bot turn past s's end is a branch from the parent's
// leaf. eq decides turn identity (by id or by hash). ok is false when the
// prefix does not line up with s.
func alignBranch(s, prefix []turnFP, prefixEndsBot bool, eq func(a, b turnFP) bool) (overlap int, leaf bool, replyTo string, ok bool) {
	if len(prefix) == 0 || len(s) == 0 {
		return 0, false, "", false
	}
	find := func(t turnFP) int {
		for j := len(s) - 1; j >= 0; j-- {
			if eq(s[j], t) {
				return j
			}
		}
		return -1
	}
	p := prefix
	trailing := false
	j := find(p[len(p)-1])
	if j < 0 && prefixEndsBot {
		// The final bot turn is a reply s has not recorded yet.
		trailing = true
		p = p[:len(p)-1]
		if len(p) == 0 {
			return 0, false, "", false
		}
		j = find(p[len(p)-1])
	}
	if j < 0 {
		return 0, false, "", false
	}
	n := min(len(p), j+1)
	for i := 0; i < n; i++ {
		if !eq(p[len(p)-1-i], s[j-i]) {
			return 0, false, "", false
		}
	}
	switch {
	case trailing && s[j].user:
		replyTo = s[j].id
	case !trailing && !s[j].user && j > 0 && s[j-1].user:
		replyTo = s[j-1].id
	}
	leaf = trailing && j == len(s)-1
	if trailing {
		n++ // the unrecorded reply is part of the shared history too
	}
	return n, leaf, replyTo, true
}

// findBranchParent looks for the live session of userID that the copied
// prefix was branched from. Caller holds r.mu.
func (r *Router) findBranchParent(userID string, prefix []Turn) (branchMatch, bool) {
	fp := turnFingerprints(prefix)
	if len(fp) == 0 {
		return branchMatch{}, false
	}
	endsBot := prefix[len(prefix)-1].Role == "bot"
	byID := func(a, b turnFP) bool { return a.id == b.id }
	byHash := func(a, b turnFP) bool { return a.hash == b.hash && a.user == b.user }
	for _, mode := range []struct {
		eq       func(a, b turnFP) bool
		min      int
		byHashed bool
	}{{byID, 1, false}, {byHash, minHashOverlap, true}} {
		var best branchMatch
		ambiguous := false
		for _, st := range r.sessions {
			if st.userID != userID {
				continue
			}
			n, leaf, replyTo, ok := alignBranch(st.seenTurns, fp, endsBot, mode.eq)
			if !ok || n < mode.min {
				continue
			}
			// Longest overlap wins; on a tie a leaf match (the session
			// the branch was taken from, not an earlier fork of it) wins.
			switch {
			case n > best.overlap, n == best.overlap && leaf && !best.leaf:
				best = branchMatch{parent: st, overlap: n, leaf: leaf, replyTo: replyTo}
				ambiguous = false
			case n == best.overlap && leaf == best.leaf:
				ambiguous = true
			}
		}
		if best.parent == nil {
			continue
		}
		if ambiguous {
			kitlog.Debugf("branch: %d-turn prefix matches several sessions equally (hash=%v); not forking", best.overlap, mode.byHashed)
			return branchMatch{}, false
		}
		return best, true
	}
	return branchMatch{}, false
}

// recordTurnLeaf stores the agent's session-tree leaf id at the end of the
// reply to user turn userMsgID (session/prompt response _meta.leafId, via
// promptTurn), so a branch from an earlier turn forks exactly there
// instead of falling back to a reseed.
func (r *Router) recordTurnLeaf(st *sessionState, userMsgID, leaf string) {
	if userMsgID == "" || leaf == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if st.turnLeaves == nil {
		st.turnLeaves = make(map[string]string)
	}
	st.turnLeaves[userMsgID] = leaf
}

// tryFork handles a Poe branch: when the first query of a new conversation
// carries copied turns that match a live session, fork that session into
// st (already set up for convID) and report true. Any miss or failure is
// logged and reported false so the caller falls back to resume/reseed.
func (r *Router) tryFork(ctx context.Context, convID string, query []Turn, st *sessionState) bool {
	if len(query) < 2 || query[len(query)-1].Role != "user" {
		return false
	}
	r.mu.Lock()
	m, ok := r.findBranchParent(st.userID, query[:len(query)-1])
	var parentSID acp.SessionId
	var at string
	var parentTurns []turnFP
	if ok {
		parentSID = m.parent.sessionID
		at = m.parent.turnLeaves[m.replyTo]
		parentTurns = m.parent.seenTurns
	}
	r.mu.Unlock()
	if !ok {
		kitlog.Debugf("branch conv=%s: no live session matches the %d copied turns; seeding fresh", convID, len(query)-1)
		return false
	}
	fallback := func(why string, args ...any) bool {
		log.Printf("branch conv=%s from conv=%s: "+why+"; reseeding from the transcript instead", append([]any{convID, m.parent.convID}, args...)...)
		return false
	}
	if m.parent.agent() != st.agent() || m.parent.host != st.host {
		return fallback("parent lives on another agent or host")
	}
	if at != "" && m.leaf && !st.agent().Caps().ForkAt {
		at = "" // the leaf is the branch point anyway
	}
	if at == "" {
		if !m.leaf {
			return fallback("branch point is earlier than the parent's last turn and no per-turn leaf is known")
		}
		if !m.parent.queue.Idle() {
			return fallback("parent is mid-turn, its leaf is not the branch point")
		}
	}
	f, ok := st.agent().(Forker)
	if !ok {
		return fallback("agent cannot fork")
	}
	sid, err := f.ForkSession(ctx, st.cwd, parentSID, at, st)
	if err != nil {
		if errors.Is(err, client.ErrForkUnsupported) {
			return fallback("agent does not support session/fork: %v", err)
		}
		return fallback("session/fork failed: %v", err)
	}
	if at == "" {
		// A parent turn that started while we forked would have moved the
		// leaf past the branch point. Every turn replaces seenTurns under
		// r.mu before it is queued, so an unchanged slice proves none did.
		r.mu.Lock()
		moved := !sameTurns(m.parent.seenTurns, parentTurns) || !m.parent.queue.Idle()
		r.mu.Unlock()
		if moved {
			r.releaseQuietly(st.agent(), convID, sid)
			return fallback("parent took a turn during the fork")
		}
	}
	st.sessionID = sid
	// The fork carries the parent's history, system prompt included.
	st.pendingSystemPromptInline = false
	log.Printf("branch conv=%s: forked sid=%s from conv=%s sid=%s (at=%q)", convID, sid, m.parent.convID, parentSID, at)
	return true
}

// sameTurns reports whether a is the very slice b (not merely equal). b is a
// matched parent's seenTurns, so never empty.
func sameTurns(a, b []turnFP) bool {
	return len(a) == len(b) && &a[0] == &b[0]
}

// releaseQuietly drops a session the relay will not use.
func (r *Router) releaseQuietly(a Agent, convID string, sid acp.SessionId) {
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if err := a.ReleaseSession(ctx, sid); err != nil {
		r.noteReleaseError("branch conv="+convID, sid, err)
	}
}

// promptTurn runs one turn and returns its leaf id when the agent reports
// one (client.TurnPrompter, which *client.AgentProc satisfies). An agent
// that cannot report it gives an empty leaf.
func promptTurn(ctx context.Context, a Agent, sid acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, string, error) {
	if tp, ok := a.(client.TurnPrompter); ok {
		tr, err := tp.PromptTurn(ctx, sid, blocks)
		return tr.Stop, tr.LeafID, err
	}
	stop, err := a.Prompt(ctx, sid, blocks)
	return stop, "", err
}
