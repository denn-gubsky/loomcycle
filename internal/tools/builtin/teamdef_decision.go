package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/decisionq"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// teamdef_decision.go — a team's `decision` states, where they meet the
// deployment: which decision models it has, and the call a walk makes.
//
// What a decision state is, and that every answer it routes on has an edge, is
// teamgraph's to check and refuses a save. What is here cannot refuse one: the
// operator's model list is a fact about a deployment, and a definition written
// on another must still be storable. It is reported as unrunnable.

// decisionIssues reports each decision state this deployment could not run:
// no decision models at all, a model the operator does not list, or more
// questions or options than the named model takes.
func (t *TeamDef) decisionIssues(def teamgraph.Definition) []teamIssue {
	var out []teamIssue
	for i, st := range def.States {
		h := st.Handler
		if h.Kind != teamgraph.HandlerDecision {
			continue
		}
		base := fmt.Sprintf("states[%d].handler", i)
		issue := func(kind, field, detail string) {
			path := base
			if field != "" {
				path += "." + field
			}
			out = append(out, teamIssue{Kind: kind, Severity: severityUnrunnable, Path: path, State: st.ID, Field: field, Detail: detail})
		}
		if t.Decision == nil || t.Decision.Service == nil {
			issue(teamIssueDecisionUnconfigured, "", fmt.Sprintf("state %q asks a decision model, and this deployment declares none", st.ID))
			continue
		}
		svc := t.Decision.Service
		name := h.Model
		if name == "" {
			name = svc.Default()
		}
		var limits *decisionq.Limits
		var names []string
		for _, m := range svc.Models() {
			names = append(names, m.Name)
			if m.Name == name {
				lim := m.Limits
				limits = &lim
			}
		}
		if limits == nil {
			issue(teamIssueDecisionModelUnknown, "model", fmt.Sprintf("state %q names decision model %q, which is not one this deployment offers (%s)",
				st.ID, h.Model, strings.Join(names, ", ")))
			continue
		}
		// The shape was checked when the definition was saved; what a model's
		// own limits add is how many questions and options it takes.
		for _, fault := range decisionq.Validate(h.Questions, *limits) {
			if fault.Code != decisionq.CodeTooManyQuestions && fault.Code != decisionq.CodeBadOptions {
				continue
			}
			field := "questions"
			if fault.Question != "" {
				field = teamgraph.PathKey(field, fault.Question)
			}
			issue(teamIssueDecisionLimits, field, fmt.Sprintf("state %q: decision model %q does not take this: %s", st.ID, name, fault.Message))
		}
	}
	return out
}

// decisionAuthorityIssues refuses a decision state its author may not write.
//
// A decision state reaches a decision model with no agent in between, so
// without this an agent granted TeamDef and not Decision could write a team
// with one, run it, and ask the models it was never given. The rule is the one
// a team's own agents are held to: what a definition can do is within what its
// author holds. An author inside a run must hold the Decision tool, and may
// name only a model its own `decision` block allows. The operator, authoring
// through an operator surface, is not narrowed.
//
// It is judged at create and at fork, on the merged definition: a fork is an
// authoring act by whoever forks, so a state it carries over is judged under
// the forker's authority like one it adds.
func (t *TeamDef) decisionAuthorityIssues(ctx context.Context, def teamgraph.Definition) []teamIssue {
	if authorIsOperatorPlane(ctx) {
		return nil
	}
	holdsTool := false
	for _, name := range tools.AgentTools(ctx) {
		if name == "Decision" {
			holdsTool = true
		}
	}
	var operator []string
	operatorDefault := ""
	if t.Decision != nil && t.Decision.Service != nil {
		operatorDefault = t.Decision.Service.Default()
		for _, m := range t.Decision.Service.Models() {
			operator = append(operator, m.Name)
		}
	}
	var out []teamIssue
	for i, st := range def.States {
		if st.Handler.Kind != teamgraph.HandlerDecision {
			continue
		}
		base := fmt.Sprintf("states[%d].handler", i)
		if !holdsTool {
			out = append(out, teamIssue{Kind: teamIssueDecisionAuthority, Severity: severityRefused, Path: base, State: st.ID,
				Detail: fmt.Sprintf("state %q asks a decision model, and its author does not hold the Decision tool — "+
					"a team may only do what its author may", st.ID)})
			continue
		}
		// With no models on this deployment there is no narrowing to judge
		// against; the state is reported unrunnable by the sweep.
		if operator == nil {
			continue
		}
		// Judged by the name the state will ask at run time: a state naming
		// no model asks the OPERATOR's default, whatever default the author's
		// own block names.
		name := st.Handler.Model
		if name == "" {
			name = operatorDefault
		}
		if !slices.Contains(operator, name) {
			continue // not a model here at all: the sweep reports it unrunnable
		}
		if _, allowed, ok := tools.DecisionPolicy(ctx).Pick(name, operatorDefault, operator); !ok {
			out = append(out, teamIssue{Kind: teamIssueDecisionAuthority, Severity: severityRefused, Path: base + ".model", State: st.ID, Field: "model",
				Detail: fmt.Sprintf("state %q asks decision model %q, which its author may not ask (it may ask: %s) — "+
					"a team may only do what its author may", st.ID, name, strings.Join(allowed, ", "))})
		}
	}
	return out
}

// decider is the call a walk's decision states make: the Decision tool's own,
// so a walk's call is held to what a run's is — the run it is charged to,
// whose provider key it may spend, and the tool's wording of a failure, which
// never carries the operator's endpoint.
//
// The agent narrowing is cleared: a decision state belongs to the team, not
// to whichever agent started the walk, as a team's member agents run under
// their own definitions and not the starter's. What the state may ask was
// judged against its AUTHOR when the team was saved (decisionAuthorityIssues).
func (t *TeamDef) decider() teamrun.DecideFunc {
	return func(ctx context.Context, model string, about map[string]any, questions map[string]decisionq.Question) (string, error) {
		if t.Decision == nil || t.Decision.Service == nil {
			return "", &teamrun.DecisionError{Code: decisionNotConfigured, Message: "this deployment declares no decision models"}
		}
		raw, err := json.Marshal(decisionInput{Model: model, State: aboutJSON(about), Questions: questions})
		if err != nil {
			return "", &teamrun.DecisionError{Code: decisionInvalidInput, Message: "the questions are not readable"}
		}
		res, err := t.Decision.Execute(tools.WithDecisionPolicy(ctx, nil), raw)
		if err != nil {
			return "", err
		}
		if res.IsError {
			code := DecisionFailureCode(res.Text)
			if code == "" {
				return "", &teamrun.DecisionError{Code: decisionq.CodeCallFailed, Message: res.Text}
			}
			return "", &teamrun.DecisionError{Code: code, Message: strings.TrimPrefix(res.Text, "Decision: "+code+": ")}
		}
		return res.Text, nil
	}
}

// aboutJSON encodes a value decoded from JSON, which cannot fail to encode.
func aboutJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}
