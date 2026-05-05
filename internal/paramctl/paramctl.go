// Package paramctl builds the Poe parameter_controls schema from the
// agent's available models and the operator's resolved defaults.
//
// The schema's `default_value`s and the runtime Defaults() must agree
// — one is what the UI shows, the other is what the relay applies on
// the first turn of every conversation. They are produced from a
// single Resolve() call so they cannot drift.
package paramctl

import (
	"log"

	"github.com/kfet/poe-acp/internal/acpclient"
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
func Resolve(cfg config.Defaults, models []acpclient.ModelInfo, probeCurrent string) router.Options {
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

func hasModel(models []acpclient.ModelInfo, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// Build assembles the parameter_controls schema. Callers MUST pass the
// same `defaults` they configured on the router (via Resolve), so the
// UI's `default_value`s match what the relay applies at runtime.
//
// If models is empty (probe failed or agent is unauthed) the model
// dropdown is omitted entirely.
//
// Model order is preserved as received from the agent: the agent owns
// priority semantics (capability score, cost tier) that the relay
// can't see, so re-sorting here would clobber a meaningful order.
func Build(models []acpclient.ModelInfo, defaults router.Options) *poeproto.ParameterControls {
	var controls []poeproto.Control

	if len(models) > 0 {
		opts := make([]poeproto.ValueNamePair, 0, len(models))
		for _, m := range models {
			opts = append(opts, poeproto.ValueNamePair{Value: m.ID, Name: m.Name})
		}
		ctl := poeproto.Control{
			Control:       "drop_down",
			Label:         "Model",
			ParameterName: "model",
			Options:       opts,
		}
		if defaults.Model != "" {
			ctl.DefaultValue = defaults.Model
		}
		controls = append(controls, ctl)
	}

	controls = append(controls,
		poeproto.Control{
			Control:       "drop_down",
			Label:         "Thinking",
			ParameterName: "thinking",
			DefaultValue:  defaults.Thinking,
			Options:       append([]poeproto.ValueNamePair(nil), ThinkingLevels...),
		},
		poeproto.Control{
			Control:       "toggle_switch",
			Label:         "Hide thinking output",
			ParameterName: "hide_thinking",
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
