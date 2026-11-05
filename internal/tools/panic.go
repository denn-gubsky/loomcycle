package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// A tool that panics — a parser meeting a file it was not written for, a nil
// it did not expect — is a bug in that tool, and must cost the one call, not
// the process: a tool call runs on its own goroutine, where an unrecovered
// panic ends every run on the instance. The dispatcher recovers it into a
// failed call the model reads like any other, logs it for the operator and
// counts it.

// PanicCounter counts tool calls that panicked, by tool, for /metrics
// (loomcycle_tool_panics_total). One is made at boot and handed to every
// dispatcher that runs tools; a nil counter counts nothing.
type PanicCounter struct {
	mu sync.Mutex
	n  map[string]uint64
}

// NewPanicCounter returns an empty counter.
func NewPanicCounter() *PanicCounter { return &PanicCounter{n: map[string]uint64{}} }

func (c *PanicCounter) add(tool string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.n[tool]++
	c.mu.Unlock()
}

// Snapshot returns the counts so far, by tool name.
func (c *PanicCounter) Snapshot() map[string]uint64 {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.n)
}

// SetPanicCounter sets where the dispatcher counts the tool calls it
// recovered from a panic.
func (d *Dispatcher) SetPanicCounter(c *PanicCounter) { d.panics = c }

// guarded runs one tool call, recovering a panic into a tool_panicked
// result; panicked reports that it did.
func (d *Dispatcher) guarded(ctx context.Context, name string, run func() (Result, error)) (res Result, panicked bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			res, panicked, err = d.Panicked(ctx, name, p), true, nil
		}
	}()
	res, err = run()
	return res, false, err
}

// Panicked is the result of a tool call that panicked with p: logged at Error
// with the run and a reference, counted, and answered as a failure that names
// the tool and the reference but never the stack or the panic value, which
// may quote what the call was given. Call it from the deferred recover of
// the goroutine the panic happened on. Nil-safe.
func (d *Dispatcher) Panicked(ctx context.Context, name string, p any) Result {
	ref := stackRef()
	var counter *PanicCounter
	if d != nil {
		counter = d.panics
	}
	counter.add(name)
	attrs := []any{"tool", name, "run_id", RunID(ctx), "ref", ref}
	if RunIsNoTrace(ctx) {
		// A no-trace run's content is written nowhere; a panic value may
		// quote it. Its type and the stack (code positions only) still say
		// what broke.
		attrs = append(attrs, "panic_type", fmt.Sprintf("%T", p))
	} else {
		attrs = append(attrs, "panic", fmt.Sprint(p))
	}
	attrs = append(attrs, "stack", string(debug.Stack()))
	slog.ErrorContext(ctx, "tool call panicked", attrs...)
	return Result{
		Text:    fmt.Sprintf("tool_panicked: the %s tool failed unexpectedly on this call and was stopped (reference %s).", name, ref),
		IsError: true,
		Error: &ErrorInfo{
			Category:    CategoryBusiness,
			Retryable:   false,
			Description: "The same call would fail the same way. Try different input or another way to the goal; the operator can find this failure by its reference.",
		},
	}
}

// stackRef is a short hash of where the panic happened: the functions and
// lines from the panicking frame back to the function that recovered it,
// below the runtime's own. The same bug gives the same reference whatever
// path reached it, so the reference groups its log lines, and it holds no
// address or argument. It is called from Panicked, called from the deferred
// closure that recovered.
func stackRef() string {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(3, pcs)
	h := sha256.New()
	frames := runtime.CallersFrames(pcs[:n])
	first, more := frames.Next()
	// The deferred closure is <recoverer>.funcN; the panic's frames end at
	// the recoverer.
	recoverer := first.Function
	if i := strings.LastIndex(recoverer, ".func"); i > 0 {
		recoverer = recoverer[:i]
	}
	past := false
	for kept := 0; more && kept < 16; {
		var f runtime.Frame
		f, more = frames.Next()
		switch {
		case f.Function == "runtime.gopanic":
			past = true
		case !past || strings.HasPrefix(f.Function, "runtime."):
		case f.Function == recoverer:
			more = false
		default:
			fmt.Fprintf(h, "%s:%d\n", f.Function, f.Line)
			kept++
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}
