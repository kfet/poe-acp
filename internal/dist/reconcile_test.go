package dist

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testLockBody = `{"poe_acp":"0.56.0","fir":{"version":"0.98.1","policy":"floor"},` +
	`"exts":{"github.com/kfet/fir-exts":"2b2caa7"},"resolved_at":"2026-08-15T04:25:34Z"}`

// lockServer serves testLockBody with an ETag and counts how many full
// bodies it had to send, so conditional-GET behaviour is observable.
func lockServer(t *testing.T, body string) (*httptest.Server, *int) {
	t.Helper()
	sent := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"etag-1"`)
		if r.Header.Get("If-None-Match") == `"etag-1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		sent++
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &sent
}

func TestFetchLockConditional(t *testing.T) {
	srv, sent := lockServer(t, testLockBody)
	cache := filepath.Join(t.TempDir(), "dist.lock.cache")

	lock, etag, err := fetchLock(srv.Client(), srv.URL, cache)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if lock.PoeACP.Version != "0.56.0" || etag != `"etag-1"` {
		t.Fatalf("lock=%+v etag=%q", lock, etag)
	}
	// Second fetch revalidates: 304, body served from cache.
	lock2, etag2, err := fetchLock(srv.Client(), srv.URL, cache)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if lock2.Fir.Policy != PolicyFloor || etag2 != etag {
		t.Fatalf("lock2=%+v etag2=%q", lock2, etag2)
	}
	if *sent != 1 {
		t.Fatalf("server sent %d bodies, want 1 (conditional GET not used)", *sent)
	}
}

