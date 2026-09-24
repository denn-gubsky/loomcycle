// Package codehook runs code-js hook bodies: operator JavaScript that decides
// on a tool call in-process, without a webhook round-trip and without spending
// tokens.
//
// A body defines a top-level function hook(ev) and returns its decision. It
// runs in the code-js sandbox (no fetch, require, filesystem, eval or
// Function; a deterministic clock and RNG), and its only tool is Interruption
// (ask and notify). That is how a hook holds a call for a person: it asks, and
// decides on the answer.
//
// # Replay, the same way a code agent runs
//
// Each run of the JavaScript builds a fresh runtime and replays the answers
// already recorded for this invocation. When the body reaches an Interruption
// call it has no recorded answer for, the run stops there; the runner makes
// the call (the ask blocks until a person answers, it times out, or the run is
// cancelled), records the result, and runs the body again from the start. No
// goroutine is parked inside the JavaScript engine, and the time a person
// takes is never counted against the hook's timeout, which bounds each run of
// the code.
//
// A fresh runtime per run is also why there is no runtime pool: a reused
// runtime would carry one call's globals into the next, across agents and
// tenants, and goja has no reset.
package codehook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	// maxAsks bounds the Interruption calls one invocation may make. Each one
	// replays the body again, and a body that asks in a loop would otherwise
	// hold the tool call forever.
	maxAsks = 16
	// maxPayloadBytes and maxDecisionBytes match the webhook response cap.
	maxPayloadBytes  = 1 << 20
	maxDecisionBytes = 1 << 20
	// maxCallStack stops runaway recursion before it exhausts the stack.
	maxCallStack = 512
	// compileTimeout bounds evaluating a body's top level at registration.
	compileTimeout = time.Second
)

// Runner runs code-js hook bodies. It is safe for concurrent use: every run
// builds its own runtime.
type Runner struct {
	// interruption is the Interruption tool a body's ask and notify call. It
	// runs under the hook's own grant, whether or not the agent may interrupt.
	interruption tools.Tool

	mu    sync.RWMutex
	cache map[string]*goja.Program // sha256 of the body → compiled program
}

// New returns a Runner whose bodies ask through interruption. A nil tool makes
// every Interruption call fail, which the hook's fail mode then decides.
func New(interruption tools.Tool) *Runner {
	return &Runner{interruption: interruption, cache: make(map[string]*goja.Program)}
}

var _ hooks.CodeRunner = (*Runner)(nil)

// Compile parses a body and evaluates its top level, and requires it to
// define hook(ev). The top level runs with no tool bound, so a body cannot ask
// while it is being registered.
func (r *Runner) Compile(src string) error {
	prog, err := r.program(src)
	if err != nil {
		return err
	}
	rt := newRuntime(0, 0)
	_ = rt.Set("Interruption", rt.NewDynamicObject(unavailableTool{rt}))
	timer := time.AfterFunc(compileTimeout, func() { rt.Interrupt(errTopLevelTimeout) })
	defer timer.Stop()
	if _, err := rt.RunProgram(prog); err != nil {
		return fmt.Errorf("evaluating the body: %w", err)
	}
	if _, ok := goja.AssertFunction(rt.Get("hook")); !ok {
		return errors.New("the body defines no top-level hook(ev) function")
	}
	return nil
}

var errTopLevelTimeout = errors.New("the body's top level ran longer than 1s")

// program returns the compiled body, caching by content so every call of a
// hook reuses one parse.
func (r *Runner) program(src string) (*goja.Program, error) {
	sum := sha256.Sum256([]byte(src))
	key := hex.EncodeToString(sum[:])
	r.mu.RLock()
	prog, ok := r.cache[key]
	r.mu.RUnlock()
	if ok {
		return prog, nil
	}
	prog, err := goja.Compile("hook", src, false)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	r.mu.Lock()
	r.cache[key] = prog
	r.mu.Unlock()
	return prog, nil
}

