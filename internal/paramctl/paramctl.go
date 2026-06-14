// Package paramctl builds the Poe parameter_controls schema from the
// agent's available models and the operator's resolved defaults.
//
// The schema's `default_value`s and the runtime Defaults() must agree
// — one is what the UI shows, the other is what the relay applies on
// the first turn of every conversation. They are produced from a
// single Resolve() call so they cannot drift.
//
// Model selection adapts to how many providers the agent exposes:
//
//	Single provider — one flat `Model` drop_down with parameter_name
//	                  `model` (the legacy/back-compat shape). The
//	                  Provider dropdown is omitted entirely because a
//	                  one-option picker is pure noise; bots wired to a
//	                  single provider (e.g. a Sakana- or Anthropic-only
//	                  relay) get the minimum-surface UI they want.
//	Multiple providers — cascading dropdowns:
//	  Provider          — top-level drop_down, options derived from the
//	                      prefix-before-first-slash of each model ID
//	                      (e.g. "anthropic", "openai"). Models with no
//	                      slash are grouped under "other".
//	  Model (per-prov)  — one drop_down per provider, gated by a
//	                      `condition` control on `provider == <P>`.
//	                      parameter_name is `model_<sanitised provider>`
//	                      so each provider carries its own remembered
//	                      selection independently.
//
// The relay reconciles the user-visible state back to a single agent
// model id in router.ParseOptions: bare `model` (collapsed shape and
// legacy/back-compat) wins; otherwise `model_<provider>` (looked up by
// the chosen `provider` value) is used. Empty parameters fall back to
// the resolved Defaults().
package paramctl

import (
	"log"
	"strings"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/poe-acp/internal/config"
	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/router"
)

// ThinkingLevels is the v1 thinking dropdown options. Wire values match
// fir's `ai.ThinkingLevel` constants. The full set is offered for every
// model: levels the current model doesn't support are soft-failed by
// the router (see applyOptions) so the dropdown stays consistent across
// model switches without nagging the user.
var ThinkingLevels = []poeproto.ValueNamePair{
	{Value: "off", Name: "Off"},
	{Value: "minimal", Name: "Minimal"},
	{Value: "low", Name: "Low"},
	{Value: "medium", Name: "Medium"},
	{Value: "high", Name: "High"},
	{Value: "xhigh", Name: "Extra-high"},
	{Value: "max", Name: "Max"},
}

// DefaultThinking is the built-in fallback when the operator has not
// configured `defaults.thinking` in config.json.
const DefaultThinking = "medium"

// DefaultHideThinking is the built-in fallback when the operator has not
// configured `defaults.hide_thinking` in config.json.
const DefaultHideThinking = true

// OtherProvider is the bucket label for models whose ID has no '/'
// prefix. Kept stable so config defaults and tests can target it.
const OtherProvider = "other"

// Resolve picks the operator-facing defaults.
//
// Resolution order per field:
//  1. config.json `defaults.<field>` if non-empty
//  2. probed agent state (only for Model: probeCurrent if it appears
//     in the available list)
//  3. built-in fallback (thinking="medium"; model="" → no default,
//     UI shows the first option, relay does not call set_model)
//
// The Model resolution validates against the probed list and logs a
// warning on miss; the configured value is dropped on miss so the
// relay does not call set_model with a phantom value.
func Resolve(cfg config.Defaults, models []client.ModelInfo, probeCurrent string) router.Options {
	o := router.Options{
		Thinking:     DefaultThinking,
		HideThinking: DefaultHideThinking,
	}
	if cfg.Thinking != "" {
		o.Thinking = cfg.Thinking
	}
	if cfg.HideThinking != nil {
		o.HideThinking = *cfg.HideThinking
	}

	// Model resolution: only meaningful if we have an available list.
	if len(models) == 0 {
		return o
	}
	switch {
	case cfg.Model != "":
		if hasModel(models, cfg.Model) {
			o.Model = cfg.Model
		} else {
			log.Printf("paramctl: configured defaults.model %q is not in the agent's available list (%d models); omitting default. Update config.json or fir auth.", cfg.Model, len(models))
		}
	case probeCurrent != "" && hasModel(models, probeCurrent):
		o.Model = probeCurrent
	}
	return o
}

