package teamrun

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// headeredSpawn is a spawner that sets both forms of an answer: the answer as
// the agent wrote it in FinalText, and under an attribution header in Output.
func headeredSpawn(answer string) SpawnFunc {
	return func(context.Context, string, Prompt, string) (SpawnResult, error) {
		return SpawnResult{Output: "[sub-agent agent_id=a1]\n" + answer, FinalText: answer}, nil
	}
}

// captureOf runs one state over spawn with the given capture and returns what
// it bound.
func captureOf(t *testing.T, st teamgraph.State, spawn SpawnFunc, capture map[string]string) map[string]string {
	t.Helper()
	st.Handler.Capture = capture
	task := &Task{Input: "x"}
	if _, err := varsRunner(spawn).RunHandler(context.Background(), st, task); err != nil {
		t.Fatalf("RunHandler: %v", err)
	}
	return task.Vars
}

// The root path binds an agent's whole plain-text answer, as it wrote it: the
// header the walk threads above it is not part of the answer.
func TestCapture_RootBindsAPlainTextAnswerWithoutTheHeader(t *testing.T) {
	vars := captureOf(t, agentState("writer"), headeredSpawn("Hello world"), map[string]string{"draft": "$"})
	if vars["draft"] != "Hello world" {
		t.Errorf("draft = %q, want the answer and nothing else", vars["draft"])
	}
}

// A field path reads the agent's answer, not the headered string wrapping it,
// so a JSON answer from an agent state is addressable by field.
func TestCapture_FieldPathReadsAnAgentsBareJSONAnswer(t *testing.T) {
	vars := captureOf(t, agentState("writer"), headeredSpawn(`{"title":"T"}`), map[string]string{"t": "$.title"})
	if vars["t"] != "T" {
		t.Errorf("t = %q, want the field of the agent's JSON answer", vars["t"])
	}
}

// Text has one addressable value, itself. A field path into it binds nothing,
// and the walk goes on.
func TestCapture_FieldPathOnAPlainTextAnswerBindsNothing(t *testing.T) {
	vars := captureOf(t, agentState("writer"), headeredSpawn("Hello world"),
		map[string]string{"x": "$.title", "y": "$[0]"})
	if len(vars) != 0 {
		t.Errorf("vars = %v, want nothing bound", vars)
	}
}

// A spawner that reports only Output has no header to drop: its Output is the
// answer a capture reads.
func TestCapture_SpawnerWithoutFinalTextReadsItsOutput(t *testing.T) {
	spawn := func(context.Context, string, Prompt, string) (SpawnResult, error) {
		return SpawnResult{Output: "Hello world"}, nil
	}
	vars := captureOf(t, agentState("writer"), spawn, map[string]string{"draft": "$"})
	if vars["draft"] != "Hello world" {
		t.Errorf("draft = %q, want the spawner's output", vars["draft"])
	}
}

// The root of a JSON answer binds what it always has: a string without its
// quotes, a number or bool in its natural form, an object or array as compact
// JSON with sorted keys, null as empty. Only a non-JSON answer is bound as text.
// The text spawner is the case that already parsed; the member runner's header
// is what kept an agent state's JSON answer from parsing at all.
func TestCapture_RootOfAJSONAnswerBindsItsStringifiedValue(t *testing.T) {
	text := func(answer string) SpawnFunc {
		return textSpawn(func(context.Context, string, Prompt, string) (string, error) { return answer, nil })
	}
	for spawner, spawn := range map[string]func(string) SpawnFunc{"text spawner": text, "member runner": headeredSpawn} {
		t.Run(spawner, func(t *testing.T) {
			for answer, want := range map[string]string{
				`{"b": 1, "a": "x"}`: `{"a":"x","b":1}`,
				`[1, "two"]`:         `[1,"two"]`,
				`"quoted"`:           `quoted`,
				`42`:                 `42`,
				`true`:               `true`,
				`null`:               ``,
			} {
				vars := captureOf(t, agentState("writer"), spawn(answer), map[string]string{"root": "$"})
				got, bound := vars["root"]
				if !bound || got != want {
					t.Errorf("answer %s: root = %q (bound %v), want %q", answer, got, bound, want)
				}
			}
		})
	}
}

