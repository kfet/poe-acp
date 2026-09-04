package router

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/remotefs"
	"github.com/kfet/poe-acp/internal/poeupload"
)

// errCapturingSink records Error events so a test can assert a failure
// was NOT reported through a frame Poe does not render.
type errCapturingSink struct {
	scanSink
	mu   sync.Mutex
	errs []string
}

func (s *errCapturingSink) Error(msg, kind string) error {
	s.mu.Lock()
	s.errs = append(s.errs, msg+"|"+kind)
	s.mu.Unlock()
	return nil
}

func (s *errCapturingSink) errorEvents() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.errs...)
}

// fakeProvisioner records what the router asked to be created or copied
// onto the agent's host, and can fail either operation.
type fakeProvisioner struct {
	mu       sync.Mutex
	mkdirs   []string
	pushes   [][2]string
	fetches  []string
	mkdirErr error
	pushErr  error
	fetchErr error
}

func (f *fakeProvisioner) Mkdir(_ context.Context, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mkdirs = append(f.mkdirs, dir)
	return f.mkdirErr
}

func (f *fakeProvisioner) Push(_ context.Context, src, dstParent string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes = append(f.pushes, [2]string{src, dstParent})
	return f.pushErr
}

func (f *fakeProvisioner) Fetch(_ context.Context, remotePath, dstDir string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches = append(f.fetches, remotePath)
	if f.fetchErr != nil {
		return "", f.fetchErr
	}
	local := filepath.Join(dstDir, filepath.Base(remotePath))
	if err := os.WriteFile(local, []byte("from the agent"), 0o644); err != nil {
		return "", err
	}
	return local, nil
}

func (f *fakeProvisioner) snapshot() ([]string, [][2]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.mkdirs...), append([][2]string(nil), f.pushes...)
}

func endTurnAgent() *fakeAgent {
	return newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
}

