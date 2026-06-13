package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// assetName returns the raw-binary asset name Run computes for this host.
func assetName() string {
	return fmt.Sprintf("poe-acp-%s-%s", runtime.GOOS, assetArch(runtime.GOARCH))
}

// makeRelease builds a fake raw-binary asset + matching checksums.txt for
// the current GOOS/GOARCH. Returns (assetName, binaryBytes, checksums).
func makeRelease(body string) (string, []byte, []byte) {
	asset := assetName()
	bin := []byte(body)
	sum := sha256.Sum256(bin)
	checks := []byte(fmt.Sprintf("%s  %s\nffffffff  other-asset\n", hex.EncodeToString(sum[:]), asset))
	return asset, bin, checks
}

// rewriteTransport rewrites the upstream URLs that selfupdate.Run
// hard-codes (api.github.com + github.com) to point at a test server.
type rewriteTransport struct{ host string }

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Host {
	case "api.github.com":
		req.URL.Scheme = "http"
		req.URL.Host = rt.host
	case "github.com":
		req.URL.Scheme = "http"
		req.URL.Host = rt.host
		req.URL.Path = "/dl" + req.URL.Path
	}
	return http.DefaultTransport.RoundTrip(req)
}

func clientFor(t *testing.T, srv *httptest.Server) (exe string, client *http.Client) {
	t.Helper()
	exe = filepath.Join(t.TempDir(), "poe-acp")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	t.Cleanup(srv.Close)
	return exe, &http.Client{Transport: &rewriteTransport{host: host}}
}

// fixture builds a synthetic release for tag/body and returns a stub exe
// + an http client wired to a server serving it.
func fixture(t *testing.T, tag, body string, overrideChecks []byte) (exe string, client *http.Client) {
	t.Helper()
	asset, bin, checks := makeRelease(body)
	if overrideChecks != nil {
		checks = overrideChecks
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q}`, tag)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			w.Write(bin)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			w.Write(checks)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	exe, client = clientFor(t, srv)
	return exe, client
}

func runWith(currentVersion, version string, exe string, client *http.Client, checkOnly bool, out io.Writer) (Result, error) {
	return Run(currentVersion, Options{
		Repo:       "x/y",
		Version:    version,
		HTTPClient: client,
		CheckOnly:  checkOnly,
		Stdout:     out,
		Stderr:     out,
		ExecPath:   func() (string, error) { return exe, nil },
	})
}

func TestRun_HappyPath(t *testing.T) {
	exe, client := fixture(t, "v0.2.0", "new-binary", nil)
	var out strings.Builder
	res, err := runWith("0.1.0", "", exe, client, false, &out)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if !res.Updated || res.Target != "v0.2.0" || res.Current != "v0.1.0" {
		t.Fatalf("unexpected result: %+v", res)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "new-binary" {
		t.Fatalf("binary not replaced: %q", got)
	}
	fi, _ := os.Stat(exe)
	if fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("replaced binary not executable: %v", fi.Mode())
	}
	if !strings.Contains(out.String(), "updated") {
		t.Fatalf("missing success line: %s", out.String())
	}
}

func TestRun_AlreadyUpToDate(t *testing.T) {
	exe, client := fixture(t, "v0.1.0", "x", nil)
	var out strings.Builder
	res, err := runWith("0.1.0", "", exe, client, false, &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated {
		t.Fatal("should not have updated")
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("expected up-to-date: %s", out.String())
	}
}

func TestRun_CheckOnly(t *testing.T) {
	exe, client := fixture(t, "v9.9.9", "x", nil)
	var out strings.Builder
	res, err := runWith("0.1.0", "", exe, client, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated {
		t.Fatal("CheckOnly must not update")
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Fatalf("CheckOnly should not modify binary: %q", got)
	}
	if !strings.Contains(out.String(), "update available") {
		t.Fatalf("missing 'update available': %s", out.String())
	}
}

func TestRun_ExplicitVersion(t *testing.T) {
	// Server claims "irrelevant" as latest, but we pin "0.3.0" (without
	// v-prefix) — exercises the explicit-version path and ensureV.
	exe, client := fixture(t, "v0.3.0", "v3", nil)
	if _, err := runWith("v0.1.0", "0.3.0", exe, client, false, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "v3" {
		t.Fatalf("binary not replaced: %q", got)
	}
}

func TestRun_ChecksumMismatch(t *testing.T) {
	bad := []byte(fmt.Sprintf("deadbeef  %s\n", assetName()))
	exe, client := fixture(t, "v0.2.0", "x", bad)
	_, err := runWith("0.1.0", "", exe, client, false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
}

func TestRun_NoChecksumEntry(t *testing.T) {
	exe, client := fixture(t, "v0.2.0", "x", []byte("ffff  unrelated-asset\n"))
	_, err := runWith("0.1.0", "", exe, client, false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no checksum entry") {
		t.Fatalf("got %v", err)
	}
}

func TestRun_ManagedPathRefused(t *testing.T) {
	_, err := Run("0.1.0", Options{
		Stdout:   io.Discard,
		Stderr:   io.Discard,
		ExecPath: func() (string, error) { return "/opt/homebrew/bin/poe-acp", nil },
	})
	if err == nil || !strings.Contains(err.Error(), "managed by a package manager") {
		t.Fatalf("got %v", err)
	}
}

func TestRun_ExecPathError(t *testing.T) {
	_, err := Run("0.1.0", Options{
		Stdout:   io.Discard,
		Stderr:   io.Discard,
		ExecPath: func() (string, error) { return "", fmt.Errorf("boom") },
	})
	if err == nil || !strings.Contains(err.Error(), "locate self") {
		t.Fatalf("got %v", err)
	}
}

func TestRun_LatestAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	exe, client := clientFor(t, srv)
	_, err := runWith("0.1.0", "", exe, client, false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "resolve latest") {
		t.Fatalf("got %v", err)
	}
}

func TestRun_DownloadAssetError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v0.2.0"}`)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	exe, client := clientFor(t, httptest.NewServer(mux))
	_, err := runWith("0.1.0", "", exe, client, false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "download asset") {
		t.Fatalf("got %v", err)
	}
}

