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
	if modelCtl == nil || modelCtl.name != "dropdown" || modelCtl.def != "anthropic/sonnet" {
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
		`"control":"dropdown"`,
		`"control":"toggle_switch"`,
		`"options":[`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("json missing %q\nfull: %s", want, s)
		}
	}
}
