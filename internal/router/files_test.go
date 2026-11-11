package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/poe-acp/internal/poeupload"
)

func TestPostFileTo(t *testing.T) {
	r := mkRouterWithUploader(t, true)
	if !r.CanUpload() {
		t.Fatal("CanUpload false")
	}
	if _, err := r.PostFileTo("c", command.OutFile{Name: "a.md", Data: []byte("x")}); err == nil {
		t.Fatal("want error without active turn")
	}
	sink := &captureSink{}
	r.setActiveTurn("c", sink, stFor(r, t.TempDir()))
	if _, err := r.PostFileTo("c", command.OutFile{Name: "a.md", MimeType: "text/plain", Data: []byte("x"), Caption: "cap"}); err != nil {
		t.Fatal(err)
	}
	if len(sink.files) != 1 || sink.files[0].url != "https://poe/x" || sink.files[0].name != "a.md" || sink.files[0].ct != "text/markdown" {
		t.Fatalf("files = %+v", sink.files)
	}
	if !strings.Contains(sink.text.String(), "cap") {
		t.Fatalf("caption missing: %q", sink.text.String())
	}
}

func TestPostFileTo_Errors(t *testing.T) {
	if _, err := (&Router{}).PostFileTo("c", command.OutFile{}); err == nil || (&Router{}).CanUpload() {
		t.Fatal("want disabled")
	}
	r := mkRouterWithUploader(t, false)
	r.setActiveTurn("c", &captureSink{}, stFor(r, t.TempDir()))
	if _, err := r.PostFileTo("c", command.OutFile{Name: "a", Data: []byte("x")}); err == nil {
		t.Fatal("want upload error")
	}
	r = mkRouterWithUploader(t, true)
	r.setActiveTurn("c", errFileSink2{&captureSink{}}, stFor(r, t.TempDir()))
	if _, err := r.PostFileTo("c", command.OutFile{Name: "a", Data: []byte("x")}); err == nil {
		t.Fatal("want sink error")
	}
}

// keeps the declared type when Poe reports none.
func TestPostFileTo_DeclaredMime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"attachment_url":"https://poe/y"}`))
	}))
	defer srv.Close()
	r := mkRouterWithUploader(t, true)
	r.uploader = poeupload.New("k", srv.URL, srv.Client())
	sink := &captureSink{}
	r.setActiveTurn("c", sink, stFor(r, t.TempDir()))
	if _, err := r.PostFileTo("c", command.OutFile{Name: "a", MimeType: "image/png", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if sink.files[0].ct != "image/png" {
		t.Fatalf("mime = %q", sink.files[0].ct)
	}
}

func TestInboundAttachments(t *testing.T) {
	var auth []string
	mode := "head"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		auth = append(auth, req.Header.Get("Authorization"))
		if req.URL.Path == "/redir" {
			http.Redirect(w, req, "/a", http.StatusFound)
			return
		}
		if req.Method == http.MethodHead {
			if mode == "head" {
				w.Header().Set("Content-Length", "5")
				return
			}
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if req.Header.Get("Range") != "bytes=0-0" {
			t.Errorf("range = %q", req.Header.Get("Range"))
		}
		switch mode {
		case "range":
			w.Header().Set("Content-Range", "bytes 0-0/7")
			w.WriteHeader(http.StatusPartialContent)
		case "badrange":
			w.Header().Set("Content-Range", "junk")
			w.WriteHeader(http.StatusPartialContent)
		case "hangup":
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	r := mkRouterWithUploader(t, true)
	r.cfg.HTTPClient = srv.Client()

	if _, err := r.AttachmentsIn("c"); err == nil {
		t.Fatal("want no-session error")
	}
	if _, err := r.DownloadLinkFor("c", "x"); err == nil {
		t.Fatal("want no-session error")
	}
	st := stFor(r, t.TempDir())
	r.sessions["c"] = st
	a := Attachment{URL: srv.URL + "/a", Name: "a.txt", ContentType: "text/plain"}
	b := Attachment{URL: srv.URL + "/redir", Name: "b.png", ContentType: "image/png"}
	bad := Attachment{URL: "http://insecure/c", Name: "c"}
	r.recordInbound(st, []Turn{
		{Attachments: []Attachment{a, {URL: ""}}},
		{Attachments: []Attachment{bad, b, a}},
	})
	got, err := r.AttachmentsIn("c")
	if err != nil || len(got) != 3 || got[0].Name != "a.txt" || got[1].Name != "b.png" || got[2].Name != "c" || got[0].Size != -1 {
		t.Fatalf("AttachmentsIn = %+v %v", got, err)
	}
	for _, g := range got {
		if strings.Contains(g.ID, "http") {
			t.Fatalf("id leaks url: %q", g.ID)
		}
	}
	dt, err := r.DownloadLinkFor("c", got[1].ID)
	if err != nil || dt.URL != b.URL || dt.Size != 5 || dt.MimeType != "image/png" || dt.Name != "b.png" {
		t.Fatalf("DownloadLinkFor = %+v %v", dt, err)
	}
	for _, h := range auth {
		if h != "" {
			t.Fatal("credential sent")
		}
	}
	if _, err := r.DownloadLinkFor("c", got[2].ID); err == nil {
		t.Fatal("want non-https refusal")
	}
	if _, err := r.DownloadLinkFor("c", "nope"); err == nil {
		t.Fatal("want unknown id error")
	}
	mode = "range"
	if dt, err := r.DownloadLinkFor("c", got[0].ID); err != nil || dt.Size != 7 {
		t.Fatalf("range fallback = %+v %v", dt, err)
	}
	for _, m := range []string{"badrange", "plain", "hangup"} {
		mode = m
		if _, err := r.DownloadLinkFor("c", got[0].ID); err == nil {
			t.Fatalf("%s: want error", m)
		}
	}
	srv.Close()
	if _, err := r.DownloadLinkFor("c", got[0].ID); err == nil {
		t.Fatal("want transport error")
	}
}

func TestHeadSize_InsecureRedirect(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "http://example.invalid/x", http.StatusFound)
	}))
	defer srv.Close()
	r := mkRouterWithUploader(t, true)
	r.cfg.HTTPClient = srv.Client()
	u, _ := url.Parse(srv.URL)
	if _, err := r.headSize(u); err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("err = %v", err)
	}
}

func TestRecordInbound_Cap(t *testing.T) {
	r := mkRouterWithUploader(t, true)
	st := stFor(r, t.TempDir())
	var atts []Attachment
	for i := 0; i < maxInbound+5; i++ {
		atts = append(atts, Attachment{URL: "https://x/" + strings.Repeat("a", i+1)})
	}
	r.recordInbound(st, []Turn{{Attachments: atts}, {Attachments: atts[:1]}})
	if len(st.inbound) != maxInbound {
		t.Fatalf("len = %d", len(st.inbound))
	}
}

// MCP tokens are minted from the session cwd's base name; a key that
// convDirName hashes (thread_user "conv/user") must map back to the
// router key, or every MCP tool misses the conversation.
func TestConvForDir(t *testing.T) {
	r := mkRouterWithUploader(t, true)
	key := "c1/u1"
	st, _, err := r.getOrCreate(context.Background(), key, "", nil, "")
	if err != nil || st == nil {
		t.Fatalf("getOrCreate: %v", err)
	}
	if got := r.ConvForDir(filepath.Base(st.cwd)); got != key {
		t.Fatalf("ConvForDir = %q, want %q", got, key)
	}
	if got := r.ConvForDir("plain"); got != "plain" {
		t.Fatalf("ConvForDir(plain) = %q", got)
	}
}
