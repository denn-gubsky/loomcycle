package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/statepatch"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// --- RFC CR L2: structured execution state (the `context.mode: stateful` loop) ---
//
// Instead of appending history, the model is fed only (P = the preamble/system,
// Σ = the structured state object, O = the latest observation) and emits, via one
// `emit_state` tool call, (reasoning, patch, action). The runtime validates the
// patch against the state schema, merges it into Σ with null-deletion, discards
// the reasoning, executes the named action to produce the next observation, and
// loops. Cost is O(T): the fed prompt never grows. The full event stream (each
// EventContextState marker) is still persisted for audit.
//
// This is a self-contained loop, deliberately separate from the append/recap
// Run() body so it cannot regress the shipped path. PR1 scope: autonomous runs.
// Interactive steering, pause/park, and cross-instance resume of a stateful run
// are not wired here yet (a stateful run is short-horizon-per-step and re-derives
// cheaply); they compose on top later.

// contextStatefulMode reports whether the resolved policy selects L2 stateful.
func contextStatefulMode(cx *config.Context) bool {
	return cx != nil && cx.Mode != nil && *cx.Mode == config.ContextModeStateful
}

// contextAutoMode reports whether the policy is tier-routed (mode: auto).
func contextAutoMode(cx *config.Context) bool {
	return cx != nil && cx.Mode != nil && *cx.Mode == config.ContextModeAuto
}

// resolveAutoContextMode turns mode:auto into a concrete mode (RFC CR tier-
// routing): a local backend → recap (schema-free, safe for a weaker model), a
// frontier API → stateful. An interactive run never resolves to stateful — that
// loop has no steer/park — so it takes recap regardless of tier. Returns a CLONE
// carrying the concrete mode so the shared agent def is never mutated.
func resolveAutoContextMode(cx *config.Context, local, interactive bool) *config.Context {
	mode := config.ContextModeStateful
	if local || interactive {
		mode = config.ContextModeRecap
	}
	out := cx.Clone()
	out.Mode = &mode
	return out
}

const emitStateToolName = "emit_state"

