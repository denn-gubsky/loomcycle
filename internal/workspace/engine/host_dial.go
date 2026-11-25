package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
)

// Dial connects to 127.0.0.1:port on the worker's own host, once it has
// checked that every listener on the port is a process of one of the
// declaring servers' process groups (ErrNotOwnListener otherwise). Every
// process of a host worker shares that loopback, so without the check a
// server that only declared a port would reach the core's API, a database
// or any other local service on it. A server that leaves its group (setsid)
// is not reached. The host engine still isolates nothing else (RFC GW §4.3,
// D6).
func (h *Host) Dial(ctx context.Context, req DialRequest) (Conn, error) {
	if err := validPort(req.Port); err != nil {
		return nil, err
	}
	if err := hostOwnsPort(ctx, req); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: dialTimeout}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(req.Port)))
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%w: port %d", ErrUnreachable, req.Port)
		}
		return nil, fmt.Errorf("dial: %w", err)
	}
	return c.(*net.TCPConn), nil
}

// errNotOwnListener is a port whose listener is not the server's own: no
// process of its group listens on it, or a process outside the group does
// too (on macOS a more specific bind takes the connection).
var errNotOwnListener = errors.New("the port is not listened on by the server's own processes")

// ErrNotOwnListener is a port the declaring server does not listen on
// itself.
var ErrNotOwnListener = errNotOwnListener

// hostOwnsPort is Dial's ownership check.
func hostOwnsPort(ctx context.Context, req DialRequest) error {
	var last error = ErrNotOwnListener
	for _, o := range req.Owners {
		hp, ok := o.(*hostProcess)
		if !ok || hp.cmd.Process == nil {
			continue
		}
		// The program leads its own group (procgroup.StartInOwnGroup).
		err := ownListener(ctx, req.Port, hp.cmd.Process.Pid)
		if err == nil {
			return nil
		}
		last = err
	}
	if errors.Is(last, errNotOwnListener) {
		return fmt.Errorf("%w: port %d", ErrNotOwnListener, req.Port)
	}
	return fmt.Errorf("dial: %w", last)
}
