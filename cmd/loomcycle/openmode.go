package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// openModeWarnInterval is how often a server in open mode repeats its warning,
// so it is still in the log an operator reads days after boot.
const openModeWarnInterval = 10 * time.Minute

// openModeRefusal returns the error that stops a boot in open mode on a
// listener beyond loopback (RFC ES #8), or nil.
//
// Open mode — no legacy LOOMCYCLE_AUTH_TOKEN and no admin operator token —
// serves every route, token minting included, to whoever reaches the port. That
// is a convenience on 127.0.0.1 and an open door on 0.0.0.0, so the second
// needs the operator to say so with LOOMCYCLE_ALLOW_OPEN_MODE=1.
//
// authConfigured must be the server's own predicate, evaluated after the store
// opens: an admin token in the store turns auth on with no legacy secret.
func openModeRefusal(authConfigured bool, env *config.Env) error {
	if authConfigured || env.AllowOpenMode {
		return nil
	}
	public := env.NonLoopbackListeners()
	if len(public) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to start in open mode on %s: with no LOOMCYCLE_AUTH_TOKEN "+
		"and no admin operator token every route is unauthenticated, and this address is "+
		"reachable beyond this machine. Set LOOMCYCLE_AUTH_TOKEN (e.g. `openssl rand -hex 32`), "+
		"bind to 127.0.0.1, or set LOOMCYCLE_ALLOW_OPEN_MODE=1 to serve unauthenticated anyway",
		strings.Join(public, ", "))
}

// warnOpenMode logs the open-mode warning now and every interval after while
// the server stays in open mode. It returns when ctx ends, or once auth is
// configured (an admin token minted at runtime): a running server does not move
// back into open mode.
func warnOpenMode(ctx context.Context, interval time.Duration, authConfigured func(context.Context) bool, logf func(string, ...any)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if authConfigured(ctx) {
			return
		}
		logf("WARNING: open mode — no LOOMCYCLE_AUTH_TOKEN and no admin operator token, " +
			"so every HTTP and gRPC route is unauthenticated (dev only). Set LOOMCYCLE_AUTH_TOKEN " +
			"or mint an admin operator token.")
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
