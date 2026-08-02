//go:build unix

package httpsrv_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/poe-acp/internal/httpsrv"
	"github.com/kfet/poe-acp/internal/router"
	"github.com/kfet/poe-acp/internal/supervisor"
)

// wedgedAgent models the failure mode behind the 2026-07-26 incident: a
// turn whose Prompt never returns, so the SSE handler never returns, so
// http.Server.Shutdown never returns. Its idle clock is deliberately
// irrelevant here — the test disables the idle-write backstop (1h) to
// isolate the OUTER net.
type wedgedAgent struct {
	entered chan struct{}
	release chan struct{}
	sinks   map[acp.SessionId]client.SessionUpdateSink
}

func (a *wedgedAgent) Caps() client.Caps { return client.Caps{} }
func (a *wedgedAgent) ListSessions(context.Context, string) ([]client.SessionInfo, error) {
	return nil, nil
}
func (a *wedgedAgent) ResumeSession(context.Context, string, acp.SessionId, client.SessionUpdateSink) error {
	return nil
}
func (a *wedgedAgent) NewSession(_ context.Context, _ string, sink client.SessionUpdateSink, _ []acp.ContentBlock) (acp.SessionId, error) {
	id := acp.SessionId("s-wedge")
	a.sinks[id] = sink
	return id, nil
}
func (a *wedgedAgent) Prompt(context.Context, acp.SessionId, []acp.ContentBlock) (acp.StopReason, error) {
	close(a.entered)
	<-a.release // never returns on its own
	return acp.StopReasonEndTurn, nil
}
func (a *wedgedAgent) Cancel(context.Context, acp.SessionId) error         { return nil }
func (a *wedgedAgent) ReleaseSession(context.Context, acp.SessionId) error { return nil }
func (a *wedgedAgent) SetModel(context.Context, acp.SessionId, string) error {
	return nil
}
func (a *wedgedAgent) SetConfigOption(context.Context, acp.SessionId, string, string) error {
	return nil
}
func (a *wedgedAgent) Models() ([]client.ModelInfo, string)    { return nil, "" }
func (a *wedgedAgent) AvailableCommands() []client.CommandInfo { return nil }