func hasModel(models []client.ModelInfo, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// ProviderOf returns the prefix-before-first-slash of a model id. An
// id with no '/' (or an empty id) is bucketed under OtherProvider.
func ProviderOf(modelID string) string {
	i := strings.IndexByte(modelID, '/')
	if i <= 0 {
		return OtherProvider
	}
	return modelID[:i]
}

// ProviderParamName is the parameter_name used for the per-provider
// Model dropdown. The provider id is sanitised so the result is a
// stable identifier (letters/digits/underscore only); the unsanitised
// provider id is still what's compared against in the `condition`
// block.
func ProviderParamName(provider string) string {
	return poeproto.ProviderParamPrefix + sanitiseProvider(provider)
}

// sanitiseProvider folds the provider id into [a-z0-9_], lowercased.
// Any other byte becomes '_'. Used only to derive parameter_name; the
// human-facing provider value remains the original string.
func sanitiseProvider(p string) string {
	if p == "" {
		return OtherProvider
	}
	b := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
			b = append(b, c)
		case c >= 'A' && c <= 'Z':
			b = append(b, c+('a'-'A'))
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}

// providerGroup is a single provider's bucket of models. Order within
// each group is the order models appeared in the agent's list.
type providerGroup struct {
	id     string // raw provider id (option `value`)
	models []client.ModelInfo
}

// groupByProvider buckets models by ProviderOf(id), preserving both
// the first-seen provider order and the order of models within each
// provider.
func groupByProvider(models []client.ModelInfo) []providerGroup {
	idx := make(map[string]int, 8)
	var out []providerGroup
	for _, m := range models {
		p := ProviderOf(m.ID)
		if i, ok := idx[p]; ok {
			out[i].models = append(out[i].models, m)
			continue
		}
		idx[p] = len(out)
		out = append(out, providerGroup{id: p, models: []client.ModelInfo{m}})
	}
	return out
}

// Providers returns the first-seen-ordered provider ids derived from
// the given model list. Exported for tests and HTTP introspection.
func Providers(models []client.ModelInfo) []string {
	groups := groupByProvider(models)
	out := make([]string, len(groups))
	for i, g := range groups {
		out[i] = g.id
	}
	return out
}

// Build assembles the parameter_controls schema. Callers MUST pass the
// same `defaults` they configured on the router (via Resolve), so the
// UI's `default_value`s match what the relay applies at runtime.
//
// If models is empty (probe failed or agent is unauthed) the
// provider+model dropdowns are omitted entirely; only Thinking and
// Hide thinking remain.
//
// If the model list resolves to exactly one provider, the schema
// collapses to a single bare `model` drop_down (no Provider picker, no
// `condition` wrapper). The router accepts that legacy shape via
// ParseOptions's bare-`model` path, so no router change is needed.
//
// Model order is preserved as received from the agent: the agent owns
// priority semantics (capability score, cost tier) that the relay
// can't see, so re-sorting here would clobber a meaningful order.
// Providers are listed in first-seen order for the same reason.
func Build(models []client.ModelInfo, defaults router.Options) *poeproto.ParameterControls {
	var controls []poeproto.Control

	switch groups := groupByProvider(models); {
	case len(groups) == 0:
		// No models known — skip provider/model controls entirely.
	case len(groups) == 1:
		// Single provider — collapse to a bare `model` drop_down. Use
		// defaults.Model only if it actually lives in this group; the
		// hasModel check guards a misconfigured default whose provider
		// happens to be the one we have but whose id isn't in the
		// returned model list.
		g := groups[0]
		modelOpts := make([]poeproto.ValueNamePair, 0, len(g.models))
		for _, m := range g.models {
			modelOpts = append(modelOpts, poeproto.ValueNamePair{Value: m.ID, Name: m.Name})
		}
		modelCtl := poeproto.Control{
			Control:       "drop_down",
			Label:         "Model",
			ParameterName: poeproto.ParamModel,
			Options:       modelOpts,
		}
		switch {
		case defaults.Model != "" && hasModel(g.models, defaults.Model):
			modelCtl.DefaultValue = defaults.Model
		case len(g.models) > 0:
			modelCtl.DefaultValue = g.models[0].ID
		}
		controls = append(controls, modelCtl)
	default:
		defaultProvider := ""
		if defaults.Model != "" {
			defaultProvider = ProviderOf(defaults.Model)
		}

		// Provider dropdown.
		provOpts := make([]poeproto.ValueNamePair, 0, len(groups))
		for _, g := range groups {
			provOpts = append(provOpts, poeproto.ValueNamePair{Value: g.id, Name: g.id})
		}
		provCtl := poeproto.Control{
			Control:       "drop_down",
			Label:         "Provider",
			ParameterName: poeproto.ParamProvider,
			Options:       provOpts,
		}
		if defaultProvider != "" && hasProvider(groups, defaultProvider) {
			provCtl.DefaultValue = defaultProvider
		}
		controls = append(controls, provCtl)

		// One conditional Model dropdown per provider.
		for _, g := range groups {
			modelOpts := make([]poeproto.ValueNamePair, 0, len(g.models))
			for _, m := range g.models {
				modelOpts = append(modelOpts, poeproto.ValueNamePair{Value: m.ID, Name: m.Name})
			}
			modelCtl := poeproto.Control{
				Control:       "drop_down",
				Label:         "Model",
				ParameterName: ProviderParamName(g.id),
				Options:       modelOpts,
			}
			// Per-provider default: defaults.Model wins if it lives in
			// this provider; otherwise default to the first model so
			// the UI never lands on an empty selection when the user
			// switches providers.
			switch {
			case g.id == defaultProvider && defaults.Model != "":
				modelCtl.DefaultValue = defaults.Model
			case len(g.models) > 0:
				modelCtl.DefaultValue = g.models[0].ID
			}
			controls = append(controls, poeproto.Control{
				Control: "condition",
				Condition: &poeproto.Condition{
					Comparator: "eq",
					Left:       poeproto.ParamOperand(poeproto.ParamProvider),
					Right:      poeproto.LiteralOperand(g.id),
				},
				Controls: []poeproto.Control{modelCtl},
			})
		}
	}

	controls = append(controls,
		poeproto.Control{
			Control:       "drop_down",
			Label:         "Thinking",
			ParameterName: poeproto.ParamThinking,
			DefaultValue:  defaults.Thinking,
			Options:       append([]poeproto.ValueNamePair(nil), ThinkingLevels...),
		},
		poeproto.Control{
			Control:       "toggle_switch",
			Label:         "Hide thinking output",
			ParameterName: poeproto.ParamHideThinking,
			DefaultValue:  defaults.HideThinking,
		},
	)

	return &poeproto.ParameterControls{
		APIVersion: poeproto.ParameterControlsAPIVersion,
		Sections: []poeproto.Section{{
			Name:     "Options",
			Controls: controls,
		}},
	}
}

func hasProvider(groups []providerGroup, id string) bool {
	for _, g := range groups {
		if g.id == id {
			return true
		}
	}
	return false
}
