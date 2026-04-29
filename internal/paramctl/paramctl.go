// Package paramctl builds the Poe parameter_controls schema from the
// agent's available models and the relay's static control set.
package paramctl

import (
	"github.com/kfet/poe-acp-relay/internal/acpclient"
	"github.com/kfet/poe-acp-relay/internal/poeproto"
)

// ThinkingLevels is the v1 thinking dropdown options. Wire values match
// fir's `ai.ThinkingLevel` constants: "off" for disabled, then minimal
// through high.
var ThinkingLevels = []poeproto.ValueNamePair{
	{Value: "off", Name: "Off"},
	{Value: "minimal", Name: "Minimal"},
	{Value: "low", Name: "Low"},
	{Value: "medium", Name: "Medium"},
	{Value: "high", Name: "High"},
}

// DefaultThinking is the v1 default thinking level. Matches fir's own
// default so the agent's behaviour out of the box is unchanged.
const DefaultThinking = "medium"

// Build assembles the parameter_controls schema from a model snapshot.
// If models is empty (probe failed or agent is unauthed) the model
// dropdown is omitted.
func Build(models []acpclient.ModelInfo, currentModelID string) *poeproto.ParameterControls {
	var controls []poeproto.Control

	if len(models) > 0 {
		opts := make([]poeproto.ValueNamePair, 0, len(models))
		for _, m := range models {
			opts = append(opts, poeproto.ValueNamePair{Value: m.ID, Name: m.Name})
		}
		def := currentModelID
		if def == "" {
			def = models[0].ID
		}
		controls = append(controls, poeproto.Control{
			Control:       "dropdown",
			Label:         "Model",
			ParameterName: "model",
			DefaultValue:  def,
			Options:       opts,
		})
	}

	controls = append(controls,
		poeproto.Control{
			Control:       "dropdown",
			Label:         "Thinking",
			ParameterName: "thinking",
			DefaultValue:  DefaultThinking,
			Options:       append([]poeproto.ValueNamePair(nil), ThinkingLevels...),
		},
		poeproto.Control{
			Control:       "toggle_switch",
			Label:         "Hide thinking output",
			ParameterName: "hide_thinking",
			DefaultValue:  false,
		},
	)

	return &poeproto.ParameterControls{
		Sections: []poeproto.Section{{
			Name:     "Options",
			Controls: controls,
		}},
	}
}
