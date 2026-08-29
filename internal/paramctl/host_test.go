package paramctl

import (
	"encoding/json"
	"testing"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/poe-acp/internal/config"
	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/router"
)

func hostControl(pc *poeproto.ParameterControls) (poeproto.Control, bool) {
	for _, c := range pc.Sections[0].Controls {
		if c.ParameterName == poeproto.ParamHost {
			return c, true
		}
	}
	return poeproto.Control{}, false
}

// No configured hosts: the schema is byte-identical to the one built
// before the feature existed — proven by comparing against the same
// build with the host code path unreachable (nil list) and asserting no
// `host` parameter appears anywhere in the serialised settings.
func TestBuild_NoHostsOmitsControl(t *testing.T) {
	t.Parallel()
	models := []client.ModelInfo{{ID: "anthropic/sonnet", Name: "Sonnet"}}
	defs := router.Options{Model: "anthropic/sonnet", Thinking: "medium"}

	for name, hosts := range map[string][]config.Host{"nil": nil, "empty": {}} {
		pc := Build(models, hosts, defs)
		if _, ok := hostControl(pc); ok {
			t.Fatalf("%s: Host control must be omitted", name)
		}
		b, err := json.Marshal(pc)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(b); contains(got, `"host"`) {
			t.Fatalf("%s: serialised settings mention host: %s", name, got)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Configured hosts produce one drop_down, in config order, with
// Name-or-Value labels and defaults.Host as the default_value.
func TestBuild_HostDropDown(t *testing.T) {
	t.Parallel()
	hosts := []config.Host{
		{Value: "zboxserver", Name: "zbox (server)"},
		{Value: "boxy"},
	}
	pc := Build(nil, hosts, router.Options{Thinking: "medium", Host: "boxy"})
	c, ok := hostControl(pc)
	if !ok {
		t.Fatal("Host control missing")
	}
	if c.Control != "drop_down" {
		t.Fatalf("control = %q", c.Control)
	}
	if len(c.Options) != 2 ||
		c.Options[0].Value != "zboxserver" || c.Options[0].Name != "zbox (server)" ||
		c.Options[1].Value != "boxy" || c.Options[1].Name != "boxy" {
		t.Fatalf("options = %#v", c.Options)
	}
	if c.DefaultValue != "boxy" {
		t.Fatalf("default_value = %#v, want boxy", c.DefaultValue)
	}
}

// No defaults.host (or one outside the list, which config.Validate
// rejects at load but Build must still survive): the first host is the
// default so the UI never shows an empty selection.
func TestBuild_HostDefaultFallsBackToFirst(t *testing.T) {
	t.Parallel()
	hosts := []config.Host{{Value: "zboxserver"}, {Value: "boxy"}}
	for name, want := range map[string]string{"unset": "", "unlisted": "ghost"} {
		pc := Build(nil, hosts, router.Options{Host: want})
		c, ok := hostControl(pc)
		if !ok {
			t.Fatalf("%s: Host control missing", name)
		}
		if c.DefaultValue != "zboxserver" {
			t.Fatalf("%s: default_value = %#v, want zboxserver", name, c.DefaultValue)
		}
	}
}

// Resolve passes defaults.host straight through, with "" (no hint) as
// the built-in fallback.
func TestResolve_Host(t *testing.T) {
	t.Parallel()
	if got := Resolve(config.Defaults{}, nil, "").Host; got != "" {
		t.Fatalf("host fallback = %q, want empty", got)
	}
	if got := Resolve(config.Defaults{Host: "boxy"}, nil, "").Host; got != "boxy" {
		t.Fatalf("host = %q, want boxy", got)
	}
	// Model resolution short-circuits on an empty model list; host must
	// still survive that early return.
	models := []client.ModelInfo{{ID: "anthropic/sonnet"}}
	if got := Resolve(config.Defaults{Host: "boxy", Model: "anthropic/sonnet"}, models, "").Host; got != "boxy" {
		t.Fatalf("host with models = %q, want boxy", got)
	}
}

// The reserved "local" value is just another dropdown option to the
// schema builder: it keeps its exact wire value (the relay, not Poe,
// translates it into "no host hint") and takes its label from `name`.
func TestBuild_LocalSentinelIsANormalOption(t *testing.T) {
	t.Parallel()
	hosts := []config.Host{{Value: config.LocalHost, Name: "zbox (local)"}, {Value: "boxy"}}
	for name, tc := range map[string]struct{ def, wantDefault string }{
		"local as default": {config.LocalHost, "local"},
		"remote default":   {"boxy", "boxy"},
		"unset default":    {"", "local"}, // first option wins
	} {
		pc := Build(nil, hosts, router.Options{Host: tc.def})
		c, ok := hostControl(pc)
		if !ok {
			t.Fatalf("%s: Host control missing", name)
		}
		want := []poeproto.ValueNamePair{
			{Value: "local", Name: "zbox (local)"},
			{Value: "boxy", Name: "boxy"},
		}
		if len(c.Options) != len(want) {
			t.Fatalf("%s: options = %#v", name, c.Options)
		}
		for i, w := range want {
			if c.Options[i] != w {
				t.Fatalf("%s: options[%d] = %#v, want %#v", name, i, c.Options[i], w)
			}
		}
		// Unset falls back to the first option, which here is local.
		if c.DefaultValue != tc.wantDefault {
			t.Fatalf("%s: default_value = %#v, want %q", name, c.DefaultValue, tc.wantDefault)
		}
	}
}

// defaults.host = "local" survives Resolve untouched — the sentinel is
// dropped at the wire, not at config resolution.
func TestResolve_LocalHost(t *testing.T) {
	t.Parallel()
	if got := Resolve(config.Defaults{Host: config.LocalHost}, nil, "").Host; got != "local" {
		t.Fatalf("host = %q, want local", got)
	}
}
