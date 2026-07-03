package mcpattach

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"sync"
)

// Listener accepts connections from mcp-serve redirector subprocesses
// over a unix socket. Each connection opens with a newline-terminated
// preamble line {"token":"<tok>"}; the token is resolved to a
// conversation id (server-side, unspoofable) and the MCP loop is then
// run over the same connection.
type Listener struct {
	path    string
	resolve func(token string) (conv string, ok bool)
	attach  AttachFunc
	suggest SuggestFunc
	ln      net.Listener
	wg      sync.WaitGroup
}

// Listen creates the unix socket at path (replacing any stale one),
// tightens it to 0600, and starts accepting. resolve maps a preamble
// token to its conversation; attach and suggest perform the actual tool
// actions. Call Close to stop and remove the socket.
func Listen(path string, resolve func(token string) (conv string, ok bool), attach AttachFunc, suggest SuggestFunc) (*Listener, error) {
	_ = os.Remove(path) // clear stale socket from a prior run
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	mustChmod(path)
	l := &Listener{path: path, resolve: resolve, attach: attach, suggest: suggest, ln: ln}
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

// handle reads the preamble, authenticates the token, then runs the MCP
// loop over the SAME buffered reader so bytes the client pipelined after
// the preamble are not lost. Any auth failure simply closes the conn.
func (l *Listener) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReaderSize(c, 1<<20)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return // client hung up before sending a preamble
	}
	var pre struct {
		Token string `json:"token"`
	}
	if jerr := json.Unmarshal(line, &pre); jerr != nil {
		return // malformed preamble
	}
	conv, ok := l.resolve(pre.Token)
	if !ok {
		return // unknown token
	}
	h := Handlers{
		Attach: func(path, name string, inline bool) error {
			return l.attach(conv, path, name, inline)
		},
		Suggest: func(replies []string) error {
			return l.suggest(conv, replies)
		},
	}
	_ = RunMCP(br, c, h)
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
