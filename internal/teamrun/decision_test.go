package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/decisionq"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

const triageAnswer = `{"model":"decide","answers":{` +
	`"route":{"type":"choice","choice":"billing","probabilities":{"billing":0.9,"support":0.1},"confidence":0.8},` +
	`"urgent":{"type":"noul","noul":0.62}}}`

func triageState() teamgraph.State {
	return teamgraph.State{ID: "triage", Handler: teamgraph.Handler{
		Kind:  teamgraph.HandlerDecision,
		Model: "deep",
		About: json.RawMessage(`{"ticket":"{{thread.output}}","meta":{"tier":"${var.tier}","tags":["${team.state}",7]}}`),
		Questions: map[string]decisionq.Question{
			"route":  {Type: "choice", Instructions: "Which team?", Criteria: json.RawMessage(`{"billing":null,"support":null}`)},
			"urgent": {Type: "noul", Instructions: "Within the hour?"},
		},
		Route: "route",
		Capture: map[string]string{
			"team": "$.answers.route.choice", "urgent": "$.answers.urgent.noul", "p": "$.answers.route.probabilities.billing",
		},
	}}
}

// asked records one decision call and answers it.
type asked struct {
	calls     int
	model     string
	about     map[string]any
	questions map[string]decisionq.Question
	answer    string
	err       error
}

func (a *asked) decide(_ context.Context, model string, about map[string]any, questions map[string]decisionq.Question) (string, error) {
	a.calls++
	a.model, a.about, a.questions = model, about, questions
	return a.answer, a.err
}

func decisionRunner(t *testing.T, a *asked) *agentRunner {
	r := varsRunner(textSpawn(func(context.Context, string, Prompt, string) (string, error) {
		t.Fatal("a decision state must not start an agent run")
		return "", nil
	}))
	r.decide = a.decide
	return r
}

// One call, about the expanded object; the answer routes the walk and binds
// variables; what the state was handed goes on to the next state unchanged.
func TestDecisionState_AsksOnceRoutesBindsAndThreadsItsInput(t *testing.T) {
	a := &asked{answer: triageAnswer}
	r := decisionRunner(t, a)
	task := &Task{Input: "My March invoice was charged twice", Vars: map[string]string{"tier": "gold"}}

	oc, err := r.RunHandler(context.Background(), triageState(), task)
	if err != nil {
		t.Fatalf("RunHandler: %v", err)
	}
	if a.calls != 1 || a.model != "deep" || len(a.questions) != 2 {
		t.Errorf("the model was asked %d times as %q with %d questions, want once as deep with 2", a.calls, a.model, len(a.questions))
	}
	wantAbout := map[string]any{
		"ticket": "My March invoice was charged twice",
		"meta":   map[string]any{"tier": "gold", "tags": []any{"triage", float64(7)}},
	}
	if !reflect.DeepEqual(a.about, wantAbout) {
		t.Errorf("the model was asked about %v, want %v", a.about, wantAbout)
	}
	want := Outcome{Output: "My March invoice was charged twice", Edge: "conditional:billing", Fallback: "success", Answer: triageAnswer}
	if oc != want {
		t.Errorf("outcome = %+v, want %+v", oc, want)
	}
	if task.Vars["team"] != "billing" || task.Vars["urgent"] != "0.62" || task.Vars["p"] != "0.9" {
		t.Errorf("vars = %v, want team, urgent and p bound from the answer", task.Vars)
	}
}

// The previous state's output is another agent's text. It lands where the
// author put the slot and is not read for tokens or slots itself, and a
// variable cannot write the slot's marker.
func TestDecisionState_TheHandOffIsDataNotTemplate(t *testing.T) {
	a := &asked{answer: triageAnswer}
	r := decisionRunner(t, a)
	task := &Task{
		Input: "${var.tier} {{thread.output}} {{document:/secret}}",
		Vars:  map[string]string{"tier": "{{thread.output}}"},
	}
	if _, err := r.RunHandler(context.Background(), triageState(), task); err != nil {
		t.Fatalf("RunHandler: %v", err)
	}
	if got := a.about["ticket"]; got != task.Input {
		t.Errorf("ticket = %q, want the hand-off as it was written", got)
	}
	if got := a.about["meta"].(map[string]any)["tier"]; got != "" {
		t.Errorf("tier = %q, want a variable carrying a placeholder dropped", got)
	}
}