// TestWorkerDrain_BoundedByDeadlineWithWedgedStream is the fail-first
// regression for the stale-worker leak. A real HTTP server serves a real
// Poe query whose turn is wedged; the worker then gets its SIGTERM
// equivalent. On the old code the drain was
// `srv.Shutdown(context.Background())` — unbounded — so the worker never
// exited and every reload leaked a generation until systemd SIGKILLed the
// control group. With the bound, the drain force-cuts at the deadline,
// names the abandoned stream, and the worker exits.
func TestWorkerDrain_BoundedByDeadlineWithWedgedStream(t *testing.T) {
	agent := &wedgedAgent{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		sinks:   make(map[acp.SessionId]client.SessionUpdateSink),
	}
	defer close(agent.release)

	rtr, err := router.New(router.Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := httpsrv.New(httpsrv.Config{
		Router:            rtr,
		HeartbeatInterval: 0,
		// Disable the inner net: this test is about the outer one.
		IdleWriteTimeout: time.Hour,
		SSEWriteTimeout:  time.Hour,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()

	body, err := json.Marshal(map[string]any{
		"type": "query", "conversation_id": "c-wedge", "user_id": "u-1", "message_id": "m-1",
		"query": []map[string]any{{"role": "user", "content": "hi", "message_id": "m-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		resp, err := http.Post("http://"+ln.Addr().String()+"/poe", "application/json", bytes.NewReader(body))
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-agent.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("turn never reached the agent")
	}

	var abandoned []httpsrv.StreamInfo
	res := make(chan supervisor.DrainResult, 1)
	go func() {
		got, _ := supervisor.DrainServer(srv, 200*time.Millisecond, func() {
			abandoned = h.InFlight()
		})
		res <- got
	}()

	select {
	case got := <-res:
		if got != supervisor.DrainForced {
			t.Fatalf("got %v want DrainForced", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("worker drain never returned with a wedged stream — the drain is unbounded")
	}

	if len(abandoned) != 1 {
		t.Fatalf("want exactly one abandoned stream logged, got %+v", abandoned)
	}
	if abandoned[0].ConvID != "c-wedge" || abandoned[0].UserID != "u-1" || abandoned[0].MessageID != "m-1" {
		t.Fatalf("abandoned stream not identifiable: %+v", abandoned[0])
	}
	if abandoned[0].Age <= 0 {
		t.Fatalf("abandoned stream age must be positive, got %v", abandoned[0].Age)
	}
	<-reqDone
}

// TestWorkerDrain_SealsInFlightStreamsGracefully is the fail-first
// regression for the truncated-SSE incident: on a force-cut drain the
// old code only LOGGED the abandoned streams and then let srv.Close()
// cut the connection mid-stream, so Poe rendered a red transport error
// ("peer disconnected before response") with retry left to its
// discretion. The stream must instead be SEALED at the protocol level
// first — an `error` event carrying allow_retry, then the terminal
// `done` — and `done` must appear exactly once even though the
// handler's own finalization backstop also runs.
func TestWorkerDrain_SealsInFlightStreamsGracefully(t *testing.T) {
	agent := &wedgedAgent{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		sinks:   make(map[acp.SessionId]client.SessionUpdateSink),
	}
	defer close(agent.release)

	rtr, err := router.New(router.Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := httpsrv.New(httpsrv.Config{
		Router:            rtr,
		HeartbeatInterval: 0,
		IdleWriteTimeout:  time.Hour,
		SSEWriteTimeout:   time.Hour,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()

	body, err := json.Marshal(map[string]any{
		"type": "query", "conversation_id": "c-seal", "user_id": "u-1", "message_id": "m-1",
		"query": []map[string]any{{"role": "user", "content": "hi", "message_id": "m-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wire := make(chan []byte, 1)
	go func() {
		resp, err := http.Post("http://"+ln.Addr().String()+"/poe", "application/json", bytes.NewReader(body))
		if err != nil {
			wire <- nil
			return
		}
		defer func() { _ = resp.Body.Close() }()
		// The force-close cuts the connection: ReadAll returns what
		// arrived plus an error, and what arrived is the whole point.
		b, _ := io.ReadAll(resp.Body)
		wire <- b
	}()

	select {
	case <-agent.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("turn never reached the agent")
	}

	sealed := -1
	res := make(chan supervisor.DrainResult, 1)
	go func() {
		got, _ := supervisor.DrainServer(srv, 200*time.Millisecond, func() {
			sealed = h.SealInFlight(httpsrv.AbandonMessage, httpsrv.DefaultSealTimeout)
		})
		res <- got
	}()

	select {
	case got := <-res:
		if got != supervisor.DrainForced {
			t.Fatalf("got %v want DrainForced", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("worker drain never returned")
	}
	if sealed != 1 {
		t.Fatalf("want exactly one sealed stream, got %d", sealed)
	}

	var got []byte
	select {
	case got = <-wire:
	case <-time.After(10 * time.Second):
		t.Fatal("client never finished reading the stream")
	}
	s := string(got)
	if !strings.Contains(s, "event: error") {
		t.Fatalf("no terminal error event on the wire:\n%s", s)
	}
	if !strings.Contains(s, `"allow_retry":true`) {
		t.Fatalf("error event must permit retry:\n%s", s)
	}
	if !strings.Contains(s, httpsrv.AbandonMessage) {
		t.Fatalf("error event must carry a legible message:\n%s", s)
	}
	if n := strings.Count(s, "event: done"); n != 1 {
		t.Fatalf("want exactly one done event, got %d:\n%s", n, s)
	}
	if strings.Index(s, "event: error") > strings.Index(s, "event: done") {
		t.Fatalf("error must precede done:\n%s", s)
	}
}
