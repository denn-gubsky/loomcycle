package main

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// boot carries what the server's boot phases hand to one another. main runs
// the phases in order; each reads what an earlier one left here and adds its
// own. A dependency built late but needed by something built earlier (a
// closure that runs only once the process serves) is read from here at call
// time, never copied, so it sees the value assigned after it was built.
type boot struct {
	// From the command line.
	mcpMode     bool
	cfgPaths    []string
	presetFlags []string

	cfg          *config.Config
	otelShutdown func(context.Context) error

	// cleanups replace the defers main once held; see runCleanups.
	cleanups []func()
}

// addCleanup registers f to run when main returns.
func (b *boot) addCleanup(f func()) { b.cleanups = append(b.cleanups, f) }

// runCleanups runs the registered cleanups last first, as the defers they
// replace ran. Each is deferred, not called in a loop, so one that panics
// does not keep the others from running — again as with defers.
func (b *boot) runCleanups() {
	for _, f := range b.cleanups {
		defer f()
	}
}
