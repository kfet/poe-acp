package mcpattach

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestExchange_Success(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		var req SocketRequest
		_ = json.NewDecoder(b).Decode(&req)
		_ = json.NewEncoder(b).Encode(SocketResponse{OK: true})
		b.Close()
	}()
	if err := exchange(a, SocketRequest{Path: "/x"}); err != nil {
		t.Fatalf("exchange: %v", err)
	}
}

func TestExchange_NotOK(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		var req SocketRequest
		_ = json.NewDecoder(b).Decode(&req)
		_ = json.NewEncoder(b).Encode(SocketResponse{Error: "denied"})
		b.Close()
	}()
	if err := exchange(a, SocketRequest{Path: "/x"}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("want denied error, got %v", err)
	}
}

func TestExchange_SendError(t *testing.T) {
	a, _ := net.Pipe()
	a.Close() // write fails
	if err := exchange(a, SocketRequest{Path: "/x"}); err == nil || !strings.Contains(err.Error(), "send") {
		t.Fatalf("want send error, got %v", err)
	}
}

func TestExchange_RecvError(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		var req SocketRequest
		_ = json.NewDecoder(b).Decode(&req)
		b.Close() // close without responding
	}()
	if err := exchange(a, SocketRequest{Path: "/x"}); err == nil || !strings.Contains(err.Error(), "recv") {
		t.Fatalf("want recv error, got %v", err)
	}
}

// failWriter errors on every Write to exercise the encode-error return.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write boom") }

func TestRunStdioServer_WriteError(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	if err := RunStdioServer(in, failWriter{}, "/no", "c", "t"); err == nil {
		t.Fatal("want write error propagated")
	}
}