func TestRun_DownloadChecksumsError(t *testing.T) {
	asset, bin, _ := makeRelease("x")
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v0.2.0"}`)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/"+asset) {
			w.Write(bin)
			return
		}
		http.NotFound(w, r)
	})
	exe, client := clientFor(t, httptest.NewServer(mux))
	_, err := runWith("0.1.0", "", exe, client, false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "download checksums") {
		t.Fatalf("got %v", err)
	}
}

func TestRun_EmptyTagName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":""}`)
	}))
	exe, client := clientFor(t, srv)
	_, err := runWith("0.1.0", "", exe, client, false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "empty tag_name") {
		t.Fatalf("got %v", err)
	}
}

func TestRun_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	exe, client := clientFor(t, srv)
	_, err := runWith("0.1.0", "", exe, client, false, io.Discard)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRun_TempDirError(t *testing.T) {
	asset, bin, checks := makeRelease("x")
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v0.2.0"}`)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			w.Write(bin)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			w.Write(checks)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	client := &http.Client{Transport: &rewriteTransport{host: host}}
	// ExecPath in a non-existent dir → EvalSymlinks fails (resolved=exe
	// fallback) and MkdirTemp under filepath.Dir fails → tempdir error.
	_, err := Run("0.1.0", Options{
		HTTPClient: client,
		Stdout:     io.Discard,
		Stderr:     io.Discard,
		ExecPath:   func() (string, error) { return "/no/such/dir/poe-acp", nil },
	})
	if err == nil || !strings.Contains(err.Error(), "tempdir") {
		t.Fatalf("got %v", err)
	}
}