// Run executes the hook for one call and returns its decision.
func (r *Runner) Run(ctx context.Context, h *hooks.Hook, event string, payload any) (hooks.CodeDecision, error) {
	prog, err := r.program(h.Code)
	if err != nil {
		return hooks.CodeDecision{}, err
	}
	ev, err := eventValue(event, payload)
	if err != nil {
		return hooks.CodeDecision{}, err
	}
	// One seed and clock for every run of this invocation, so the body replays
	// identically; different per call, so two calls do not share a sequence.
	seed := seedFor(h.ID, ev)
	anchor := time.Now().UnixMilli()

	// The hook's own grant: it may ask (and notify) under the run's pending cap
	// whether or not the agent itself may interrupt.
	askCtx := tools.WithInterruptionPolicy(ctx, tools.InterruptionPolicyValue{
		Enabled:    true,
		Kinds:      []string{"question"},
		MaxPending: tools.InterruptionPolicy(ctx).MaxPending,
	})

	var recorded []record
	for {
		ret, next, err := r.runOnce(ctx, prog, ev, recorded, seed, anchor, h.Timeout)
		if err != nil {
			return hooks.CodeDecision{}, err
		}
		if next == nil {
			return decode(ret)
		}
		if len(recorded) >= maxAsks {
			return hooks.CodeDecision{}, fmt.Errorf("the hook made more than %d Interruption calls", maxAsks)
		}
		if r.interruption == nil {
			return hooks.CodeDecision{}, errors.New("Interruption is not configured on this server")
		}
		res, err := r.interruption.Execute(askCtx, next.input)
		if err != nil {
			return hooks.CodeDecision{}, fmt.Errorf("Interruption: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return hooks.CodeDecision{}, err
		}
		recorded = append(recorded, record{input: next.input, text: res.Text, isError: res.IsError})
	}
}

// record is one Interruption call already made in this invocation, replayed on
// every later run.
type record struct {
	input   json.RawMessage
	text    string
	isError bool
}

// call is the first Interruption call a run made that had no recorded result.
type call struct{ input json.RawMessage }

// runOnce runs the body once, replaying recorded. It returns the value hook(ev)
// returned, or the next call to make.
func (r *Runner) runOnce(ctx context.Context, prog *goja.Program, ev map[string]any, recorded []record, seed uint32, anchor int64, budget time.Duration) (json.RawMessage, *call, error) {
	rt := newRuntime(seed, anchor)
	st := &replay{rt: rt, recorded: recorded}
	_ = rt.Set("Interruption", rt.NewDynamicObject(&interruptionTool{rt: rt, st: st}))

	stop := make(chan struct{})
	defer close(stop)
	go st.watch(ctx, stop, budget)

	ret, err := func() (goja.Value, error) {
		if _, err := rt.RunProgram(prog); err != nil {
			return nil, err
		}
		fn, ok := goja.AssertFunction(rt.Get("hook"))
		if !ok {
			return nil, errors.New("the body defines no top-level hook(ev) function")
		}
		return fn(goja.Undefined(), codejs.StableJSValue(rt, ev))
	}()
	if err == nil {
		// The body returned. The watcher may still fire in the instant after
		// (an interrupt then lands on a runtime nobody uses), so a finished body
		// is never reported as timed out or cancelled.
		out, err := exportDecision(ret)
		return out, nil, err
	}
	switch cause := interruptCause(st.cause.Load()); {
	case cause == causeCancel:
		return nil, nil, ctx.Err()
	case cause == causeTimeout:
		return nil, nil, fmt.Errorf("the hook ran longer than its %s timeout", budget)
	case st.diverged != "":
		return nil, nil, errors.New(st.diverged)
	case st.next != nil:
		return nil, st.next, nil
	default:
		return nil, nil, thrown(err)
	}
}

// newRuntime builds a sandboxed runtime: the code-js hardening, JSON field
// names, and a bounded call stack.
func newRuntime(seed uint32, anchor int64) *goja.Runtime {
	rt := goja.New()
	rt.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	codejs.HardenSandbox(rt, seed, anchor)
	rt.SetMaxCallStackSize(maxCallStack)
	return rt
}

type interruptCause int32

const (
	causeNone interruptCause = iota
	causeCancel
	causeTimeout
)

// replay drives one run of the body.
type replay struct {
	rt       *goja.Runtime
	recorded []record
	k        int

	next     *call
	diverged string
	// cause is set by watch before it interrupts the runtime, and read after
	// the run returns — it crosses goroutines, hence atomic.
	cause atomic.Int32
}

// watch stops the runtime when the run is cancelled or the budget elapses.
func (st *replay) watch(ctx context.Context, stop <-chan struct{}, budget time.Duration) {
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		st.cause.Store(int32(causeCancel))
		st.rt.Interrupt(ctx.Err())
	case <-timer.C:
		st.cause.Store(int32(causeTimeout))
		st.rt.Interrupt(context.DeadlineExceeded)
	case <-stop:
	}
}

// call replays a recorded Interruption result, or stops the run at the first
// call that has none.
func (st *replay) call(input json.RawMessage) (text string, isError bool, ok bool) {
	if st.next != nil || st.diverged != "" {
		// Already stopping: the interrupt lands at the next instruction, and a
		// call made in between must not replace the one that stopped the run.
		return "", false, false
	}
	idx := st.k
	st.k++
	if idx < len(st.recorded) {
		rec := st.recorded[idx]
		if !codejs.SameCanonicalJSON(rec.input, input) {
			// The body made a different call this time: its control flow does not
			// depend only on ev and the answers. Feeding it the old answer would
			// decide on the wrong question.
			st.diverged = fmt.Sprintf("the hook's Interruption call #%d changed between runs (%s, then %s); a hook must decide only from ev and the answers it gets", idx+1, rec.input, input)
			st.rt.Interrupt(errDiverged)
			return "", false, false
		}
		return rec.text, rec.isError, true
	}
	st.next = &call{input: append(json.RawMessage(nil), input...)}
	st.rt.Interrupt(errNeedsAnswer)
	return "", false, false
}

var (
	errDiverged    = errors.New("replay diverged")
	errNeedsAnswer = errors.New("waiting for an answer")
)

// interruptionTool is the body's Interruption object: ask and notify.
type interruptionTool struct {
	rt *goja.Runtime
	st *replay
}

