package router

// Durable tool DETAIL rendering (the show_tool_details option).
//
// The one-line record of a tool call (emitToolLine, router.go) answers
// "what did the agent run"; it does not answer "with what arguments"
// or "did it work". Both of those live in the ACP payload the relay
// used to drop on the floor: SessionUpdateToolCall.Content (the agent's
// start block — for a remote-exec tool, the host and the full command)
// and the TERMINAL tool_call_update (status plus result text).
//
// This file renders both, under three hard constraints:
//
//   - Framing. Everything stays inside the tool line's blockquote:
//     every emitted line is "> "-prefixed, and no unescaped construct
//     may let the content out. Code fences are normalised and balanced
//     (an odd fence count gets a synthetic closer) so a truncated or
//     hostile block cannot swallow the answer that follows; anywhere
//     else backticks are neutralised to "'" exactly like the title.
//   - Bounding. Per content block: maxDetailLines lines and
//     maxDetailChars characters, head and tail kept, middle replaced by
//     an elision marker. This is chat on a phone; an unbounded 4000-line
//     test log would bury the answer.
//   - Opt-out fidelity. Nothing here runs unless BOTH show_tools and
//     show_tool_details are on, so the previous output is reproduced
//     byte for byte when the toggle is off.
import (
	"fmt"
	"regexp"
	"strings"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/poe-acp/internal/poeproto"
)

const (
	// maxDetailLines bounds one rendered content block, INCLUDING the
	// elision marker line.
	maxDetailLines = 12
	// maxDetailChars bounds one rendered content block by size, so a
	// dozen 10 KB lines cannot slip past the line cap.
	maxDetailChars = 800
	// maxDetailLineRunes bounds a SINGLE line, so one enormous line
	// (minified JSON, a base64 blob) still leaves room for a second.
	maxDetailLineRunes = maxDetailChars / 2
)

// toolState is the per-turn memory of one tool call, keyed by
// ToolCallId. It exists so a tool_call_update — which may carry neither
// title nor kind — can be rendered with the call it belongs to, and so
// only the FIRST terminal update for a call produces output.
type toolState struct {
	title string
	kind  acp.ToolKind
	done  bool // terminal group already emitted
	// seen holds the rendered form of every content block already
	// emitted for THIS call, so the terminal update can drop blocks
	// that merely repeat the start block (a remote-exec tool echoes
	// the command in both). Keyed per call: two different calls that
	// happen to render identically both keep their content.
	seen map[string]struct{}
}

// markSeen records rendered blocks as already emitted for this call.
func (st *toolState) markSeen(blocks []string) {
	if st.seen == nil {
		st.seen = make(map[string]struct{}, len(blocks))
	}
	for _, b := range blocks {
		st.seen[b] = struct{}{}
	}
}

// dropSeen returns blocks minus any whose rendered form was already
// emitted for this call.
func (st *toolState) dropSeen(blocks []string) []string {
	out := blocks[:0:0]
	for _, b := range blocks {
		if _, dup := st.seen[b]; dup {
			continue
		}
		out = append(out, b)
	}
	return out
}

// newToolStates allocates the per-turn correlation map, or nil when
// detail rendering is off (no map, no bookkeeping, no cost).
func newToolStates(o Options) map[acp.ToolCallId]*toolState {
	if !o.ShowTools || !o.ShowToolDetails {
		return nil
	}
	return make(map[acp.ToolCallId]*toolState)
}

// rememberTool records the title/kind of a tool call for correlation
// with its later updates. A repeat tool_call for the same id replaces
// the entry wholesale — ACP semantics are "this call is starting", so
// the newest title wins and its terminal group is due again.
func (td *turnDef) rememberTool(id acp.ToolCallId, title string, kind acp.ToolKind) {
	td.tools[id] = &toolState{title: title, kind: kind}
}

// emitToolDetail appends a tool call's content blocks as continuation
// lines of the blockquote emitTools opened. Called immediately after
// emitToolLine, so chunkMode/first are already correct and no separator
// beyond a single newline is needed. Every block it renders is recorded
// on the call's toolState so the terminal update can skip repeats.
func emitToolDetail(td *turnDef, id acp.ToolCallId, content []acp.ToolCallContent) {
	blocks := detailBlocks(content)
	if len(blocks) == 0 {
		return
	}
	if st := td.tools[id]; st != nil {
		st.markSeen(blocks)
	}
	_ = td.sink.Text(poeproto.EscapeReservedFlags("\n" + strings.Join(blocks, "\n")))
}

