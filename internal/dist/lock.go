// Package dist implements `poe-acp reconcile`: pull the fleet-wide
// dist.lock, compare it with what this host is actually running, and
// (with --apply) converge.
//
// Pull, not push: the fleet is intermittently online, so an ssh-push
// never reaches a sleeping laptop. Each host fetches the lock itself and
// converges on its own schedule.
//
// There is no state file beyond the lock's ETag cache and a status.json
// snapshot: everything else is derived from what is on disk right now.
package dist

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Policy says how a running version must be brought to the locked one.
type Policy string

const (
	// PolicyPin converges exactly, including downgrade.
	PolicyPin Policy = "pin"
	// PolicyFloor upgrades when below the locked version and leaves an
	// already-newer version alone (reported as "ahead").
	PolicyFloor Policy = "floor"
)

// Artefact is one locked component. It decodes from either a bare
// version string (policy defaults to pin, so pre-policy locks keep
// parsing) or an object: {"version": "0.98.1", "policy": "floor"}.
type Artefact struct {
	Version string `json:"version"`
	Policy  Policy `json:"policy,omitempty"`
}

// UnmarshalJSON accepts the string and the object form.
func (a *Artefact) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		a.Version, a.Policy = s, PolicyPin
		return nil
	}
	type raw Artefact // no recursion
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("artefact: want version string or object: %w", err)
	}
	*a = Artefact(r)
	if a.Policy == "" {
		a.Policy = PolicyPin
	}
	if a.Policy != PolicyPin && a.Policy != PolicyFloor {
		return fmt.Errorf("artefact: unknown policy %q", a.Policy)
	}
	return nil
}

// Lock is the fleet-wide target.
type Lock struct {
	PoeACP     Artefact          `json:"poe_acp"`
	Fir        Artefact          `json:"fir"`
	Exts       map[string]string `json:"exts,omitempty"`
	ResolvedAt string            `json:"resolved_at,omitempty"`
}

// Action is what the comparison of a running version with a locked one
// demands.
type Action string

const (
	// ActionNone: already at the locked version.
	ActionNone Action = "ok"
	// ActionUpgrade: running below the lock.
	ActionUpgrade Action = "upgrade"
	// ActionDowngrade: running above a pinned lock — pin converges
	// exactly, so this is a real action.
	ActionDowngrade Action = "downgrade"
	// ActionAhead: running above a floor lock — leave alone, report.
	ActionAhead Action = "ahead"
	// ActionUnknown: one of the versions could not be parsed.
	ActionUnknown Action = "unknown"
)

// Decide compares the running version with the locked artefact.
func Decide(running string, want Artefact) Action {
	if running == "" || want.Version == "" {
		return ActionUnknown
	}
	switch cmpVersion(running, want.Version) {
	case 0:
		return ActionNone
	case -1:
		return ActionUpgrade
	default:
		if want.Policy == PolicyFloor {
			return ActionAhead
		}
		return ActionDowngrade
	}
}

// cmpVersion compares dotted numeric versions ("v" prefix and any
// -suffix tolerated): -1 if a<b, 0 if equal, 1 if a>b. A non-numeric
// field sorts as 0, which is fine for the "0.55.0" shapes we lock.
func cmpVersion(a, b string) int {
	fa, fb := fields(a), fields(b)
	for i := 0; i < len(fa) || i < len(fb); i++ {
		var x, y int
		if i < len(fa) {
			x = fa[i]
		}
		if i < len(fb) {
			y = fb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func fields(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ \t"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}
	return out
}
