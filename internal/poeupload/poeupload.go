// Package poeupload implements the Poe server-bot output-attachment
// upload, used to attach files to the bot's own reply in a Poe
// conversation. It is a from-scratch Go port of fastapi-poe's
// upload_file (the POST to file_upload_3RD_PARTY_POST). The returned
// URL is then advertised to the client via an SSE `file` event (see
// poeproto.SSEWriter.File).
package poeupload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
)

// DefaultEndpoint is Poe's third-party file-upload endpoint. The bot
// authenticates with its access key in the Authorization header
// (matching fastapi-poe, which sends the access_key as a bare token).
const DefaultEndpoint = "https://www.quora.com/poe_api/file_upload_3RD_PARTY_POST"

// Result is the parsed upload response.
type Result struct {
	URL      string // attachment_url: the download URL Poe serves
	MimeType string // mime_type Poe assigned
	Name     string // file basename advertised to the client
}

// Uploader uploads local files to Poe. The zero value is not usable;
// use New.
type Uploader struct {
	endpoint  string
	accessKey string
	client    *http.Client
}

// New returns an Uploader. A nil client uses http.DefaultClient. An
// empty endpoint uses DefaultEndpoint.
func New(accessKey, endpoint string, client *http.Client) *Uploader {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Uploader{endpoint: endpoint, accessKey: accessKey, client: client}
}

// UploadFile reads path from disk and uploads it as a multipart `file`
// field (filename = base of path). It returns the Poe-served URL and
// MIME type.
func (u *Uploader) UploadFile(ctx context.Context, path string) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, fmt.Errorf("poeupload: open %s: %w", path, err)
	}
	defer f.Close()
	return u.UploadReader(ctx, filepath.Base(path), f)
}

// UploadReader uploads the bytes from r as a multipart `file` field
// with the given filename.
func (u *Uploader) UploadReader(ctx context.Context, filename string, r io.Reader) (Result, error) {
	if u.accessKey == "" {
		return Result{}, fmt.Errorf("poeupload: empty access key")
	}
	if filename == "" {
		return Result{}, fmt.Errorf("poeupload: empty filename")
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	mustNil(err)
	if _, err := io.Copy(fw, r); err != nil {
		return Result{}, fmt.Errorf("poeupload: copy: %w", err)
	}
	mustNil(mw.Close())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.endpoint, &body)
	if err != nil {
		return Result{}, fmt.Errorf("poeupload: new request: %w", err)
	}
	req.Header.Set("Authorization", u.accessKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := u.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("poeupload: do: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("poeupload: status %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed struct {
		AttachmentURL string `json:"attachment_url"`
		MimeType      string `json:"mime_type"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Result{}, fmt.Errorf("poeupload: decode response: %w", err)
	}
	if parsed.AttachmentURL == "" {
		return Result{}, fmt.Errorf("poeupload: response missing attachment_url: %s", string(respBody))
	}
	return Result{URL: parsed.AttachmentURL, MimeType: parsed.MimeType, Name: filename}, nil
}
