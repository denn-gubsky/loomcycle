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
    "final": {"type": "string", "description": "the final answer text, when done"}
  },
  "required": ["patch"]
}`),
	}
}

type emitStateOut struct {
	Reasoning string         `json:"reasoning"`
	Patch     map[string]any `json:"patch"`
	Action    *stateAction   `json:"action"`
	Done      bool           `json:"done"`
	Final     string         `json:"final"`
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

// callForEmitState makes one provider call and returns the emit_state tool input,
// the call's usage, and any error. NO OnEvent hook is set — the returned channel
// is the event source (mirrors summarizeWith); streaming text is ignored, the
// tool call is what matters.
func callForEmitState(ctx context.Context, provider providers.Provider, req providers.Request) (json.RawMessage, *providers.Usage, error) {
	ch, err := provider.Call(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	var input json.RawMessage
	var usage *providers.Usage
	var streamErr string
	for ev := range ch {
		switch ev.Type {
		case providers.EventToolCall:
			if ev.ToolUse != nil && ev.ToolUse.Name == emitStateToolName && input == nil {
				input = ev.ToolUse.Input
			}
		case providers.EventDone:
			usage = ev.Usage
		case providers.EventError:
			streamErr = ev.Error
		}
	}
	if streamErr != "" {
		return nil, usage, errors.New(streamErr)
	}
	if len(input) == 0 {
		return nil, usage, errors.New("model did not call emit_state")
	}
	return input, usage, nil
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
	holder := &tools.ExecStateHolder{Sigma: sigma}
	dispatchCtx := tools.WithExecutionState(ctx, holder) // the action sees the live Σ (Context op=state)

	obs := initialObservation(initial)
	var total providers.Usage

	for iter := 0; iter < maxIter; iter++ {
		if err := ctx.Err(); err != nil {
			return RunResult{StopReason: "cancelled", Iterations: iter, Usage: total, State: sigma}, err
		}
		msgs := []providers.Message{statefulUserMessage(sigma, obs)}
		var es *emitStateOut
		for attempt := 0; ; attempt++ {
			req := providers.Request{Model: opts.Model, System: statefulSystem, Messages: msgs, Tools: emitTool, MaxTokens: opts.MaxTokens, Effort: opts.Effort}
			applyStatefulSampling(&req, opts.Sampling)
			input, usage, err := callForEmitState(ctx, opts.Provider, req)
			addUsage(&total, usage)
			if err != nil {
				emit(providers.Event{Type: providers.EventError, Error: "stateful step failed: " + err.Error()})
				return RunResult{StopReason: "error", Iterations: iter, Usage: total, State: sigma}, err
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
		holder.Sigma = sigma
		emit(providers.Event{Type: providers.EventContextState,
			ContextState: &providers.ContextStateEventInfo{State: sigma, Patch: es.Patch, Iter: iter, Action: actionName(es), Reasoning: es.Reasoning}})

		// Terminal: done flag, or no action named.
		if es.Done || es.Action == nil || strings.TrimSpace(es.Action.Tool) == "" {
			final := es.Final
			if final == "" {
				final = es.Reasoning
			}
			emit(providers.Event{Type: providers.EventText, Text: final})
			emit(providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &total})
			return RunResult{StopReason: "end_turn", FinalText: final, Iterations: iter + 1, Usage: total, State: sigma}, nil
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
	return RunResult{StopReason: "max_iterations", Iterations: maxIter, Usage: total, State: sigma}, nil
}