// emitToolResult renders the completion of a tool call: a status marker,
// the remembered title when it is not obvious from context, and the
// update's content. Only TERMINAL statuses (completed/failed) produce
// output — everything else stays spinner-only, exactly as before this
// option existed.
//
// Three reductions apply, all unconditional:
//
//   - Adjacency. When this call's own start line was the most recent
//     durable tool write and nothing landed in between, the label is
//     dropped: the marker sits directly under the line it completes, so
//     repeating the title says nothing. Parallel calls are routine, so
//     ANY intervening write (another tool's line or group, message or
//     thought text) restores the full label — an unlabelled marker must
//     never be ambiguous.
//   - Content dedupe. Blocks already rendered at start time for THIS
//     call are dropped (see toolState.seen).
//   - Silent success. A `completed` update with nothing left to render
//     emits nothing at all: the start line already recorded the call.
//     `failed` ALWAYS renders — failures must be loud.
func emitToolResult(td *turnDef, first *bool, chunkMode *chunkKind, u *acp.SessionToolCallUpdate) {
	if u.Status == nil {
		return
	}
	var marker string
	switch *u.Status {
	case acp.ToolCallStatusCompleted:
		marker = "✓"
	case acp.ToolCallStatusFailed:
		marker = "✗"
	default:
		return
	}
	var (
		title string
		kind  acp.ToolKind
	)
	st := td.tools[u.ToolCallId]
	if st != nil {
		if st.done {
			// A tool call may report terminal state more than once
			// (status echoed on a later content update). One group.
			return
		}
		st.done = true
		title, kind = st.title, st.kind
	}
	if u.Title != nil && *u.Title != "" {
		title = *u.Title
	}
	if u.Kind != nil && *u.Kind != "" {
		kind = *u.Kind
	}

	blocks := detailBlocks(u.Content)
	if st != nil {
		blocks = st.dropSeen(blocks)
	}
	if *u.Status == acp.ToolCallStatusCompleted && len(blocks) == 0 {
		// Silent success: the start line stands as the whole record.
		// Nothing is written, so the stream position (chunkMode, the
		// adjacency memory, the blockquote framing) is left untouched.
		return
	}

	// Read the adjacency memory BEFORE openToolBlock mutates chunkMode.
	head := "> `" + marker + "`"
	if !td.toolAdjacent(u.ToolCallId, *chunkMode) {
		head = "> `" + marker + " " + strings.ReplaceAll(toolLineLabel(title, kind), "`", "'") + "`"
	}

	prefix := openToolBlock(td, first, chunkMode)
	out := prefix + head
	if len(blocks) > 0 {
		out += "\n" + strings.Join(blocks, "\n")
	}
	td.markToolWrite(u.ToolCallId)
	_ = td.sink.Text(poeproto.EscapeReservedFlags(out))
}

// detailBlocks renders the text content blocks of a tool call, one
// joined blockquote-prefixed string per block: bounded, framing-safe.
// Blocks stay separate (rather than one flat line list) because dedupe
// is per block. Non-text variants (diff, terminal, images) are skipped:
// they have no compact one-block rendering that survives the bounding
// rules.
func detailBlocks(content []acp.ToolCallContent) []string {
	var out []string
	for _, c := range content {
		if c.Content == nil || c.Content.Content.Text == nil {
			continue
		}
		if lines := quoteDetailBlock(c.Content.Content.Text.Text); len(lines) > 0 {
			out = append(out, strings.Join(lines, "\n"))
		}
	}
	return out
}

// fenceRe matches a line that is nothing but a code fence, optionally
// indented and with an info string. Only such lines are treated as
// fences; a stray "```" mid-line is neutralised like any other backtick.
var fenceRe = regexp.MustCompile("^[ \t]*(?:`{3,}|~{3,})[ \t]*([A-Za-z0-9_+-]*)[ \t]*$")

// quoteDetailBlock bounds one content block and renders it as
// blockquote lines, keeping fenced-code framing balanced.
func quoteDetailBlock(text string) []string {
	lines := boundDetail(text)
	if len(lines) == 0 {
		return nil
	}
	out := make([]string, 0, len(lines)+1)
	fences := 0
	for _, l := range lines {
		s, isFence := sanitiseDetailLine(l)
		if isFence {
			fences++
		}
		if s == "" {
			out = append(out, ">")
			continue
		}
		out = append(out, "> "+s)
	}
	if fences%2 == 1 {
		// Truncation (or hostile input) left a fence open: close it, or
		// it would swallow the rest of the answer.
		out = append(out, "> ```")
	}
	return out
}

// sanitiseDetailLine makes one line safe to place after "> ". Fence
// lines are normalised to a plain backtick fence (info string kept, it
// is regex-restricted to word characters) and reported so the caller
// can balance them; every other line has its backticks neutralised so
// no code span can be opened and left dangling.
func sanitiseDetailLine(l string) (string, bool) {
	l = strings.ReplaceAll(l, "\r", "")
	l = strings.ReplaceAll(l, "\t", "    ")
	if m := fenceRe.FindStringSubmatch(l); m != nil {
		return "```" + m[1], true
	}
	return strings.TrimRight(strings.ReplaceAll(l, "`", "'"), " "), false
}

// boundDetail splits a content block into lines and enforces the line
// and character budgets, keeping the head and the tail and replacing the
// middle with an elision marker. Returns lines WITHOUT quote prefixes.
func boundDetail(text string) []string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = capRunesEllipsis(l, maxDetailLineRunes)
	}
	n := len(out)
	if n <= maxDetailLines && runeLen(out, n, 0) <= maxDetailChars {
		return out
	}
	// Elide: keep k lines (head + tail) plus one marker line, shrinking
	// k until the character budget holds too. k >= 2 always, and each
	// line is already capped at half the budget, so this terminates
	// within budget.
	k := maxDetailLines - 1
	if k > n-1 {
		k = n - 1
	}
	for {
		head := (k + 1) / 2
		tail := k - head
		if k <= 2 || runeLen(out, head, tail) <= maxDetailChars {
			elided := n - head - tail
			res := make([]string, 0, k+1)
			res = append(res, out[:head]...)
			res = append(res, fmt.Sprintf("… %d lines elided …", elided))
			res = append(res, out[n-tail:]...)
			return res
		}
		k--
	}
}

// runeLen totals the rune count of the first `head` and last `tail`
// lines of s (newlines not counted — they are framing, not content).
func runeLen(s []string, head, tail int) int {
	total := 0
	for _, l := range s[:head] {
		total += len([]rune(l))
	}
	for _, l := range s[len(s)-tail:] {
		total += len([]rune(l))
	}
	return total
}

// capRunesEllipsis truncates s to at most n runes, marking the cut with
// a trailing ellipsis so the user knows the line was clipped.
func capRunesEllipsis(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
