package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHosts_LoadAndLabels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
	  "hosts": [
	    {"value": "zboxserver", "name": "zbox (server)"},
	    {"value": "boxy"}
	  ],
	  "defaults": {"host": "boxy"}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, ok, err := Load(path)
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if len(cfg.Hosts) != 2 {
		t.Fatalf("hosts = %#v", cfg.Hosts)
	}
	if got := cfg.Hosts[0].Label(); got != "zbox (server)" {
		t.Fatalf("label = %q", got)
	}
	if got := cfg.Hosts[1].Label(); got != "boxy" {
		t.Fatalf("label fallback = %q", got)
	}
	if cfg.Defaults.Host != "boxy" {
		t.Fatalf("defaults.host = %q", cfg.Defaults.Host)
	}
	vals := Values(cfg.Hosts)
	if len(vals) != 2 || vals[0] != "zboxserver" || vals[1] != "boxy" {
		t.Fatalf("Values = %#v", vals)
	}
	if Values(nil) != nil {
		t.Fatal("Values(nil) must be nil")
	}
}

// No hosts key: zero value, and nothing about the rest of the config
// changes — the pre-feature shape still loads.
func TestHosts_AbsentIsZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"bot_name":"b","defaults":{"thinking":"high"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, ok, err := Load(path)
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if cfg.Hosts != nil || cfg.Defaults.Host != "" {
		t.Fatalf("want zero hosts, got %#v / %q", cfg.Hosts, cfg.Defaults.Host)
	}
}

func TestHosts_ValidateErrors(t *testing.T) {
	cases := map[string]struct {
		cfg  Config
		want string
	}{
		"empty value": {
			cfg:  Config{Hosts: []Host{{Value: "a"}, {Name: "no value"}}},
			want: "hosts[1].value",
		},
		"duplicate": {
			cfg:  Config{Hosts: []Host{{Value: "a"}, {Value: "a"}}},
			want: "duplicate",
		},
		"default not listed": {
			cfg:  Config{Hosts: []Host{{Value: "a"}}, Defaults: Defaults{Host: "b"}},
			want: "defaults.host",
		},
	}
	for name, c := range cases {
		err := c.cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want mention of %q", name, err, c.want)
		}
	}
}

// defaults.host with no curated list is legal: it pins every
// conversation to one host without a user-facing dropdown.
func TestHosts_ValidateDefaultWithoutList(t *testing.T) {
	if err := (Config{Defaults: Defaults{Host: "zboxserver"}}).Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := (Config{Hosts: []Host{{Value: "a"}}, Defaults: Defaults{Host: "a"}}).Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// An unknown key inside a host entry must fail loudly like any other
// config typo (DisallowUnknownFields applies to nested structs too).
func TestHosts_UnknownKeyRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"hosts":[{"value":"a","lable":"typo"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown nested key")
	}
}

// The serialised shape must stay stable: a host list round-trips
// through JSON with the documented keys only.
func TestHosts_JSONShape(t *testing.T) {
	b, err := json.Marshal(Config{Hosts: []Host{{Value: "a"}, {Value: "b", Name: "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); !strings.Contains(got, `"hosts":[{"value":"a"},{"value":"b","name":"B"}]`) {
		t.Fatalf("json = %s", got)
	}
	// A config with no hosts must not emit the key at all.
	b, err = json.Marshal(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hosts") {
		t.Fatalf("empty config emitted hosts: %s", b)
	}
}
