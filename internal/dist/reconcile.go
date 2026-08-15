package dist

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// errNoRunner is what a reconcile with no command runner reports for the
// fir-side artefacts; the caller wires exec (or a fake) in.
var errNoRunner = errors.New("no command runner configured")

// Options configures one reconcile run. Nothing is changed unless Apply.
type Options struct {
	LockURL string // fleet lock URL (default DefaultLockURL)
	Dir     string // config dir: holds the lock cache and status.json
	BinPath string // live poe-acp binary on this host
	Version string // poe-acp version currently running
	Repo    string // github owner/repo poe-acp releases come from
	Apply   bool   // actually converge; default is dry-run

	HTTP *http.Client
	// Log receives one greppable sentence per action.
	Log func(format string, args ...any)
	// Run executes a command, returning combined output (seam for fir).
	Run func(name string, args ...string) (string, error)
	// Download stages the given poe-acp version, checksum-verified, at dst.
	Download func(repo, version, dst string) error
	// Install swaps the staged binary in and brings a worker up from it,
	// reverting on failure. Supplied by the supervisor.
	Install func(staged string) error
}

// Result is what one run found and did.
type Result struct {
	Lock    Lock
	ETag    string
	PoeACP  Action
	Fir     Action
	FirVer  string
	Actions []string // done, or would be done in dry-run
	Drift   []string // what this host cannot fix itself
}

// Reconcile pulls the lock, compares it with this host and — with Apply
// — converges. Artefacts are independent: a fir failure must not stop a
// poe-acp swap, so nothing here returns early except a lock fetch error.
func Reconcile(opts Options) (Result, error) {
	var res Result
	if opts.LockURL == "" {
		opts.LockURL = DefaultLockURL
	}
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	if opts.Run == nil {
		opts.Run = func(string, ...string) (string, error) { return "", errNoRunner }
	}
	if opts.Repo == "" {
		opts.Repo = "kfet/poe-acp"
	}

	lock, etag, err := fetchLock(opts.HTTP, opts.LockURL, filepath.Join(opts.Dir, "dist.lock.cache"))
	if err != nil {
		return res, err
	}
	res.Lock, res.ETag = lock, etag
	mode := "dry-run"
	if opts.Apply {
		mode = "apply"
	}
	opts.Log("reconcile (%s): lock resolved_at=%s poe_acp=%s fir=%s/%s",
		mode, lock.ResolvedAt, lock.PoeACP.Version, lock.Fir.Version, lock.Fir.Policy)

	res.PoeACP = reconcilePoeACP(&opts, lock.PoeACP, &res)
	res.Fir = reconcileFir(&opts, lock.Fir, &res)
	reconcileExts(&opts, lock, &res)

	if err := writeStatus(&opts, &res); err != nil {
		opts.Log("reconcile: WARN status.json: %v", err)
	}
	return res, nil
}

// reconcilePoeACP converges the relay's own binary. Policy pin: exact
// match, downgrade included.
func reconcilePoeACP(opts *Options, want Artefact, res *Result) Action {
	act := Decide(opts.Version, want)
	switch act {
	case ActionNone:
		opts.Log("reconcile: poe-acp at lock (%s)", opts.Version)
		return act
	case ActionAhead, ActionUnknown:
		res.note(opts, &res.Drift, "poe-acp %s vs lock %s (%s): %s", opts.Version, want.Version, want.Policy, act)
		return act
	}
	if !opts.Apply {
		res.note(opts, &res.Actions, "would swap poe-acp %s -> %s (%s)", opts.Version, want.Version, act)
		return act
	}
	if opts.Download == nil || opts.Install == nil {
		res.note(opts, &res.Drift, "poe-acp swap %s -> %s skipped: no downloader/installer", opts.Version, want.Version)
		return act
	}
	staged := opts.BinPath + ".new"
	if err := opts.Download(opts.Repo, want.Version, staged); err != nil {
		os.Remove(staged)
		res.note(opts, &res.Drift, "poe-acp download %s FAILED: %v", want.Version, err)
		return act
	}
	if err := opts.Install(staged); err != nil {
		res.note(opts, &res.Drift, "poe-acp swap %s -> %s FAILED: %v", opts.Version, want.Version, err)
		return act
	}
	res.note(opts, &res.Actions, "swapped %s -> %s", opts.Version, want.Version)
	return act
}

