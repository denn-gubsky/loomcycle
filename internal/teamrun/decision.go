package teamrun

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/decisionq"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// decision.go — running a `decision` state.
//
// The state makes one call to a decision model and starts no run. The call is
// the walk's own: it is made on the walk's ctx, so whatever the server charges
// a decision call to (the run on that ctx) is the walk's run.

// DecideFunc asks one of the operator's decision models (model "" = the
// default) the questions about `about`, and returns its answer as JSON, an
// object whose `answers` holds one entry per question.
//
// A failure it reports as a *DecisionError carries the model's code; any other
// error is a call that failed for a reason with no code.
type DecideFunc func(ctx context.Context, model string, about map[string]any, questions map[string]decisionq.Question) (answer string, err error)

// WithDecider supplies the decision call. Without it a definition containing a
// decision state cannot run: refused at the state, like a starter with no
// channel executor.
func WithDecider(f DecideFunc) RunnerOption {
	return func(r *agentRunner) { r.decide = f }
}

// DecisionError is a decision state whose call failed. Code is the decision
// model's own (timeout, model_not_found, prompt_too_large, …), so a client
// can mark the state and say why without reading the sentence.
type DecisionError struct {
	State   string
	Code    string
	Message string
}

func (e *DecisionError) Error() string {
	return fmt.Sprintf("decision: %s: %s", e.Code, e.Message)
}

func (r *agentRunner) runDecision(ctx context.Context, st teamgraph.State, task *Task, env Env) (Outcome, error) {
	h := st.Handler
	if r.decide == nil {
		return Outcome{}, &DecisionError{State: st.ID, Code: "decision_not_configured",
			Message: "the state asks a decision model, and none is wired on this server"}
	}
	var about map[string]any
	if err := json.Unmarshal(h.About, &about); err != nil {
		// Validate refused this at create and fork; a definition stored some
		// other way fails here rather than asking about nothing.
		return Outcome{}, fmt.Errorf("state %q `about` is not a JSON object: %w", st.ID, err)
	}
	about, _ = r.expandAbout(st.ID, about, task.Input, env).(map[string]any)

	answer, err := r.decide(ctx, h.Model, about, h.Questions)
	if err != nil {
		if de, ok := err.(*DecisionError); ok {
			de.State = st.ID
		}
		return Outcome{}, err
	}
	// The state hands on what it was handed: the answer reaches later states
	// through the variables it binds, and the step through Answer.
	oc := Outcome{Output: task.Input, Answer: answer}
	if h.Route != "" {
		routed, err := routedAnswer(h, answer)
		if err != nil {
			return Outcome{}, &DecisionError{State: st.ID, Code: decisionq.CodeCallFailed, Message: err.Error()}
		}
		oc.Edge, oc.Fallback = teamgraph.RouteEdge(routed), teamgraph.OnSuccess
	}
	return r.capturedFrom(st, task, oc, answer)
}

// expandAbout resolves the ${…} tokens in every string value under v, then
// drops the previous state's output into its slot. In that order, and with a
// variable carrying {{ or }} refused by Expand, so nothing a variable holds
// can write the slot's marker: the hand-off lands only where the definition's
// author put it, and what it contains is never scanned.
func (r *agentRunner) expandAbout(stateID string, v any, threaded string, env Env) any {
	switch t := v.(type) {
	case string:
		out, refused := Expand(t, env)
		r.noteRefused(stateID, "about", refused)
		return strings.ReplaceAll(out, ThreadedOutputSlot, threaded)
	case map[string]any:
		for k, e := range t {
			t[k] = r.expandAbout(stateID, e, threaded, env)
		}
	case []any:
		for i, e := range t {
			t[i] = r.expandAbout(stateID, e, threaded, env)
		}
	}
	return v
}

// routedAnswer reads, from a decision answer, what the state's routed question
// answered: the chosen option of a choice, or true/false for a yes/no at the
// state's threshold.
func routedAnswer(h teamgraph.Handler, answer string) (string, error) {
	var doc struct {
		Answers map[string]struct {
			Choice *string  `json:"choice"`
			Noul   *float64 `json:"noul"`
		} `json:"answers"`
	}
	if err := json.Unmarshal([]byte(answer), &doc); err != nil {
		return "", fmt.Errorf("the answer is not readable")
	}
	a, ok := doc.Answers[h.Route]
	if !ok {
		return "", fmt.Errorf("the reply carries no answer to %q", h.Route)
	}
	switch h.Questions[h.Route].Type {
	case decisionq.TypeChoice:
		if a.Choice == nil || *a.Choice == "" {
			return "", fmt.Errorf("the answer to %q names no option", h.Route)
		}
		return *a.Choice, nil
	case decisionq.TypeNoul:
		if a.Noul == nil {
			return "", fmt.Errorf("the answer to %q carries no probability", h.Route)
		}
		threshold := teamgraph.DefaultThreshold
		if h.Threshold != nil {
			threshold = *h.Threshold
		}
		if *a.Noul >= threshold {
			return teamgraph.RouteTrue, nil
		}
		return teamgraph.RouteFalse, nil
	}
	return "", fmt.Errorf("question %q is not one a state can route on", h.Route)
}
