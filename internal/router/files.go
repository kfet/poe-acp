package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/mcphost"
)

// MCP files capabilities (acp-kit/command), Poe flavour:
//
//   - command.FilePoster: send_file with inline bytes. Poe has no
//     out-of-turn push, so a file can only land on the conversation's
//     in-flight turn, exactly like the `attach` tool.
//   - command.DownloadLinker: Poe attachment URLs are already public,
//     credential-free https URLs, so they are handed out as they are.
//   - command.UploadTargeter is deliberately NOT implemented: Poe offers
//     no third-party upload URL, and posting is bound to a live turn.

// maxInbound caps how many attachments a conversation remembers.
const maxInbound = 50

// recordInbound remembers every attachment in query (oldest turn first
// on the wire) newest first, deduplicated by URL.
func (r *Router) recordInbound(st *sessionState, query []Turn) {
	var out []Attachment
	seen := map[string]struct{}{}
	for i := len(query) - 1; i >= 0 && len(out) < maxInbound; i-- {
		atts := query[i].Attachments
		for j := len(atts) - 1; j >= 0 && len(out) < maxInbound; j-- {
			a := atts[j]
			if a.URL == "" {
				continue
			}
			if _, dup := seen[a.URL]; dup {
				continue
			}
			seen[a.URL] = struct{}{}
			out = append(out, a)
		}
	}
	r.mu.Lock()
	st.inbound = out
	r.mu.Unlock()
}

// inboundID is the stable id of an attachment: a hash of its URL, so
// the URL itself is never echoed as an id.
func inboundID(a Attachment) string {
	h := sha256.Sum256([]byte(a.URL))
	return hex.EncodeToString(h[:8])
}

func (r *Router) inbound(convID string) ([]Attachment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[convID]
	if !ok {
		return nil, fmt.Errorf("no session for conversation %s", convID)
	}
	return append([]Attachment(nil), st.inbound...), nil
}

// AttachmentsIn implements command.DownloadLinker. Poe reports no
// sizes, so Size is -1.
func (r *Router) AttachmentsIn(convID string) ([]command.InboundAttachment, error) {
	atts, err := r.inbound(convID)
	if err != nil {
		return nil, err
	}
	out := make([]command.InboundAttachment, 0, len(atts))
	for _, a := range atts {
		out = append(out, command.InboundAttachment{
			ID: inboundID(a), Name: a.Name, MimeType: a.ContentType, Size: -1,
		})
	}
	return out, nil
}

// DownloadLinkFor implements command.DownloadLinker: it returns the
// attachment's public Poe URL with the exact size from a HEAD request.
func (r *Router) DownloadLinkFor(convID, attachmentID string) (mcphost.DownloadTarget, error) {
	atts, err := r.inbound(convID)
	if err != nil {
		return mcphost.DownloadTarget{}, err
	}
	for _, a := range atts {
		if inboundID(a) != attachmentID {
			continue
		}
		u, perr := url.Parse(a.URL)
		if perr != nil || u.Scheme != "https" || u.User != nil {
			return mcphost.DownloadTarget{}, fmt.Errorf("attachment %s has no public https url", attachmentID)
		}
		size, serr := r.headSize(u)
		if serr != nil {
			return mcphost.DownloadTarget{}, serr
		}
		return mcphost.DownloadTarget{URL: a.URL, Name: a.Name, MimeType: a.ContentType, Size: size}, nil
	}
	return mcphost.DownloadTarget{}, fmt.Errorf("no attachment %q in this conversation", attachmentID)
}

// headSize returns the exact size of the file at u: Content-Length
// from a HEAD, or, when the server will not say, the total from a
// one-byte ranged GET. Redirects must stay on https.
func (r *Router) headSize(u *url.URL) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hc := *r.httpClient()
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) >= 10 {
			return fmt.Errorf("refusing redirect to %s", req.URL.Redacted())
		}
		return nil
	}
	do := func(method string, h http.Header) (*http.Response, error) {
		resp, err := hc.Do((&http.Request{Method: method, URL: u, Header: h}).WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("attachment size: %w", err)
		}
		resp.Body.Close()
		return resp, nil
	}
	resp, err := do(http.MethodHead, http.Header{})
	if err != nil {
		return 0, err
	}
	if resp.StatusCode == http.StatusOK && resp.ContentLength >= 0 {
		return resp.ContentLength, nil
	}
	resp, err = do(http.MethodGet, http.Header{"Range": {"bytes=0-0"}})
	if err != nil {
		return 0, err
	}
	var first, last, total int64
	if resp.StatusCode == http.StatusPartialContent {
		if _, serr := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &first, &last, &total); serr == nil {
			return total, nil
		}
	}
	return 0, fmt.Errorf("attachment size unknown (status %d)", resp.StatusCode)
}

// PostFileTo implements command.FilePoster: it uploads the bytes to
// Poe and emits them as an attachment on the conversation's in-flight
// turn. Poe cannot post outside a turn.
func (r *Router) PostFileTo(convID string, f command.OutFile) ([]string, error) {
	if r.uploader == nil {
		return nil, fmt.Errorf("attachments not enabled")
	}
	t, ok := r.active.Get(convID)
	if !ok {
		return nil, fmt.Errorf("no active turn for conversation %s: Poe can only send files during a reply", convID)
	}
	at := t.Value.(activeTurn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := r.uploader.UploadReader(ctx, f.Name, bytes.NewReader(f.Data))
	if err != nil {
		return nil, err
	}
	mt := f.MimeType
	if res.MimeType != "" {
		mt = res.MimeType
	}
	if err := at.sink.File(res.URL, mt, f.Name, ""); err != nil {
		return nil, err
	}
	if f.Caption != "" {
		_ = at.sink.Text("\n" + f.Caption + "\n")
	}
	return nil, nil
}

// CanUpload reports whether the relay can upload output files to Poe
// (an access key is configured).
func (r *Router) CanUpload() bool { return r.uploader != nil }

// ConvForDir maps a session cwd's base name (the key MCP tokens are
// minted for) back to the router key. convDirName hashes keys that are
// not filename-safe, e.g. thread_user "conv/user"; unknown names are
// returned unchanged.
func (r *Router) ConvForDir(dir string) string {
	if k, ok := r.dirs.Load(dir); ok {
		return k.(string)
	}
	return dir
}
