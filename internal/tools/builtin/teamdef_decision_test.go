package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// noCallDriver is a decision driver for tests that never reach a model.
type noCallDriver struct{}

func (noCallDriver) Decide(context.Context, decision.Request) (*decision.Response, error) {
	panic("the test does not call a decision model")
}
func (noCallDriver) Limits(string) decision.Limits {
	return decision.Limits{MaxQuestions: 64, MinOptions: 2, MaxOptions: 26}
}

// decisionTeamFixture is localTeamFixture on a deployment offering two
// decision models, "decide" (the default) and "deep".
func decisionTeamFixture(t *testing.T) *TeamDef {
	t.Helper()
	tool, _ := localTeamFixture(t)
	svc, err := decision.NewService("decide", []decision.ModelSpec{
		{Name: "decide", Provider: "ollama", Model: "nimble", Driver: noCallDriver{}},
		{Name: "deep", Provider: "ollama", Model: "clef", Driver: noCallDriver{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tool.Decision = &Decision{Service: svc}
	return tool
}

// gateTeam is one decision state, asking model ("" = none named), and an end.
func gateTeam(model string) string {
	m := ""
	if model != "" {
		m = `"model":"` + model + `",`
	}
	return `{"entry":"gate","states":[
	  {"state":"gate","handler":{"kind":"decision",` + m + `"about":{"text":"{{thread.output}}"},
	    "questions":{"ok":{"type":"noul","instructions":"Is it acceptable?"}}}},
	  {"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"gate","to":"done","on":"success"}]}`
}

// A decision state reaches a decision model with no agent in between. An
// agent granted TeamDef and not Decision must not get there by writing a team:
// what a definition can do is within what its author holds.
func TestTeamDefCreate_ADecisionStateNeedsAnAuthorWhoHoldsTheDecisionTool(t *testing.T) {
	tool := decisionTeamFixture(t)
	without := localAuthorCtx("", []string{"any"}, []string{"TeamDef", "Read"})
	wantRefused(t, teamOp(t, tool, without, "create", "gated", gateTeam("")), "an author without Decision",
		"does not hold the Decision tool")

	// verify reports it where a create refuses it, with the place.
	res := teamOp(t, tool, without, "verify", "gated", gateTeam(""))
	var report struct {
		Valid  bool `json:"valid"`
		Issues []struct{ Kind, Severity, Path, State string }
	}
	if err := json.Unmarshal([]byte(res.Text), &report); err != nil {
		t.Fatalf("verify: %s", res.Text)
	}
	if report.Valid || len(report.Issues) != 1 || report.Issues[0].Kind != "decision_authority" ||
		report.Issues[0].Severity != "refused" || report.Issues[0].Path != "states[0].handler" {
		t.Errorf("verify = %s, want one decision_authority refusal at states[0].handler", res.Text)
	}

	with := localAuthorCtx("", []string{"any"}, []string{"TeamDef", "Decision"})
	if res := teamOp(t, tool, with, "create", "gated", gateTeam("deep")); res.IsError {
		t.Errorf("an author holding Decision was refused: %s", res.Text)
	}
	// A fork is an authoring act by whoever forks: the state it carries over
	// is judged under the forker's authority.
	wantRefused(t, teamOp(t, tool, without, "fork", "gated", `{"max_iterations":3}`), "a fork by an author without Decision",
		"does not hold the Decision tool")
}

// An author whose own `decision` block narrows the operator's models may not
// write a state that asks one outside it — including by naming none, which
// asks the operator's default at run time.
func TestTeamDefCreate_ADecisionStateMayAskOnlyWhatItsAuthorMay(t *testing.T) {
	tool := decisionTeamFixture(t)
	narrowed := tools.WithDecisionPolicy(
		localAuthorCtx("", []string{"any"}, []string{"TeamDef", "Decision"}),
		&config.AgentDecision{Models: []string{"deep"}, Default: "deep"})

	wantRefused(t, teamOp(t, tool, narrowed, "create", "gated", gateTeam("decide")), "a model outside the author's",
		`asks decision model "decide"`, "it may ask: deep")
	wantRefused(t, teamOp(t, tool, narrowed, "create", "gated", gateTeam("")), "the operator's default, outside the author's",
		`asks decision model "decide"`)
	if res := teamOp(t, tool, narrowed, "create", "gated", gateTeam("deep")); res.IsError {
		t.Errorf("the author's own model was refused: %s", res.Text)
	}
}

// The operator, authoring through an operator surface, is the root of
// authority: it holds no tool list to be narrowed by.
func TestTeamDefCreate_TheOperatorMayWriteADecisionState(t *testing.T) {
	tool := decisionTeamFixture(t)
	operator := localAuthorCtx("", []string{"any"}, []string{"*"})
	if res := teamOp(t, tool, operator, "create", "gated", gateTeam("deep")); res.IsError {
		t.Errorf("the operator was refused: %s", res.Text)
	}
	// A model this deployment lacks is stored, and reported as unrunnable.
	if res := teamOp(t, tool, operator, "create", "elsewhere", gateTeam("faraway")); res.IsError {
		t.Fatalf("a model the deployment lacks refused a save: %s", res.Text)
	}
	res, err := tool.Execute(operator, json.RawMessage(`{"op":"verify","name":"elsewhere"}`))
	if err != nil || !strings.Contains(res.Text, `"decision_model_unknown"`) || !strings.Contains(res.Text, `"runnable":false`) {
		t.Errorf("verify = %s (%v), want decision_model_unknown and not runnable", res.Text, err)
	}
}
