package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
			res, panicked, err = d.panickedInTool(ctx, name, p), true, nil
		}
	}()
	res, err = run()
	return res, false, err
}

// panickedInTool is the result of a tool that panicked with p: the tool was
// stopped. Called from the deferred recover in guarded.
func (d *Dispatcher) panickedInTool(ctx context.Context, name string, p any) Result {
	return d.panicked(ctx, name, p, stackRef(), true)
}

// PanickedAroundCall is the result of a call whose surrounding code (a hook,
// the dispatch around the tool) panicked with p on the tool goroutine. The
// tool may already have run, so the answer says the outcome is unknown rather
// than that the tool was stopped, and the call counts as a failure for the
// repeat guard as a failed call does (a call whose tool failed and whose
// post-hook then panicked is counted twice). Call it from the deferred
// recover of the goroutine the panic happened on. Nil-safe.
func (d *Dispatcher) PanickedAroundCall(ctx context.Context, name string, input json.RawMessage, p any) Result {
	r := d.panicked(ctx, name, p, stackRef(), false)
	if d != nil {
		d.noteResult(name, input, r)
	}
	return r
}

// panicked logs a recovered panic at Error with the run and the reference,
// counts it, and answers it as a failure that names the tool and the
// reference but never the stack or the panic value, which may quote what the
// call was given. inTool says the panic came from inside the tool, which was
// therefore stopped.
func (d *Dispatcher) panicked(ctx context.Context, name string, p any, ref string, inTool bool) Result {
	var counter *PanicCounter
	if d != nil {
		counter = d.panics
	}
	counter.add(d.panicLabel(name))
	attrs := []any{"tool", cutLogName(name), "in_tool", inTool, "run_id", RunID(ctx), "ref", ref}
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
	if !inTool {
		return Result{
			Text:    fmt.Sprintf("tool_panicked: the call to %s failed unexpectedly around the tool, and its outcome is unknown: it may have taken effect (reference %s).", name, ref),
			IsError: true,
			Error: &ErrorInfo{
				Category:    CategoryBusiness,
				Retryable:   false,
				Description: "Check whether the call took effect before you send it again; the operator can find this failure by its reference.",
			},
		}
	}
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

// panicLabel is the label a panic is counted under in
// loomcycle_tool_panics_total. The name a call carries is the model's, so it
// is the label only when it names a tool this dispatcher has; a name a
// fallback would resolve is counted as "mcp" (its server part is the model's
// too), and anything else as "other": a model cannot mint a series per guess.
// The tool map is built once and never written, so reading it here is safe.
func (d *Dispatcher) panicLabel(name string) string {
	if d != nil {
		if _, ok := d.tools[name]; ok {
			return name
		}
	}
	if strings.HasPrefix(name, "mcp__") {
		return "mcp"
	}
	return "other"
}

// cutLogName bounds a model-chosen tool name in a log line.
func cutLogName(name string) string {
	if len(name) > 128 {
		return name[:128] + "…"
	}
	return name
}

// stackRef is a short hash of where the panic happened: the functions and
// lines of at most 16 frames from the panicking one back toward the function
// that recovered it, below the runtime's own. Panics of one bug reached
// through the same frames within that window share a reference, which groups
// their log lines; a shallow tool reached by different callers within it may
// not. It holds no address or argument. It must be called from a function
// called by the deferred closure that recovered (panickedInTool,
// PanickedAroundCall): the frames it skips assume that depth.
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
