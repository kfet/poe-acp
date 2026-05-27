package statusline

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProviderEmoji(t *testing.T) {
	cases := map[string]string{
		"anthropic":          "🏛️",
		"Anthropic":          "🏛️",
		"  claude  ":         "🏛️",
		"openai":             "🌐",
		"codex":              "🌐",
		"google":             "✨",
		"google-antigravity": "✨",
		"gemini":             "✨",
		"xai":                "✖️",
		"grok":               "✖️",
		"meta-llama":         "🦙",
		"deepseek":           "🐋",
		"cohere":             "🔗",
		"sakana":             "🐡",
		"poe":                "👻",
		"openrouter":         "🔀",
		"groq":               "⚡",
		"mistral":            "🌪️",
		"mistralai":          "🌪️",
		"copilot":            "🐙",
		"github-copilot":     "🐙",
		// Unknown providers must return "" so the segment is dropped.
		"":               "",
		"weirdcorp":      "",
		"other":          "",
		"some-new-thing": "",
	}
	for slug, want := range cases {
		if got := ProviderEmoji(slug); got != want {
			t.Errorf("ProviderEmoji(%q) = %q, want %q", slug, got, want)
		}
	}
}

func TestHeaderRendering(t *testing.T) {
	cases := []struct {
		name string
		in   Status
		want string
	}{
		{"all-empty", Status{}, ""},
		{"emoji-only", Status{ProviderEmoji: "🏛️"}, "🏛️"},
		{"mood-only", Status{Mood: "steady"}, "steady"},
		{"plan-only", Status{Plan: "2/5"}, "2/5"},
		{"emoji-mood", Status{ProviderEmoji: "🏛️", Mood: "steady"}, "🏛️ • steady"},
		{"emoji-mood-plan", Status{ProviderEmoji: "🏛️", Mood: "steady", Plan: "2/5"}, "🏛️ • steady • 2/5"},
		{"mood-plan-no-emoji", Status{Mood: "steady", Plan: "2/5"}, "steady • 2/5"},
		// Whitespace fields are equivalent to absent.
		{"whitespace-mood", Status{ProviderEmoji: "🏛️", Mood: "   "}, "🏛️"},
	}
	for _, tc := range cases {
		if got := Header(tc.in); got != tc.want {
			t.Errorf("%s: Header(%#v) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestHeaderLengthCap(t *testing.T) {
	// Both mood and plan must be capped at MaxFieldRunes runes.
	longMood := strings.Repeat("a", MaxFieldRunes+10)
	longPlan := strings.Repeat("9/", MaxFieldRunes) // 24 runes
	s := Status{ProviderEmoji: "🏛️", Mood: longMood, Plan: longPlan}
	got := Header(s)
	wantMood := strings.Repeat("a", MaxFieldRunes)
	if !strings.Contains(got, wantMood) {
		t.Errorf("expected capped mood %q in header %q", wantMood, got)
	}
	// And the plan segment must be no longer than MaxFieldRunes runes.
	parts := strings.Split(got, " • ")
	if len(parts) != 3 {
		t.Fatalf("expected 3 segments, got %d: %q", len(parts), got)
	}
	if r := []rune(parts[1]); len(r) > MaxFieldRunes {
		t.Errorf("mood segment over cap: %d runes (%q)", len(r), parts[1])
	}
	if r := []rune(parts[2]); len(r) > MaxFieldRunes {
		t.Errorf("plan segment over cap: %d runes (%q)", len(r), parts[2])
	}
}

func TestHeaderMultiByteRuneCap(t *testing.T) {
	// Ensure rune-aware (not byte-aware) truncation — single emoji is
	// multiple bytes but one rune, must count as one.
	mood := strings.Repeat("🙂", MaxFieldRunes+5)
	got := Header(Status{Mood: mood})
	if r := []rune(got); len(r) != MaxFieldRunes {
		t.Errorf("emoji mood capped to %d runes, want %d (%q)", len(r), MaxFieldRunes, got)
	}
}

func TestSpinnerRendering(t *testing.T) {
	// Empty status still produces a visible frame.
	if got := Spinner(Status{}, "."); got != "> _Thinking._" {
		t.Errorf("empty spinner = %q", got)
	}
	if got := Spinner(Status{}, "..."); got != "> _Thinking..._" {
		t.Errorf("empty spinner ... = %q", got)
	}
	// Default dots fallback when empty.
	if got := Spinner(Status{}, ""); got != "> _Thinking._" {
		t.Errorf("default-dots spinner = %q", got)
	}
	// Full status: emoji + mood + plan + Thinking....
	got := Spinner(Status{ProviderEmoji: "🏛️", Mood: "steady", Plan: "2/5"}, "..")
	want := "> _🏛️ • steady • 2/5 • Thinking.._"
	if got != want {
		t.Errorf("full spinner = %q, want %q", got, want)
	}
	// Just provider (unknown mood/plan).
	if got := Spinner(Status{ProviderEmoji: "🌐"}, "."); got != "> _🌐 • Thinking._" {
		t.Errorf("emoji-only spinner = %q", got)
	}
}

func TestParseMeta(t *testing.T) {
	// Absent extension key — ok false.
	mood, plan, ok := ParseMeta(map[string]any{"other.ext/v1": map[string]any{}})
	if ok || mood != "" || plan != "" {
		t.Errorf("absent: ok=%v mood=%q plan=%q", ok, mood, plan)
	}
	// nil map.
	if _, _, ok := ParseMeta(nil); ok {
		t.Error("nil meta should report ok=false")
	}
	// Present but empty payload — ok true, fields empty.
	mood, plan, ok = ParseMeta(map[string]any{ExtensionID: map[string]any{}})
	if !ok || mood != "" || plan != "" {
		t.Errorf("empty payload: ok=%v mood=%q plan=%q", ok, mood, plan)
	}
	// Map-shaped payload (typical SDK decoding).
	mood, plan, ok = ParseMeta(map[string]any{
		ExtensionID: map[string]any{"mood": "steady", "plan": "2/5"},
	})
	if !ok || mood != "steady" || plan != "2/5" {
		t.Errorf("map payload: ok=%v mood=%q plan=%q", ok, mood, plan)
	}
	// RawMessage-shaped payload (alternative decoding path).
	raw := json.RawMessage(`{"mood":"curious","plan":"1/3"}`)
	mood, plan, ok = ParseMeta(map[string]any{ExtensionID: raw})
	if !ok || mood != "curious" || plan != "1/3" {
		t.Errorf("raw payload: ok=%v mood=%q plan=%q", ok, mood, plan)
	}
	// Over-long fields are capped on parse.
	long := strings.Repeat("x", MaxFieldRunes+5)
	mood, _, _ = ParseMeta(map[string]any{ExtensionID: map[string]any{"mood": long}})
	if r := []rune(mood); len(r) != MaxFieldRunes {
		t.Errorf("parse cap: mood %d runes, want %d", len(r), MaxFieldRunes)
	}
	// Non-string values are ignored rather than rejected.
	mood, plan, ok = ParseMeta(map[string]any{
		ExtensionID: map[string]any{"mood": 42, "plan": "ok"},
	})
	if !ok || mood != "" || plan != "ok" {
		t.Errorf("non-string mood: ok=%v mood=%q plan=%q", ok, mood, plan)
	}
}

func TestExtensionIDStable(t *testing.T) {
	// The wire key is part of the protocol — a typo would silently
	// break interop with fir / other emitters. Pin it here.
	if ExtensionID != "dev.poe-acp.status-line/v1" {
		t.Fatalf("ExtensionID = %q", ExtensionID)
	}
}

func TestProviderEmojiForModel(t *testing.T) {
	cases := map[string]string{
		"anthropic/claude-sonnet-4": "🏛️",
		"openai/gpt-5":              "🌐",
		"google/gemini-2.5-pro":     "✨",
		"weirdcorp/x":               "", // unknown provider
		"":                          "", // empty id → no slash → empty
		"no-slash-here":             "", // missing /
		"/leading-slash":            "", // i==0 → treated as no slash
		"/":                         "",
	}
	for id, want := range cases {
		if got := ProviderEmojiForModel(id); got != want {
			t.Errorf("ProviderEmojiForModel(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestParseMetaFallbackUnmarshal(t *testing.T) {
	// Default switch arm: the value is neither map[string]any nor
	// json.RawMessage — e.g. a struct that needs re-marshal. The
	// parser must best-effort round-trip through JSON.
	type emitted struct {
		Mood string `json:"mood"`
		Plan string `json:"plan"`
	}
	mood, plan, ok := ParseMeta(map[string]any{
		ExtensionID: emitted{Mood: "flat", Plan: "0/1"},
	})
	if !ok || mood != "flat" || plan != "0/1" {
		t.Errorf("fallback unmarshal: ok=%v mood=%q plan=%q", ok, mood, plan)
	}
	// Best-effort path on an unmarshallable input still returns ok=true
	// with empty fields rather than panicking.
	mood, plan, ok = ParseMeta(map[string]any{
		ExtensionID: func() {}, // not JSON-marshallable
	})
	if !ok || mood != "" || plan != "" {
		t.Errorf("unmarshallable: ok=%v mood=%q plan=%q", ok, mood, plan)
	}
}
