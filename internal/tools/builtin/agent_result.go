package builtin

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A spawn's `result` argument: how the child's outcome is handed back.
const (
	// resultText (the default, "" too) is the child's answer as text behind
	// its attribution line — what a parent MODEL reads.
	resultText = "text"
	// resultObject is a JSON object with the child's ids, status, answer and
	// usage — what a parent PROGRAM reads without parsing text.
	resultObject = "object"
)

// checkResultMode validates a spawn's `result` argument. field names it in the
// refusal.
func checkResultMode(field, mode string) (tools.Result, bool) {
	switch mode {
	case "", resultText, resultObject:
		return tools.Result{}, true
	}
	return errValidation(fmt.Sprintf("%s: unknown value %q (want %q or %q)", field, mode, resultText, resultObject),
		"Use \"object\" for a JSON object with the child's status and usage, or omit it for the answer as text."), false
}

// spawnObject is a child's outcome as an object (`result: "object"`).
//
// A child that RAN always produces one, whatever its end: a failed, cancelled,
// timed-out or capped child is reported in `status` and `error`, not raised,
// because a program wants the run id and the usage of the run that failed as
// much as of the one that worked. A child that never started — an unknown
// agent, a refused spawn — has no run to describe and is an error as before.
type spawnObject struct {
	AgentID string `json:"agent_id"`
	RunID   string `json:"run_id"`
	// Status is the run's terminal status — completed, failed, cancelled,
	// rejected — except "timeout" for a child its timeout_ms stopped and
	// "max_iterations" for one that stopped at its iteration limit, as a
	// parallel_spawn row reports them.
	Status     string `json:"status"`
	StopReason string `json:"stop_reason,omitempty"`
	// FinalText is the child's answer with no attribution line. Present for a
	// completed child and for one stopped at its iteration limit (its last
	// answer, which may be incomplete); absent for every other end.
	FinalText string `json:"final_text,omitempty"`
	// Structured is the answer parsed against the child's output_format;
	// State a stateful child's final state.
	Structured map[string]any `json:"structured,omitempty"`
	State      map[string]any `json:"state,omitempty"`
	Usage      spawnUsage     `json:"usage"`
	Error      string         `json:"error,omitempty"`
	// Truncated is set when anything was cut or left out to fit the parent's
	// window; the *_omitted flags say which whole value was left out. The
	// full answer stays on the child's run.
	Truncated         bool `json:"truncated,omitempty"`
	StructuredOmitted bool `json:"structured_omitted,omitempty"`
	StateOmitted      bool `json:"state_omitted,omitempty"`
}

// spawnUsage is what the child's run used. The field names are the ones every
// JSON surface of the runtime gives a run's usage. Which key paid, and what it
// cost, are not here: those are the operator's to see, not the agent's.
type spawnUsage struct {
	InputTokens         int    `json:"input_tokens"`
	OutputTokens        int    `json:"output_tokens"`
	CacheCreationTokens int    `json:"cache_creation_input_tokens"`
	CacheReadTokens     int    `json:"cache_read_input_tokens"`
	Model               string `json:"model,omitempty"`
	Provider            string `json:"provider,omitempty"`
}

// bareAnswer strips the attribution line the text result carries
// ("[sub-agent agent_id=… run_id=…]\n"), leaving the answer and anything a
// subagent_stop hook added after it.
func bareAnswer(output string) string {
	if !strings.HasPrefix(output, "[sub-agent ") {
		return output
	}
	if _, rest, ok := strings.Cut(output, "\n"); ok {
		return rest
	}
	return ""
}

// newSpawnObject builds the object for a child from what its run reported
// (out) and what the runner returned for it: output is the text result AFTER
// the parent's subagent_stop hooks, so what they added is kept and what they
// refused is not handed on. ok is false when no run was created.
func newSpawnObject(out *tools.ChildOutcome, name string, timeoutMs int, output string, timedOut bool, err error) (spawnObject, bool) {
	if out == nil || out.RunID == "" {
		return spawnObject{}, false
	}
	obj := spawnObject{
		AgentID: out.AgentID, RunID: out.RunID, Status: out.Status, StopReason: out.StopReason,
		Usage: spawnUsage{
			InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens,
			CacheCreationTokens: out.Usage.CacheCreationTokens, CacheReadTokens: out.Usage.CacheReadTokens,
			Model: out.Usage.Model, Provider: out.Usage.Provider,
		},
	}
	var capped *ChildCappedError
	switch {
	case timedOut:
		obj.Status, obj.Error = "timeout", ChildTimedOutMessage(name, timeoutMs, out.RunID)
	case errors.As(err, &capped):
		obj.Status, obj.Error = ChildStatusMaxIterations, capped.Error()
		obj.FinalText, obj.Structured, obj.State = bareAnswer(capped.Output), out.Structured, capped.State
	case err != nil:
		obj.Error = err.Error()
		if obj.Status == "completed" {
			// The run completed but its answer did not reach the parent: a
			// subagent_stop hook refused it. "completed" beside no answer
			// would read as a child with nothing to say.
			obj.Status = "rejected"
		}
	default:
		obj.FinalText, obj.Structured, obj.State = bareAnswer(output), out.Structured, out.State
	}
	return obj, true
}

// capTo fits the object into limit characters of the parent's window (0 = no
// window known, nothing is cut). The structured answer and the state are each
// kept whole or left out whole — a JSON value cut mid-structure would not
// parse — and the text takes what is left. The same order a text result uses.
func (o *spawnObject) capTo(limit int) {
	if limit <= 0 {
		return
	}
	if e, cut := cutOnRune(o.Error, limit); cut {
		o.Error, o.Truncated = e, true
	}
	limit -= len(o.Error)
	if n := jsonSize(o.Structured); n > limit {
		o.Structured, o.StructuredOmitted, o.Truncated = nil, true, true
	} else {
		limit -= n
	}
	if n := jsonSize(o.State); n > limit {
		o.State, o.StateOmitted, o.Truncated = nil, true, true
	} else {
		limit -= n
	}
	if text, cut := cutOnRune(o.FinalText, max(limit, 0)); cut {
		o.FinalText, o.Truncated = text, true
	}
}

// result renders the object as the tool result: JSON text, which a program
// receives parsed.
func (o spawnObject) result() tools.Result {
	b, err := json.Marshal(o)
	if err != nil {
		return errFrom(fmt.Sprintf("internal: marshal the sub-agent's result: %s", err), err)
	}
	return tools.Result{Text: string(b)}
}
