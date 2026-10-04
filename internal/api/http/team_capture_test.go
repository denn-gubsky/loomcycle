package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// captureThenUse runs two agent states through the real member runner: the
// first answers `answer` and captures it by `path` into ${var.v}, the second's
// input_template uses the variable. It returns what the second agent's model
// was sent, and the value the capture bound.
func captureThenUse(t *testing.T, answer, path string) (sent, bound string) {
	t.Helper()
	h := newReviewHarness(t)
	h.prov.answer = answer
	r := teamrun.NewAgentRunner(h.srv.runTeamMember)
	task := &teamrun.Task{Input: "go", WalkID: "wlk_capture"}
	first := teamgraph.State{ID: "draft", Handler: teamgraph.Handler{
		Kind: teamgraph.HandlerAgent, Agent: "writer", Capture: map[string]string{"v": path}}}
	second := teamgraph.State{ID: "use", Handler: teamgraph.Handler{
		Kind: teamgraph.HandlerAgent, Agent: "writer", InputTemplate: "Captured: ${var.v}."}}
	for _, st := range []teamgraph.State{first, second} {
		if _, err := r.RunHandler(context.Background(), st, task); err != nil {
			t.Fatalf("state %q: %v", st.ID, err)
		}
	}
	seen := h.prov.seen()
	if len(seen) != 2 {
		t.Fatalf("the model was called %d times, want once per state", len(seen))
	}
	return seen[1], task.Vars["v"]
}

// Through the real member runner, whose Output carries the attribution header:
// a capture reads the agent's own answer, so the next state's prompt gets the
// whole text for the root path and the field for a field path.
func TestTeamWalk_CaptureReadsAnAgentStatesBareAnswer(t *testing.T) {
	for name, tc := range map[string]struct{ answer, path, want string }{
		"plain text by root":  {"Hello world", "$", "Captured: Hello world."},
		"JSON by field":       {`{"title":"T"}`, "$.title", "Captured: T."},
		"plain text by field": {"Hello world", "$.title", "Captured: ."},
	} {
		t.Run(name, func(t *testing.T) {
			if got, _ := captureThenUse(t, tc.answer, tc.path); got != tc.want {
				t.Errorf("the next state's model was sent %q, want %q", got, tc.want)
			}
		})
	}
}

// A captured answer that carries a placeholder is dropped from the prompt it
// is used in rather than expanded there — the whole text of an answer as much
// as a field projected out of a JSON one.
func TestTeamWalk_CapturedPlaceholderNeverReachesTheNextPrompt(t *testing.T) {
	for name, tc := range map[string]struct{ answer, path string }{
		"plain text by root": {"{{document:/secret}}", "$"},
		"JSON by field":      {`{"x":"{{document:/secret}}"}`, "$.x"},
	} {
		t.Run(name, func(t *testing.T) {
			got, bound := captureThenUse(t, tc.answer, tc.path)
			if bound != "{{document:/secret}}" {
				t.Fatalf("captured %q, want the placeholder text — with nothing bound the prompt proves nothing", bound)
			}
			if got != "Captured: ." {
				t.Errorf("the next state's model was sent %q, want the value dropped", got)
			}
		})
	}
}
