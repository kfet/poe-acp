package paramctl

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kfet/poe-acp-relay/internal/acpclient"
)

func TestBuild_NoModelsOmitsModelDropdown(t *testing.T) {
	t.Parallel()
	pc := Build(nil, "")
	if pc == nil || len(pc.Sections) != 1 {
		t.Fatalf("expected 1 section, got %#v", pc)
	}
	for _, c := range pc.Sections[0].Controls {
		if c.ParameterName == "model" {
			t.Fatalf("model dropdown should be omitted when no models are known")
		}
	}
	// Should still have thinking + hide_thinking.
	want := map[string]bool{"thinking": false, "hide_thinking": false}
	for _, c := range pc.Sections[0].Controls {
		if _, ok := want[c.ParameterName]; ok {
			want[c.ParameterName] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Fatalf("missing control %s", k)
		}
	}
}

func TestBuild_WithModels(t *testing.T) {
	t.Parallel()
	pc := Build([]acpclient.ModelInfo{
		{ID: "anthropic/sonnet", Name: "Sonnet"},
		{ID: "openai/gpt-5", Name: "GPT-5"},
	}, "anthropic/sonnet")

	var modelCtl *struct{ name, def string }
	for _, c := range pc.Sections[0].Controls {
		if c.ParameterName == "model" {
			d, _ := c.DefaultValue.(string)
			modelCtl = &struct{ name, def string }{c.Control, d}
			if len(c.Options) != 2 || c.Options[0].Value != "anthropic/sonnet" {
				t.Fatalf("options = %+v", c.Options)
			}
		}
	}
	if modelCtl == nil || modelCtl.name != "drop_down" || modelCtl.def != "anthropic/sonnet" {
		t.Fatalf("model control = %+v", modelCtl)
	}

	// JSON round-trip — make sure it serialises with snake_case fields.
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
		`"options":[`,
		`"api_version":"2"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("json missing %q\nfull: %s", want, s)
		}
	}
}

// TestBuildAndDefaultsAgree pins the schema's `default_value`s to the
// runtime Defaults() so the relay applies what the UI promises. If
// either side changes without the other, this fails.
func TestBuildAndDefaultsAgree(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		models  []acpclient.ModelInfo
		current string
	}{
		{
			"with models",
			[]acpclient.ModelInfo{{ID: "anthropic/sonnet", Name: "Sonnet"}},
			"anthropic/sonnet",
		},
		{
			"probe failed",
			nil,
			"",
		},
		{
			"models present, no current → first model",
			[]acpclient.ModelInfo{{ID: "anthropic/sonnet", Name: "Sonnet"}},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := Build(tc.models, tc.current)
			d := Defaults(tc.models, tc.current)
			// Walk the schema and pull each default_value out.
			schemaDefaults := map[string]any{}
			for _, sec := range pc.Sections {
				for _, c := range sec.Controls {
					schemaDefaults[c.ParameterName] = c.DefaultValue
				}
			}
			// thinking
			if got, want := schemaDefaults["thinking"], d.Thinking; got != want {
				t.Errorf("thinking: schema=%v defaults=%v", got, want)
			}
			// hide_thinking
			if got, want := schemaDefaults["hide_thinking"], d.HideThinking; got != want {
				t.Errorf("hide_thinking: schema=%v defaults=%v", got, want)
			}
			// model — present iff models non-empty
			if mv, ok := schemaDefaults["model"]; ok {
				if d.Model == "" {
					t.Errorf("schema has model=%v but Defaults().Model is empty", mv)
				}
				if mv != d.Model {
					t.Errorf("model: schema=%v defaults=%v", mv, d.Model)
				}
			} else {
				if d.Model != "" {
					t.Errorf("schema omits model dropdown but Defaults().Model=%q", d.Model)
				}
			}
		})
	}
}
