package main

import (
	"context"
	"fmt"
	"log/slog"
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
// Open mode — no legacy LOOMCYCLE_AUTH_TOKEN, no config `principals:` and no
// admin operator token — serves every route, token minting included, to
// whoever reaches the port. That is a convenience on 127.0.0.1 and an open door
// on 0.0.0.0, so the second needs the operator to say so with
// LOOMCYCLE_ALLOW_OPEN_MODE=1.
//
// authConfigured and authErr must be the server's own predicate, evaluated
// after the store opens: an admin token in the store turns auth on with no
// legacy secret. When the store could not be asked (authErr), the boot is
// refused outright — guessing "configured" would skip this check, and guessing
// "open" would refuse for the wrong reason.
func openModeRefusal(authConfigured bool, authErr error, env *config.Env) error {
	if authErr != nil {
		return fmt.Errorf("refusing to start: cannot tell whether auth is configured, "+
			"because reading the admin operator tokens failed: %w", authErr)
	}
	if authConfigured || env.AllowOpenMode {
		return nil
	}
	public := env.NonLoopbackListeners()
	if len(public) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to start in open mode on %s: with no LOOMCYCLE_AUTH_TOKEN, "+
		"no config `principals:` and no admin operator token every route is unauthenticated, "+
		"and this address is reachable beyond this machine. "+
		"Set LOOMCYCLE_AUTH_TOKEN (e.g. `openssl rand -hex 32`) or declare a principal, "+
		"bind to 127.0.0.1, or set LOOMCYCLE_ALLOW_OPEN_MODE=1 to serve unauthenticated anyway",
		strings.Join(public, ", "))
}

// warnOpenMode logs the open-mode warning now and every interval after while
// the server stays in open mode. It returns when ctx ends, or once auth is
// configured (an admin token minted at runtime): a running server does not move
// back into open mode. A tick whose store read fails neither warns nor stops.
func warnOpenMode(ctx context.Context, interval time.Duration, authConfigured func(context.Context) (bool, error), logger *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		on, err := authConfigured(ctx)
		if err == nil && on {
			return
		}
		if err == nil {
			logger.Warn("open mode — no LOOMCYCLE_AUTH_TOKEN, no config principals and no admin " +
				"operator token, so every HTTP and gRPC route is unauthenticated (dev only). Set " +
				"LOOMCYCLE_AUTH_TOKEN, declare a principal, or mint an admin operator token.")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
