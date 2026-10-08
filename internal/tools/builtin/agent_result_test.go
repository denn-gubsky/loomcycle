package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// outcomeRunner stands in for the server's sub-run: it reports an outcome to
// whoever asked for one, and returns what a real child would.
type outcomeRunner struct {
	status, text string
	structured   map[string]any
	state        map[string]any
	err          error
	noRun        bool // the child never started: no run, nothing reported
	sawSink      []bool
}

func (r *outcomeRunner) run(ctx context.Context, name, _, _ string) (string, map[string]any, string, error) {
	sink := tools.ChildOutcomeSink(ctx)
	r.sawSink = append(r.sawSink, sink != nil)
	if r.noRun {
		return "", nil, "", r.err
	}
	if sink != nil {
		*sink = tools.ChildOutcome{
			AgentID: "a_" + name, RunID: "r_" + name, SessionID: "s_" + name, Status: r.status, StopReason: "end_turn",
			FinalText: r.text, Structured: r.structured, State: r.state,
			Usage: providers.Usage{InputTokens: 120, OutputTokens: 45, CacheCreationTokens: 7, CacheReadTokens: 9, Model: "m-1", Provider: "p-1",
				CredentialSource: "operator"},
		}
	}
	if r.err != nil {
		var capped *ChildCappedError
		if errors.As(r.err, &capped) {
			return capped.Output, capped.State, "r_" + name, r.err
		}
		return "", nil, "r_" + name, r.err
	}
	return "[sub-agent agent_id=a_" + name + " run_id=r_" + name + "]\n" + r.text, r.state, "r_" + name, nil
}

func (r *outcomeRunner) tool() *AgentTool {
	return &AgentTool{RunDetailed: r.run, Run: func(ctx context.Context, n, p, d string) (string, error) {
		out, _, _, err := r.run(ctx, n, p, d)
		return out, err
	}}
}

func decodeObject(t *testing.T, text string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		t.Fatalf("the result is not a JSON object (%v): %s", err, text)
	}
	return obj
}

// The request's done-when, at the tool: a spawn that asks for the object gets
// the child's answer with no header, its ids, and its usage, as JSON.
func TestAgentTool_ResultObjectDescribesTheChildsRun(t *testing.T) {
	r := &outcomeRunner{status: "completed", text: `{"score": 7}`, structured: map[string]any{"score": float64(7)}, state: map[string]any{"step": "done"}}
	res := execJSON(t, r.tool(), context.Background(), `{"name":"judge","prompt":"p","result":"object"}`)
	if res.IsError {
		t.Fatalf("spawn: %s", res.Text)
	}
	obj := decodeObject(t, res.Text)
	for k, want := range map[string]any{"agent_id": "a_judge", "run_id": "r_judge", "status": "completed", "stop_reason": "end_turn", "final_text": `{"score": 7}`} {
		if obj[k] != want {
			t.Errorf("%s = %v, want %v", k, obj[k], want)
		}
	}
	usage, _ := obj["usage"].(map[string]any)
	for k, want := range map[string]any{"input_tokens": float64(120), "output_tokens": float64(45),
		"cache_creation_input_tokens": float64(7), "cache_read_input_tokens": float64(9), "model": "m-1", "provider": "p-1"} {
		if usage[k] != want {
			t.Errorf("usage.%s = %v, want %v", k, usage[k], want)
		}
	}
	// Which key paid is the operator's to see, not the agent's.
	if len(usage) != 6 {
		t.Errorf("usage carries more than the six documented fields: %v", usage)
	}
	if s, _ := obj["structured"].(map[string]any); s["score"] != float64(7) {
		t.Errorf("structured = %v", obj["structured"])
	}
	if s, _ := obj["state"].(map[string]any); s["step"] != "done" {
		t.Errorf("state = %v", obj["state"])
	}
	if strings.Contains(res.Text, "[sub-agent") {
		t.Errorf("the object carries the attribution line: %s", res.Text)
	}
}

