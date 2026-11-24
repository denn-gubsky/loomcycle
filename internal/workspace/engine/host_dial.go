package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
)

// Dial connects to 127.0.0.1:port on the worker's own host. Every process
// of a host worker shares that loopback, so this reaches what the worker's
// user could reach with exec: the host engine isolates nothing (RFC GW
// §4.3, D6), and the caller's check of the port is a check, not a boundary.
func (h *Host) Dial(ctx context.Context, req DialRequest) (Conn, error) {
	if err := validPort(req.Port); err != nil {
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