// TestProvisionerDefaultsToLocal: an unset Provisioner must behave
// exactly like today's local-agent relay.
func TestProvisionerDefaultsToLocal(t *testing.T) {
	r, err := New(Config{Agent: endTurnAgent(), StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.provisioner(); got != remotefs.Local {
		t.Fatalf("provisioner() = %#v, want remotefs.Local", got)
	}
	p := &fakeProvisioner{}
	r.cfg.Provisioner = p
	if r.provisioner() != p {
		t.Fatalf("configured provisioner not used")
	}
}

// TestGetOrCreateProvisionsConvDir: the cwd is created on the agent's
// host BEFORE any session is acquired — session/load and session/resume
// carry the cwd too, so provisioning only in front of session/new would
// leave the resume path broken.
func TestGetOrCreateProvisionsConvDir(t *testing.T) {
	p := &fakeProvisioner{}
	r, err := New(Config{
		Agent: endTurnAgent(), StateDir: t.TempDir(),
		SessionTTL: time.Hour, Provisioner: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := r.getOrCreate(t.Context(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, "")
	if err != nil {
		t.Fatalf("getOrCreate: %v", err)
	}
	mkdirs, _ := p.snapshot()
	if len(mkdirs) != 1 || mkdirs[0] != st.cwd {
		t.Fatalf("mkdirs = %v, want [%s]", mkdirs, st.cwd)
	}
}

// TestGetOrCreateFailsLoudlyWhenProvisionFails: the whole point. A cwd
// the agent cannot see does not fail on the agent side — it silently
// becomes $HOME — so the relay must refuse to create the session at all.
func TestGetOrCreateFailsLoudlyWhenProvisionFails(t *testing.T) {
	fa := endTurnAgent()
	p := &fakeProvisioner{mkdirErr: errors.New("Permission denied (publickey).")}
	r, err := New(Config{
		Agent: fa, StateDir: t.TempDir(),
		SessionTTL: time.Hour, Provisioner: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, gerr := r.getOrCreate(t.Context(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, "")
	if gerr == nil {
		t.Fatal("want error when the agent host cannot be provisioned")
	}
	for _, want := range []string{"provision conv dir on the agent's host", "Permission denied (publickey)."} {
		if !strings.Contains(gerr.Error(), want) {
			t.Errorf("error %q missing %q", gerr, want)
		}
	}
	// No session may be installed, and the agent must not have been
	// asked for one.
	r.mu.Lock()
	n := len(r.sessions)
	r.mu.Unlock()
	if n != 0 {
		t.Errorf("sessions = %d, want 0", n)
	}
	if got := atomic.LoadInt32(&fa.newSessCalls); got != 0 {
		t.Errorf("NewSession calls = %d, want 0", got)
	}
}

// attachServer serves one small text file for attachment downloads.
func attachServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "payload")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func routerWithProvisioner(t *testing.T, p remotefs.Provisioner) *Router {
	t.Helper()
	r, err := New(Config{
		Agent: endTurnAgent(), StateDir: t.TempDir(),
		SessionTTL: time.Hour, Provisioner: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBuildPromptBlocksPushesStagedAttachments(t *testing.T) {
	srv := attachServer(t)
	p := &fakeProvisioner{}
	r := routerWithProvisioner(t, p)
	cwd := t.TempDir()

	turn := Turn{
		Role: "user", Content: "look", MessageID: "m1",
		Attachments: []Attachment{
			{Name: "a.txt", URL: srv.URL + "/a", ContentType: "text/plain"},
			{Name: "b.txt", URL: srv.URL + "/b", ContentType: "text/plain"},
		},
	}
	blocks := r.buildPromptBlocks(t.Context(), cwd, []Turn{turn}, turn, "look", false, false)

	_, pushes := p.snapshot()
	if len(pushes) != 1 {
		t.Fatalf("pushes = %v, want exactly one per message", pushes)
	}
	wantSrc := cwd + "/" + attachmentDirName + "/m1"
	wantDst := cwd + "/" + attachmentDirName
	if pushes[0] != [2]string{wantSrc, wantDst} {
		t.Errorf("push = %v, want [%s %s]", pushes[0], wantSrc, wantDst)
	}
	if n := countFileLinks(blocks); n != 2 {
		t.Errorf("file:// links = %d, want 2", n)
	}
}

func TestBuildPromptBlocksNoPushWithoutStagedFiles(t *testing.T) {
	p := &fakeProvisioner{}
	r := routerWithProvisioner(t, p)

	// An attachment Poe already parsed for us never touches disk.
	r.cfg.Agent = endTurnAgent()
	turn := Turn{
		Role: "user", Content: "look", MessageID: "m1",
		Attachments: []Attachment{
			{Name: "a.txt", URL: "https://poe/a", ContentType: "text/plain", ParsedContent: "hello"},
			{URL: ""}, // skipped entirely
		},
	}
	r.buildPromptBlocks(t.Context(), t.TempDir(), []Turn{turn}, turn, "look", false, false)
	if _, pushes := p.snapshot(); len(pushes) != 0 {
		t.Fatalf("pushes = %v, want none", pushes)
	}
}

// TestBuildPromptBlocksPushFailureDegradesToHTTPS: a prompt is never
// failed for attachment IO, but it must not hand the agent a file://
// path that does not exist on its side either.
func TestBuildPromptBlocksPushFailureDegradesToHTTPS(t *testing.T) {
	srv := attachServer(t)
	p := &fakeProvisioner{pushErr: errors.New("no route to host")}
	r := routerWithProvisioner(t, p)

	turn := Turn{
		Role: "user", Content: "look", MessageID: "m1",
		Attachments: []Attachment{
			{Name: "a.txt", URL: srv.URL + "/a", ContentType: "text/plain"},
		},
	}
	blocks := r.buildPromptBlocks(t.Context(), t.TempDir(), []Turn{turn}, turn, "look", false, false)
	if n := countFileLinks(blocks); n != 0 {
		t.Errorf("file:// links = %d, want 0 after a failed push", n)
	}
	if !hasLinkPrefix(blocks, srv.URL) {
		t.Errorf("blocks %v do not carry the https fallback link", blocks)
	}
}

// TestBuildPromptBlocksPushFailureKeepsInlineImage: the image bytes ride
// in the prompt itself, so a filesystem failure must not drop them.
func TestBuildPromptBlocksPushFailureKeepsInlineImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
	}))
	defer srv.Close()
	p := &fakeProvisioner{pushErr: errors.New("no route to host")}
	r := routerWithProvisioner(t, p)

	turn := Turn{
		Role: "user", Content: "look", MessageID: "m1",
		Attachments: []Attachment{
			{Name: "a.png", URL: srv.URL + "/a.png", ContentType: "image/png"},
		},
	}
	blocks := r.buildPromptBlocks(t.Context(), t.TempDir(), []Turn{turn}, turn, "look", false, false)
	var images int
	for _, b := range blocks {
		if b.Image != nil {
			images++
		}
	}
	if images != 1 {
		t.Errorf("inline images = %d, want 1", images)
	}
	if n := countFileLinks(blocks); n != 0 {
		t.Errorf("file:// links = %d, want 0", n)
	}
}

func countFileLinks(blocks []acp.ContentBlock) int {
	n := 0
	for _, b := range blocks {
		if b.ResourceLink != nil && strings.HasPrefix(b.ResourceLink.Uri, "file://") {
			n++
		}
	}
	return n
}

func hasLinkPrefix(blocks []acp.ContentBlock, prefix string) bool {
	for _, b := range blocks {
		if b.ResourceLink != nil && strings.HasPrefix(b.ResourceLink.Uri, prefix) {
			return true
		}
	}
	return false
}

func TestConvDirName(t *testing.T) {
	// Ordinary Poe ids must pass through unchanged, or every existing
	// conversation directory on a deployed host stops resolving.
	for _, id := range []string{
		"c-abc123", "0197f0d0-1a2b-4c3d-8e9f-0011223344ff", "a.b_c-D9",
	} {
		if got := convDirName(id); got != id {
			t.Errorf("convDirName(%q) = %q, want it unchanged", id, got)
		}
	}
	// Anything that could mean something to a path must not.
	for _, id := range []string{
		"", ".", "..", "../../etc", "a/b", ".hidden", "a b", "a\x00b", "ünïcode",
	} {
		got := convDirName(id)
		if got == id || strings.ContainsAny(got, `/\`) || strings.HasPrefix(got, ".") {
			t.Errorf("convDirName(%q) = %q, want a hashed component", id, got)
		}
	}
	// Stable: the same id always maps to the same directory.
	if convDirName("a/b") != convDirName("a/b") {
		t.Error("convDirName is not stable")
	}
	if convDirName("a/b") == convDirName("a/c") {
		t.Error("convDirName collides on distinct ids")
	}
}

// TestProvisionFailureIsVisibleToTheUser: Poe does not render the text
// of an `error` event, and an invisible failure is the whole bug, so
// this one must arrive as assistant text.
func TestProvisionFailureIsVisibleToTheUser(t *testing.T) {
	r := routerWithProvisioner(t, &fakeProvisioner{
		mkdirErr: errors.New("Could not resolve hostname miki"),
	})
	sink := &errCapturingSink{}
	err := r.Prompt(t.Context(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, sink)
	if err == nil {
		t.Fatal("want error")
	}
	joined := sink.joined()
	if evs := sink.errorEvents(); len(evs) != 0 {
		t.Errorf("want assistant text, not an error event: %v", evs)
	}
	if !strings.Contains(joined, provisionFailedMsg) ||
		!strings.Contains(joined, "Could not resolve hostname miki") {
		t.Errorf("user saw %q, want the provisioning failure spelled out", joined)
	}

}

func TestUploadAgentFileFetchesFromTheAgentHost(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got, _ = io.ReadAll(req.Body)
		_, _ = io.WriteString(w, `{"attachment_url":"https://poe/x","mime_type":"text/plain"}`)
	}))
	defer srv.Close()

	p := &fakeProvisioner{}
	r := routerWithProvisioner(t, p)
	r.uploader = poeupload.New("k", srv.URL, srv.Client())

	if _, err := r.uploadAgentFile(t.Context(), "/on/agent/out.txt"); err != nil {
		t.Fatalf("uploadAgentFile: %v", err)
	}
	p.mu.Lock()
	fetches := append([]string(nil), p.fetches...)
	p.mu.Unlock()
	if len(fetches) != 1 || fetches[0] != "/on/agent/out.txt" {
		t.Fatalf("fetches = %v, want the agent-side path", fetches)
	}
	if !strings.Contains(string(got), "from the agent") {
		t.Errorf("uploaded body %q does not carry the fetched bytes", got)
	}
}

func TestUploadAgentFileFetchFails(t *testing.T) {
	r := routerWithProvisioner(t, &fakeProvisioner{fetchErr: errors.New("no route to host")})
	r.uploader = poeupload.New("k", "http://127.0.0.1:1", nil)
	if _, err := r.uploadAgentFile(t.Context(), "/on/agent/out.txt"); err == nil ||
		!strings.Contains(err.Error(), "no route to host") {
		t.Fatalf("err = %v", err)
	}
}

func TestUploadAgentFileScratchDirFails(t *testing.T) {
	defer swap(&osMkdirTemp, func(string, string) (string, error) {
		return "", errors.New("mkdtemp-fail")
	})()
	r := routerWithProvisioner(t, &fakeProvisioner{})
	if _, err := r.uploadAgentFile(t.Context(), "/x"); err == nil ||
		!strings.Contains(err.Error(), "mkdtemp-fail") {
		t.Fatalf("err = %v", err)
	}
}

func TestProvisionErrorUnwraps(t *testing.T) {
	base := errors.New("boom")
	err := error(provisionError{base})
	if !errors.Is(err, base) {
		t.Errorf("provisionError does not unwrap to its cause")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("Error() = %q", err.Error())
	}
}
