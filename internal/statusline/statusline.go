// Package statusline implements the dev.poe-acp.status-line/v1 ACP
// extension renderer: a compact one-line header prepended to assistant
// responses and the live Thinking… indicator, so mobile users see
// fir-style mood/plan signals they'd otherwise miss without a TUI.
//
// The provider emoji is relay-owned (poe-acp knows which provider the
// active model belongs to). The mood and plan strings are agent-emitted
// via session/update._meta and treated as opaque length-capped labels.
//
// See docs/ext/status-line.md for the wire spec.
package statusline

import (
	"encoding/json"
	"strings"
)

// ExtensionID is the _meta key both sides use to advertise support and
// to carry per-update mood/plan payloads.
const ExtensionID = "dev.poe-acp.status-line/v1"

// MaxFieldRunes caps the rendered length of mood and plan. Mobile chat
// surfaces have very little horizontal room; an oversize agent label
// must not push the header off-screen or wrap.
const MaxFieldRunes = 12

// ProviderEmojiForModel resolves the provider emoji from a fully
// qualified model id of the form "<provider>/<model>" (the convention
// used by both fir and the Poe parameter_controls layer). An id with
// no '/' (or an empty id) is treated as unknown — empty string return.
//
// Convenience wrapper around ProviderEmoji + the conventional slug
// split, exposed so callers don't need to reach into router internals.
func ProviderEmojiForModel(modelID string) string {
	i := strings.IndexByte(modelID, '/')
	if i <= 0 {
		return ""
	}
	return ProviderEmoji(modelID[:i])
}

// ProviderEmoji maps a provider slug (case-insensitive) to the emoji
// shown in the status header. Returns "" for unknown providers, which
// the renderer treats as a dropped segment.
//
// The mapping is relay-owned by design: poe-acp dispatches the turn
// and therefore knows the provider authoritatively. Agents do NOT
// supply this — see docs/ext/status-line.md.
func ProviderEmoji(slug string) string {
	switch strings.ToLower(strings.TrimSpace(slug)) {
	case "anthropic", "claude":
		return "🏛️"
	case "openai", "codex":
		return "🌐"
	case "poe":
		return "👻"
	case "google", "gemini", "google-antigravity", "antigravity":
		return "✨"
	case "copilot", "github-copilot", "github":
		return "🐙"
	case "sakana":
		return "🐡"
	case "xai", "grok":
		return "✖️"
	case "mistral", "mistralai":
		return "🌪️"
	case "meta", "meta-llama", "llama":
		return "🦙"
	case "openrouter":
		return "🔀"
	case "groq":
		return "⚡"
	case "deepseek":
		return "🐋"
	case "cohere":
		return "🔗"
	default:
		return ""
	}
}

// Status is the renderable state of one status header.
type Status struct {
	// ProviderEmoji is the relay-resolved emoji for the provider that
	// will service the turn. Empty means unknown provider → segment
	// dropped.
	ProviderEmoji string
	// Mood is the agent-supplied mood label (opaque string).
	Mood string
	// Plan is the agent-supplied plan progress label (opaque string).
	Plan string
}

// ParseMeta extracts the v1 mood/plan fields from a session/update
// _meta map. Returns (mood, plan, ok). ok is true if the extension key
// was present, regardless of whether mood/plan themselves were set.
// Both fields are returned trimmed and capped to MaxFieldRunes.
//
// Unknown sub-keys are ignored; non-string values are treated as
// absent rather than rejected (forward compat).
func ParseMeta(meta map[string]any) (mood, plan string, ok bool) {
	if meta == nil {
		return "", "", false
	}
	raw, present := meta[ExtensionID]
	if !present {
		return "", "", false
	}
	// The SDK decodes _meta as map[string]any with sub-objects landing
	// as either map[string]any or json.RawMessage depending on call
	// path. Normalise via re-marshal/unmarshal.
	var payload struct {
		Mood string `json:"mood"`
		Plan string `json:"plan"`
	}
	switch v := raw.(type) {
	case map[string]any:
		if s, ok := v["mood"].(string); ok {
			payload.Mood = s
		}
		if s, ok := v["plan"].(string); ok {
			payload.Plan = s
		}
	case json.RawMessage:
		_ = json.Unmarshal(v, &payload)
	default:
		// Best-effort: re-marshal whatever it is and try again.
		if b, err := json.Marshal(v); err == nil {
			_ = json.Unmarshal(b, &payload)
		}
	}
	return capRunes(strings.TrimSpace(payload.Mood), MaxFieldRunes),
		capRunes(strings.TrimSpace(payload.Plan), MaxFieldRunes),
		true
}

// Header renders the final-message header (no "Thinking…" suffix).
// Returns "" when nothing would be shown — caller drops the prepend
// entirely. Segments are joined with " • " and empty segments are
// dropped.
func Header(s Status) string {
	parts := segments(s)
	return strings.Join(parts, " • ")
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
	parts := segments(s)
	parts = append(parts, "Thinking"+dots)
	return "> _" + strings.Join(parts, " • ") + "_"
}

// segments returns the non-empty header segments in order.
func segments(s Status) []string {
	out := make([]string, 0, 3)
	if e := strings.TrimSpace(s.ProviderEmoji); e != "" {
		out = append(out, e)
	}
	if m := capRunes(strings.TrimSpace(s.Mood), MaxFieldRunes); m != "" {
		out = append(out, m)
	}
	if p := capRunes(strings.TrimSpace(s.Plan), MaxFieldRunes); p != "" {
		out = append(out, p)
	}
	return out
}

// capRunes truncates s to at most n runes. No ellipsis is appended:
// the cap is tight (12 runes) and the agent picks the label, so an
// ellipsis would only steal another character of meaning. Callers
// pass MaxFieldRunes (a positive constant), so no n<=0 guard.
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
