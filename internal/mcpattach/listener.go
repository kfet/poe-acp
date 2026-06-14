package mcpattach

import (
	"encoding/json"
	"net"
	"os"
	"sync"
)

// AttachFunc performs the actual delivery for one validated request
// (upload + emit the file event on the conv's active turn). Returns an
// error to surface back to the agent's tool call.
type AttachFunc func(req SocketRequest) error

// Listener accepts attach relays from mcp-attach subprocesses over a
// unix socket and dispatches them to an AttachFunc. Token-authenticated.
type Listener struct {
	path  string
	token string
	fn    AttachFunc
	ln    net.Listener
	wg    sync.WaitGroup
}

// Listen creates the unix socket at path (replacing any stale one) and
// starts accepting. Call Close to stop and remove the socket.
func Listen(path, token string, fn AttachFunc) (*Listener, error) {
	_ = os.Remove(path) // clear stale socket from a prior run
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	mustChmod(path)
	l := &Listener{path: path, token: token, fn: fn, ln: ln}
	l.wg.Add(1)
	go l.serve()
	return l, nil
}

func (l *Listener) serve() {
	defer l.wg.Done()
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return // listener closed
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.handle(c)
		}()
	}
}

func (l *Listener) handle(c net.Conn) {
	defer c.Close()
	var req SocketRequest
	if err := json.NewDecoder(c).Decode(&req); err != nil {
		_ = json.NewEncoder(c).Encode(SocketResponse{Error: "bad request"})
		return
	}
	if req.Token != l.token {
		_ = json.NewEncoder(c).Encode(SocketResponse{Error: "unauthorized"})
		return
	}
	resp := SocketResponse{OK: true}
	if err := l.fn(req); err != nil {
		resp = SocketResponse{Error: err.Error()}
	}
	_ = json.NewEncoder(c).Encode(resp)
}

// Path returns the socket path.
func (l *Listener) Path() string { return l.path }

// Close stops accepting, waits for in-flight handlers, and removes the
// socket file.
func (l *Listener) Close() error {
	// Close errors on a listener are not actionable; best-effort.
	_ = l.ln.Close()
	l.wg.Wait()
	_ = os.Remove(l.path)
	return nil
}
