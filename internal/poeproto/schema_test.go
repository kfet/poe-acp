package poeproto_test

import (
	"bytes"
	"embed"
	"encoding/json"
	"io/fs"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/kfet/poe-acp-relay/internal/acpclient"
	"github.com/kfet/poe-acp-relay/internal/paramctl"
	"github.com/kfet/poe-acp-relay/internal/poeproto"
)

//go:embed testdata/*.schema.json
var schemaFS embed.FS

// loadSchema compiles a vendored Pydantic-derived JSON Schema. Schemas
// are regenerated via scripts/regen-poe-schema.sh; never hand-edit.
func loadSchema(t *testing.T, file string) *jsonschema.Schema {
	t.Helper()
	data, err := fs.ReadFile(schemaFS, "testdata/"+file)
	if err != nil {
		t.Fatalf("read embedded schema %s: %v", file, err)
	}
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	url := "mem:///" + file
	if err := c.AddResource(url, bytes.NewReader(data)); err != nil {
		t.Fatalf("add schema %s: %v", file, err)
	}
	s, err := c.Compile(url)
	if err != nil {
		t.Fatalf("compile %s: %v", file, err)
	}
	return s
}

// validate marshals v to JSON, re-parses it as map[string]any (the form
// jsonschema validates), and checks against schema. Returns a friendly
// error on failure.
func validate(t *testing.T, schema *jsonschema.Schema, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := schema.Validate(doc); err != nil {
		t.Fatalf("schema validation failed:\n%v\n--- emitted JSON ---\n%s", err, indent(b))
	}
}

func indent(b []byte) string {
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return string(b)
	}
	return out.String()
}

// TestParameterControls_MatchesPoeSchema validates several realistic
// shapes (no models / with models / many models) against the upstream
// fastapi_poe ParameterControls JSON Schema. Catches drift from the
// expected snake_case wire format and forbidden extra fields.
func TestParameterControls_MatchesPoeSchema(t *testing.T) {
	t.Parallel()
	schema := loadSchema(t, "parameter_controls.schema.json")

	cases := []struct {
		name   string
		models []acpclient.ModelInfo
		cur    string
	}{
		{name: "no_models"},
		{
			name: "two_models",
			models: []acpclient.ModelInfo{
				{ID: "anthropic/sonnet", Name: "Sonnet"},
				{ID: "openai/gpt-5", Name: "GPT-5"},
			},
			cur: "anthropic/sonnet",
		},
		{
			name:   "model_with_unset_default",
			models: []acpclient.ModelInfo{{ID: "x/y", Name: "XY"}},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pc := paramctl.Build(tc.models, tc.cur)
			validate(t, schema, pc)
		})
	}
}

// TestSettingsResponse_MatchesPoeSchema validates the full settings
// response (the actual JSON Poe receives) against the upstream
// SettingsResponse schema.
func TestSettingsResponse_MatchesPoeSchema(t *testing.T) {
	t.Parallel()
	schema := loadSchema(t, "settings_response.schema.json")

	pc := paramctl.Build([]acpclient.ModelInfo{
		{ID: "anthropic/sonnet", Name: "Sonnet"},
	}, "anthropic/sonnet")

	resp := poeproto.SettingsResponse{
		AllowAttachments:    false,
		IntroductionMessage: "hi",
		ParameterControls:   pc,
	}
	validate(t, schema, resp)
}

// TestParameterControls_RejectsLegacyDropdownSpelling is a guard test:
// the previous "dropdown" wire value (rejected by Poe) must fail
// validation, otherwise the schema isn't actually catching the bug.
func TestParameterControls_RejectsLegacyDropdownSpelling(t *testing.T) {
	t.Parallel()
	schema := loadSchema(t, "parameter_controls.schema.json")

	bad := map[string]any{
		"api_version": "2",
		"sections": []any{map[string]any{
			"controls": []any{map[string]any{
				"control":        "dropdown", // <- the bug
				"label":          "Model",
				"parameter_name": "model",
				"options":        []any{map[string]any{"value": "a", "name": "A"}},
			}},
		}},
	}
	b, _ := json.Marshal(bad)
	var doc any
	_ = json.Unmarshal(b, &doc)
	if err := schema.Validate(doc); err == nil {
		t.Fatal("schema accepted legacy 'dropdown' control name; should have rejected")
	}
}

// Note: the upstream schema marks api_version with a default but does
// not list it as required (Pydantic literal-with-default behaviour),
// so a missing-api_version object would pass schema validation. The
// runtime guarantee is enforced by paramctl_test.go's JSON assertion
// that every Build() emits "api_version":"2".