// Without `result`, nothing changes: the text result, byte for byte, and the
// child's run is not asked for an outcome.
func TestAgentTool_WithoutResultTheTextResultIsUnchanged(t *testing.T) {
	for _, input := range []string{`{"name":"judge","prompt":"p"}`, `{"name":"judge","prompt":"p","result":"text"}`} {
		r := &outcomeRunner{status: "completed", text: "the answer"}
		res := execJSON(t, r.tool(), context.Background(), input)
		if want := "[sub-agent agent_id=a_judge run_id=r_judge]\nthe answer"; res.Text != want || res.IsError {
			t.Errorf("%s → %q (error %v), want %q", input, res.Text, res.IsError, want)
		}
		if len(r.sawSink) != 1 || r.sawSink[0] {
			t.Errorf("%s: the child was asked for an outcome nobody wants", input)
		}
	}
}

// A child that ran is described, not raised, whatever its end.
func TestAgentTool_ResultObjectReportsAChildThatDidNotComplete(t *testing.T) {
	capped := &ChildCappedError{Name: "judge", Limit: 4, RunID: "r_judge",
		Output: "[sub-agent agent_id=a_judge run_id=r_judge]\nits last answer", State: map[string]any{"step": "mid"}}
	for name, tc := range map[string]struct {
		r          *outcomeRunner
		wantStatus string
		wantText   string
	}{
		"failed":             {&outcomeRunner{status: "failed", err: errors.New("sub-agent \"judge\" failed: provider down")}, "failed", ""},
		"cancelled":          {&outcomeRunner{status: "cancelled", err: errors.New("context canceled")}, "cancelled", ""},
		"at its limit":       {&outcomeRunner{status: "completed", err: capped}, "max_iterations", "its last answer"},
		"refused by a hook":  {&outcomeRunner{status: "completed", text: "an answer", err: errors.New("the result of sub-agent \"judge\" was refused: policy")}, "rejected", ""},
		"rejected in review": {&outcomeRunner{status: "rejected", err: errors.New("the answer of sub-agent \"judge\" was rejected")}, "rejected", ""},
	} {
		t.Run(name, func(t *testing.T) {
			res := execJSON(t, tc.r.tool(), context.Background(), `{"name":"judge","prompt":"p","result":"object"}`)
			if res.IsError {
				t.Fatalf("a child that ran was raised as an error: %s", res.Text)
			}
			obj := decodeObject(t, res.Text)
			if obj["status"] != tc.wantStatus || obj["error"] == nil || obj["run_id"] != "r_judge" {
				t.Errorf("object = %v, want status %s with an error and the run id", obj, tc.wantStatus)
			}
			if got, _ := obj["final_text"].(string); got != tc.wantText {
				t.Errorf("final_text = %q, want %q", got, tc.wantText)
			}
			if u, _ := obj["usage"].(map[string]any); u["output_tokens"] != float64(45) {
				t.Errorf("usage = %v, want the failed run's usage too", obj["usage"])
			}
		})
	}
}

// A child that never started has no run to describe: an error, as before.
func TestAgentTool_ResultObjectStillRaisesAChildThatNeverStarted(t *testing.T) {
	r := &outcomeRunner{noRun: true, err: errors.New(`unknown sub-agent "judge"`)}
	res := execJSON(t, r.tool(), context.Background(), `{"name":"judge","prompt":"p","result":"object"}`)
	if !res.IsError || !strings.Contains(res.Text, "unknown sub-agent") {
		t.Errorf("result = %q (error %v), want the spawn's own error", res.Text, res.IsError)
	}
}

