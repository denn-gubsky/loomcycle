//go:build linux || darwin

package engine

import (
	"context"
	"fmt"
	"os"
	"syscall"
)

// ownListener checks that port's listeners on the host are all processes of
// process group pgid, and that there is at least one: a server declaring a
// port does not make it the server's (the security review of #2916). The
// worker's own process is never one, whatever group it is in.
func ownListener(ctx context.Context, port, pgid int) error {
	pids, err := listenerPIDs(ctx, port)
	if err != nil {
		return fmt.Errorf("finding who listens on port %d: %w", port, err)
	}
	if len(pids) == 0 {
		return errNotOwnListener
	}
	self := os.Getpid()
	for _, pid := range pids {
		if pid == self {
			return errNotOwnListener
		}
		g, err := syscall.Getpgid(pid)
		if err != nil || g != pgid {
			return errNotOwnListener
		}
	}
	return nil
}
