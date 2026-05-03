package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

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
			"hide_thinking": false
		},
		"agent": {"profile": "fir"}
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
	if cfg.Agent.Profile != "fir" {
		t.Errorf("profile: %q", cfg.Agent.Profile)
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
	for _, lvl := range []string{"", "off", "minimal", "low", "medium", "high"} {
		c := Config{Defaults: Defaults{Thinking: lvl}}
		if err := c.Validate(); err != nil {
			t.Errorf("thinking=%q: %v", lvl, err)
		}
	}
}
