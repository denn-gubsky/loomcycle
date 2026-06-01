package codejs

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
	"go.opentelemetry.io/otel/trace"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// providerID is the stable wire identity of the synthetic code provider.
const providerID = "code-js"

// syntheticModel is the model string reported on usage/OTEL for every
// code-agent run (RFC J Decision 9). Token counters are always zero.
const syntheticModel = "loomcycle/code-js"

// DefaultRunTimeout bounds a single replay turn's wall-clock (a CPU-bound JS
// loop) via Interrupt, when the operator sets no
// LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS. The run's overall deadline is the
// loop's ctx.
const DefaultRunTimeout = 120 * time.Second

// fixedEpochMs / fixedSeed pin the clock + RNG across ALL runs under
// LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=1 (cross-run reproducibility for
// testing / snapshot equality). 2023-11-14T22:13:20Z.
const (
	fixedEpochMs int64  = 1700000000000
	fixedSeed    uint32 = 0x9e3779b9
)

// Config is the provider's construction-time configuration, sourced from the
// LOOMCYCLE_CODE_AGENTS_* env knobs in cmd/loomcycle/main.go.
type Config struct {
	CodeRoot      string // resolved $LOOMCYCLE_CODE_AGENTS_ROOT (default ./agent_code)
	Deterministic bool   // LOOMCYCLE_CODE_AGENTS_DETERMINISTIC (cross-run reproducibility)
	RunTimeout    time.Duration
	Logf          func(format string, args ...any)
}

// Provider is the RFC J synthetic code-js Provider, Appendix-B replay model.
// One instance is shared by every code-agent; it resolves each agent's JS from
// the RunMeta agent name on ctx. It is STATELESS across Call invocations: each
// Call builds a fresh runtime, replays the run's recorded tool results from
// the transcript, and stops at the first un-recorded call. No continuation, no
// registry, no parked goroutine.
type Provider struct {
	compiler      *compiler
	deterministic bool
	runTimeout    time.Duration
	logf          func(string, ...any)

	counter atomic.Uint64 // mints unique tool_use IDs for the transcript
}

