package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func boolPtr(b bool) *bool { return &b }

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
	if cfg != (Config{}) {
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
	if cfg != (Config{}) {
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
// stream-shaping knobs. A zero Defaults MUST resolve to today's
// behaviour — no coalescing, animated spinner — so an existing config
// file changes nothing.
func TestDefaults_Stream(t *testing.T) {
	t.Parallel()
	zero := Defaults{}.Stream()
	if zero.CoalesceInterval != 0 {
		t.Errorf("zero config enabled coalescing: %s", zero.CoalesceInterval)
	}
	if !zero.CoalesceGrid || !zero.SpinnerAnimate {
		t.Errorf("zero config = %+v, want grid+animate defaults", zero)
	}

	on := Defaults{CoalesceMs: 3000}.Stream()
	if on.CoalesceInterval != 3*time.Second || !on.CoalesceGrid {
		t.Errorf("coalesce_ms=3000 → %+v", on)
	}

	off := Defaults{
		CoalesceMs:     1000,
		CoalesceGrid:   boolPtr(false),
		SpinnerAnimate: boolPtr(false),
	}.Stream()
	if off.CoalesceInterval != time.Second || off.CoalesceGrid || off.SpinnerAnimate {
		t.Errorf("explicit opt-outs → %+v", off)
	}
}

func TestLoad_StreamKnobs(t *testing.T) {
	t.Parallel()
	p := writeFile(t, `{"defaults":{"coalesce_ms":3000,"coalesce_grid":false,"spinner_animate":false}}`)
	cfg, ok, err := Load(p)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	s := cfg.Defaults.Stream()
	if s.CoalesceInterval != 3*time.Second || s.CoalesceGrid || s.SpinnerAnimate {
		t.Fatalf("resolved %+v", s)
	}
}

func TestValidate_CoalesceMsBounds(t *testing.T) {
	t.Parallel()
	// Out of range: negative is nonsense, and an unbounded value starves
	// mid-turn output (or, large enough, overflows time.Duration
	// negative and silently degrades to "off" — a failure the operator
	// would never see).
	for _, ms := range []int{-1, MaxCoalesceMs + 1, 600_000} {
		err := Config{Defaults: Defaults{CoalesceMs: ms}}.Validate()
		if err == nil || !strings.Contains(err.Error(), "coalesce_ms") {
			t.Errorf("coalesce_ms=%d: want a coalesce_ms error, got %v", ms, err)
		}
	}
	// In range, including both endpoints.
	for _, ms := range []int{0, 1, 3000, MaxCoalesceMs} {
		c := Config{Defaults: Defaults{CoalesceMs: ms}}
		if err := c.Validate(); err != nil {
			t.Errorf("coalesce_ms=%d: %v", ms, err)
		}
	}
}
