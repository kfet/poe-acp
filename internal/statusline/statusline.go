// Package statusline renders the poe-acp side of the
// dev.acp-kit.status-line/v1 ACP extension: a compact one-line status
// line — model identity, mood, plan — shown as the live Thinking…
// spinner during the turn and as an italic FOOTER under the finished
// answer, so mobile users see fir-style signals they'd otherwise miss
// without a TUI.
//
// Footer, not header: mood and plan are agent-supplied and usually
// arrive mid-turn, long after the first token. Rendering the line at
// the END of the answer means it carries the LATEST snapshot instead
// of whatever was known before the agent had started thinking.
//
// The shared wire-contract pieces (extension id, length cap, Status
// type, ProviderEmoji map, ShortModelName, ParseMeta) live in
// github.com/kfet/acp-kit/statusline and are re-exported here so
// existing call sites keep a single import. Only the poe-acp-specific
// renderers (Header, Footer, Spinner) — which use poe markdown and the
// blockquote+italic spinner style — are owned here.
//
// See docs/ext/status-line.md for the wire spec.
package statusline

import (
	"strconv"
	"strings"

	kit "github.com/kfet/acp-kit/statusline"
)

// Re-exports from the kit so internal callers keep the existing
// `statusline.Foo` spelling and only one import.

// ExtensionID is the _meta key both sides use to advertise support
// and to carry per-update mood/plan payloads.
const ExtensionID = kit.ExtensionID

// MaxFieldRunes caps the rendered length of mood and plan.
const MaxFieldRunes = kit.MaxFieldRunes

// MaxTrailingFieldRunes caps the LAST segment of the spinner line — the
// live activity label. Wider than MaxFieldRunes because nothing follows
// it, so it cannot push the mood/plan header off a narrow screen.
const MaxTrailingFieldRunes = kit.MaxTrailingFieldRunes

// Status is the renderable state of one status header.
type Status = kit.Status

// ProviderEmojiForModel resolves the provider emoji from a fully
// qualified model id of the form "<provider>/<model>".
func ProviderEmojiForModel(modelID string) string { return kit.ProviderEmojiForModel(modelID) }

// ShortModelName derives the compact display name shown next to the
// provider emoji from a fully qualified model id
// ("anthropic/claude-opus-4-5-20251001" → "opus-4.5").
func ShortModelName(modelID string) string { return kit.ShortModelName(modelID) }

// ProviderEmoji maps a provider slug (case-insensitive) to the emoji
// shown in the status header.
func ProviderEmoji(slug string) string { return kit.ProviderEmoji(slug) }

// ParseMeta extracts the v1 mood/plan fields from a session/update
// _meta map.
func ParseMeta(meta map[string]any) (mood, plan string, ok bool) { return kit.ParseMeta(meta) }

// Header renders the bare status line (no "Thinking…" suffix, no
// markup). Returns "" when nothing would be shown — the caller then
// emits nothing at all. Segments are joined with " • " and empty
// segments are dropped; the provider emoji and model name share the
// first segment ("🏛️ opus-4.5 • steady • 2/5").
//
// This is the raw line. The final-answer surface is Footer, which
// wraps it in the blank line + italics that make it read as a
// signature under the answer rather than as body text.
func Header(s Status) string {
	return strings.Join(kit.Segments(s), " • ")
}

// Footer renders the status line as it is appended to the END of a
// finished answer: a blank line, then the line in italics.
//
//	\n\n_🏛️ opus-4.5 • steady • 2/5_
//
// Returns "" when there is nothing to show (unknown provider, no model,
// no agent _meta), so the caller appends nothing rather than a stray
// blank line or an empty pair of underscores.
//
// The leading "\n\n" is part of the footer, not the caller's job: it is
// what stops the italic line being swallowed into the answer's last
// paragraph by the Markdown renderer.
func Footer(s Status) string {
	line := Header(s)
	if line == "" {
		return ""
	}
	return "\n\n_" + line + "_"
}

// Spinner renders the live thinking indicator. The label argument is
// the leading verb (e.g. "Thinking", "running bash", "waiting") and
// falls back to "Thinking" when empty — mid-turn keepalive frames pass
// the running tool's label so the user sees what the agent is doing
// during a long tool call. The dots argument is the current animation
// frame (e.g. ".", "..", "..."). The status segments include the model
// identity ("🏛️ opus-4.5"), so the live line names the model servicing
// the turn just as the final footer does. The result is wrapped in a
// Markdown blockquote + italic so it matches poe-acp's existing
// heartbeat styling; the spinner is a single block.
//
// Always emits a visible frame — even with no status segments, the
// caller still needs liveness signal, so the bare "> _Thinking..._"
// is returned.
func Spinner(s Status, label, dots string) string {
	if label == "" {
		label = "Thinking"
	}
	if dots == "" {
		dots = "."
	}
	parts := kit.Segments(s)
	parts = append(parts, label+dots)
	return "> _" + strings.Join(parts, " • ") + "_"
}

// PlanEntry is one item of the agent's current plan, already normalised
// by the router: Content is single-line and rune-capped, Status is the
// raw ACP status string ("pending" | "in_progress" | "completed").
type PlanEntry struct {
	Content string
	Status  string
}

// MaxPlanEntries bounds how many plan items a keepalive frame renders.
// Every frame re-sends the whole answer plus the transient region (see
// the bandwidth note on sink.emitSpinnerFrame), so a 40-step plan must
// not multiply that cost. The overflow is summarised as "… +N more".
const MaxPlanEntries = 8

// PlanChecklist renders the transient plan checklist that sits directly
// below the spinner line inside the keepalive frame. Each entry is its
// own Markdown blockquote line so the whole frame — spinner + checklist
// — renders as one quote block, and the whole thing is wiped when the
// final answer replaces the transient region.
//
// Returns "" when there is nothing worth showing (no entries, or every
// entry has empty content), so the caller appends nothing.
func PlanChecklist(entries []PlanEntry) string {
	var lines []string
	skipped := 0
	for _, e := range entries {
		if e.Content == "" {
			continue
		}
		if len(lines) == MaxPlanEntries {
			skipped++
			continue
		}
		lines = append(lines, "> "+planStatusEmoji(e.Status)+" "+e.Content)
	}
	if len(lines) == 0 {
		return ""
	}
	if skipped > 0 {
		lines = append(lines, "> _… +"+strconv.Itoa(skipped)+" more_")
	}
	return strings.Join(lines, "\n")
}

// planStatusEmoji maps an ACP plan-entry status to its checklist glyph.
// An unknown status renders as pending — a plan item the relay cannot
// classify is, from the user's point of view, simply not done yet.
func planStatusEmoji(status string) string {
	switch status {
	case "completed":
		return "✅"
	case "in_progress":
		return "⏳"
	default:
		return "▫️"
	}
}