// New builds the provider. RunTimeout falls back to DefaultRunTimeout.
func New(cfg Config) *Provider {
	if cfg.RunTimeout <= 0 {
		cfg.RunTimeout = DefaultRunTimeout
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Provider{
		compiler:      newCompiler(cfg.CodeRoot),
		deterministic: cfg.Deterministic,
		runTimeout:    cfg.RunTimeout,
		logf:          logf,
	}
}

func (p *Provider) ID() string { return providerID }

// Capabilities: code-js streams events but has no LLM-shaped knobs — no native
// cache, no parallel tool calls (one frontier at a time), no thinking/effort.
func (p *Provider) Capabilities() providers.Capabilities {
	// UnboundedIterations: a code-agent's run() makes an arbitrary number of
	// SEQUENTIAL tool calls, each a loop turn; the MaxIterations soft-cap is
	// unusable here. The run is bounded by the run-level wall-clock deadline
	// (see Call/interruptWatch), not by an iteration count.
	return providers.Capabilities{Streaming: true, UnboundedIterations: true}
}

// Probe always succeeds — code-js is in-process, always reachable.
func (p *Provider) Probe(ctx context.Context) error { return nil }

// ListModels returns empty (not nil): code-js is reachable, but its "models"
// are agent JS files resolved at run time, not a fixed enumerable list.
func (p *Provider) ListModels(ctx context.Context) ([]string, error) {
	return []string{}, nil
}

// Compile validates that an agent's index.js exists and parses, returning its
// content hash. Called by the AgentDef loader at load time so a broken
// code-agent fails the load (not the first fire) and the hash is available for
// AgentDef lineage / the provider.code_hash OTEL attribute.
func (p *Provider) Compile(agentName string) (hash string, err error) {
	c, err := p.compiler.load(agentName)
	if err != nil {
		return "", err
	}
	return c.hash, nil
}

// Call runs the agent's JS for one turn and streams events. A code-agent run
// is a SEQUENCE of Call invocations driven by the loop: each Call replays the
// tool results recorded in req.Messages (fast-forward, no dispatch), then —
//   - reaches an un-recorded tool call → emits EventToolCall + StopReason
//     "tool_use"; the loop dispatches and re-invokes Call with the result
//     appended, advancing the frontier by one.
//   - run() returns → emits the final text + EventDone "end_turn".
//   - run() throws → EventError, kind code_agent_threw.
//
// The runtime is built and discarded WITHIN this Call — nothing is held across
// the loop's dispatch gap. The provider never imports internal/tools and never
// dispatches a tool itself.
func (p *Provider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	out := make(chan providers.Event)

	meta, _ := providers.RunMetaFromContext(ctx)
	prog, err := p.compiler.load(meta.AgentName)
	if err != nil {
		go emitErr(out, "code_agent_load: "+err.Error())
		return out, nil
	}
	seed, anchorMs := p.determinism(meta)
	// budget bounds THIS turn's wall-clock — but it is the WHOLE RUN's remaining
	// budget, not a fresh per-turn timeout: derived from the stable run start
	// (RunMeta.StartedAt) so the sum across all replay turns can never exceed
	// RunTimeout. This is what lets the loop exempt code-js from the
	// MaxIterations cap (Capabilities().UnboundedIterations) and rely on the
	// timeout as the sole bound. Falls back to a flat per-turn RunTimeout when
	// StartedAt is unstamped (direct, non-loop callers / tests).
	budget := p.runTimeout
	if !meta.StartedAt.IsZero() {
		budget = time.Until(meta.StartedAt.Add(p.runTimeout))
	}
	// Emit a loomcycle.provider.call span for parity with the real LLM drivers
	// (which each open one). The synthetic provider makes no HTTP request, so
	// this is the canonical place to attach provider.code_hash (RFC J Decision
	// 9) — operators can answer "which index.js version produced this run" and
	// filter synthetic-code runs via provider.kind. Span is ended in runTurn.
	spanCtx, span := lcotel.RecordProviderCall(ctx, lcotel.ProviderCallAttrs{
		Provider: providerID,
		Model:    syntheticModel,
		Kind:     "synthetic-code",
		CodeHash: prog.hash,
	})
	go p.runTurn(spanCtx, out, span, prog.prog, buildInput(req, meta), extractRecorded(req), toolNames(req), seed, anchorMs, budget)
	return out, nil
}

// runTurn executes one replay turn on its own (short-lived) goroutine: build
// runtime → harden + hook → bind → run() → emit the turn's outcome → close.
// The goroutine lives only for the JS execution (µs–ms), never across a
// dispatch gap.
func (p *Provider) runTurn(ctx context.Context, out chan providers.Event, span trace.Span, prog *goja.Program, input map[string]any, recorded []toolRecord, allowed []string, seed uint32, anchorMs int64, budget time.Duration) {
	defer close(out)
	defer span.End()

	rt := goja.New()
	rt.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	hardenSandbox(rt, seed, anchorMs)

	state := &replayState{rt: rt, recorded: recorded}
	buildBindFunc(allowed)(rt, state.emit)

	// Interrupt the runtime on ctx cancel / per-turn timeout. A replay turn is
	// short, but a CPU-bound JS loop (while(true){}) executes bytecode and so
	// is interruptible. Stopped as soon as the turn finishes.
	stop := make(chan struct{})
	defer close(stop)
	go p.interruptWatch(ctx, rt, stop, budget)

	if _, err := rt.RunProgram(prog); err != nil {
		out <- errorEvent(p.classifyRunErr(state, ctx, err, "evaluating index.js"))
		return
	}
	runFn, ok := goja.AssertFunction(rt.Get("run"))
	if !ok {
		out <- providers.Event{Type: providers.EventError, Error: "code_agent_threw: index.js defines no top-level run(input) function"}
		return
	}

	ret, err := runFn(goja.Undefined(), rt.ToValue(input))
	if err != nil {
		// A frontier or divergence Interrupt surfaces here as the error.
		if state.frontier != nil {
			fr := state.frontier
			id := fmt.Sprintf("cj-%d-%d", p.counter.Add(1), fr.idx)
			out <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: fr.name, Input: fr.input}}
			out <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: zeroUsage()}
			return
		}
		out <- errorEvent(p.classifyRunErr(state, ctx, err, "run"))
		return
	}

	if final := extractFinalText(rt, ret); final != "" {
		out <- providers.Event{Type: providers.EventText, Text: final}
	}
	out <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: zeroUsage()}
}

