package engine

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// relaySource is the relay Dial starts in a session. It is passed to
// python3 -I -c, so nothing of it lives in the workspace, where the session
// could change it, and the session image needs nothing beyond python3.
//
//go:embed relay.py
var relaySource string

// RelayArgv is the program Dial starts in a session to reach port.
func RelayArgv(port int) []string {
	return []string{"python3", "-I", "-c", relaySource, strconv.Itoa(port)}
}

// Dial starts the relay in the workspace's session (Start: the session's
// user, limits and network, whatever P4's egress gave it) and returns its
// stdin and stdout as the stream. The worker never dials into a session and
// the session gains no network: the relay connects to 127.0.0.1 inside it.
func (c *Container) Dial(ctx context.Context, req DialRequest) (Conn, error) {
	if err := validPort(req.Port); err != nil {
		return nil, err
	}
	p, err := c.Start(ctx, StartRequest{Workspace: req.Workspace, Root: req.Root, Dir: req.Root, Argv: RelayArgv(req.Port)})
	if err != nil {
		return nil, fmt.Errorf("dial: starting the relay: %w", err)
	}
	return relayed(ctx, p, req.Port)
}

// relayed waits for the relay's "O" (connected) and wraps it as a Conn. A
// relay that ends first could not connect: its standard error says why.
func relayed(ctx context.Context, p Process, port int) (Conn, error) {
	ready := make(chan error, 1)
	safego.Go(context.WithoutCancel(ctx), "workspace.engine.relay-ready", func() {
		var b [1]byte
		_, err := io.ReadFull(p.Stdout(), b[:])
		if err == nil && b[0] != 'O' {
			err = fmt.Errorf("the relay's first byte was %q", b[0])
		}
		ready <- err
	})
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	select {
	case err := <-ready:
		if err == nil {
			return &relayConn{p: p}, nil
		}
		_ = p.Kill()
		<-p.Done()
		why := strings.TrimSpace(p.Stderr())
		if strings.Contains(why, "relay: connect:") {
			return nil, fmt.Errorf("%w: port %d (%s)", ErrUnreachable, port, strings.TrimPrefix(why, "relay: connect: "))
		}
		if why == "" {
			why = err.Error()
		}
		return nil, fmt.Errorf("dial: the relay ended: %s", why)
	case <-ctx.Done():
		_ = p.Kill()
		return nil, fmt.Errorf("dial: %w", ctx.Err())
	}
}

// relayConn is a relay's stdin and stdout. Close kills it.
type relayConn struct {
	p    Process
	once sync.Once
}

func (r *relayConn) Read(b []byte) (int, error)  { return r.p.Stdout().Read(b) }
func (r *relayConn) Write(b []byte) (int, error) { return r.p.Stdin().Write(b) }
func (r *relayConn) CloseWrite() error           { return r.p.Stdin().Close() }

func (r *relayConn) Close() error {
	var err error
	r.once.Do(func() { err = r.p.Kill() })
	return err
}