// emitStateToolSpec is the ONLY tool a stateful step offers: the model must call
// it, which is how the runtime reliably gets a structured {reasoning, patch,
// action} object without a provider-specific forced-output primitive.
func emitStateToolSpec() providers.ToolSpec {
	return providers.ToolSpec{
		Name: emitStateToolName,
		Description: "Advance the task by emitting your state update and next action. Call this EXACTLY ONCE per step. " +
			"`patch` is a JSON merge-patch applied to the state object (a null value deletes a key) — keep the state the single " +
			"source of truth for everything you must remember. `action` names the next tool to run; omit it (or set done=true) to " +
			"finish, putting your answer in `final`.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "reasoning": {"type": "string", "description": "your step reasoning; it is discarded after this step, so record durable facts in the patch instead"},
    "patch": {"type": "object", "description": "a JSON merge-patch applied to the state; a null value deletes a key"},
    "action": {"type": "object", "properties": {"tool": {"type": "string"}, "input": {"type": "object"}}, "description": "the next tool to run; omit to finish"},
    "done": {"type": "boolean", "description": "true when the task is complete"},
    "final": {"type": "string", "description": "the final answer text, when done"},
    "propose_schema": {"type": "object", "description": "OPTIONAL: propose a JSON-Schema (object with typed properties) that this task's state should conform to, when no schema is set or a better one is apparent. It is recorded for an operator to review and adopt; it does NOT take effect this run."}
  },
  "required": ["patch"]
}`),
	}
}

type emitStateOut struct {
	Reasoning     string         `json:"reasoning"`
	Patch         map[string]any `json:"patch"`
	Action        *stateAction   `json:"action"`
	Done          bool           `json:"done"`
	Final         string         `json:"final"`
	ProposeSchema map[string]any `json:"propose_schema"`
}

type stateAction struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
}

func parseEmitState(input json.RawMessage) (*emitStateOut, error) {
	var out emitStateOut
	if err := json.Unmarshal(input, &out); err != nil {
		return nil, fmt.Errorf("emit_state was not valid JSON: %w", err)
	}
	if out.Patch == nil {
		out.Patch = map[string]any{} // a step with no state change is legal
	}
	return &out, nil
}

func actionName(es *emitStateOut) string {
	if es.Action == nil {
		return ""
	}
	return es.Action.Tool
}

// buildStatefulSystem augments the resolved preamble P with the state protocol
// instructions, the available action-tool catalog, and the state schema.
func buildStatefulSystem(base []providers.ContentBlock, toolSpecs []providers.ToolSpec, schema map[string]any) []providers.ContentBlock {
	var b strings.Builder
	b.WriteString("\n\n## Structured execution mode\n")
	b.WriteString("You run in structured-state mode. You do NOT call the task tools directly. Each step you are shown the current state (a JSON object) and the latest observation; respond by calling `emit_state` exactly once:\n")
	b.WriteString("- `reasoning`: your thinking for this step. It is DISCARDED afterwards, so put anything you must remember into the patch.\n")
	b.WriteString("- `patch`: a JSON merge-patch applied to the state. Set keys to record progress; a null value deletes a key.\n")
	b.WriteString("- `action`: the next tool to run, as {\"tool\": <name>, \"input\": {…}}. The runtime executes it and hands you its output as the next observation.\n")
	b.WriteString("- Finish by omitting `action` (or setting `done: true`) and putting your answer in `final`.\n")
	if len(toolSpecs) > 0 {
		b.WriteString("\n### Action tools you may name\n")
		for _, t := range toolSpecs {
			fmt.Fprintf(&b, "- `%s`: %s\n", t.Name, oneLineDesc(t.Description))
		}
	}
	if len(schema) > 0 {
		if sj, err := json.Marshal(schema); err == nil {
			fmt.Fprintf(&b, "\n### State schema (the state, and every patch, must conform)\n%s\n", string(sj))
		}
	}
	out := append([]providers.ContentBlock(nil), base...)
	return append(out, providers.ContentBlock{Type: "text", Text: b.String()})
}

func oneLineDesc(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return strings.TrimSpace(s)
}

// statefulUserMessage renders the fed context for one step: only Σ + O (no
// history). This is the whole point — the prompt stays flat over the horizon.
func statefulUserMessage(sigma map[string]any, obs string) providers.Message {
	sj, _ := json.Marshal(sigma)
	var b strings.Builder
	fmt.Fprintf(&b, "Current state:\n%s\n\nLatest observation:\n%s", string(sj), obs)
	return providers.Message{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: b.String()}}}
}

// initialObservation renders the run's task (the seed segments) as the first
// observation O_0.
func initialObservation(msgs []providers.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		for _, c := range m.Content {
			if c.Type == "text" && c.Text != "" {
				b.WriteString(c.Text)
				b.WriteString("\n")
			}
		}
	}
	s := strings.TrimSpace(b.String())
	if s == "" {
		return "(no task provided)"
	}
	return "Task: " + s
}

// errNoEmitState is the model answering the wrong SHAPE, as opposed to the
// provider or the transport failing.
//
// ⚠️ THE DISTINCTION IS THE WHOLE POINT. A 500, a dropped socket or a refused
// request is nothing the model can be asked to fix, so it terminates the run.
// "You replied in prose instead of calling the tool" is a correctable mistake
// of exactly the kind the rejected-patch path already re-prompts for, and
// treating the two identically is what turned one conversational reply into a
// dead run on step zero.
var errNoEmitState = errors.New("model did not call emit_state")

// emitStateCall is one step's raw result: the tool input when the model complied,
// and — when it did not — what it produced instead.
type emitStateCall struct {
	input json.RawMessage
	// text is the assistant's prose. Kept because it is BOTH halves of the fix:
	// the retry shows the model its own reply, and the terminal error can say
	// what the run actually got rather than only what it wanted.
	text string
	// thinking is a reasoning trace, kept SEPARATE and never replayed. A model
	// whose entire output was thinking looks identical from outside to one that
	// emitted nothing, and those call for different answers — raise the budget
	// versus change the model. (The same confusion produced the silent recap
	// failure: a thinking model's output never reached the text accumulator.)
	thinking string
	usage    *providers.Usage
}

// callForEmitState makes one provider call and returns the emit_state tool input
// plus whatever else the model produced. NO OnEvent hook is set — the returned
// channel is the event source (mirrors summarizeWith); the tool call is what
// matters, but the text is no longer thrown away.
func callForEmitState(ctx context.Context, provider providers.Provider, req providers.Request) (emitStateCall, error) {
	var out emitStateCall
	ch, err := provider.Call(ctx, req)
	if err != nil {
		return out, err
	}
	var text, thinking strings.Builder
	var streamErr string
	for ev := range ch {
		switch ev.Type {
		case providers.EventToolCall:
			if ev.ToolUse != nil && ev.ToolUse.Name == emitStateToolName && out.input == nil {
				out.input = ev.ToolUse.Input
			}
		case providers.EventText:
			text.WriteString(ev.Text)
		case providers.EventThinking:
			thinking.WriteString(ev.Text)
		case providers.EventDone:
			out.usage = ev.Usage
		case providers.EventError:
			streamErr = ev.Error
		}
	}
	out.text = strings.TrimSpace(text.String())
	out.thinking = strings.TrimSpace(thinking.String())
	if streamErr != "" {
		return out, errors.New(streamErr)
	}
	if len(out.input) == 0 {
		return out, errNoEmitState
	}
	return out, nil
}

// producedInstead describes, for an operator, what came back when the tool call
// did not — the difference between "answered the wrong way", "only thought" and
// "returned nothing at all", which are three different problems.
func producedInstead(c emitStateCall) string {
	switch {
	case c.text != "":
		return fmt.Sprintf("it replied with %d characters of prose instead: %q", len(c.text), snippet(c.text))
	case c.thinking != "":
		return fmt.Sprintf("it produced only a reasoning trace (%d characters) and no reply: %q",
			len(c.thinking), snippet(c.thinking))
	default:
		return "it returned no content at all"
	}
}

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func addUsage(dst *providers.Usage, u *providers.Usage) {
	if u == nil {
		return
	}
	dst.InputTokens += u.InputTokens
	dst.OutputTokens += u.OutputTokens
	dst.CacheCreationTokens += u.CacheCreationTokens
	dst.CacheReadTokens += u.CacheReadTokens
	if u.Model != "" {
		dst.Model = u.Model
	}
}

func applyStatefulSampling(req *providers.Request, s *config.Sampling) {
	if s == nil {
		return
	}
	req.Temperature = s.Temperature
	req.TopP = s.TopP
	req.TopK = s.TopK
	req.FrequencyPenalty = s.FrequencyPenalty
	req.PresencePenalty = s.PresencePenalty
	req.Seed = s.Seed
	req.Stop = s.Stop
}

// runStateful executes the L2 loop. `system` is the resolved preamble P (already
// split from opts.Segments); `initial` is the seed conversation (the task);
// `toolSpecs` is the action-tool catalog; `emit` forwards + persists events.
func runStateful(ctx context.Context, opts RunOptions, system []providers.ContentBlock, initial []providers.Message, toolSpecs []providers.ToolSpec, emit func(providers.Event)) (RunResult, error) {
	cx := opts.Context
	var schema map[string]any
	onInvalid := config.ContextDefaultOnInvalidPatch
	maxRetries := config.ContextDefaultMaxPatchRetries
	if cx != nil {
		schema = cx.StateSchema
		if cx.OnInvalidPatch != nil {
			onInvalid = *cx.OnInvalidPatch
		}
		if cx.MaxPatchRetries != nil {
			maxRetries = *cx.MaxPatchRetries
		}
	}
	maxIter := opts.MaxIterations
	if maxIter <= 0 {
		maxIter = 16
	}

	statefulSystem := buildStatefulSystem(system, toolSpecs, schema)
	emitTool := []providers.ToolSpec{emitStateToolSpec()}

	sigma := map[string]any{}
	// lastWritten records which step last touched each Σ key, so eviction can
	// break ties by recency WITHIN a retention class. The class is the
	// operator's statement of what matters; recency only orders equals.
	lastWritten := map[string]int{}
	// ⚠️ STATEFUL HAD NO FOOTPRINT AT ALL. The transcript is rebuilt from
	// (Σ, observation) each step so it cannot accumulate — which is why the
	// gate was never wired here — but Σ ITSELF accumulates, and
	// statefulUserMessage serialises the whole of it into the prompt every
	// step. Nothing measured it, nothing bounded it, and the run grew until
	// the provider refused.
	preambleTokens := estimatePreambleTokens(system, toolSpecs)
	lastIn := preambleTokens + sigmaTokens(sigma)
	lastWindow := effectiveWindow(0, opts)
	// Same rule as the append/recap gate: EVICT on the estimate if you must,
	// but only REPORT a number a provider returned. A driver that reports no
	// usage would otherwise leave lastIn at the preamble estimate forever and
	// tell the operator its state cannot be reduced — about a Σ that is empty.
	footprintMeasured := false
	seenExhaustedState := map[int]bool{}
	holder := &tools.ExecStateHolder{Sigma: sigma}
	dispatchCtx := tools.WithExecutionState(ctx, holder) // the action sees the live Σ (Context op=state)

	obs := initialObservation(initial)
	var total providers.Usage
	var lastProposed map[string]any // the last schema the model proposed that differs from the active one

	for iter := 0; iter < maxIter; iter++ {
		if err := ctx.Err(); err != nil {
			return RunResult{StopReason: "cancelled", Iterations: iter, Usage: total, State: sigma}, err
		}
		msgs := []providers.Message{statefulUserMessage(sigma, obs)}
		var es *emitStateOut
		for attempt := 0; ; attempt++ {
			// RFC DG: say on the WIRE what the system prompt has only ever
			// asked for. Narrowing `tools` to emit_state was never enough —
			// the run that produced this RFC had exactly one tool offered and
			// replied in prose anyway.
			//
			// A provider without the parameter DROPS it and the step proceeds
			// unforced, which is why this is an optimisation and not a
			// precondition: the contract still lives in the prompt, and a
			// model that ignores it is still re-prompted rather than fatal.
			req := providers.Request{Model: opts.Model, System: statefulSystem, Messages: msgs, Tools: emitTool, MaxTokens: opts.MaxTokens, Effort: opts.Effort,
				ToolChoice: providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: emitStateToolName}}
			applyStatefulSampling(&req, opts.Sampling)
			call, err := callForEmitState(ctx, opts.Provider, req)
			input, usage := call.input, call.usage
			addUsage(&total, usage)
			if usage != nil {
				// The provider's own count of the whole request, which is what
				// the eviction threshold must measure against — the same
				// numerator the append/recap gate uses.
				if in := usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens; in > 0 {
					lastIn = in
					footprintMeasured = true
				}
				lastWindow = effectiveWindow(usage.MaxContextTokens, opts)
			}
			if err != nil && !errors.Is(err, errNoEmitState) {
				// Transport or provider fault — nothing the model can correct.
				emit(providers.Event{Type: providers.EventError, Error: "stateful step failed: " + err.Error()})
				return RunResult{StopReason: "error", Iterations: iter, Usage: total, State: sigma}, err
			}
			if err != nil {
				// ⚠️ THE SAME BUDGET AS A REJECTED PATCH, and it used to get
				// none. `on_invalid_patch` / `max_patch_retries` covered a patch
				// that arrived and failed validation; a model that answered in
				// prose — the ordinary behaviour of a local model handed a
				// conversational question — got one shot and killed the run on
				// step zero, with its reply discarded unread.
				//
				// Both are the same condition from the runtime's side: the
				// output was not usable, and the model is the one who can fix
				// it. So it is re-prompted with what it actually said.
				if onInvalid == "fail" || attempt >= maxRetries {
					msg := fmt.Sprintf("stateful step failed: model did not call emit_state after %d attempt(s) — %s. "+
						"context.mode: stateful requires a model that reliably emits tool calls; "+
						"raise context.max_patch_retries, or move this agent to a model that does%s",
						attempt+1, producedInstead(call), unforcedNote(opts))
					emit(providers.Event{Type: providers.EventError, Error: msg})
					return RunResult{StopReason: "error", Iterations: iter, Usage: total, State: sigma}, errors.New(msg)
				}
				// Show it its own reply, then say what was required. An EMPTY
				// assistant turn is never appended — the providers reject one,
				// so a model that returned nothing gets the instruction alone.
				// A reasoning trace is not replayed either: it is not an
				// assistant turn the API will accept back.
				if call.text != "" {
					msgs = append(msgs, providers.Message{Role: "assistant",
						Content: []providers.ContentBlock{{Type: "text", Text: call.text}}})
				}
				msgs = append(msgs, providers.Message{Role: "user",
					Content: []providers.ContentBlock{{Type: "text", Text: "That reply was not usable: in structured " +
						"execution mode the ONLY accepted response is a single `emit_state` tool call, and prose is " +
						"discarded. Send the same content again as `emit_state` — put your answer in `final` with " +
						"`done: true` if you are finished, otherwise name the next `action`."}}})
				continue
			}
			parsed, perr := parseEmitState(input)
			var verr error
			if perr == nil {
				verr = statepatch.ValidatePatch(schema, parsed.Patch)
			}
			if perr != nil || verr != nil {
				cause := perr
				if cause == nil {
					cause = verr
				}
				if onInvalid == "fail" || attempt >= maxRetries {
					msg := fmt.Sprintf("stateful patch rejected after %d attempt(s): %v", attempt+1, cause)
					emit(providers.Event{Type: providers.EventError, Error: msg})
					return RunResult{StopReason: "invalid_patch", Iterations: iter, Usage: total, State: sigma}, errors.New(msg)
				}
				// Rollback-retry: show the model its rejected emit_state + the reason,
				// as a proper tool_use/tool_result pair, and ask for a correction.
				tid := fmt.Sprintf("es-%d-%d", iter, attempt)
				msgs = append(msgs,
					providers.Message{Role: "assistant", Content: []providers.ContentBlock{{Type: "tool_use", ToolUseID: tid, ToolName: emitStateToolName, ToolInput: input}}},
					providers.Message{Role: "user", Content: []providers.ContentBlock{{Type: "tool_result", ToolUseID: tid, Text: "emit_state rejected: " + cause.Error() + ". Emit a corrected emit_state."}}})
				continue
			}
			es = parsed
			break
		}

		sigma = statepatch.Merge(sigma, es.Patch)
		for k := range es.Patch {
			lastWritten[k] = iter
		}
		holder.Sigma = sigma

		// ⚠️ STRUCTURAL COMPACTION. Summarising is the wrong operation on a
		// state object: Σ is validated against state_schema, and prose is not a
		// Σ — the next patch would have nothing well-formed to merge into. The
		// structural equivalent is EVICTION of the least significant entries.
		var evicted []string
		if aboveBackstop(opts.Compaction, lastIn, lastWindow) && backstopAvailable(opts.Compaction) {
			// The budget is Σ's share of the window. The preamble is fixed for
			// the run and the observation is one step, so Σ is the only part
			// eviction can move.
			budget := lastWindow*compactionKeptTailBudgetPct/100 - preambleTokens
			if plan := sigmaEvictionPlan(sigma, schema, lastWritten, budget); len(plan) > 0 {
				// Bank BEFORE dropping. Σ is the run's working memory, and
				// silently losing it is worse than a large prompt — a later
				// recall must be able to fetch back what was evicted.
				bankEvictedSigma(ctx, opts, emit, sigma, plan)
				for _, k := range plan {
					delete(sigma, k)
					delete(lastWritten, k)
				}
				holder.Sigma = sigma
				evicted = plan
				lastIn = preambleTokens + sigmaTokens(sigma)
			} else if footprintMeasured {
				// Nothing evictable and still over: every key is core, or the
				// preamble alone is the problem. Either way the run is heading
				// for the provider's limit and must say so.
				reportExhausted(emit, seenExhaustedState, opts.Compaction, lastIn, lastWindow,
					[]providers.ContextTierVerdict{{
						Mode:   "stateful",
						Reason: providers.DistillDeclineSplitDeclined,
						Message: "stateful state cannot be reduced: every key is retention " +
							"core (the default) — declare x-retention: scratch or derived on " +
							"the state_schema properties that are safe to drop",
					}})
			}
		}

		// Model-proposed schema (RFC CR): recorded for the operator to review +
		// adopt (by forking the agent def's context.state_schema). INERT — it does
		// not change validation this run. Only surfaced when it differs from the
		// active schema, so an agent restating the adopted schema is not noise.
		var proposed map[string]any
		if len(es.ProposeSchema) > 0 && schemasDiffer(es.ProposeSchema, schema) {
			proposed = es.ProposeSchema
			lastProposed = es.ProposeSchema
		}
		emit(providers.Event{Type: providers.EventContextState,
			ContextState: &providers.ContextStateEventInfo{State: sigma, Patch: es.Patch, Iter: iter, Action: actionName(es), Reasoning: es.Reasoning, ProposedSchema: proposed, Evicted: evicted}})

		// Recall harvest (RFC CT): stateful is the most lossy mode — only Σ and the
		// latest observation are fed, so each step's reasoning + observation are
		// otherwise discarded. Embed them into the run-scoped index so a later
		// Recall(query) can fetch the original detail back. nil-safe when recall off.
		stepSpan := []providers.Message{
			{Role: "assistant", Reasoning: es.Reasoning,
				Content: []providers.ContentBlock{{Type: "text", Text: obs}}},
		}
		opts.RecallIndex.Harvest(ctx, stepSpan)
		// Persistent-memory harvest (RFC CT P2): bank the same evicted step for the
		// consolidator when the agent opted in. No-op unless context.harvest_to_memory.
		harvestToMemory(ctx, opts, emit, stepSpan)

		// Terminal: done flag, or no action named.
		if es.Done || es.Action == nil || strings.TrimSpace(es.Action.Tool) == "" {
			final := es.Final
			if final == "" {
				final = es.Reasoning
			}
			emit(providers.Event{Type: providers.EventText, Text: final})
			emit(providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &total})
			return RunResult{StopReason: "end_turn", FinalText: final, Iterations: iter + 1, Usage: total, State: sigma, ProposedSchema: lastProposed}, nil
		}

		// Execute the named action → next observation.
		if opts.Dispatcher == nil {
			obs = "ERROR: no tools are available to run action " + es.Action.Tool
		} else {
			tid := fmt.Sprintf("es-act-%d", iter)
			emit(providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: tid, Name: es.Action.Tool, Input: es.Action.Input}})
			res := opts.Dispatcher.Execute(dispatchCtx, es.Action.Tool, es.Action.Input)
			emit(providers.Event{Type: providers.EventToolResult, ToolUse: &providers.ToolUse{ID: tid, Name: es.Action.Tool, Input: es.Action.Input}, Text: res.Text, IsError: res.IsError})
			obs = res.Text
			if res.IsError {
				obs = "ERROR: " + res.Text
			}
		}
	}

	emit(providers.Event{Type: providers.EventDone, StopReason: "max_iterations", Usage: &total})
	return RunResult{StopReason: "max_iterations", Iterations: maxIter, Usage: total, State: sigma, ProposedSchema: lastProposed}, nil
}

// schemasDiffer reports whether two schema objects are not JSON-equal (a nil/empty
// active schema differs from any non-empty proposal). Used to suppress a proposal
// that merely restates the already-adopted schema.
func schemasDiffer(a, b map[string]any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return true
	}
	return string(ab) != string(bb)
}

// unforcedNote says whether the run could even ASK for the tool on the wire.
//
// ⚠️ THE TWO FAILURES READ IDENTICALLY WITHOUT IT, and they call for opposite
// next moves. A model that ignored a protocol-level constraint needs replacing;
// one that was never given the constraint may be fine on a provider that has
// one. Ollama has no tool_choice on /api/chat, so a forced request there
// degrades rather than being refused (RFC DG) — which is the right call, and
// exactly the kind of silent degradation this codebase keeps having to make
// visible after the fact.
func unforcedNote(opts RunOptions) string {
	if opts.Provider == nil || opts.Provider.Capabilities().SupportsToolChoice {
		return ""
	}
	return fmt.Sprintf(" (note: provider %q has no tool_choice on the wire, so the call was NOT "+
		"enforced — the tool was requested in the prompt only)", opts.Provider.ID())
}
