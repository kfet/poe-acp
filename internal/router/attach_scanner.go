package router

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	kitlog "github.com/kfet/acp-kit/log"
	"github.com/kfet/poe-acp/internal/poeupload"
)

// Output attachments — agent-driven via an HTML-comment directive the
// agent emits on its own line in the assistant message stream:
//
//	<!--poe-attach path="/abs/or/relative/file" name="Nice Name" inline-->
//
// The relay intercepts the directive (it never reaches the user — and
// even if interception failed, an HTML comment renders invisibly),
// uploads the file to Poe, and emits a `file` SSE event so the file
// appears as an attachment on the bot's reply. `path` is required;
// relative paths resolve against the conversation working dir. `name`
// (optional) overrides the displayed filename. The bare `inline` token
// makes Poe render the file inline — for images — by emitting an
// ![name][ref] markdown reference bound to the upload's inline_ref.
//
// Detection is line-oriented: a directive must occupy its own line.
// The scanner only holds back text once a line begins with "<!--";
// all other text streams through unbuffered, preserving token-level
// streaming smoothness.

const attachAmbig = "<!--"

var attachDirectiveRe = regexp.MustCompile(`^<!--\s*poe-attach\s+(.+?)\s*-->$`)
var attachPathRe = regexp.MustCompile(`\bpath\s*=\s*"([^"]*)"`)
var attachNameRe = regexp.MustCompile(`\bname\s*=\s*"([^"]*)"`)
var attachInlineRe = regexp.MustCompile(`(^|\s)inline(\s|$)`)

const inlineRefAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func newInlineRef() string {
	b := make([]byte, 8)
	for i := range b {
		b[i] = inlineRefAlphabet[rand.Intn(len(inlineRefAlphabet))]
	}
	return string(b)
}

// attachScanner extracts poe-attach directives from an assistant
// message stream, uploading referenced files and emitting `file`
// events via the sink. One instance per turn; all methods are called
// from the single drainChunks goroutine, so no synchronisation.
type attachScanner struct {
	up      *poeupload.Uploader
	sink    ChunkSink
	cwd     string
	line    string // current line buffered since the last newline
	emitted int    // bytes of line already forwarded to the sink
}

// newAttachScanner returns nil if uploads are not configured.
func (r *Router) newAttachScanner(sink ChunkSink, cwd string) *attachScanner {
	if r.uploader == nil {
		return nil
	}
	return &attachScanner{up: r.uploader, sink: sink, cwd: cwd}
}

// Feed processes a chunk of assistant message text.
func (s *attachScanner) Feed(text string) {
	for {
		nl := strings.IndexByte(text, '\n')
		if nl < 0 {
			s.line += text
			s.maybeEagerForward()
			return
		}
		s.line += text[:nl]
		s.completeLine()
		s.line, s.emitted = "", 0
		text = text[nl+1:]
	}
}

// maybeEagerForward streams the un-emitted tail of the current
// incomplete line immediately, unless the line could still grow into a
// directive (i.e. it begins like an HTML comment).
func (s *attachScanner) maybeEagerForward() {
	lt := strings.TrimLeft(s.line, " \t")
	ambiguous := strings.HasPrefix(attachAmbig, lt) || strings.HasPrefix(lt, attachAmbig)
	if ambiguous {
		return
	}
	if s.emitted < len(s.line) {
		_ = s.sink.Text(s.line[s.emitted:])
		s.emitted = len(s.line)
	}
}

// completeLine handles a line whose terminating newline has arrived.
func (s *attachScanner) completeLine() {
	trimmed := strings.TrimSpace(s.line)
	if s.emitted == 0 {
		if m := attachDirectiveRe.FindStringSubmatch(trimmed); m != nil {
			if s.processDirective(m[1]) {
				return // directive consumed; newline swallowed
			}
		}
	}
	_ = s.sink.Text(s.line[s.emitted:] + "\n")
}

// Flush emits any held trailing text at end of turn.
func (s *attachScanner) Flush() {
	if s.line == "" {
		return
	}
	trimmed := strings.TrimSpace(s.line)
	if s.emitted == 0 {
		if m := attachDirectiveRe.FindStringSubmatch(trimmed); m != nil {
			if s.processDirective(m[1]) {
				s.line, s.emitted = "", 0
				return
			}
		}
	}
	_ = s.sink.Text(s.line[s.emitted:])
	s.line, s.emitted = "", 0
}

// processDirective uploads the referenced file and emits a file event.
// Returns false if attrs are unusable (caller then forwards the line as
// ordinary text). Upload failures return true (directive consumed) and
// surface a short italic note instead.
func (s *attachScanner) processDirective(attrs string) bool {
	pm := attachPathRe.FindStringSubmatch(attrs)
	if pm == nil || pm[1] == "" {
		return false
	}
	path := pm[1]
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.cwd, path)
	}
	inline := attachInlineRe.MatchString(attrs)
	name := ""
	if nm := attachNameRe.FindStringSubmatch(attrs); nm != nil {
		name = nm[1]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := s.up.UploadFile(ctx, path)
	if err != nil {
		kitlog.Logf("poe-attach upload failed (%s): %v", path, err)
		_ = s.sink.Text(fmt.Sprintf("\n_(attachment failed: %s)_\n", filepath.Base(path)))
		return true
	}
	if name == "" {
		name = res.Name
	}
	ref := ""
	if inline {
		ref = newInlineRef()
	}
	if err := s.sink.File(res.URL, res.MimeType, name, ref); err != nil {
		kitlog.Logf("poe-attach file event failed (%s): %v", name, err)
		return true
	}
	if inline {
		_ = s.sink.Text(fmt.Sprintf("\n![%s][%s]\n", name, ref))
	}
	kitlog.Debugf("poe-attach delivered %s (%s) inline=%v", name, res.URL, inline)
	return true
}
