package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// walkDeclaring walks a one-agent team that declares vars, from task, through
// the real member runner, and returns what the agent's model was sent.
func walkDeclaring(t *testing.T, vars map[string]string, template string, task *teamrun.Task) string {
	t.Helper()
	h := newReviewHarness(t)
	h.prov.answer = "done"
	d := teamgraph.Definition{
		Entry: "write",
		Vars:  vars,
		States: []teamgraph.State{
			{ID: "write", Handler: teamgraph.Handler{Kind: teamgraph.HandlerAgent, Agent: "writer", InputTemplate: template}},
			{ID: "end", Handler: teamgraph.Handler{Kind: teamgraph.HandlerTerminal}},
		},
		Transitions: []teamgraph.Transition{{From: "write", To: "end", On: teamgraph.OnSuccess}},
	}
	if err := teamgraph.Validate(d); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, err := teamrun.Walk(context.Background(), d, task, teamrun.NewAgentRunner(h.srv.runTeamMember)); err != nil {
		t.Fatalf("walk: %v", err)
	}
	seen := h.prov.seen()
	if len(seen) != 1 {
		t.Fatalf("the model was called %d times, want once", len(seen))
	}
	return seen[0]
}

// Through the real member runner and prompt assembly: the model is sent the
// team's declared default, and a value the walk started with instead of it.
func TestTeamWalk_MemberPromptReceivesDeclaredDefaultOrStartValue(t *testing.T) {
	vars := map[string]string{"tone": "formal", "lang": "en"}
	const template = "Tone ${var.tone}, language ${var.lang}."

	if got, want := walkDeclaring(t, vars, template, &teamrun.Task{Input: "go", WalkID: "wlk_vars1"}), "Tone formal, language en."; got != want {
		t.Errorf("the model was sent %q, want %q", got, want)
	}
	started := &teamrun.Task{Input: "go", WalkID: "wlk_vars2"}
	started.SetVar("tone", "casual")
	if got, want := walkDeclaring(t, vars, template, started), "Tone casual, language en."; got != want {
		t.Errorf("the model was sent %q, want %q", got, want)
	}
}

// A default is literal text: the ${…} in it reaches the model as written,
// because prompt assembly substitutes a variable once and does not read what
// it wrote.
func TestTeamWalk_DefaultContainingATokenReachesTheModelUnexpanded(t *testing.T) {
	vars := map[string]string{"note": "${var.other} on ${now.date}", "other": "resolved-other"}
	got := walkDeclaring(t, vars, "Note: ${var.note}", &teamrun.Task{Input: "go", WalkID: "wlk_vars3"})
	if want := "Note: ${var.other} on ${now.date}"; got != want {
		t.Errorf("the model was sent %q, want the default's own characters %q", got, want)
	}
}
