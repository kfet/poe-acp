package paramctl

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kfet/poe-acp/internal/acpclient"
	"github.com/kfet/poe-acp/internal/config"
	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/router"
)

func boolPtr(b bool) *bool { return &b }

var twoModels = []acpclient.ModelInfo{
	{ID: "anthropic/sonnet", Name: "Sonnet"},
	{ID: "openai/gpt-5", Name: "GPT-5"},
}

func TestResolve_ConfigWins(t *testing.T) {
	t.Parallel()
	d := Resolve(config.Defaults{Model: "openai/gpt-5", Thinking: "high", HideThinking: boolPtr(true)},
		twoModels, "anthropic/sonnet")
	if d.Model != "openai/gpt-5" || d.Thinking != "high" || !d.HideThinking {
		t.Fatalf("got %+v", d)
	}
}

func TestResolve_HideThinkingDefault(t *testing.T) {
	t.Parallel()
	// nil (unset) → built-in default = true
	d := Resolve(config.Defaults{}, twoModels, "")
	if !d.HideThinking {
		t.Fatalf("nil HideThinking should default to true, got %v", d.HideThinking)
	}
}

func TestResolve_HideThinkingExplicitFalse(t *testing.T) {
	t.Parallel()
	// operator explicitly sets false → must override the default
	d := Resolve(config.Defaults{HideThinking: boolPtr(false)}, twoModels, "")
	if d.HideThinking {
		t.Fatalf("explicit HideThinking=false should override default, got %v", d.HideThinking)
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

func TestBuild_NoModelsOmitsProviderAndModelDropdowns(t *testing.T) {
	t.Parallel()
	pc := Build(nil, router.Options{Thinking: DefaultThinking})
	for _, c := range pc.Sections[0].Controls {
		switch c.ParameterName {
		case "model", "provider":
			t.Fatalf("model/provider control %q should be omitted when no models are known", c.ParameterName)
		}
		if c.Control == "condition" {
			t.Fatalf("condition control should be omitted when no models are known")
		}
	}
}

func TestBuild_WithModels_CascadingProviderAndModelDropdowns(t *testing.T) {
	t.Parallel()
	defs := router.Options{Model: "anthropic/sonnet", Thinking: "medium"}
	pc := Build(twoModels, defs)

	ctls := pc.Sections[0].Controls
	// Provider dropdown is first.
	prov := ctls[0]
	if prov.ParameterName != "provider" || prov.Control != "drop_down" {
		t.Fatalf("first control should be provider drop_down, got %+v", prov)
	}
	if d, _ := prov.DefaultValue.(string); d != "anthropic" {
		t.Fatalf("provider default = %v want anthropic", prov.DefaultValue)
	}
	gotProvs := []string{}
	for _, o := range prov.Options {
		gotProvs = append(gotProvs, o.Value)
	}
	if !reflect.DeepEqual(gotProvs, []string{"anthropic", "openai"}) {
		t.Fatalf("provider options = %v want [anthropic openai]", gotProvs)
	}

	// Followed by one condition control per provider, each containing
	// a single Model drop_down.
	type pair struct {
		paramName, comparator, providerLit, defaultModel, firstOption string
	}
	wantConds := []pair{
		{"model_anthropic", "eq", "anthropic", "anthropic/sonnet", "anthropic/sonnet"},
		{"model_openai", "eq", "openai", "openai/gpt-5", "openai/gpt-5"},
	}
	for i, want := range wantConds {
		c := ctls[1+i]
		if c.Control != "condition" {
			t.Fatalf("ctls[%d] control = %q want condition", 1+i, c.Control)
		}
		if c.Condition == nil || c.Condition.Comparator != want.comparator {
			t.Fatalf("ctls[%d] condition = %+v", 1+i, c.Condition)
		}
		if c.Condition.Left.ParameterName != "provider" {
			t.Fatalf("ctls[%d] condition.left = %+v", 1+i, c.Condition.Left)
		}
		if v, _ := c.Condition.Right.Literal.(string); v != want.providerLit {
			t.Fatalf("ctls[%d] condition.right literal = %v want %s", 1+i, c.Condition.Right.Literal, want.providerLit)
		}
		if len(c.Controls) != 1 || c.Controls[0].ParameterName != want.paramName {
			t.Fatalf("ctls[%d] inner = %+v", 1+i, c.Controls)
		}
		inner := c.Controls[0]
		if inner.Control != "drop_down" || inner.Label != "Model" {
			t.Fatalf("inner control = %+v", inner)
		}
		if d, _ := inner.DefaultValue.(string); d != want.defaultModel {
			t.Fatalf("inner default = %v want %s", inner.DefaultValue, want.defaultModel)
		}
		if inner.Options[0].Value != want.firstOption {
			t.Fatalf("inner first option = %v want %s", inner.Options[0], want.firstOption)
		}
	}

	b, err := json.Marshal(pc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		`"parameter_name":"provider"`,
		`"parameter_name":"model_anthropic"`,
		`"parameter_name":"model_openai"`,
		`"control":"condition"`,
		`"comparator":"eq"`,
		`"literal":"anthropic"`,
		`"default_value":"anthropic/sonnet"`,
		`"control":"drop_down"`,
		`"control":"toggle_switch"`,
		`"api_version":"2"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("json missing %q\nfull: %s", want, s)
		}
	}
	// The legacy bare-`model` parameter_name must NOT appear in the new
	// schema — `model_<provider>` is the per-provider replacement.
	if strings.Contains(s, `"parameter_name":"model"`) {
		t.Errorf("legacy bare model parameter_name should not appear in cascading schema; full:\n%s", s)
	}
}

// TestBuild_ProviderGrouping pins the bucketing rules: first-seen
// provider order, intra-provider order preserved, slash-less ids fall
// into the "other" bucket.
func TestBuild_ProviderGrouping(t *testing.T) {
	t.Parallel()
	models := []acpclient.ModelInfo{
		{ID: "openai/gpt-5", Name: "GPT-5"},
		{ID: "anthropic/sonnet", Name: "Sonnet"},
		{ID: "openai/gpt-4o", Name: "GPT-4o"},
		{ID: "kimi-k2", Name: "Kimi K2"}, // no slash → other
		{ID: "anthropic/opus", Name: "Opus"},
		{ID: "/leading-slash", Name: "Leading slash"}, // empty provider → other
	}
	provs := Providers(models)
	want := []string{"openai", "anthropic", "other"}
	if !reflect.DeepEqual(provs, want) {
		t.Fatalf("providers = %v want %v", provs, want)
	}

	pc := Build(models, router.Options{Thinking: DefaultThinking})
	ctls := pc.Sections[0].Controls
	// Inner model options should appear in the order they were given.
	var openaiInner, otherInner []string
	for _, c := range ctls {
		if c.Control != "condition" || c.Condition == nil {
			continue
		}
		lit, _ := c.Condition.Right.Literal.(string)
		inner := c.Controls[0]
		var ids []string
		for _, o := range inner.Options {
			ids = append(ids, o.Value)
		}
		switch lit {
		case "openai":
			openaiInner = ids
		case "other":
			otherInner = ids
		}
	}
	if !reflect.DeepEqual(openaiInner, []string{"openai/gpt-5", "openai/gpt-4o"}) {
		t.Fatalf("openai models = %v", openaiInner)
	}
	if !reflect.DeepEqual(otherInner, []string{"kimi-k2", "/leading-slash"}) {
		t.Fatalf("other models = %v", otherInner)
	}
}

// TestBuild_ProviderParamSanitisation ensures awkward provider ids
// (case, punctuation) map to safe parameter_names while still
// referencing the unsanitised provider id in the condition literal.
func TestBuild_ProviderParamSanitisation(t *testing.T) {
	t.Parallel()
	models := []acpclient.ModelInfo{
		{ID: "Foo-Bar.Baz/model-x", Name: "X"},
	}
	pc := Build(models, router.Options{Thinking: DefaultThinking})
	var cond *poeproto.Control
	for i := range pc.Sections[0].Controls {
		if pc.Sections[0].Controls[i].Control == "condition" {
			cond = &pc.Sections[0].Controls[i]
			break
		}
	}
	if cond == nil {
		t.Fatal("no condition control emitted")
	}
	lit, _ := cond.Condition.Right.Literal.(string)
	if lit != "Foo-Bar.Baz" {
		t.Fatalf("condition literal = %q want Foo-Bar.Baz", lit)
	}
	if got := cond.Controls[0].ParameterName; got != "model_foo_bar_baz" {
		t.Fatalf("inner parameter_name = %q want model_foo_bar_baz", got)
	}
}

// TestProviderParamName_EmptyBucketsToOther guards the empty-string
// short-circuit so the synthesised parameter_name stays non-degenerate
// ("model_other", matching the value-side bucket label).
func TestProviderParamName_EmptyBucketsToOther(t *testing.T) {
	t.Parallel()
	if got, want := ProviderParamName(""), "model_other"; got != want {
		t.Fatalf("ProviderParamName(\"\") = %q want %q", got, want)
	}
}

// TestBuild_DefaultModelProviderNotInList covers the defensive
// hasProvider guard: a caller passing defaults.Model whose provider is
// absent from the model list (would only happen on a bypass of
// Resolve) must still produce a valid schema — no provider default,
// per-provider defaults driven by the first model in each group.
func TestBuild_DefaultModelProviderNotInList(t *testing.T) {
	t.Parallel()
	models := []acpclient.ModelInfo{{ID: "openai/gpt-5", Name: "GPT-5"}}
	defs := router.Options{Model: "anthropic/sonnet", Thinking: DefaultThinking}
	pc := Build(models, defs)
	prov := pc.Sections[0].Controls[0]
	if prov.ParameterName != "provider" {
		t.Fatalf("first ctl = %+v", prov)
	}
	if prov.DefaultValue != nil {
		t.Fatalf("provider default should be nil when model's provider is absent, got %v", prov.DefaultValue)
	}
	// The openai bucket should still have its first-model default.
	cond := pc.Sections[0].Controls[1]
	if d, _ := cond.Controls[0].DefaultValue.(string); d != "openai/gpt-5" {
		t.Fatalf("openai default = %v want openai/gpt-5", cond.Controls[0].DefaultValue)
	}
}

// TestBuildAndResolveAgree pins the schema's `default_value`s to the
// resolved Defaults so the UI promise matches what ParseOptions overlays.
//
// In the cascading shape, the effective Model default lives on
// `model_<provider-of-defaults.Model>`; the bare `model` parameter is
// no longer emitted. The Provider dropdown's default_value mirrors
// ProviderOf(defaults.Model).
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
			collect(pc.Sections, schemaDefaults)
			if got, want := schemaDefaults["thinking"], d.Thinking; got != want {
				t.Errorf("thinking: schema=%v defaults=%v", got, want)
			}
			if got, want := schemaDefaults["hide_thinking"], d.HideThinking; got != want {
				t.Errorf("hide_thinking: schema=%v defaults=%v", got, want)
			}
			// In the new cascading shape, defaults.Model is carried on
			// `model_<provider>` for the matching provider, and the
			// Provider dropdown's default mirrors that provider.
			if d.Model == "" {
				if _, ok := schemaDefaults["provider"]; ok {
					t.Errorf("provider default set when defaults.Model is empty: %v", schemaDefaults["provider"])
				}
				return
			}
			prov := ProviderOf(d.Model)
			if got := schemaDefaults["provider"]; got != prov {
				t.Errorf("provider: schema=%v want %q", got, prov)
			}
			paramName := ProviderParamName(prov)
			if got := schemaDefaults[paramName]; got != d.Model {
				t.Errorf("%s: schema=%v defaults.Model=%v", paramName, got, d.Model)
			}
		})
	}
}

// collect flattens every control's (parameter_name, default_value)
// across nested condition controls.
func collect(secs []poeproto.Section, into map[string]any) {
	var walk func(ctls []poeproto.Control)
	walk = func(ctls []poeproto.Control) {
		for _, c := range ctls {
			if c.DefaultValue != nil && c.ParameterName != "" {
				into[c.ParameterName] = c.DefaultValue
			}
			if len(c.Controls) > 0 {
				walk(c.Controls)
			}
		}
	}
	for _, s := range secs {
		walk(s.Controls)
	}
}