// classifyRunErr maps a non-frontier run() error to a code-agent error string.
func (p *Provider) classifyRunErr(state *replayState, ctx context.Context, err error, where string) string {
	switch {
	case state.diverged != nil:
		d := state.diverged
		return fmt.Sprintf("code_agent_replay_divergence: tool call #%d was %q on a prior turn but %q on replay — non-deterministic control flow; set LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=1 or remove unhooked non-determinism", d.idx, d.expected, d.got)
	case ctx.Err() != nil:
		return "code_agent_cancelled: " + ctx.Err().Error()
	default:
		return fmt.Sprintf("code_agent_threw: %s: %s", where, err)
	}
}

// interruptWatch Interrupts the runtime if the turn's ctx is cancelled or the
// remaining run budget elapses, then exits when the turn finishes (stop
// closed). budget is the WHOLE RUN's remaining wall-clock (see Call), so the
// last turn to cross the run deadline is interrupted — the run total can't
// exceed RunTimeout even with the loop's iteration cap disabled.
func (p *Provider) interruptWatch(ctx context.Context, rt *goja.Runtime, stop <-chan struct{}, budget time.Duration) {
	if budget <= 0 {
		// Run already over its total budget — interrupt this turn at once.
		budget = time.Millisecond
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		rt.Interrupt(ctx.Err())
	case <-timer.C:
		rt.Interrupt(context.DeadlineExceeded)
	case <-stop:
	}
}

// determinism returns the (seed, anchorMs) for a run. Always-on within-run
// determinism: seed from the run id, anchor from the run start. Under the
// deterministic flag, both are fixed across all runs (cross-run reproducible).
func (p *Provider) determinism(meta providers.RunMeta) (uint32, int64) {
	if p.deterministic {
		return fixedSeed, fixedEpochMs
	}
	seed := fixedSeed
	if meta.RunID != "" || meta.AgentName != "" {
		seed = hash32(meta.RunID + "|" + meta.AgentName)
	}
	anchor := fixedEpochMs
	if !meta.StartedAt.IsZero() {
		anchor = meta.StartedAt.UnixMilli()
	}
	return seed, anchor
}

func hash32(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// buildInput assembles the JS run(input) argument: the latest user prompt text
// plus a metadata object. Credentials are deliberately absent (RFC F).
func buildInput(req providers.Request, meta providers.RunMeta) map[string]any {
	return map[string]any{
		"prompt": latestUserText(req),
		"metadata": map[string]any{
			"user_id": meta.UserID,
			"agent":   meta.AgentName,
		},
	}
}

// latestUserText concatenates the text blocks of the most recent user-role
// message (the fresh prompt on the first turn).
func latestUserText(req providers.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		var sb strings.Builder
		for _, b := range m.Content {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	return ""
}

// toolNames returns the names of the tools the loop exposed to this run
// (from the agent's allowed_tools). The bindings register ONLY these.
func toolNames(req providers.Request) []string {
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		names = append(names, t.Name)
	}
	return names
}

func zeroUsage() *providers.Usage { return &providers.Usage{Model: syntheticModel} }

func errorEvent(msg string) providers.Event {
	return providers.Event{Type: providers.EventError, Error: msg}
}

func emitErr(out chan providers.Event, msg string) {
	out <- providers.Event{Type: providers.EventError, Error: msg}
	close(out)
}

// compile-time assertion that the provider satisfies the interface.
var _ providers.Provider = (*Provider)(nil)
