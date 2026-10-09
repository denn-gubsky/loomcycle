package teamgraph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/decisionq"
)

// decision.go — a state that asks a decision model.
//
// A `decision` state makes one call to a decision model: typed questions about
// a JSON object, answered with probabilities and no text. It binds what was
// answered through `capture` and, when it names a `route` question, takes the
// transition that answer selects. It starts no agent run.
//
// What is checked here needs no model: the questions' shape, which question
// routes, and that every answer it can give has somewhere to go. Whether the
// deployment has the model named, and how many questions that model takes, is
// a fact about a deployment and is reported by the TeamDef tool.

// The two answers a yes/no question routes on.
const (
	RouteTrue  = "true"
	RouteFalse = "false"
)

// DefaultThreshold is the probability of yes at or above which a routed yes/no
// answer is `true`, when the state names none.
const DefaultThreshold = 0.5

// RouteEdge is the transition label a routed answer selects.
func RouteEdge(answer string) string { return OnConditional + ":" + answer }

// RouteAnswers lists every answer a decision state's routed question can give,
// sorted, with ok=false when the state routes on nothing it can: no `route`,
// one naming no question, or a question that is neither a choice nor a yes/no.
func RouteAnswers(h Handler) (answers []string, ok bool) {
	q, has := h.Questions[h.Route]
	if h.Route == "" || !has {
		return nil, false
	}
	switch q.Type {
	case decisionq.TypeNoul:
		return []string{RouteFalse, RouteTrue}, true
	case decisionq.TypeChoice:
		opts := q.Options()
		return opts, len(opts) > 0
	}
	return nil, false
}

// decisionFieldSet names the first decision-only field a handler sets ("" when
// none), for the refusal on a state of another kind.
func decisionFieldSet(h Handler) string {
	return firstOf(fieldIf{"about", len(h.About) > 0}, fieldIf{"questions", len(h.Questions) > 0},
		fieldIf{"route", h.Route != ""}, fieldIf{"threshold", h.Threshold != nil}, fieldIf{"model", h.Model != ""})
}

func validateDecision(out *issues, at stateAt, h Handler) {
	stateID := at.id
	if f := agentFieldSet(h); f != "" {
		out.in(at, f, "team definition: state %q decision handler must not set agent/agents/consolidator — it asks a decision model, it runs no agent", stateID)
	}
	if f := firstOf(fieldIf{"system_prompt", h.SystemPrompt != ""}, fieldIf{"input_template", h.InputTemplate != ""},
		fieldIf{"wait", h.Wait != ""}); f != "" {
		out.in(at, f, "team definition: state %q sets `%s` but is kind %q — a decision model reads `about` and `questions`, and nothing else", stateID, f, h.Kind)
	}
	validateAbout(out, at, h.About)
	for _, fault := range decisionq.Validate(h.Questions, decisionq.AnyModel) {
		field := "questions"
		if fault.Question != "" {
			field = PathKey(field, fault.Question)
		}
		out.in(at, field, "team definition: state %q %s", stateID, fault.Error())
	}
	if h.Route != "" {
		q, has := h.Questions[h.Route]
		switch {
		case !has:
			out.in(at, "route", "team definition: state %q `route` names %q, which is not one of its questions (%s)",
				stateID, h.Route, strings.Join(sortedQuestionNames(h.Questions), ", "))
		case q.Type == decisionq.TypeScore:
			out.in(at, "route", "team definition: state %q `route` names %q, a score — a score is a number, not a choice of "+
				"transition; route on a choice or a yes/no question, and capture the score into a variable", stateID, h.Route)
		}
	}
	if h.Threshold != nil {
		q := h.Questions[h.Route]
		switch {
		case q.Type != decisionq.TypeNoul:
			out.in(at, "threshold", "team definition: state %q sets `threshold`, which says where yes begins for a routed "+
				"yes/no question — this state routes on none", stateID)
		case *h.Threshold <= 0 || *h.Threshold >= 1:
			out.in(at, "threshold", "team definition: state %q `threshold` must be above 0 and below 1 (got %v)", stateID, *h.Threshold)
		}
	}
}

