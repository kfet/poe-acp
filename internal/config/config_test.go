package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kfet/acp-kit/client"
)

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

func TestLoad_OpenError(t *testing.T) {
	t.Parallel()
	// Use a path under a non-directory: triggers ENOTDIR (non NotExist).
	p := writeFile(t, `{}`)
	_, _, err := Load(filepath.Join(p, "child.json"))
	if err == nil || strings.Contains(err.Error(), "no such") {
		t.Fatalf("expected open error, got %v", err)
	}
}

func TestBoolPtr(t *testing.T) {
	if v := boolPtr(true); v == nil || !*v {
		t.Fatal("boolPtr broken")
	}
	if v := intPtr(7); v == nil || *v != 7 {
		t.Fatal("intPtr broken")
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestLoad_Missing(t *testing.T) {
	t.Parallel()
	cfg, ok, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file should not be an error: %v", err)
	}
	if ok {
		t.Fatalf("ok=true for missing file")
	}
	if !reflect.DeepEqual(cfg, Config{}) {
		t.Fatalf("expected zero config, got %#v", cfg)
	}
}

func TestLoad_Valid(t *testing.T) {
	t.Parallel()
	p := writeFile(t, `{
		"bot_name": "kfet-fir",
		"defaults": {
			"model": "anthropic/claude-sonnet-4-6",
			"thinking": "medium",
			"hide_thinking": false,
			"show_plans": false,
			"show_tools": true,
			"show_tool_details": false
		},
		"agent": {"profile": "fir"},
		"system_prompt_file": "prompt.md",
		"disable_system_prompt": true
	}`)
	cfg, ok, err := Load(p)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if cfg.BotName != "kfet-fir" {
		t.Errorf("bot_name: %q", cfg.BotName)
	}
	if cfg.Defaults.Model != "anthropic/claude-sonnet-4-6" {
		t.Errorf("model: %q", cfg.Defaults.Model)
	}
	if cfg.Defaults.Thinking != "medium" {
		t.Errorf("thinking: %q", cfg.Defaults.Thinking)
	}
	if cfg.Defaults.HideThinking == nil || *cfg.Defaults.HideThinking != false {
		t.Errorf("hide_thinking: %v", cfg.Defaults.HideThinking)
	}
	if cfg.Defaults.ShowPlans == nil || *cfg.Defaults.ShowPlans != false {
		t.Errorf("show_plans: %v", cfg.Defaults.ShowPlans)
	}
	if cfg.Defaults.ShowTools == nil || *cfg.Defaults.ShowTools != true {
		t.Errorf("show_tools: %v", cfg.Defaults.ShowTools)
	}
	if cfg.Defaults.ShowToolDetails == nil || *cfg.Defaults.ShowToolDetails != false {
		t.Errorf("show_tool_details: %v", cfg.Defaults.ShowToolDetails)
	}
	if cfg.Agent.Profile != "fir" {
		t.Errorf("profile: %q", cfg.Agent.Profile)
	}
	if cfg.SystemPromptFile != "prompt.md" {
		t.Errorf("system_prompt_file: %q", cfg.SystemPromptFile)
	}
	if !cfg.DisableSystemPrompt {
		t.Errorf("disable_system_prompt: %v", cfg.DisableSystemPrompt)
	}
}

func TestLoad_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	p := writeFile(t, `{"bot_nam": "typo"}`)
	_, _, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestLoad_RejectsBadThinking(t *testing.T) {
	t.Parallel()
	p := writeFile(t, `{"defaults":{"thinking":"bogus"}}`)
	_, _, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("expected thinking validation error, got %v", err)
	}
}

func TestLoad_EmptyJSON(t *testing.T) {
	t.Parallel()
	p := writeFile(t, `{}`)
	cfg, ok, err := Load(p)
	if err != nil || !ok {
		t.Fatalf("load: %v / ok=%v", err, ok)
	}
	if !reflect.DeepEqual(cfg, Config{}) {
		t.Fatalf("expected zero config, got %#v", cfg)
	}
}

