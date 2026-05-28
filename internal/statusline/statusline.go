// Package statusline renders the poe-acp side of the
// dev.acp-kit.status-line/v1 ACP extension: a compact one-line header
// prepended to assistant responses and the live Thinking… indicator,
// so mobile users see fir-style mood/plan signals they'd otherwise
// miss without a TUI.
//
// The shared wire-contract pieces (extension id, length cap, Status
// type, ProviderEmoji map, ParseMeta) live in
// github.com/kfet/acp-kit/statusline and are re-exported here so
// existing call sites keep a single import. Only the poe-acp-specific
// renderers (Header, Spinner) — which use poe markdown and the
// blockquote+italic spinner style — are owned here.
//
// See docs/ext/status-line.md for the wire spec.
package statusline

import (
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

// Status is the renderable state of one status header.
type Status = kit.Status

// ProviderEmojiForModel resolves the provider emoji from a fully
// qualified model id of the form "<provider>/<model>".
func ProviderEmojiForModel(modelID string) string { return kit.ProviderEmojiForModel(modelID) }

// ProviderEmoji maps a provider slug (case-insensitive) to the emoji
// shown in the status header.
func ProviderEmoji(slug string) string { return kit.ProviderEmoji(slug) }

// ParseMeta extracts the v1 mood/plan fields from a session/update
// _meta map.
func ParseMeta(meta map[string]any) (mood, plan string, ok bool) { return kit.ParseMeta(meta) }

// Header renders the final-message header (no "Thinking…" suffix).
// Returns "" when nothing would be shown — caller drops the prepend
// entirely. Segments are joined with " • " and empty segments are
// dropped.
func Header(s Status) string {
	return strings.Join(kit.Segments(s), " • ")
}

// Spinner renders the live thinking indicator. The dots argument is
// the current animation frame (e.g. ".", "..", "..."). The result is
// wrapped in a Markdown blockquote + italic so it matches poe-acp's
// existing heartbeat styling; the spinner is a single block.
//
// Always emits a visible frame — even with no status segments, the
// caller still needs liveness signal, so the bare "> _Thinking..._"
// is returned.
func Spinner(s Status, dots string) string {
	if dots == "" {
		dots = "."
	}
	parts := kit.Segments(s)
	parts = append(parts, "Thinking"+dots)
	return "> _" + strings.Join(parts, " • ") + "_"
}
