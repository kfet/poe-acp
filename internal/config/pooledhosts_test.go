package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// A host with its own `agent_cmd` is POOLED: the relay runs an agent
// process for it rather than hinting placement to the single default
// agent. Everything about that distinction is decided by these two
// accessors, so they are pinned here.
func TestHost_PooledAndProvision(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		host       Host
		wantPooled bool
		wantProv   string
	}{
		"hint host is not pooled and provisions nothing": {
			host: Host{Value: "miki"},
		},
		"pooled host provisions on its own value by default": {
			host:       Host{Value: "miki", AgentCmd: "ssh -T miki fir --mode acp"},
			wantPooled: true, wantProv: "miki",
		},
		"explicit ssh_host wins": {
			host:       Host{Value: "miki", AgentCmd: "ssh -T m fir --mode acp", SSHHost: "miki.internal"},
			wantPooled: true, wantProv: "miki.internal",
		},
		"ssh_host local means the relay's own filesystem": {
			host:       Host{Value: "second", AgentCmd: "fir --mode acp", SSHHost: LocalHost},
			wantPooled: true, wantProv: "",
		},
		"ssh_host is ignored without an agent_cmd": {
			host: Host{Value: "miki", SSHHost: "elsewhere"},
		},
	} {
		if got := tc.host.Pooled(); got != tc.wantPooled {
			t.Errorf("%s: Pooled = %v, want %v", name, got, tc.wantPooled)
		}
		if got := tc.host.Provision(); got != tc.wantProv {
			t.Errorf("%s: Provision = %q, want %q", name, got, tc.wantProv)
		}
	}
}

func TestHosts_PooledValidation(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cfg     Config
		wantErr string
	}{
		"pooled host is accepted": {cfg: Config{Hosts: []Host{
			{Value: "miki", AgentCmd: "ssh -T miki .local/bin/fir --mode acp"},
		}}},
		"pooled host with a local filesystem is accepted": {cfg: Config{Hosts: []Host{
			{Value: "second", AgentCmd: "fir --mode acp", SSHHost: LocalHost},
		}}},
		"agent_cmd on the reserved sentinel is rejected": {
			cfg: Config{Hosts: []Host{{Value: LocalHost, AgentCmd: "fir --mode acp"}}},
			// The sentinel means "the relay's own --agent-cmd"; a second
			// answer to the same question is a config mistake.
			wantErr: "not allowed on the reserved",
		},
		"ssh_host without agent_cmd is rejected": {
			cfg:     Config{Hosts: []Host{{Value: "miki", SSHHost: "miki"}}},
			wantErr: "only meaningful with `agent_cmd`",
		},
		"blank agent_cmd is rejected": {
			cfg:     Config{Hosts: []Host{{Value: "miki", AgentCmd: "   "}}},
			wantErr: "must not be blank",
		},
		"unusable ssh destination is rejected": {
			cfg:     Config{Hosts: []Host{{Value: "miki", AgentCmd: "fir --mode acp", SSHHost: "bad host"}}},
			wantErr: "ssh_host",
		},
	} {
		err := tc.cfg.Validate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: Validate: %v", name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want %q", name, err, tc.wantErr)
		}
	}
}

// The new keys are part of the operator-facing JSON contract, and a
// config that uses them must survive the strict decoder.
func TestHosts_PooledJSONShape(t *testing.T) {
	t.Parallel()
	const doc = `{"hosts":[
		{"value":"local","name":"here"},
		{"value":"miki","agent_cmd":"ssh -T miki fir --mode acp"},
		{"value":"beta","agent_cmd":"fir --mode acp","ssh_host":"local"}
	]}`
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.Hosts[0].Pooled() || !cfg.Hosts[1].Pooled() || !cfg.Hosts[2].Pooled() {
		t.Fatalf("pooling misparsed: %+v", cfg.Hosts)
	}
	if got := cfg.Hosts[1].Provision(); got != "miki" {
		t.Fatalf("hosts[1] provision = %q", got)
	}
	if got := cfg.Hosts[2].Provision(); got != "" {
		t.Fatalf("hosts[2] provision = %q, want the relay's own filesystem", got)
	}
}
