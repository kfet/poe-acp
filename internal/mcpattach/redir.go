package mcpattach

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
)

// RunFromEnv is the entry point for the `mcp-attach` subcommand: a dumb
// redirector. It reads the socket path + token from the env the parent
// set, dials the socket, writes the preamble, and pipes stdin↔socket.
func RunFromEnv() error {
	socket := os.Getenv(EnvSocket)
	token := os.Getenv(EnvToken)
	if socket == "" {
		return fmt.Errorf("mcp-attach: %s not set", EnvSocket)
	}
	return redirect(socket, token, os.Stdin, os.Stdout)
}

// redirect dials the unix socket and pumps the streams.
func redirect(socket, token string, in io.Reader, out io.Writer) error {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return fmt.Errorf("dial %s: %w", socket, err)
	}
	defer conn.Close()
	return pump(conn, token, in, out)
}

// pump writes the {"token":...} preamble line, then io.Copy's in→conn
// and conn→out concurrently. It returns when the conn→out direction
// completes (the server closed its side), having half-closed the write
// side once stdin hits EOF so the server's MCP loop terminates. This
// relies on the invariant that the server always closes the connection
// after it sees stdin EOF (RunMCP returns on read EOF and the listener
// defers conn.Close); otherwise the <-done wait would block forever.
func pump(conn net.Conn, token string, in io.Reader, out io.Writer) error {
	pre, _ := json.Marshal(struct {
		Token string `json:"token"`
	}{Token: token})
	if _, err := conn.Write(append(pre, '\n')); err != nil {
		return fmt.Errorf("write preamble: %w", err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(conn, in)
		halfCloseWrite(conn) // signal EOF to the server's MCP loop
	}()
	go func() {
		_, _ = io.Copy(out, conn)
		close(done) // server closed its side: redirection is finished
	}()
	<-done
	return nil
}

// halfCloseWrite closes only the write half of conn if supported (real
// unix sockets do; net.Pipe does not), so the server sees EOF on stdin
// while we keep reading its response.
func halfCloseWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}
