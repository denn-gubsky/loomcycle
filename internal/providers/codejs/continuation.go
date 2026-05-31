package codejs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
)

// errNoRun is the outcome when an agent's index.js defines no top-level
// run() function. Surfaced as a code_agent_threw EventError.
var errNoRun = errors.New("index.js defines no top-level run(input) function")

// toolReq is one JS-initiated tool call crossing from the runtime goroutine
// to the provider's pump goroutine. Name + Input are ALREADY the loop-facing
// tool name and input (the bindings translate memory.get(...) into
// {"Memory", {"op":"get",...}}); the pump emits them verbatim as an
// EventToolCall. resp is buffered(1) so the pump never blocks delivering the
// result back.
//
// CROSS-GOROUTINE RULE: only plain Go data crosses here — never a goja.Value
// (goja runtimes are single-goroutine; a Value escaping its runtime goroutine
// is a data race). Input is json.RawMessage; the result returns as a string.
type toolReq struct {
	name  string
	input json.RawMessage
	resp  chan toolResp
}

type toolResp struct {
	text    string
	isError bool
}

// outcome is the terminal result of run(): the final text, or an error (a JS
// throw, a missing run(), a ctx cancel/timeout that unwound the JS, or an
// Interrupt of a CPU-bound loop).
type outcome struct {
	finalText string
	err       error
}

// continuation owns one in-flight code-agent run: a goja Runtime running on
// its OWN goroutine, parked on a Go channel at each tool call while the loop
// dispatches (Appendix-A Mechanism 1). It survives across the multiple
// Provider.Call invocations of one run, keyed by token.
type continuation struct {
	token  string
	runCtx context.Context
	cancel context.CancelFunc
	rt     *goja.Runtime

	// settled flips true (on the runtime goroutine) the instant run() has
	// finished and the runtime goroutine is exiting. watch() reads it to
	// avoid calling rt.Interrupt on an already-exited runtime on the normal
	// completion path — the write happens-before the done send, which
	// happens-before pump's cancel(), which happens-before watch's wake, so
	// the read is correctly ordered.
	settled atomic.Bool

	// dispatch: runtime goroutine → pump. A *toolReq when the JS calls a
	// tool. done: runtime goroutine → pump, exactly once, when run()
	// settles. done is buffered(1) so the JS goroutine can settle-and-exit
	// even if the run was abandoned (no pump reading) — the leak backstop.
	dispatch chan *toolReq
	done     chan outcome

	// seq numbers the tool calls within this run; folded into the minted
	// tool_use ID (cj-<token>-<seq>) so the round-tripped tool_result can be
	// matched back. Touched only by the pump goroutine (provider.go),
	// serialized by the loop's one-Call-at-a-time drive.
	seq int
	// pending is the tool call the JS is currently parked on, awaiting a
	// loop-dispatched result. Set by the pump when it receives a toolReq;
	// read by the next (resume) Call to deliver the result. Safe without a
	// lock: the loop does not invoke the resume Call until the suspend
	// Call's event channel has closed (happens-before), and only the pump
	// touches it.
	pending   *toolReq
	pendingID string
}

// bindFunc registers the agent's permitted tool surface onto rt, using emit
// as the suspend primitive. Supplied by the provider (it knows req.Tools).
type bindFunc func(rt *goja.Runtime, emit toolEmitter)

// toolEmitter is the host-side suspend point a bound JS tool function calls.
// It runs ON the runtime goroutine, parks it while the loop dispatches, and
// returns the loop's result. err != nil means ctx cancel/timeout (the binding
// throws it into JS, which typically propagates to run failure); isError
// means the tool returned IsError (the binding throws a catchable JS error).
type toolEmitter func(name string, input json.RawMessage) (text string, isError bool, err error)