// reconcileFir does NOT manage fir's binary — fir has its own
// self-updater. We ask it to update and then verify the lock's floor.
// A fir that is AHEAD of a floor is left alone; a fir above a PIN is
// drift we report, because downgrading fir is a deliberate manual act.
func reconcileFir(opts *Options, want Artefact, res *Result) Action {
	cur, err := firVersion(opts)
	if err != nil {
		res.note(opts, &res.Drift, "fir version unavailable: %v", err)
		return ActionUnknown
	}
	res.FirVer = cur
	act := Decide(cur, want)
	switch act {
	case ActionNone:
		opts.Log("reconcile: fir at lock (%s)", cur)
		return act
	case ActionAhead:
		res.note(opts, &res.Actions, "fir ahead of lock (%s > %s), leaving", cur, want.Version)
		return act
	case ActionDowngrade:
		res.note(opts, &res.Drift, "fir %s above pinned lock %s; downgrade is manual", cur, want.Version)
		return act
	case ActionUnknown:
		res.note(opts, &res.Drift, "fir version %q not comparable with lock %q", cur, want.Version)
		return act
	}
	if !opts.Apply {
		res.note(opts, &res.Actions, "would run `fir update` (%s -> at least %s)", cur, want.Version)
		return act
	}
	if out, err := opts.Run("fir", "update"); err != nil {
		res.note(opts, &res.Drift, "fir update FAILED: %v: %s", err, firstLine(out))
		return act
	}
	now, err := firVersion(opts)
	switch {
	case err != nil:
		res.note(opts, &res.Drift, "fir version unavailable after update: %v", err)
	case Decide(now, want) == ActionUpgrade:
		res.FirVer = now
		res.note(opts, &res.Drift, "fir still below lock after update (%s < %s)", now, want.Version)
	default:
		res.FirVer = now
		res.note(opts, &res.Actions, "fir updated %s -> %s (lock floor %s)", cur, now, want.Version)
	}
	return act
}

// reconcileExts keeps fir's extension packages current. Revisions are
// pinned by fir's own package manager, so "update, then verify each
// locked source is installed" is the whole contract.
func reconcileExts(opts *Options, lock Lock, res *Result) {
	srcs := make([]string, 0, len(lock.Exts))
	for src := range lock.Exts {
		srcs = append(srcs, src)
	}
	if len(srcs) == 0 {
		return
	}
	if !opts.Apply {
		res.note(opts, &res.Actions, "would run `fir packages update` for %s", strings.Join(srcs, " "))
		return
	}
	if out, err := opts.Run("fir", "packages", "update"); err != nil {
		res.note(opts, &res.Drift, "fir packages update FAILED: %v: %s", err, firstLine(out))
		return
	}
	list, err := opts.Run("fir", "packages", "list")
	if err != nil {
		res.note(opts, &res.Drift, "fir packages list FAILED: %v", err)
		return
	}
	for _, src := range srcs {
		if strings.Contains(list, src) {
			res.note(opts, &res.Actions, "ext %s updated", src)
			continue
		}
		res.note(opts, &res.Drift, "ext %s not installed after update", src)
	}
}

// note logs one greppable sentence and files it under a bucket.
func (r *Result) note(opts *Options, bucket *[]string, format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	opts.Log("reconcile: %s", s)
	*bucket = append(*bucket, s)
}

// firVersion asks fir what it is; the version is the last field of the
// first output line, with any "v" prefix stripped.
func firVersion(opts *Options) (string, error) {
	out, err := opts.Run("fir", "--version")
	if err != nil {
		return "", err
	}
	f := strings.Fields(firstLine(out))
	if len(f) == 0 {
		return "", errors.New("empty output")
	}
	return strings.TrimPrefix(f[len(f)-1], "v"), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