func TestFetchLockErrors(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer notFound.Close()
	badJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "{not json")
	}))
	defer badJSON.Close()
	truncated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		fmt.Fprint(w, "{")
		panic(http.ErrAbortHandler) // cut the body mid-response
	}))
	defer truncated.Close()
	staleCache := func(t *testing.T) string {
		// A cache with an ETag but no body forces the "304 with nothing
		// to serve" path.
		p := filepath.Join(t.TempDir(), "cache")
		if err := os.WriteFile(p, []byte(`{"etag":"\"etag-1\""}`), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	tests := []struct {
		name    string
		url     func(t *testing.T) string
		cache   func(t *testing.T) string
		wantErr string
	}{
		{name: "bad url", url: func(*testing.T) string { return "://bad" }, wantErr: "missing protocol"},
		{name: "unreachable", url: func(*testing.T) string { return "http://127.0.0.1:1/lock" }, wantErr: "fetch lock"},
		{name: "http error", url: func(*testing.T) string { return notFound.URL }, wantErr: "404"},
		{name: "bad json", url: func(*testing.T) string { return badJSON.URL }, wantErr: "parse lock"},
		{name: "truncated body", url: func(*testing.T) string { return truncated.URL }, wantErr: "fetch lock"},
		{
			name:    "304 with empty cache",
			url:     func(t *testing.T) string { srv, _ := lockServer(t, testLockBody); return srv.URL },
			cache:   staleCache,
			wantErr: "no cached body",
		},
		{
			name: "cache unwritable",
			url:  func(t *testing.T) string { srv, _ := lockServer(t, testLockBody); return srv.URL },
			cache: func(t *testing.T) string {
				dir := filepath.Join(t.TempDir(), "ro")
				if err := os.Mkdir(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(dir, "cache")
			},
			wantErr: "cache lock",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cache := filepath.Join(t.TempDir(), "cache")
			if tc.cache != nil {
				cache = tc.cache(t)
			}
			_, _, err := fetchLock(http.DefaultClient, tc.url(t), cache)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v want containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestFetchLockCorruptCacheOnNotModified: a cached body that no longer
// parses is reported, not silently treated as an empty lock.
func TestFetchLockCorruptCacheOnNotModified(t *testing.T) {
	srv, _ := lockServer(t, testLockBody)
	cache := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(cache, []byte(`{"etag":"\"etag-1\"","body":"nope"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetchLock(srv.Client(), srv.URL, cache); err == nil {
		t.Fatal("want error for a corrupt cached body")
	}
}

func TestWriteJSONRenameFails(t *testing.T) {
	dir := t.TempDir()
	// Renaming a file over a non-empty directory fails; the temp file
	// must not be left behind.
	target := filepath.Join(dir, "status.json")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(target, map[string]string{"a": "b"}); err == nil {
		t.Fatal("want rename error")
	}
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

// fakeRun is a scripted command runner: it records calls and replies
// from a table keyed by the joined argv.
type fakeRun struct {
	replies map[string]struct {
		out string
		err error
	}
	calls []string
}

func (f *fakeRun) run(name string, args ...string) (string, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, key)
	r, ok := f.replies[key]
	if !ok {
		return "", fmt.Errorf("unexpected command %q", key)
	}
	return r.out, r.err
}

func okRun(firVersion string) *fakeRun {
	return &fakeRun{replies: map[string]struct {
		out string
		err error
	}{
		"fir --version":         {out: "fir " + firVersion + "\n"},
		"fir update":            {out: "updated\n"},
		"fir packages update":   {out: "ok\n"},
		"fir packages list":     {out: "github.com/kfet/fir-exts  2b2caa7\n"},
		"fir --version-after":   {out: ""},
		"fir packages list all": {out: ""},
	}}
}

// baseOpts wires a reconcile against a local lock server with fakes for
// every side effect.
func baseOpts(t *testing.T, run *fakeRun) (Options, string) {
	t.Helper()
	srv, _ := lockServer(t, testLockBody)
	dir := t.TempDir()
	return Options{
		LockURL: srv.URL,
		Dir:     dir,
		BinPath: filepath.Join(dir, "poe-acp"),
		Version: "0.55.0",
		HTTP:    srv.Client(),
		Run:     run.run,
	}, dir
}

func hasNote(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

func TestReconcileDryRun(t *testing.T) {
	run := okRun("0.98.0")
	opts, dir := baseOpts(t, run)
	var logged strings.Builder
	opts.Log = func(f string, a ...any) { fmt.Fprintf(&logged, f+"\n", a...) }

	res, err := Reconcile(opts)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.PoeACP != ActionUpgrade || res.Fir != ActionUpgrade {
		t.Fatalf("actions poe=%s fir=%s", res.PoeACP, res.Fir)
	}
	for _, want := range []string{"would swap poe-acp 0.55.0 -> 0.56.0", "would run `fir update`", "would run `fir packages update`"} {
		if !hasNote(res.Actions, want) {
			t.Fatalf("actions %v missing %q", res.Actions, want)
		}
	}
	if !strings.Contains(logged.String(), "reconcile (dry-run)") {
		t.Fatalf("log=%q", logged.String())
	}
	// Dry-run touches nothing but its own bookkeeping.
	if len(run.calls) != 1 || run.calls[0] != "fir --version" {
		t.Fatalf("dry-run ran %v", run.calls)
	}
	var st Status
	readJSON(t, StatusPath(dir), &st)
	if st.Applied || st.PoeACP != "0.55.0" || st.Fir != "0.98.0" || st.Lock.PoeACP.Version != "0.56.0" {
		t.Fatalf("status=%+v", st)
	}
	if st.LockETag == "" || st.Time == "" || st.Host == "" {
		t.Fatalf("status metadata missing: %+v", st)
	}
}

func TestReconcileApplySwapsAndUpdates(t *testing.T) {
	run := okRun("0.98.0")
	run.replies["fir --version"] = struct {
		out string
		err error
	}{out: "0.98.0"}
	opts, dir := baseOpts(t, run)
	opts.Apply = true
	// After `fir update` the version reads back at the floor: script the
	// second --version call by swapping the reply mid-flight.
	calls := 0
	base := run.run
	opts.Run = func(name string, args ...string) (string, error) {
		if name == "fir" && len(args) == 1 && args[0] == "--version" {
			calls++
			if calls > 1 {
				return "0.98.1", nil
			}
		}
		return base(name, args...)
	}
	var staged string
	opts.Download = func(repo, version, dst string) error {
		staged = dst
		if repo != "kfet/poe-acp" || version != "0.56.0" {
			t.Fatalf("download(%q,%q)", repo, version)
		}
		return os.WriteFile(dst, []byte("new"), 0o755)
	}
	installed := ""
	opts.Install = func(s string) error { installed = s; return nil }

	res, err := Reconcile(opts)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if installed != staged || staged != filepath.Join(dir, "poe-acp.new") {
		t.Fatalf("staged=%q installed=%q", staged, installed)
	}
	for _, want := range []string{"swapped 0.55.0 -> 0.56.0", "fir updated 0.98.0 -> 0.98.1", "ext github.com/kfet/fir-exts updated"} {
		if !hasNote(res.Actions, want) {
			t.Fatalf("actions %v missing %q", res.Actions, want)
		}
	}
	if len(res.Drift) != 0 {
		t.Fatalf("unexpected drift %v", res.Drift)
	}
	var st Status
	readJSON(t, StatusPath(dir), &st)
	if !st.Applied || st.Fir != "0.98.1" {
		t.Fatalf("status=%+v", st)
	}
}

// TestReconcileNoop: everything already at the lock — no commands beyond
// the version probes, no actions, no drift.
func TestReconcileNoop(t *testing.T) {
	run := okRun("0.98.1")
	opts, _ := baseOpts(t, run)
	opts.Version = "0.56.0"
	opts.Apply = true
	lockNoExts := strings.Replace(testLockBody, `"exts":{"github.com/kfet/fir-exts":"2b2caa7"},`, "", 1)
	srv, _ := lockServer(t, lockNoExts)
	opts.LockURL, opts.HTTP = srv.URL, srv.Client()

	res, err := Reconcile(opts)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.PoeACP != ActionNone || res.Fir != ActionNone {
		t.Fatalf("poe=%s fir=%s", res.PoeACP, res.Fir)
	}
	if len(res.Actions) != 0 || len(res.Drift) != 0 {
		t.Fatalf("actions=%v drift=%v", res.Actions, res.Drift)
	}
}

func TestReconcileFirPolicies(t *testing.T) {
	tests := []struct {
		name       string
		firVersion string
		lock       string
		wantAction Action
		wantNote   string
		drift      bool
	}{
		{
			name: "ahead of floor is left alone", firVersion: "0.98.2", lock: testLockBody,
			wantAction: ActionAhead, wantNote: "fir ahead of lock (0.98.2 > 0.98.1), leaving",
		},
		{
			name:       "above a pin is drift",
			firVersion: "0.98.2",
			lock:       strings.Replace(testLockBody, `{"version":"0.98.1","policy":"floor"}`, `"0.98.1"`, 1),
			wantAction: ActionDowngrade, wantNote: "downgrade is manual", drift: true,
		},
		{
			name:       "unparseable lock version is drift",
			firVersion: "0.98.2",
			lock:       strings.Replace(testLockBody, `{"version":"0.98.1","policy":"floor"}`, `""`, 1),
			wantAction: ActionUnknown, wantNote: "not comparable", drift: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := okRun(tc.firVersion)
			opts, _ := baseOpts(t, run)
			srv, _ := lockServer(t, tc.lock)
			opts.LockURL, opts.HTTP = srv.URL, srv.Client()

			res, err := Reconcile(opts)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if res.Fir != tc.wantAction {
				t.Fatalf("fir action=%s want %s", res.Fir, tc.wantAction)
			}
			bucket := res.Actions
			if tc.drift {
				bucket = res.Drift
			}
			if !hasNote(bucket, tc.wantNote) {
				t.Fatalf("%v missing %q", bucket, tc.wantNote)
			}
		})
	}
}

// TestReconcileFailures: every side effect that can fail is reported as
// drift, and one artefact's failure never stops the others.
func TestReconcileFailures(t *testing.T) {
	boom := errors.New("boom")
	type reply = struct {
		out string
		err error
	}
	tests := []struct {
		name      string
		mutate    func(*Options, *fakeRun)
		wantDrift []string
	}{
		{
			name: "no downloader or installer",
			mutate: func(o *Options, _ *fakeRun) {
				o.Apply = true
			},
			wantDrift: []string{"no downloader/installer"},
		},
		{
			name: "download fails",
			mutate: func(o *Options, _ *fakeRun) {
				o.Apply = true
				o.Download = func(string, string, string) error { return boom }
				o.Install = func(string) error { t := &testing.T{}; t.Fail(); return nil }
			},
			wantDrift: []string{"poe-acp download 0.56.0 FAILED: boom"},
		},
		{
			name: "install fails",
			mutate: func(o *Options, _ *fakeRun) {
				o.Apply = true
				o.Download = func(_, _, dst string) error { return os.WriteFile(dst, []byte("x"), 0o755) }
				o.Install = func(string) error { return boom }
			},
			wantDrift: []string{"poe-acp swap 0.55.0 -> 0.56.0 FAILED: boom"},
		},
		{
			name: "own version unknown",
			mutate: func(o *Options, _ *fakeRun) {
				o.Version = ""
			},
			wantDrift: []string{"vs lock 0.56.0 (pin): unknown"},
		},
		{
			name: "fir not installed",
			mutate: func(o *Options, r *fakeRun) {
				r.replies["fir --version"] = reply{err: boom}
			},
			wantDrift: []string{"fir version unavailable: boom"},
		},
		{
			name: "fir prints nothing",
			mutate: func(o *Options, r *fakeRun) {
				r.replies["fir --version"] = reply{out: "  \n"}
			},
			wantDrift: []string{"fir version unavailable: empty output"},
		},
		{
			name: "fir update fails",
			mutate: func(o *Options, r *fakeRun) {
				o.Apply = true
				r.replies["fir update"] = reply{out: "network down\ndetail", err: boom}
			},
			wantDrift: []string{"fir update FAILED: boom: network down"},
		},
		{
			name: "fir version unreadable after update",
			mutate: func(o *Options, r *fakeRun) {
				o.Apply = true
				n := 0
				base := r.run
				o.Run = func(name string, args ...string) (string, error) {
					if name == "fir" && len(args) == 1 && args[0] == "--version" {
						if n++; n > 1 {
							return "", boom
						}
					}
					return base(name, args...)
				}
			},
			wantDrift: []string{"fir version unavailable after update: boom"},
		},
		{
			name:   "fir still below lock after update",
			mutate: func(o *Options, _ *fakeRun) { o.Apply = true },
			wantDrift: []string{
				"fir still below lock after update (0.98.0 < 0.98.1)",
			},
		},
		{
			name: "packages update fails",
			mutate: func(o *Options, r *fakeRun) {
				o.Apply = true
				r.replies["fir packages update"] = reply{out: "no network", err: boom}
			},
			wantDrift: []string{"fir packages update FAILED: boom: no network"},
		},
		{
			name: "packages list fails",
			mutate: func(o *Options, r *fakeRun) {
				o.Apply = true
				r.replies["fir packages list"] = reply{err: boom}
			},
			wantDrift: []string{"fir packages list FAILED: boom"},
		},
		{
			name: "ext missing after update",
			mutate: func(o *Options, r *fakeRun) {
				o.Apply = true
				r.replies["fir packages list"] = reply{out: "(none)\n"}
			},
			wantDrift: []string{"ext github.com/kfet/fir-exts not installed after update"},
		},
		{
			name: "no command runner at all",
			mutate: func(o *Options, _ *fakeRun) {
				o.Run = nil
			},
			wantDrift: []string{"fir version unavailable: no command runner configured"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := okRun("0.98.0")
			opts, _ := baseOpts(t, run)
			tc.mutate(&opts, run)
			res, err := Reconcile(opts)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			for _, want := range tc.wantDrift {
				if !hasNote(res.Drift, want) {
					t.Fatalf("drift %v missing %q", res.Drift, want)
				}
			}
		})
	}
}

// TestReconcilePinDowngrades: pin converges exactly, so a host running
// ahead of the lock is swapped BACK to it.
func TestReconcilePinDowngrades(t *testing.T) {
	run := okRun("0.98.1")
	opts, _ := baseOpts(t, run)
	opts.Version = "0.57.0"
	res, err := Reconcile(opts)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.PoeACP != ActionDowngrade || !hasNote(res.Actions, "would swap poe-acp 0.57.0 -> 0.56.0 (downgrade)") {
		t.Fatalf("action=%s actions=%v", res.PoeACP, res.Actions)
	}
}

// TestReconcileLockFetchFails: a lock we cannot pull is the one hard
// error — there is nothing to converge to.
func TestReconcileLockFetchFails(t *testing.T) {
	if _, err := Reconcile(Options{LockURL: "http://127.0.0.1:1/lock", Dir: t.TempDir()}); err == nil {
		t.Fatal("want error")
	}
}

// TestReconcileStatusUnwritable: a status.json we cannot write is warned
// about, not fatal — the convergence itself already happened.
func TestReconcileStatusUnwritable(t *testing.T) {
	run := okRun("0.98.1")
	opts, dir := baseOpts(t, run)
	var logged strings.Builder
	opts.Log = func(f string, a ...any) { fmt.Fprintf(&logged, f+"\n", a...) }
	// A non-empty directory where status.json belongs: the write stages
	// fine and the rename cannot land.
	if err := os.Mkdir(StatusPath(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(StatusPath(dir), "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(opts); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !strings.Contains(logged.String(), "WARN status.json") {
		t.Fatalf("want a status.json warning, log=%q", logged.String())
	}
}

// rtFunc is an http.RoundTripper from a function.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// errBody fails on read, exercising the truncated-response path.
type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, errors.New("read blew up") }
func (errBody) Close() error             { return nil }

func TestFetchLockBodyReadError(t *testing.T) {
	c := &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: errBody{}, Header: http.Header{}}, nil
	})}
	_, _, err := fetchLock(c, "http://lock.invalid/dist.lock", filepath.Join(t.TempDir(), "cache"))
	if err == nil || !strings.Contains(err.Error(), "read blew up") {
		t.Fatalf("err=%v want the read error", err)
	}
}

// TestReconcileDefaults exercises the zero-value Options path: the
// default lock URL and repo are filled in, and the run works with no
// logger and no explicit HTTP client timeout.
func TestReconcileDefaults(t *testing.T) {
	var gotURL string
	opts := Options{
		Dir:     t.TempDir(),
		Version: "0.56.0",
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			gotURL = r.URL.String()
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(testLockBody)),
				Header:     http.Header{},
			}, nil
		})},
	}
	res, err := Reconcile(opts)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if gotURL != DefaultLockURL {
		t.Fatalf("fetched %q want %q", gotURL, DefaultLockURL)
	}
	if res.PoeACP != ActionNone {
		t.Fatalf("poe-acp action=%s want ok", res.PoeACP)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}
