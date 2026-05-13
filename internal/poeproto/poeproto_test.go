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
		ResponseVersion:  SettingsResponseVersion,
		AllowAttachments: false,
		ParameterControls: &ParameterControls{
			APIVersion: ParameterControlsAPIVersion,
			Sections: []Section{{
				Name: "Options",
				Controls: []Control{
					{Control: "drop_down", Label: "Model", ParameterName: "model", DefaultValue: "x",
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
		`"response_version":2`,
		`"parameter_controls":`,
		`"api_version":"2"`,
		`"sections":`,
		`"parameter_name":"model"`,
		`"options":[{"value":"x","name":"X"}]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
}

func TestRequest_DecodesAttachments(t *testing.T) {
	t.Parallel()
	body := `{
	  "type":"query",
	  "query":[
	    {"role":"user","content":"see attached","attachments":[
	      {"url":"https://poe.example/x.png","content_type":"image/png","name":"x.png"},
	      {"url":"https://poe.example/y.pdf","content_type":"application/pdf","name":"y.pdf","parsed_content":"hello"}
	    ]}
	  ]
	}`
	req, err := Decode(strings.NewReader(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	atts := req.Query[0].Attachments
	if len(atts) != 2 {
		t.Fatalf("len=%d want 2", len(atts))
	}
	if atts[0].URL != "https://poe.example/x.png" || atts[0].ContentType != "image/png" || atts[0].Name != "x.png" {
		t.Fatalf("att[0]=%+v", atts[0])
	}
	if atts[1].ParsedContent != "hello" {
		t.Fatalf("att[1].ParsedContent=%q", atts[1].ParsedContent)
	}
}

func TestRequest_DecodesReaction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		body       string
		wantKind   string
		wantAction ReactionAction
	}{
		{"split-added", `{"type":"report_reaction","reaction":"👍","action":"added","message_id":"m1"}`, "👍", ReactionAdded},
		{"split-removed", `{"type":"report_reaction","reaction":"👎","action":"removed","message_id":"m1"}`, "👎", ReactionRemoved},
		{"plus-prefix", `{"type":"report_reaction","reaction":"+👍","message_id":"m1"}`, "👍", ReactionAdded},
		{"minus-prefix", `{"type":"report_reaction","reaction":"-👍","message_id":"m1"}`, "👍", ReactionRemoved},
		{"bare-like", `{"type":"report_reaction","reaction":"like","message_id":"m1"}`, "like", ReactionAdded},
		{"bare-dislike", `{"type":"report_reaction","reaction":"dislike","message_id":"m1"}`, "dislike", ReactionAdded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := Decode(strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if req.Reaction != tc.wantKind {
				t.Errorf("Reaction=%q want %q", req.Reaction, tc.wantKind)
			}
			if req.ReactionAction != tc.wantAction {
				t.Errorf("ReactionAction=%q want %q", req.ReactionAction, tc.wantAction)
			}
		})
	}
}
