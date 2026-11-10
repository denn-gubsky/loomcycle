// Package safego starts background goroutines that recover a panic instead of
// ending the process.
//
// An unrecovered panic in any goroutine ends the whole process, so one bug in a
// sweeper, a pubsub listener or a fan-out worker takes down every tenant's runs
// with it. Go and Run recover the panic, log it with its stack and count it in
// loomcycle_goroutine_panics_total, so the bug costs that goroutine's work
// instead.
//
// Recovery happens in the outermost frame, after fn's own deferred calls have
// run. A goroutine that releases a lock, calls WaitGroup.Done or closes a
// channel must therefore do it in a defer inside fn: a release written as a
// plain statement after the code that panicked never runs, and the recovered
// goroutine would leave the lock held or its waiter blocked.
package safego

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"sync"
)

// panics is the process-wide tally behind loomcycle_goroutine_panics_total, by
// goroutine name. It is the package's one piece of mutable state, kept global on
// purpose: the goroutines it counts are started from every layer of the binary,
// most with no handle to the server that exposes metrics, and threading a
// counter through each of them would be a far larger change than the guard.
// Names are static strings at the call sites, so the label set stays bounded.
var panics = struct {
	mu sync.Mutex
	n  map[string]uint64
}{n: map[string]uint64{}}

// Go runs fn in a new goroutine under Run.
func Go(ctx context.Context, name string, fn func()) {
	go Run(ctx, name, fn)
}

// Run calls fn on the calling goroutine and recovers a panic from it: the panic
// is logged with its stack and counted under name, and Run returns normally.
// Use it inside a long-lived loop to keep the loop alive across one bad
// iteration, or where the caller already started the goroutine.
func Run(ctx context.Context, name string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			if ctx == nil {
				ctx = context.Background()
			}
			slog.ErrorContext(ctx, "background goroutine panicked; recovered",
				"goroutine", name, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			count(name)
		}
	}()
	fn()
}

func count(name string) {
	panics.mu.Lock()
	panics.n[name]++
	panics.mu.Unlock()
}

// Panics returns the recovered panics so far, by goroutine name.
func Panics() map[string]uint64 {
	panics.mu.Lock()
	defer panics.mu.Unlock()
	return maps.Clone(panics.n)
}