// newContinuation builds the runtime, hardens the sandbox, binds the
// permitted tools, and launches the JS goroutine running run(input). It
// returns immediately; the goroutine parks at the first tool call (delivered
// on dispatch) or settles (delivered on done). parentCtx is the first Call's
// ctx — capturing it is safe because the iteration span's End() does not
// cancel it (only the run ctx's cancel/deadline does).
func newContinuation(parentCtx context.Context, token string, prog *goja.Program, input map[string]any, deterministic bool, runTimeout time.Duration, bind bindFunc) *continuation {
	runCtx, cancel := context.WithTimeout(parentCtx, runTimeout)
	c := &continuation{
		token:    token,
		runCtx:   runCtx,
		cancel:   cancel,
		rt:       goja.New(),
		dispatch: make(chan *toolReq),
		done:     make(chan outcome, 1),
	}

	// Field names follow JS convention so operator code reads naturally.
	c.rt.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	hardenSandbox(c.rt, deterministic, token)
	bind(c.rt, c.callTool)

	go c.execute(prog, input)
	return c
}

// execute runs on the runtime's OWN goroutine. It owns c.rt exclusively;
// nothing else touches the runtime or any goja.Value.
func (c *continuation) execute(prog *goja.Program, input map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			// A panic that is not a goja interrupt/exception (those return as
			// errors from the call) means a host-side bug; surface it rather
			// than crashing the process.
			c.settle(outcome{err: fmt.Errorf("code-agent host panic: %v", r)})
		}
	}()

	if _, err := c.rt.RunProgram(prog); err != nil {
		c.settle(outcome{err: fmt.Errorf("evaluating index.js: %w", err)})
		return
	}
	runFn, ok := goja.AssertFunction(c.rt.Get("run"))
	if !ok {
		c.settle(outcome{err: errNoRun})
		return
	}
	ret, err := runFn(goja.Undefined(), c.rt.ToValue(input))
	if err != nil {
		c.settle(outcome{err: err})
		return
	}
	// Extract the final text on THIS goroutine — reading a goja.Value off the
	// runtime goroutine would be a data race.
	c.settle(outcome{finalText: extractFinalText(c.rt, ret)})
}

// settle delivers the terminal outcome exactly once. done is buffered(1), so
// this never blocks even when the run was abandoned and no pump is reading.
// It marks the runtime goroutine done BEFORE the send so watch() never
// interrupts a runtime that has already exited.
func (c *continuation) settle(o outcome) {
	c.settled.Store(true)
	c.done <- o
}

// callTool is the suspend point — it runs ON the runtime goroutine. It hands
// the tool call to the pump and parks until the loop's result returns, or the
// run ctx is cancelled/times out. On ctx end it returns the ctx error, which
// the binding throws into JS via panic(rt.NewGoError(err)); since no bytecode
// is executing while parked, this is the ONLY cancel path that works here
// (Interrupt cannot reach a parked frame — goja issue #97).
func (c *continuation) callTool(name string, input json.RawMessage) (string, bool, error) {
	req := &toolReq{name: name, input: input, resp: make(chan toolResp, 1)}
	select {
	case c.dispatch <- req:
	case <-c.runCtx.Done():
		return "", false, c.runCtx.Err()
	}
	select {
	case r := <-req.resp:
		return r.text, r.isError, nil
	case <-c.runCtx.Done():
		return "", false, c.runCtx.Err()
	}
}

// teardown releases the continuation's resources. Idempotent: cancel() is
// safe to call repeatedly. The runtime goroutine, once settled or unwound by
// ctx cancel, exits on its own and the runtime is GC'd.
func (c *continuation) teardown() {
	c.cancel()
}

// extractFinalText pulls the final_text out of run()'s return value. run()
// may return {final_text: "...", metadata?} (the documented shape), a bare
// string, or nothing — all map to a string. Must be called on the runtime
// goroutine.
func extractFinalText(rt *goja.Runtime, v goja.Value) string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return ""
	}
	if obj, ok := v.(*goja.Object); ok {
		if ft := obj.Get("final_text"); ft != nil && !goja.IsUndefined(ft) && !goja.IsNull(ft) {
			return ft.String()
		}
		// An object without final_text: return its JSON so the run isn't
		// silently empty (helps operators debug a wrong return shape).
		if b, err := v.ToObject(rt).MarshalJSON(); err == nil {
			return string(b)
		}
	}
	return v.String()
}