// In a fan-out each entry chooses; an object entry carries `result` in place
// of `output` and `state`, and the call's `result` is the entries' default.
func TestAgentTool_ParallelSpawnRowsCarryTheObjectTheirEntryAskedFor(t *testing.T) {
	r := &outcomeRunner{status: "completed", text: "answer", state: map[string]any{"k": "v"}}
	res := execJSON(t, r.tool(), context.Background(), `{"op":"parallel_spawn","spawns":[
		{"name":"a","prompt":"p","result":"object"},{"name":"b","prompt":"p"}]}`)
	var env struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &env); err != nil || len(env.Results) != 2 {
		t.Fatalf("envelope: %v %s", err, res.Text)
	}
	a, b := env.Results[0], env.Results[1]
	obj, _ := a["result"].(map[string]any)
	if obj["final_text"] != "answer" || obj["run_id"] != "r_a" || a["output"] != nil || a["state"] != nil || a["ok"] != true || a["run_id"] != "r_a" {
		t.Errorf("row a = %v, want the object in result, no output or state, ok and run_id kept", a)
	}
	if u, _ := obj["usage"].(map[string]any); u["model"] != "m-1" {
		t.Errorf("row a usage = %v", obj["usage"])
	}
	if b["result"] != nil || b["output"] != "[sub-agent agent_id=a_b run_id=r_b]\nanswer" || b["state"] == nil {
		t.Errorf("row b = %v, want the text row unchanged", b)
	}

	// The call's result is the default for entries that name none.
	r2 := &outcomeRunner{status: "completed", text: "answer"}
	res = execJSON(t, r2.tool(), context.Background(), `{"op":"parallel_spawn","result":"object","spawns":[
		{"name":"a","prompt":"p"},{"name":"b","prompt":"p","result":"text"}]}`)
	// A fresh value: decoding into the first envelope would merge into its maps.
	var byDefault struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &byDefault); err != nil || len(byDefault.Results) != 2 {
		t.Fatalf("envelope: %v %s", err, res.Text)
	}
	if byDefault.Results[0]["result"] == nil || byDefault.Results[1]["result"] != nil || byDefault.Results[1]["output"] == nil {
		t.Errorf("default: rows = %v, want an object for a and text for b", byDefault.Results)
	}
}

// The object obeys the same quarter-of-the-window bound a text result does:
// the text is cut, a value too large to keep is left out whole, and both are
// flagged.
func TestSpawnObject_CapToKeepsItWithinTheParentsShare(t *testing.T) {
	obj := spawnObject{FinalText: strings.Repeat("x", 500), Structured: map[string]any{"k": "v"},
		State: map[string]any{"big": strings.Repeat("s", 400)}}
	obj.capTo(100)
	if !obj.Truncated || len(obj.FinalText) > 100 || obj.State != nil || !obj.StateOmitted || obj.Structured == nil || obj.StructuredOmitted {
		t.Errorf("capped object = %+v; want the text cut, the oversized state left out, the small structured kept", obj)
	}
	whole := spawnObject{FinalText: "short"}
	whole.capTo(0)
	whole.capTo(1000)
	if whole.Truncated || whole.FinalText != "short" {
		t.Errorf("an object within the bound was changed: %+v", whole)
	}
}

func TestAgentTool_ResultIsValidatedAndRefusedWhereItCannotApply(t *testing.T) {
	r := &outcomeRunner{status: "completed", text: "x"}
	a := r.tool()
	a.OpenChild = func(context.Context, string, string, string, int, int) (string, string, string, error) {
		t.Fatal("opened a child for a refused call")
		return "", "", "", nil
	}
	a.SendChild = func(context.Context, string, string, int) (string, string, error) {
		t.Fatal("sent a turn for a refused call")
		return "", "", nil
	}
	pollCtxValue, _ := pollCtx()
	for name, tc := range map[string]struct {
		ctx   context.Context
		input string
		want  string
	}{
		"an unknown value":    {context.Background(), `{"name":"j","prompt":"p","result":"json"}`, `unknown value "json"`},
		"an unknown entry":    {context.Background(), `{"op":"parallel_spawn","spawns":[{"name":"j","prompt":"p","result":"x"}]}`, "spawns[0].result"},
		"with mode poll":      {pollCtxValue, `{"name":"j","prompt":"p","mode":"poll","result":"object"}`, `not available with mode "poll"`},
		"a poll-mode fan-out": {pollCtxValue, `{"op":"parallel_spawn","mode":"poll","result":"object","spawns":[{"name":"j","prompt":"p"}]}`, `not available with mode "poll"`},
		"on open":             {context.Background(), `{"op":"open","name":"j","prompt":"p","result":"object"}`, "resident child"},
		"on send":             {context.Background(), `{"op":"send","child_run_id":"r","prompt":"p","result":"object"}`, "resident child"},
	} {
		t.Run(name, func(t *testing.T) {
			res := execJSON(t, a, tc.ctx, tc.input)
			if !res.IsError || !strings.Contains(res.Text, tc.want) {
				t.Errorf("result = %q (error %v), want a refusal containing %q", res.Text, res.IsError, tc.want)
			}
		})
	}
	if len(r.sawSink) != 0 {
		t.Errorf("a refused call started %d child(ren)", len(r.sawSink))
	}
}
