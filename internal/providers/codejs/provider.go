package codejs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// providerID is the stable wire identity of the synthetic code provider.
const providerID = "code-js"

// syntheticModel is the model string reported on usage/OTEL for every
// code-agent run (RFC J Decision 9). Token counters are always zero.
const syntheticModel = "loomcycle/code-js"

// DefaultRunTimeout bounds a code-agent's wall-clock when the operator sets
// no LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS. It is the ctx deadline that
// caps a run parked in a slow tool call AND the leak backstop for an
// abandoned continuation.
const DefaultRunTimeout = 120 * time.Second

// Config is the provider's construction-time configuration, sourced from the
// LOOMCYCLE_CODE_AGENTS_* env knobs in cmd/loomcycle/main.go.
type Config struct {
	CodeRoot      string // resolved $LOOMCYCLE_CODE_AGENTS_ROOT (default ./agent_code)
	Deterministic bool   // LOOMCYCLE_CODE_AGENTS_DETERMINISTIC
	RunTimeout    time.Duration
	Logf          func(format string, args ...any)
}

// Provider is the RFC J synthetic code-js Provider. One instance is shared by
// every code-agent; it resolves each agent's JS from the RunMeta agent name
// on ctx. Unlike the LLM drivers it is STATEFUL across Call invocations (it
// holds the parked JS continuation of each in-flight run) — see abi.go.
type Provider struct {
	compiler      *compiler
	deterministic bool
	runTimeout    time.Duration
	logf          func(string, ...any)

	counter atomic.Uint64 // mints run-scoped continuation tokens

	mu    sync.Mutex
	conts map[string]*continuation
}

// New builds the provider. RunTimeout falls back to DefaultRunTimeout when
// unset.
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
		conts:         make(map[string]*continuation),
	}
}

func (p *Provider) ID() string { return providerID }

// Capabilities: code-js streams events but has no LLM-shaped knobs — no
// native cache, no parallel tool calls (one suspend point at a time, RFC J
// sharp edge), no thinking, no effort.
func (p *Provider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

// Probe always succeeds — code-js is in-process, always reachable. It runs no
// network round-trip.
func (p *Provider) Probe(ctx context.Context) error { return nil }

// ListModels returns empty (not nil): code-js is reachable, but its "models"
// are agent JS files resolved at run time, not a fixed enumerable list. The
// provider is selected via an explicit pin (provider: code-js), which does
// not consult the availability matrix.
func (p *Provider) ListModels(ctx context.Context) ([]string, error) {
	return []string{}, nil
}

// Compile validates that an agent's index.js exists and parses, returning its
// content hash. Called by the AgentDef loader at load time so a broken
// code-agent fails the load (not the first scheduled fire), and so the hash
// is available for AgentDef lineage / the provider.code_hash OTEL attribute.
func (p *Provider) Compile(agentName string) (hash string, err error) {
	c, err := p.compiler.load(agentName)
	if err != nil {
		return "", err
	}
	return c.hash, nil
}

// Call runs (fresh) or RESUMES (continuation) the agent's JS and streams
// events. A code-agent run is a SEQUENCE of Call invocations: the first
// starts run(); each subsequent one carries the prior tool's result (as a
// tool_result in req.Messages, exactly as an LLM continuation does) and
// resumes the parked JS. The provider never dispatches a tool itself.
func (p *Provider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	out := make(chan providers.Event)

	if token, res, isResume := classifyRequest(req); isResume {
		c := p.get(token)
		if c == nil {
			// The continuation is gone: the run was resumed in a different
			// process or replayed from a persisted transcript. code-agents
			// hold in-process JS state and are NOT resumable that way — fail
			// loud rather than silently restarting run() from the top (which
			// would re-execute side effects).
			go emitErr(out, "code_agent_continuation_lost: this code-agent run cannot be resumed (continuation not in this process — code-agents are not resumable across restart or transcript replay)")
			return out, nil
		}
		if c.pending == nil || res.id != c.pendingID {
			// c.pending is nil only if the loop resumes before the suspend
			// turn parked (impossible on a legitimate run) or a crafted
			// transcript forged a "cj-" tool_result. Either way, refuse
			// rather than nil-deref c.pending.resp below.
			go emitErr(out, fmt.Sprintf("code_agent_protocol_error: tool_result %q does not match the parked call %q", res.id, c.pendingID))
			return out, nil
		}
		// Deliver the loop-dispatched result to the parked JS. Buffered(1), so
		// this never blocks. Safe without a lock: the loop does not invoke
		// this resume Call until the suspend Call's channel closed.
		c.pending.resp <- toolResp{text: res.text, isError: res.isError}
		go p.pump(c, out)
		return out, nil
	}

	// Fresh start: resolve the agent's JS, build the input, launch the run.
	meta, _ := providers.RunMetaFromContext(ctx)
	prog, err := p.compiler.load(meta.AgentName)
	if err != nil {
		go emitErr(out, "code_agent_load: "+err.Error())
		return out, nil
	}
	token := strconv.FormatUint(p.counter.Add(1), 10)
	c := newContinuation(ctx, token, prog.prog, buildInput(req, meta), p.deterministic, p.runTimeout, buildBindFunc(toolNames(req)))
	p.put(token, c)
	go p.watch(c)
	go p.pump(c, out)
	return out, nil
}

// pump waits for the runtime goroutine to either request a tool (→ emit
// EventToolCall + StopReason tool_use, the loop will dispatch and resume) or
// settle (→ final text + end_turn, or EventError). It closes out exactly
// once. The loop's one-Call-at-a-time drive serializes access to c.pending /
// c.seq, so no lock is needed on them.
func (p *Provider) pump(c *continuation, out chan providers.Event) {
	defer close(out)
	select {
	case req := <-c.dispatch:
		c.seq++
		id := fmt.Sprintf("cj-%s-%d", c.token, c.seq)
		c.pending = req
		c.pendingID = id
		out <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: req.name, Input: req.input}}
		out <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: zeroUsage()}
	case oc := <-c.done:
		p.remove(c.token)
		c.teardown()
		if oc.err != nil {
			out <- providers.Event{Type: providers.EventError, Error: "code_agent_threw: " + oc.err.Error()}
			return
		}
		if oc.finalText != "" {
			out <- providers.Event{Type: providers.EventText, Text: oc.finalText}
		}
		out <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: zeroUsage()}
	}
}