func TestValidate_AllThinkingLevels(t *testing.T) {
	t.Parallel()
	for _, lvl := range []string{"", "off", "minimal", "low", "medium", "high", "xhigh", "max"} {
		c := Config{Defaults: Defaults{Thinking: lvl}}
		if err := c.Validate(); err != nil {
			t.Errorf("thinking=%q: %v", lvl, err)
		}
	}
}

func TestLoad_PoeMCP(t *testing.T) {
	t.Parallel()
	p := writeFile(t, `{"poe_mcp": true}`)
	cfg, ok, err := Load(p)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if !cfg.PoeMCP {
		t.Errorf("poe_mcp: %v", cfg.PoeMCP)
	}
}

// TestDefaults_Stream pins the nil-means-default resolution of the
// stream-shaping knobs. A zero Defaults MUST resolve to the
// mobile-friendly shape — 3s grid coalescing, static spinner — and an
// explicit `"coalesce_ms": 0` MUST still disable coalescing rather than
// falling back to the default.
func TestDefaults_Stream(t *testing.T) {
	t.Parallel()
	zero := Defaults{}.Stream()
	if zero.CoalesceInterval != DefaultCoalesceMs*time.Millisecond {
		t.Errorf("zero config coalesce = %s, want %dms", zero.CoalesceInterval, DefaultCoalesceMs)
	}
	if !zero.CoalesceGrid || zero.SpinnerAnimate {
		t.Errorf("zero config = %+v, want grid on + spinner static", zero)
	}

	// Explicit 0 = operator disabling coalescing; must NOT be treated as
	// "unset" and overridden by DefaultCoalesceMs.
	disabled := Defaults{CoalesceMs: intPtr(0)}.Stream()
	if disabled.CoalesceInterval != 0 {
		t.Errorf("explicit coalesce_ms=0 → %s, want disabled", disabled.CoalesceInterval)
	}

	on := Defaults{CoalesceMs: intPtr(3000)}.Stream()
	if on.CoalesceInterval != 3*time.Second || !on.CoalesceGrid {
		t.Errorf("coalesce_ms=3000 → %+v", on)
	}

	off := Defaults{
		CoalesceMs:     intPtr(1000),
		CoalesceGrid:   boolPtr(false),
		SpinnerAnimate: boolPtr(true),
	}.Stream()
	if off.CoalesceInterval != time.Second || off.CoalesceGrid || !off.SpinnerAnimate {
		t.Errorf("explicit opt-outs → %+v", off)
	}
}

func TestLoad_StreamKnobs(t *testing.T) {
	t.Parallel()
	p := writeFile(t, `{"defaults":{"coalesce_ms":3000,"coalesce_grid":false,"spinner_animate":true}}`)
	cfg, ok, err := Load(p)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	s := cfg.Defaults.Stream()
	if s.CoalesceInterval != 3*time.Second || s.CoalesceGrid || !s.SpinnerAnimate {
		t.Fatalf("resolved %+v", s)
	}
}

// TestLoad_CoalesceUnsetVsZero pins the tri-state: a config file that
// omits coalesce_ms gets the built-in default, one that spells out 0
// gets coalescing off.
func TestLoad_CoalesceUnsetVsZero(t *testing.T) {
	t.Parallel()
	unset, ok, err := Load(writeFile(t, `{"defaults":{}}`))
	if err != nil || !ok {
		t.Fatalf("load unset: ok=%v err=%v", ok, err)
	}
	if unset.Defaults.CoalesceMs != nil {
		t.Fatalf("omitted coalesce_ms parsed as %d, want nil", *unset.Defaults.CoalesceMs)
	}
	if got := unset.Defaults.Stream().CoalesceInterval; got != DefaultCoalesceMs*time.Millisecond {
		t.Fatalf("omitted coalesce_ms → %s, want default", got)
	}

	zero, ok, err := Load(writeFile(t, `{"defaults":{"coalesce_ms":0}}`))
	if err != nil || !ok {
		t.Fatalf("load zero: ok=%v err=%v", ok, err)
	}
	if zero.Defaults.CoalesceMs == nil || *zero.Defaults.CoalesceMs != 0 {
		t.Fatalf("explicit 0 parsed as %v, want pointer to 0", zero.Defaults.CoalesceMs)
	}
	if got := zero.Defaults.Stream().CoalesceInterval; got != 0 {
		t.Fatalf("explicit coalesce_ms=0 → %s, want disabled", got)
	}
}