func TestDecisionState_AYesNoRoutesAtTheThreshold(t *testing.T) {
	for _, tc := range []struct {
		threshold *float64
		want      string
	}{
		{nil, "conditional:true"}, // 0.62 >= the default 0.5
		{ptr(0.62), "conditional:true"},
		{ptr(0.7), "conditional:false"},
	} {
		st := triageState()
		st.Handler.Route, st.Handler.Threshold = "urgent", tc.threshold
		oc, err := decisionRunner(t, &asked{answer: triageAnswer}).RunHandler(context.Background(), st, &Task{})
		if err != nil || oc.Edge != tc.want {
			t.Errorf("threshold %v: edge = %q (%v), want %q", tc.threshold, oc.Edge, err, tc.want)
		}
	}
}

func ptr(f float64) *float64 { return &f }

// With no route the state only binds: it advances on success.
func TestDecisionState_WithNoRouteItAdvancesOnSuccess(t *testing.T) {
	st := triageState()
	st.Handler.Route = ""
	task := &Task{Input: "in"}
	oc, err := decisionRunner(t, &asked{answer: triageAnswer}).RunHandler(context.Background(), st, task)
	if err != nil || oc.Edge != "" || oc.Fallback != "" || oc.Answer != triageAnswer || task.Vars["team"] != "billing" {
		t.Errorf("outcome = %+v (%v), vars = %v; want no edge named, the answer kept and bound", oc, err, task.Vars)
	}
}

// A failed call fails the state, naming it and carrying the model's code.
func TestDecisionState_AFailedCallFailsTheStateWithItsCode(t *testing.T) {
	a := &asked{err: &DecisionError{Code: decisionq.CodePromptTooLarge, Message: "the request is 9000 tokens and this model reads at most 8192"}}
	_, err := decisionRunner(t, a).RunHandler(context.Background(), triageState(), &Task{})
	var de *DecisionError
	if !errors.As(err, &de) || de.State != "triage" || de.Code != decisionq.CodePromptTooLarge ||
		!strings.HasPrefix(err.Error(), "decision: prompt_too_large: ") {
		t.Fatalf("err = %v (%+v), want a DecisionError for triage with the model's code", err, de)
	}

	// An answer that does not answer the routed question is a failed call.
	_, err = decisionRunner(t, &asked{answer: `{"answers":{"urgent":{"noul":0.1}}}`}).RunHandler(context.Background(), triageState(), &Task{})
	if !errors.As(err, &de) || de.Code != decisionq.CodeCallFailed || de.State != "triage" {
		t.Errorf("err = %v, want call_failed for a reply with no answer to the routed question", err)
	}

	// No decision call wired: refused at the state.
	r := decisionRunner(t, &asked{})
	r.decide = nil
	_, err = r.RunHandler(context.Background(), triageState(), &Task{})
	if !errors.As(err, &de) || de.Code != "decision_not_configured" {
		t.Errorf("err = %v, want decision_not_configured", err)
	}
}

// timeout_ms bounds the one call.
func TestDecisionState_TimeoutBoundsTheCall(t *testing.T) {
	st := triageState()
	st.Handler.TimeoutMS = 20
	r := decisionRunner(t, &asked{})
	r.decide = func(ctx context.Context, _ string, _ map[string]any, _ map[string]decisionq.Question) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
			return triageAnswer, nil
		}
	}
	_, err := r.RunHandler(context.Background(), st, &Task{})
	var te *TimeoutError
	if !errors.As(err, &te) || te.State != "triage" {
		t.Errorf("err = %v, want the state's timeout", err)
	}
}

func decisionTeam() teamgraph.Definition {
	end := func(id string) teamgraph.State {
		return teamgraph.State{ID: id, Handler: teamgraph.Handler{Kind: teamgraph.HandlerTerminal}}
	}
	return teamgraph.Definition{
		Entry:  "triage",
		States: []teamgraph.State{triageState(), end("billing-desk"), end("other")},
		Transitions: []teamgraph.Transition{
			{From: "triage", To: "billing-desk", On: "conditional:billing"},
			{From: "triage", To: "other", On: "success"},
		},
	}
}