// watch is the cancel + leak backstop. It blocks until the run ctx is
// cancelled or hits its deadline, then Interrupts the runtime (breaking a
// CPU-bound JS loop that is executing bytecode — Interrupt cannot reach a JS
// frame parked in a tool call, goja issue #97, but callTool's ctx select
// handles that case) and removes the continuation from the registry. runCtx
// always eventually fires (it carries the run timeout), so watch always
// terminates — no watcher leak.
func (p *Provider) watch(c *continuation) {
	<-c.runCtx.Done()
	// Only Interrupt if the runtime is still executing. On the normal
	// completion path the runtime goroutine has already settled (and pump
	// called cancel(), which woke us) — interrupting an exited runtime is a
	// harmless no-op today, but skipping it avoids touching c.rt after its
	// goroutine is gone (no reliance on goja's post-exit Interrupt behavior).
	if !c.settled.Load() {
		c.rt.Interrupt(c.runCtx.Err())
	}
	p.remove(c.token)
}

func (p *Provider) put(token string, c *continuation) {
	p.mu.Lock()
	p.conts[token] = c
	p.mu.Unlock()
}

func (p *Provider) get(token string) *continuation {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conts[token]
}

func (p *Provider) remove(token string) {
	p.mu.Lock()
	delete(p.conts, token)
	p.mu.Unlock()
}

// inFlight reports the number of live continuations — used by tests to assert
// the one-goroutine-per-run resource model is released on completion/cancel.
func (p *Provider) inFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conts)
}

// resultInfo is the tool_result extracted from a resume Request.
type resultInfo struct {
	id      string
	text    string
	isError bool
}

// classifyRequest decides whether req is a fresh start or a resume. A resume
// carries, as the most recent tool_result content block, a tool_use id this
// provider minted (prefix "cj-"). Returns (token, result, true) on resume.
func classifyRequest(req providers.Request) (string, resultInfo, bool) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		for j := len(m.Content) - 1; j >= 0; j-- {
			b := m.Content[j]
			if b.Type != "tool_result" {
				continue
			}
			if !strings.HasPrefix(b.ToolUseID, "cj-") {
				// A non-code-js tool_result shouldn't appear in a code-agent
				// transcript; ignore and keep scanning.
				continue
			}
			return tokenFromID(b.ToolUseID), resultInfo{id: b.ToolUseID, text: b.Text, isError: b.IsError}, true
		}
	}
	return "", resultInfo{}, false
}

// tokenFromID extracts the run token from a minted id "cj-<token>-<seq>".
func tokenFromID(id string) string {
	trimmed := strings.TrimPrefix(id, "cj-")
	if dash := strings.LastIndex(trimmed, "-"); dash >= 0 {
		return trimmed[:dash]
	}
	return trimmed
}

// buildInput assembles the JS run(input) argument: the latest user prompt
// text plus a metadata object. Credentials are deliberately absent (RFC F).
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
// (computed from the agent's allowed_tools). The bindings register ONLY
// these — default-deny by construction.
func toolNames(req providers.Request) []string {
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		names = append(names, t.Name)
	}
	return names
}

func zeroUsage() *providers.Usage {
	return &providers.Usage{Model: syntheticModel}
}

func emitErr(out chan providers.Event, msg string) {
	out <- providers.Event{Type: providers.EventError, Error: msg}
	close(out)
}

// compile-time assertion that the provider satisfies the interface.
var _ providers.Provider = (*Provider)(nil)