func (t *interruptionTool) Get(key string) goja.Value {
	switch key {
	case "ask", "notify":
		op := key
		return t.rt.ToValue(func(c goja.FunctionCall) goja.Value { return t.invoke(op, c) })
	}
	return nil
}
func (t *interruptionTool) Set(string, goja.Value) bool { return false }
func (t *interruptionTool) Has(key string) bool         { return key == "ask" || key == "notify" }
func (t *interruptionTool) Delete(string) bool          { return false }
func (t *interruptionTool) Keys() []string              { return []string{"ask", "notify"} }

// invoke makes one ask or notify. ask returns the answer, or null when the
// operator declined to answer; a timeout or cancellation throws, so a body
// that does not catch it fails and the hook's fail mode decides.
func (t *interruptionTool) invoke(op string, c goja.FunctionCall) goja.Value {
	args := map[string]any{}
	if a := c.Argument(0); !goja.IsUndefined(a) && !goja.IsNull(a) {
		m, ok := a.Export().(map[string]any)
		if !ok {
			panic(t.rt.NewTypeError("Interruption." + op + ": the argument must be an object"))
		}
		for k, v := range m {
			args[k] = v
		}
	}
	args["op"] = op
	input, err := json.Marshal(args)
	if err != nil {
		panic(t.rt.NewTypeError("Interruption." + op + ": " + err.Error()))
	}
	text, isError, ok := t.st.call(input)
	if !ok {
		// The runtime is being interrupted; this value is never used.
		return goja.Undefined()
	}
	if isError {
		panic(t.rt.NewGoError(errors.New(text)))
	}
	if op == "notify" {
		return goja.Undefined()
	}
	var res struct {
		Answer   string `json:"answer"`
		Declined bool   `json:"declined"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		panic(t.rt.NewGoError(fmt.Errorf("Interruption.ask: unreadable result: %w", err)))
	}
	if res.Declined {
		return goja.Null()
	}
	return t.rt.ToValue(res.Answer)
}

// unavailableTool is the Interruption object while a body is registered: its
// top level runs with nothing to ask.
type unavailableTool struct{ rt *goja.Runtime }

func (t unavailableTool) Get(key string) goja.Value {
	if key != "ask" && key != "notify" {
		return nil
	}
	return t.rt.ToValue(func(goja.FunctionCall) goja.Value {
		panic(t.rt.NewTypeError("Interruption." + key + " can only be called from inside hook(ev)"))
	})
}
func (t unavailableTool) Set(string, goja.Value) bool { return false }
func (t unavailableTool) Has(key string) bool         { return key == "ask" || key == "notify" }
func (t unavailableTool) Delete(string) bool          { return false }
func (t unavailableTool) Keys() []string              { return []string{"ask", "notify"} }

// eventValue is ev: the call a webhook would receive, plus the event name.
func eventValue(event string, payload any) (map[string]any, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding the event: %w", err)
	}
	if len(raw) > maxPayloadBytes {
		return nil, fmt.Errorf("the event is %d bytes; a code hook takes at most %d", len(raw), maxPayloadBytes)
	}
	ev := map[string]any{}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, fmt.Errorf("decoding the event: %w", err)
	}
	ev["event"] = event
	return ev, nil
}

// seedFor derives the RNG seed from the hook and the call it decides on.
func seedFor(hookID string, ev map[string]any) uint32 {
	h := fnv.New32a()
	runID, _ := ev["run_id"].(string)
	var toolID string
	if tc, ok := ev["tool_call"].(map[string]any); ok {
		toolID, _ = tc["id"].(string)
	}
	_, _ = h.Write([]byte(hookID + "|" + runID + "|" + toolID))
	return h.Sum32()
}

// exportDecision turns hook(ev)'s return value into JSON. Returning nothing
// lets the call through.
func exportDecision(v goja.Value) (json.RawMessage, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}
	m, ok := v.Export().(map[string]any)
	if !ok {
		return nil, fmt.Errorf("hook(ev) returned %s; it must return an object such as {decision: \"allow\"}", v.String())
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encoding the decision: %w", err)
	}
	if len(out) > maxDecisionBytes {
		return nil, fmt.Errorf("the decision is %d bytes; the limit is %d", len(out), maxDecisionBytes)
	}
	return out, nil
}

// decode reads the decision strictly: a misspelt field is reported, not
// silently ignored.
func decode(raw json.RawMessage) (hooks.CodeDecision, error) {
	var d hooks.CodeDecision
	if len(raw) == 0 {
		return d, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return hooks.CodeDecision{}, fmt.Errorf("reading the decision: %w", err)
	}
	return d, nil
}

// thrown describes an error the body threw. A reference to any other tool is
// a ReferenceError, and says so: Interruption is the only tool a hook has.
func thrown(err error) error {
	msg := err.Error()
	var ex *goja.Exception
	if errors.As(err, &ex) {
		msg = ex.Error()
	}
	if strings.Contains(msg, "ReferenceError") {
		return fmt.Errorf("the hook threw: %s (a code hook's only tool is Interruption)", msg)
	}
	return fmt.Errorf("the hook threw: %s", msg)
}
