package poeupload

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadFile_Multipart(t *testing.T) {
	var gotAuth, gotField, gotFilename string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		p, err := mr.NextPart()
		if err != nil {
			t.Errorf("next part: %v", err)
		}
		gotField = p.FormName()
		gotFilename = p.FileName()
		b, _ := io.ReadAll(p)
		if string(b) != "hello world" {
			t.Errorf("body = %q", b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"attachment_url":"https://poe/att/1","mime_type":"text/plain"}`)
	}))
	defer srv.Close()

	u := New("secret-key", srv.URL, srv.Client())
	res, err := u.UploadReader(context.Background(), "doc.md", strings.NewReader("hello world"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if gotAuth != "secret-key" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotField != "file" {
		t.Errorf("field = %q", gotField)
	}
	if gotFilename != "doc.md" {
		t.Errorf("filename = %q", gotFilename)
	}
	if res.URL != "https://poe/att/1" || res.MimeType != "text/plain" || res.Name != "doc.md" {
		t.Errorf("result = %+v", res)
	}
}

func TestUploadReader_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "too big")
	}))
	defer srv.Close()
	u := New("k", srv.URL, srv.Client())
	_, err := u.UploadReader(context.Background(), "f", strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("want status 400 err, got %v", err)
	}
}

func TestUploadReader_MissingURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"mime_type":"text/plain"}`)
	}))
	defer srv.Close()
	u := New("k", srv.URL, srv.Client())
	_, err := u.UploadReader(context.Background(), "f", strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "attachment_url") {
		t.Fatalf("want missing attachment_url err, got %v", err)
	}
}

func TestUploadReader_Validation(t *testing.T) {
	if _, err := New("", "", nil).UploadReader(context.Background(), "f", strings.NewReader("x")); err == nil {
		t.Error("want empty-key error")
	}
	if _, err := New("k", "", nil).UploadReader(context.Background(), "", strings.NewReader("x")); err == nil {
		t.Error("want empty-filename error")
	}
}

func TestUploadFile_OpenError(t *testing.T) {
	u := New("k", "", nil)
	if _, err := u.UploadFile(context.Background(), "/no/such/file/xyz"); err == nil {
		t.Error("want open error")
	}
}

func TestDefaultEndpoint(t *testing.T) {
	if !strings.Contains(New("k", "", nil).endpoint, "file_upload_3RD_PARTY_POST") {
		t.Error("default endpoint wrong")
	}
}