// validateAbout checks the object a decision state's questions are about.
//
// Its string values are expanded when the state runs, by the walk and not by
// prompt assembly: the ${…} tokens a vars state takes, and the previous
// state's output in its slot. Any other {{…}} would reach the model as literal
// braces, so it is refused here, where the author can see why.
func validateAbout(out *issues, at stateAt, about json.RawMessage) {
	stateID := at.id
	trimmed := bytes.TrimSpace(about)
	if len(trimmed) == 0 {
		out.in(at, "about", "team definition: state %q decision handler requires `about`: the JSON object its questions are about", stateID)
		return
	}
	var obj map[string]any
	if trimmed[0] != '{' || json.Unmarshal(trimmed, &obj) != nil {
		out.in(at, "about", "team definition: state %q `about` must be a JSON object", stateID)
		return
	}
	walkStrings(obj, "about", func(path, s string) {
		if readsSecretNamespace(s) {
			out.in(at, path, "team definition: state %q `%s` reads the credentials namespace — what a decision model is "+
				"asked about is recorded on the walk, so a secret must not be put there", stateID, path)
		}
		rest := strings.ReplaceAll(s, ThreadedOutputSlot, "")
		if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
			out.in(at, path, "team definition: state %q `%s` contains a {{…}} placeholder other than %s — a decision "+
				"state resolves ${var.*}, ${now.*}, ${team.*} and %s only", stateID, path, ThreadedOutputSlot, ThreadedOutputSlot)
		}
	})
}

// walkStrings calls fn with every string value under v, in key order, with its
// JSON path. Keys are not values and are not visited.
func walkStrings(v any, path string, fn func(path, s string)) {
	switch t := v.(type) {
	case string:
		fn(path, t)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkStrings(t[k], PathKey(path, k), fn)
		}
	case []any:
		for i, e := range t {
			walkStrings(e, fmt.Sprintf("%s[%d]", path, i), fn)
		}
	}
}

func sortedQuestionNames(qs map[string]decisionq.Question) []string {
	out := make([]string, 0, len(qs))
	for name := range qs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// validateDecisionEdges checks the transitions leaving each decision state
// against the answers it can give: every answer needs an edge (or a `success`
// edge to take the ones without their own), and no edge may wait for an
// answer that cannot come.
//
// It is why a decision is a state kind: the questions are in the definition,
// so a missing edge is refused when the team is saved, not met in the middle
// of a walk.
func validateDecisionEdges(out *issues, d Definition, states map[string]State) {
	checked := map[string]bool{}
	for i, st := range d.States {
		id := st.ID
		// A state whose id is empty or repeats an earlier one is not in
		// states; its identity is the finding, and the edges that name the id
		// belong to its first holder.
		if _, ok := states[id]; !ok || checked[id] {
			continue
		}
		checked[id] = true
		if st.Handler.Kind != HandlerDecision {
			continue
		}
		at := stateAt{index: i, id: id}
		answers, routes := RouteAnswers(st.Handler)
		can := make(map[string]bool, len(answers))
		for _, a := range answers {
			can[a] = true
		}
		labels := map[string]bool{}
		for j, t := range d.Transitions {
			if t.From != id {
				continue
			}
			labels[t.On] = true
			tp := fmt.Sprintf("transitions[%d].on", j)
			if strings.HasPrefix(t.On, OnPushback+":") {
				out.top(tp, "team definition: transition[%d] leaves decision state %q on %q — a decision state takes "+
					"`success` or `conditional:<answer>`; only a consolidator signals a pushback", j, id, t.On)
				continue
			}
			answer, isConditional := strings.CutPrefix(t.On, OnConditional+":")
			switch {
			case !isConditional:
			case st.Handler.Route == "":
				out.top(tp, "team definition: transition[%d] leaves decision state %q on %q, but the state names no "+
					"`route` question, so it always advances on `success`", j, id, t.On)
			case routes && !can[answer]:
				out.top(tp, "team definition: transition[%d] leaves decision state %q on %q, an answer its routed "+
					"question %q cannot give (it answers: %s)", j, id, t.On, st.Handler.Route, strings.Join(answers, ", "))
			}
		}
		if !routes || labels[OnSuccess] {
			continue
		}
		for _, a := range answers {
			if !labels[RouteEdge(a)] {
				out.in(at, "route", "team definition: state %q routes on question %q, and its answer %q has no transition — "+
					"add one `on` %q, or a `success` transition to take every answer without its own",
					id, st.Handler.Route, a, RouteEdge(a))
			}
		}
	}
}