// The walk takes the answer's own edge when there is one and the state's
// success edge when there is not, and records the answer on the step beside
// an output that is still the hand-off.
func TestWalk_ADecisionTakesItsAnswersEdgeOrSuccess(t *testing.T) {
	d := decisionTeam()
	if err := teamgraph.Validate(d); err != nil {
		t.Fatalf("the test's team is not valid: %v", err)
	}
	for answer, wantEnd := range map[string]string{
		triageAnswer: "billing-desk",
		strings.Replace(triageAnswer, `"choice":"billing"`, `"choice":"support"`, 1): "other",
	} {
		task := &Task{Input: "the ticket"}
		trace, err := Walk(context.Background(), d, task, decisionRunner(t, &asked{answer: answer}))
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if task.State != wantEnd || len(trace) != 1 {
			t.Fatalf("the walk ended at %q after %d steps, want %q after 1", task.State, len(trace), wantEnd)
		}
		step := trace[0]
		wantEdge := "conditional:billing"
		if wantEnd == "other" {
			wantEdge = "success"
		}
		if step.Edge != wantEdge || step.Next != wantEnd || step.Output != "the ticket" || step.Answer != answer || step.Handler != "decision" {
			t.Errorf("step = %+v, want edge %s to %s, the hand-off as output and the answer beside it", step, wantEdge, wantEnd)
		}
		if task.Input != "the ticket" {
			t.Errorf("the next state would receive %q, want the hand-off", task.Input)
		}
	}
}

// A state that names no fallback still fails the walk on an edge it has no
// transition for: the fallback is the decision state's alone.
func TestWalk_AMissingEdgeStillFailsWithoutAFallback(t *testing.T) {
	d := decisionTeam()
	r := &fakeRunner{outcomes: map[string]Outcome{"triage": {Output: "x", Edge: "conditional:support"}}}
	if _, err := Walk(context.Background(), d, &Task{}, r); err == nil || !strings.Contains(err.Error(), "no matching transition") {
		t.Errorf("err = %v, want the missing transition reported", err)
	}
}

// Each visit to a decision state is reported once, after the transition out
// of it is known: with the edge the walk took (the answer's own, or success
// when the answer has none) and the walk's ordinal for that visit, so two
// passes through one state are two reports that differ.
func TestWalk_ReportsEachDecisionVisitWithTheEdgeTaken(t *testing.T) {
	support := strings.Replace(triageAnswer, `"choice":"billing"`, `"choice":"support"`, 1)
	// intake → triage; an answer with no edge of its own goes round through
	// retry, and billing ends the walk.
	d := teamgraph.Definition{
		Entry: "intake",
		States: []teamgraph.State{
			{ID: "intake", Handler: teamgraph.Handler{Kind: teamgraph.HandlerVars, Set: map[string]string{"seen": "intake"}}},
			triageState(),
			{ID: "retry", Handler: teamgraph.Handler{Kind: teamgraph.HandlerVars, Set: map[string]string{"seen": "retry"}}},
			{ID: "billing-desk", Handler: teamgraph.Handler{Kind: teamgraph.HandlerTerminal}},
		},
		Transitions: []teamgraph.Transition{
			{From: "intake", To: "triage", On: "success"},
			{From: "triage", To: "billing-desk", On: "conditional:billing"},
			{From: "triage", To: "retry", On: "success"},
			{From: "retry", To: "triage", On: "success"},
		},
	}
	if err := teamgraph.Validate(d); err != nil {
		t.Fatalf("the test's team is not valid: %v", err)
	}
	answers := []string{support, triageAnswer}
	a := &asked{}
	r := decisionRunner(t, a)
	r.decide = func(context.Context, string, map[string]any, map[string]decisionq.Question) (string, error) {
		a.calls++
		return answers[a.calls-1], nil
	}
	var got []DecisionVisit
	task := &Task{Input: "the ticket"}
	if _, err := Walk(context.Background(), d, task, r, OnDecision(func(_ context.Context, v DecisionVisit) { got = append(got, v) })); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := []DecisionVisit{
		{State: "triage", Visit: 2, Edge: "success", Next: "retry", Answer: support},
		{State: "triage", Visit: 4, Edge: "conditional:billing", Next: "billing-desk", Answer: triageAnswer},
	}
	if len(got) != len(want) {
		t.Fatalf("the walk reported %d decisions, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("decision %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A decision whose call failed made no decision: nothing is reported.
func TestWalk_AFailedDecisionReportsNothing(t *testing.T) {
	reported := 0
	r := decisionRunner(t, &asked{err: &DecisionError{Code: "timeout", Message: "no reply"}})
	if _, err := Walk(context.Background(), decisionTeam(), &Task{}, r, OnDecision(func(context.Context, DecisionVisit) { reported++ })); err == nil {
		t.Fatal("the walk did not fail")
	}
	if reported != 0 {
		t.Errorf("a failed decision was reported %d times", reported)
	}
}
