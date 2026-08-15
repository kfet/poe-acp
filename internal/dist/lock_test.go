package dist

import (
	"encoding/json"
	"testing"
)

func TestArtefactUnmarshal(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantVer    string
		wantPolicy Policy
		wantErr    bool
	}{
		{name: "bare string defaults to pin", in: `"0.55.0"`, wantVer: "0.55.0", wantPolicy: PolicyPin},
		{name: "object with policy", in: `{"version":"0.98.1","policy":"floor"}`, wantVer: "0.98.1", wantPolicy: PolicyFloor},
		{name: "object without policy defaults to pin", in: `{"version":"0.98.1"}`, wantVer: "0.98.1", wantPolicy: PolicyPin},
		{name: "unknown policy rejected", in: `{"version":"1","policy":"latest"}`, wantErr: true},
		{name: "wrong type rejected", in: `[1,2]`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var a Artefact
			err := json.Unmarshal([]byte(tc.in), &a)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", a)
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if a.Version != tc.wantVer || a.Policy != tc.wantPolicy {
				t.Fatalf("got %+v want {%s %s}", a, tc.wantVer, tc.wantPolicy)
			}
		})
	}
}

// TestLockBackCompat: the lock shape that predates policies still parses,
// with every artefact defaulting to pin.
func TestLockBackCompat(t *testing.T) {
	var l Lock
	in := `{"poe_acp":"0.55.0","fir":"0.98.1","exts":{"github.com/kfet/fir-exts":"2b2caa7"},"resolved_at":"2026-08-15T04:25:34Z"}`
	if err := json.Unmarshal([]byte(in), &l); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if l.PoeACP.Policy != PolicyPin || l.Fir.Policy != PolicyPin {
		t.Fatalf("policies=%q/%q want pin/pin", l.PoeACP.Policy, l.Fir.Policy)
	}
	if l.Exts["github.com/kfet/fir-exts"] != "2b2caa7" || l.ResolvedAt == "" {
		t.Fatalf("lock=%+v", l)
	}
}

func TestDecide(t *testing.T) {
	pin := func(v string) Artefact { return Artefact{Version: v, Policy: PolicyPin} }
	floor := func(v string) Artefact { return Artefact{Version: v, Policy: PolicyFloor} }
	tests := []struct {
		name    string
		running string
		want    Artefact
		action  Action
	}{
		{"pin equal", "0.55.0", pin("0.55.0"), ActionNone},
		{"pin equal ignoring v prefix", "v0.55.0", pin("0.55.0"), ActionNone},
		{"pin below", "0.54.0", pin("0.55.0"), ActionUpgrade},
		{"pin above downgrades", "0.56.0", pin("0.55.0"), ActionDowngrade},
		{"floor below upgrades", "0.98.0", floor("0.98.1"), ActionUpgrade},
		{"floor equal", "0.98.1", floor("0.98.1"), ActionNone},
		{"floor above is ahead", "0.98.2", floor("0.98.1"), ActionAhead},
		{"shorter version equal", "1.2", pin("1.2.0"), ActionNone},
		{"shorter version below", "1.2", pin("1.2.1"), ActionUpgrade},
		{"prerelease suffix ignored", "0.55.0-dev", pin("0.55.0"), ActionNone},
		{"missing running", "", pin("0.55.0"), ActionUnknown},
		{"missing locked", "0.55.0", pin(""), ActionUnknown},
		{"non-numeric field sorts as zero", "abc", pin("0.0.0"), ActionNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(tc.running, tc.want); got != tc.action {
				t.Fatalf("Decide(%q,%+v)=%s want %s", tc.running, tc.want, got, tc.action)
			}
		})
	}
}