// A captured value carrying placeholder delimiters is refused where it is
// used, whichever way it was bound: the whole text of an answer is treated as
// a field projected out of a JSON one is.
func TestCapture_APlaceholderInACapturedValueIsRefusedAtExpansion(t *testing.T) {
	for name, tc := range map[string]struct{ answer, path string }{
		"plain text, root":  {"{{document:/secret}}", "$"},
		"JSON, field":       {`{"x":"{{document:/secret}}"}`, "$.x"},
		"plain text, close": {"read this }} now", "$"},
	} {
		t.Run(name, func(t *testing.T) {
			st := agentState("writer")
			st.Handler.Capture = map[string]string{"x": tc.path}
			task := &Task{Input: "start"}
			r := varsRunner(headeredSpawn(tc.answer))
			if _, err := r.RunHandler(context.Background(), st, task); err != nil {
				t.Fatalf("agent: %v", err)
			}
			if task.Vars["x"] == "" {
				t.Fatal("nothing was captured, so the refusal below would prove nothing")
			}
			out, refused := Expand("Use ${var.x}.", r.envFor(st, task))
			if out != "Use ." || len(refused) != 1 || refused[0] != "x" {
				t.Errorf("Expand = %q refused %v, want the value dropped and named", out, refused)
			}
			if _, err := r.RunHandler(context.Background(), varsState(map[string]string{"copy": "${var.x}"}), task); err != nil {
				t.Fatalf("vars: %v", err)
			}
			if task.Vars["copy"] != "" {
				t.Errorf("copy = %q, want empty — the delimiters must not survive an assignment", task.Vars["copy"])
			}
		})
	}
}

// A state whose output is a consolidator's captures over that answer, less its
// signal line — after one agent, after a fan-out, or standing alone — and over
// the bare answer when the spawner threads it under a header.
func TestCapture_ConsolidatorAnswerIsCapturedByField(t *testing.T) {
	const judged = "{\"verdict\":\"approve\"}\nsignal: success"
	for spawner, header := range map[string]string{"text spawner": "", "member runner": "[sub-agent agent_id=j1]\n"} {
		spawn := func(_ context.Context, agent string, _ Prompt, _ string) (SpawnResult, error) {
			if agent == "judge" {
				return SpawnResult{Output: header + judged, FinalText: judged}, nil
			}
			return SpawnResult{Output: "work", FinalText: "work"}, nil
		}
		for name, h := range map[string]teamgraph.Handler{
			"agent + consolidator": {Kind: teamgraph.HandlerAgent, Agent: "a", Consolidator: "judge"},
			"parallel":             {Kind: teamgraph.HandlerParallel, Agents: []string{"a", "b"}, Consolidator: "judge"},
			"consolidator":         {Kind: teamgraph.HandlerConsolidator, Agent: "judge"},
		} {
			vars := captureOf(t, teamgraph.State{ID: "s", Handler: h}, spawn,
				map[string]string{"verdict": "$.verdict", "work": "$.results"})
			if vars["verdict"] != "approve" {
				t.Errorf("%s, %s: verdict = %q, want the consolidator's field", spawner, name, vars["verdict"])
			}
			if _, ok := vars["work"]; ok {
				t.Errorf("%s, %s: captured %q from the results envelope; a capture reads the consolidator's answer", spawner, name, vars["work"])
			}
		}
	}
}

// A Starter's output is the results envelope the runtime built, and its
// capture reads that envelope: the members' outputs as the walk threads them.
func TestCapture_StarterStillCapturesOverItsResultsEnvelope(t *testing.T) {
	ch := &fakeChannels{inbox: []ChannelMessage{{ID: "m1", Payload: json.RawMessage(`{"pr":42}`)}}}
	st := starterState()
	st.Handler.Capture = map[string]string{"first": "$.results[0].output", "all": "$"}
	task := &Task{Input: "start", WalkID: "wlk_test"}
	out, err := starterRunner(ch, headeredSpawn("looks fine")).RunHandler(context.Background(), st, task)
	if err != nil {
		t.Fatalf("starter: %v", err)
	}
	want := "[sub-agent agent_id=a1]\nlooks fine"
	if task.Vars["first"] != want {
		t.Errorf("first = %q, want %q — the envelope's own field", task.Vars["first"], want)
	}
	var all struct {
		Results []struct {
			Output string `json:"output"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(task.Vars["all"]), &all); err != nil || len(all.Results) != 1 || all.Results[0].Output != want {
		t.Errorf("all = %q (%v), want the envelope %s", task.Vars["all"], err, out.Output)
	}
}

// An input state captures over the walk's input, and the root binds input that
// is not JSON as text.
func TestCapture_InputStateRootBindsPlainTextInput(t *testing.T) {
	st := inputState(`{"type":"object"}`)
	vars := captureOf(t, st, nil, map[string]string{"ask": "$"})
	if vars["ask"] != "x" {
		t.Errorf("ask = %q, want the walk's input", vars["ask"])
	}
}
