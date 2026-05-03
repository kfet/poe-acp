package acpclient

import (
	"encoding/json"
	"testing"
)

func TestParseCaps(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want Caps
	}{
		"empty": {
			raw:  `{}`,
			want: Caps{},
		},
		"loadSession only": {
			raw:  `{"agentCapabilities":{"loadSession":true}}`,
			want: Caps{LoadSession: true},
		},
		"list+resume": {
			raw:  `{"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"list":{},"resume":{}}}}`,
			want: Caps{LoadSession: true, ListSessions: true, ResumeSession: true},
		},
		"list only": {
			raw:  `{"agentCapabilities":{"sessionCapabilities":{"list":{}}}}`,
			want: Caps{ListSessions: true},
		},
		"malformed json": {
			raw:  `{"agentCapabilities":`,
			want: Caps{},
		},
		"unrelated fields ignored": {
			raw:  `{"agentInfo":{"name":"x"},"protocolVersion":1}`,
			want: Caps{},
		},
		"embeddedContext": {
			raw:  `{"agentCapabilities":{"promptCapabilities":{"embeddedContext":true}}}`,
			want: Caps{EmbeddedContext: true},
		},
		"systemPrompt cap": {
			raw:  `{"agentCapabilities":{"_meta":{"session.systemPrompt":{"version":1}}}}`,
			want: Caps{SystemPrompt: true},
		},
		"systemPrompt absent in _meta": {
			raw:  `{"agentCapabilities":{"_meta":{"other":{}}}}`,
			want: Caps{},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := parseCaps(json.RawMessage(c.raw))
			if got != c.want {
				t.Fatalf("parseCaps(%s) = %+v, want %+v", c.raw, got, c.want)
			}
		})
	}
}