func TestRun_RenameError(t *testing.T) {
	// resolved points at a NON-EMPTY directory: download + checksum
	// succeed, but os.Rename(file, nonEmptyDir) fails → "replace" error.
	asset, bin, checks := makeRelease("x")
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v0.2.0"}`)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			w.Write(bin)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			w.Write(checks)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	client := &http.Client{Transport: &rewriteTransport{host: host}}

	root := t.TempDir()
	exeDir := filepath.Join(root, "poe-acp") // resolved == a directory
	if err := os.Mkdir(exeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exeDir, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run("0.1.0", Options{
		HTTPClient: client,
		Stdout:     io.Discard,
		Stderr:     io.Discard,
		ExecPath:   func() (string, error) { return exeDir, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "replace") {
		t.Fatalf("got %v", err)
	}
}

func TestLookupChecksum_FileError(t *testing.T) {
	if _, err := lookupChecksum("/no/such/file", "x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestDownload_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	if _, err := download(http.DefaultClient, srv.URL, filepath.Join(t.TempDir(), "out"), 0o644); err == nil {
		t.Fatal("expected error")
	}
}

func TestDownload_GetError(t *testing.T) {
	if _, err := download(http.DefaultClient, "http://127.0.0.1:1/never", filepath.Join(t.TempDir(), "out"), 0o644); err == nil {
		t.Fatal("expected error")
	}
}

func TestDownload_OpenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}))
	defer srv.Close()
	if _, err := download(http.DefaultClient, srv.URL, "/no/such/dir/out", 0o644); err == nil {
		t.Fatal("expected error")
	}
}

func TestDownload_CopyError(t *testing.T) {
	// Advertise more bytes than we send, then close → io.Copy on the
	// client side returns ErrUnexpectedEOF mid-stream.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(200)
		w.Write([]byte("short"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // abort the connection mid-body
	}))
	defer srv.Close()
	if _, err := download(http.DefaultClient, srv.URL, filepath.Join(t.TempDir(), "out"), 0o644); err == nil {
		t.Fatal("expected mid-stream copy error")
	}
}

func TestResolveLatest_NewRequestError(t *testing.T) {
	if _, err := resolveLatest(http.DefaultClient, "bad\x7f/repo"); err == nil {
		t.Fatal("expected error")
	}
}

func TestResolveLatest_DoError(t *testing.T) {
	if _, err := resolveLatest(&http.Client{Transport: &rewriteTransport{host: "127.0.0.1:1"}}, "x/y"); err == nil {
		t.Fatal("expected error")
	}
}

func TestRun_Defaults_ManagedPath(t *testing.T) {
	// Omit HTTPClient/Stdout/Stderr so Run fills the defaults (covers the
	// default-fill branches); ExecPath hits a managed path so it returns
	// before any network I/O.
	_, err := Run("0.1.0", Options{
		ExecPath: func() (string, error) { return "/usr/bin/poe-acp", nil },
	})
	if err == nil || !strings.Contains(err.Error(), "managed by a package manager") {
		t.Fatalf("got %v", err)
	}
}

func TestRun_DefaultExecPath(t *testing.T) {
	// Omit ExecPath so Run falls back to os.Executable() (the test binary,
	// which is not under a managed path). Version pinned + CheckOnly so it
	// returns after the version compare without any download.
	var out strings.Builder
	res, err := Run("0.1.0", Options{
		Version:   "v9.9.9",
		CheckOnly: true,
		Stdout:    &out,
		Stderr:    &out,
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if res.ExecPath == "" {
		t.Fatal("expected resolved ExecPath from os.Executable()")
	}
}

func TestAssetArch(t *testing.T) {
	cases := map[string]string{"arm": "armv6", "arm64": "arm64", "amd64": "amd64"}
	for in, want := range cases {
		if got := assetArch(in); got != want {
			t.Errorf("assetArch(%q) = %q, want %q", in, got, want)
		}
	}
}
