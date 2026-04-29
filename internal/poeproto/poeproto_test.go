package poeproto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequest_LatestParameters(t *testing.T) {
	t.Parallel()
	body := `{
	  "type":"query",
	  "query":[
	    {"role":"user","content":"hi","parameters":{"model":"x","thinking":"high"}},
	    {"role":"bot","content":"hello"},
	    {"role":"user","content":"again","parameters":{"model":"y"}}
	  ]
	}`
	req, err := Decode(strings.NewReader(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	p := req.LatestParameters()
	if got := p["model"]; got != "y" {
		t.Fatalf("latest model = %v want y", got)
	}
}

func TestSettingsResponse_ParameterControlsMarshal(t *testing.T) {
	t.Parallel()
	r := SettingsResponse{
		AllowAttachments: false,
		ParameterControls: &ParameterControls{
			Sections: []Section{{
				Name: "Options",
				Controls: []Control{
					{Control: "dropdown", Label: "Model", ParameterName: "model", DefaultValue: "x",
						Options: []ValueNamePair{{Value: "x", Name: "X"}}},
					{Control: "toggle_switch", Label: "Hide", ParameterName: "hide_thinking", DefaultValue: false},
				},
			}},
		},
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		`"parameter_controls":`,
		`"sections":`,
		`"parameter_name":"model"`,
		`"options":[{"value":"x","name":"X"}]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
}