func TestValidate_CoalesceMsBounds(t *testing.T) {
	t.Parallel()
	// Out of range: negative is nonsense, and an unbounded value starves
	// mid-turn output (or, large enough, overflows time.Duration
	// negative and silently degrades to "off" — a failure the operator
	// would never see).
	for _, ms := range []int{-1, MaxCoalesceMs + 1, 600_000} {
		err := Config{Defaults: Defaults{CoalesceMs: intPtr(ms)}}.Validate()
		if err == nil || !strings.Contains(err.Error(), "coalesce_ms") {
			t.Errorf("coalesce_ms=%d: want a coalesce_ms error, got %v", ms, err)
		}
	}
	// In range, including both endpoints — plus unset.
	for _, ms := range []*int{nil, intPtr(0), intPtr(1), intPtr(3000), intPtr(MaxCoalesceMs)} {
		c := Config{Defaults: Defaults{CoalesceMs: ms}}
		if err := c.Validate(); err != nil {
			t.Errorf("coalesce_ms=%v: %v", ms, err)
		}
	}
}

func TestLoad_PinnedModels(t *testing.T) {
	t.Parallel()
	p := writeFile(t, `{"pinned_models": ["openrouter/stealth/ox-alpha", "anthropic/claude-sonnet-4"]}`)
	cfg, ok, err := Load(p)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	want := []string{"openrouter/stealth/ox-alpha", "anthropic/claude-sonnet-4"}
	if len(cfg.PinnedModels) != len(want) {
		t.Fatalf("got %v, want %v", cfg.PinnedModels, want)
	}
	for i := range want {
		if cfg.PinnedModels[i] != want[i] {
			t.Fatalf("got %v, want %v", cfg.PinnedModels, want)
		}
	}

	// Unknown top-level keys still fail loudly.
	p = writeFile(t, `{"pinned_model": ["x"]}`)
	if _, _, err := Load(p); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestOrderPinned(t *testing.T) {
	t.Parallel()
	mk := func(ids ...string) []client.ModelInfo {
		out := make([]client.ModelInfo, len(ids))
		for i, id := range ids {
			out[i] = client.ModelInfo{ID: id, Name: id}
		}
		return out
	}
	eq := func(got, want []client.ModelInfo) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range want {
			if got[i].ID != want[i].ID {
				return false
			}
		}
		return true
	}

	base := mk("a/x", "b/y", "a/z", "c/w")

	// Pin reorders to front, preserving pin order and relative tail order.
	got := OrderPinned(base, []string{"c/w", "b/y"})
	if !eq(got, mk("c/w", "b/y", "a/x", "a/z")) {
		t.Fatalf("pin reorder: got %v", ids(got))
	}

	// Unknown pins are ignored.
	if !eq(OrderPinned(base, []string{"nope/none"}), base) {
		t.Fatalf("unknown pin changed list: %v", ids(OrderPinned(base, []string{"nope/none"})))
	}

	// Duplicate pin hoists once.
	if !eq(OrderPinned(base, []string{"b/y", "b/y"}), mk("b/y", "a/x", "a/z", "c/w")) {
		t.Fatalf("dup pin: %v", ids(OrderPinned(base, []string{"b/y", "b/y"})))
	}

	// Empty inputs are pass-through.
	if !eq(OrderPinned(nil, []string{"a/x"}), nil) || !eq(OrderPinned(base, nil), base) {
		t.Fatalf("empty-input pass-through broken")
	}
}

func ids(ms []client.ModelInfo) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}
