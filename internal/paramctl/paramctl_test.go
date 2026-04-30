package paramctl

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kfet/poe-acp-relay/internal/acpclient"
	"github.com/kfet/poe-acp-relay/internal/config"
	"github.com/kfet/poe-acp-relay/internal/router"
)

var twoModels = []acpclient.ModelInfo{
	{ID: "anthropic/sonnet", Name: "Sonnet"},
	{ID: "openai/gpt-5", Name: "GPT-5"},
}

func TestResolve_ConfigWins(t *testing.T) {
	t.Parallel()
	d := Resolve(config.Defaults{Model: "openai/gpt-5", Thinking: "high", HideThinking: true},
		twoModels, "anthropic/sonnet")
	if d.Model != "openai/gpt-5" || d.Thinking != "high" || !d.HideThinking {
		t.Fatalf("got %+v", d)
	}
}

func TestResolve_ConfigModelMissing_FallsThrough(t *testing.T) {
	t.Parallel()
	// Configured model not in list → do not pick a phantom default.
	d := Resolve(config.Defaults{Model: "ghost/never"}, twoModels, "anthropic/sonnet")
	if d.Model != "" {
		t.Fatalf("expected empty Model on missing config value, got %q", d.Model)
	}
	if d.Thinking != DefaultThinking {
		t.Fatalf("thinking fallback: got %q", d.Thinking)
	}
}

func TestResolve_ProbeFallback(t *testing.T) {
	t.Parallel()
	d := Resolve(config.Defaults{}, twoModels, "anthropic/sonnet")
	if d.Model != "anthropic/sonnet" {
		t.Fatalf("probe fallback: got %q", d.Model)
	}
}

func TestResolve_NoModelsNoDefault(t *testing.T) {
	t.Parallel()
	d := Resolve(config.Defaults{Model: "anthropic/sonnet"}, nil, "")
	if d.Model != "" {
		t.Fatalf("no models → empty Model, got %q", d.Model)
	}
	if d.Thinking != DefaultThinking {
		t.Fatalf("thinking fallback: got %q", d.Thinking)
	}
}

func TestBuild_NoModelsOmitsModelDropdown(t *testing.T) {
	t.Parallel()
	pc := Build(nil, router.Options{Thinking: DefaultThinking})
	for _, c := range pc.Sections[0].Controls {
		if c.ParameterName == "model" {
			t.Fatalf("model dropdown should be omitted when no models are known")
		}
	}
}

func TestBuild_WithModels(t *testing.T) {
	t.Parallel()
	defs := router.Options{Model: "anthropic/sonnet", Thinking: "medium"}
	pc := Build(twoModels, defs)

	var seen bool
	for _, c := range pc.Sections[0].Controls {
		if c.ParameterName == "model" {
			seen = true
			if d, _ := c.DefaultValue.(string); d != "anthropic/sonnet" {
				t.Fatalf("default = %v", c.DefaultValue)
			}
			if len(c.Options) != 2 {
				t.Fatalf("options = %+v", c.Options)
			}
		}
	}
	if !seen {
		t.Fatalf("model dropdown missing")
	}

	b, err := json.Marshal(pc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		`"parameter_name":"model"`,
		`"default_value":"anthropic/sonnet"`,
		`"control":"drop_down"`,
		`"control":"toggle_switch"`,
		`"api_version":"2"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("json missing %q\nfull: %s", want, s)
		}
	}
}

// TestBuildAndResolveAgree pins the schema's `default_value`s to the
// resolved Defaults so the UI promise matches what ParseOptions overlays.
func TestBuildAndResolveAgree(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cfg     config.Defaults
		models  []acpclient.ModelInfo
		current string
	}{
		{"config wins",
			config.Defaults{Model: "openai/gpt-5", Thinking: "high"}, twoModels, "anthropic/sonnet"},
		{"probe fallback",
			config.Defaults{}, twoModels, "anthropic/sonnet"},
		{"no models",
			config.Defaults{Model: "anthropic/sonnet"}, nil, ""},
		{"config model missing → no default",
			config.Defaults{Model: "ghost/never"}, twoModels, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Resolve(tc.cfg, tc.models, tc.current)
			pc := Build(tc.models, d)
			schemaDefaults := map[string]any{}
			for _, sec := range pc.Sections {
				for _, c := range sec.Controls {
					if c.DefaultValue != nil {
						schemaDefaults[c.ParameterName] = c.DefaultValue
					}
				}
			}
			if got, want := schemaDefaults["thinking"], d.Thinking; got != want {
				t.Errorf("thinking: schema=%v defaults=%v", got, want)
			}
			if got, want := schemaDefaults["hide_thinking"], d.HideThinking; got != want {
				t.Errorf("hide_thinking: schema=%v defaults=%v", got, want)
			}
			if mv, ok := schemaDefaults["model"]; ok {
				if mv != d.Model {
					t.Errorf("model: schema=%v defaults=%v", mv, d.Model)
				}
			} else if d.Model != "" {
				t.Errorf("schema omits model default but Defaults().Model=%q", d.Model)
			}
		})
	}
}
