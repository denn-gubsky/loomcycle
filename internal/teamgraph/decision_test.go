package teamgraph

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/decisionq"
)

// triage is the request's example: a decision state that routes a ticket to
// one of three desks and binds three answers.
func triage() Definition {
	desk := func(id string) State { return State{ID: id, Handler: Handler{Kind: HandlerTerminal}} }
	return Definition{
		Entry: "intake",
		States: []State{
			{ID: "intake", Handler: Handler{Kind: HandlerAgent, Agent: "reader"}},
			{ID: "triage", Handler: Handler{
				Kind:  HandlerDecision,
				About: json.RawMessage(`{"ticket":"{{thread.output}}","tier":"${var.tier}","n":3}`),
				Questions: map[string]decisionq.Question{
					"route": {Type: "choice", Instructions: "Which team?",
						Criteria: json.RawMessage(`{"billing":"invoices","support":"bugs","sales":null}`)},
					"urgent": {Type: "noul", Instructions: "Within the hour?"},
					"detail": {Type: "score", Instructions: "How complete?", Criteria: json.RawMessage(`["none","some","all"]`)},
				},
				Route:   "route",
				Capture: map[string]string{"team": "$.answers.route.choice", "urgent": "$.answers.urgent.noul"},
			}},
			desk("billing-desk"), desk("support-desk"), desk("sales-desk"),
		},
		Transitions: []Transition{
			{From: "intake", To: "triage", On: OnSuccess},
			{From: "triage", To: "billing-desk", On: "conditional:billing"},
			{From: "triage", To: "support-desk", On: "conditional:support"},
			{From: "triage", To: "sales-desk", On: "conditional:sales"},
		},
	}
}

func TestDecision_ARoutedChoiceWithAnEdgePerOptionIsValid(t *testing.T) {
	if got := ValidateAll(triage()); len(got) != 0 {
		t.Fatalf("issues = %v, want none", got)
	}
}

// Every answer the routed question can give needs somewhere to go: an edge of
// its own, or the state's `success` edge.
func TestDecision_ARoutedAnswerWithNoEdgeIsRefusedUnlessSuccessTakesIt(t *testing.T) {
	d := triage()
	d.Transitions = d.Transitions[:3] // no conditional:sales
	d.States = d.States[:4]
	got := ValidateAll(d)
	if len(got) != 1 || got[0].Path != "states[1].handler.route" || got[0].State != "triage" || got[0].Field != "route" ||
		!strings.Contains(got[0].Msg, `"sales"`) || !strings.Contains(got[0].Msg, `"conditional:sales"`) {
		t.Fatalf("issues = %v, want one at states[1].handler.route naming the option sales", got)
	}

	d.Transitions = append(d.Transitions, Transition{From: "triage", To: "support-desk", On: OnSuccess})
	if got := ValidateAll(d); len(got) != 0 {
		t.Errorf("with a success edge: issues = %v, want none", got)
	}
}

func TestDecision_AYesNoRouteNeedsBothAnswers(t *testing.T) {
	d := triage()
	d.States[1].Handler.Route = "urgent"
	half := 0.8
	d.States[1].Handler.Threshold = &half
	d.States = d.States[:4]
	d.Transitions = []Transition{
		{From: "intake", To: "triage", On: OnSuccess},
		{From: "triage", To: "billing-desk", On: "conditional:true"},
		{From: "triage", To: "support-desk", On: "conditional:false"},
	}
	if got := ValidateAll(d); len(got) != 0 {
		t.Fatalf("issues = %v, want none", got)
	}
	d.Transitions = d.Transitions[:2]
	d.States = d.States[:3]
	got := ValidateAll(d)
	if len(got) != 1 || got[0].Path != "states[1].handler.route" || !strings.Contains(got[0].Msg, `"false"`) {
		t.Errorf("issues = %v, want one naming the answer false", got)
	}
}

// An edge that waits for an answer the state cannot give would never be
// taken, and a state it leads to alone would look reachable.
func TestDecision_AnEdgeNoAnswerSelectsIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Definition)
		want string
	}{
		"an option the question does not have": {
			func(d *Definition) { d.Transitions[3].On = "conditional:legal" },
			"cannot give",
		},
		"a conditional with no route": {
			func(d *Definition) {
				d.States[1].Handler.Route = ""
				d.Transitions = append(d.Transitions, Transition{From: "triage", To: "sales-desk", On: OnSuccess})
			},
			"names no `route` question",
		},
		"a pushback": {
			func(d *Definition) { d.Transitions[3].On = "pushback:redo" },
			"only a consolidator signals a pushback",
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := triage()
			tc.edit(&d)
			var hit bool
			for _, is := range ValidateAll(d) {
				if strings.HasPrefix(is.Path, "transitions[") && strings.HasSuffix(is.Path, ".on") && strings.Contains(is.Msg, tc.want) {
					hit = true
				}
			}
			if !hit {
				t.Errorf("issues = %v, want one on a transition containing %q", ValidateAll(d), tc.want)
			}
		})
	}
}

func TestDecision_RefusesAStateItCannotAsk(t *testing.T) {
	zero, one := 0.0, 1.0
	for name, tc := range map[string]struct {
		edit func(*Handler)
		path string
		want string
	}{
		"no about":         {func(h *Handler) { h.About = nil }, "about", "requires `about`"},
		"about not object": {func(h *Handler) { h.About = json.RawMessage(`["a"]`) }, "about", "must be a JSON object"},
		"no questions":     {func(h *Handler) { h.Questions = nil }, "questions", "at least one question is required"},
		"one option": {func(h *Handler) {
			h.Questions["route"] = decisionq.Question{Type: "choice", Instructions: "x", Criteria: json.RawMessage(`{"billing":null}`)}
		}, "questions.route", "bad_options"},
		"unknown type": {func(h *Handler) {
			h.Questions["urgent"] = decisionq.Question{Type: "maybe", Instructions: "x"}
		}, "questions.urgent", "bad_question"},
		"route names no question": {func(h *Handler) { h.Route = "team" }, "route", "not one of its questions (detail, route, urgent)"},
		"route names a score":     {func(h *Handler) { h.Route = "detail" }, "route", "a score"},
		"threshold with a choice": {func(h *Handler) { h.Threshold = &zero }, "threshold", "routes on none"},
		"threshold out of range": {func(h *Handler) {
			h.Route, h.Threshold = "urgent", &one
		}, "threshold", "above 0 and below 1"},
		"another placeholder": {func(h *Handler) {
			h.About = json.RawMessage(`{"a":{"b":["x","{{document:/secret}} {{thread.output}}"]}}`)
		}, "about.a.b[1]", "placeholder other than {{thread.output}}"},
		"a credential": {func(h *Handler) {
			h.About = json.RawMessage(`{"k":"${run.credentials.api}"}`)
		}, "about.k", "credentials namespace"},
		"an agent":          {func(h *Handler) { h.Agent = "reader" }, "agent", "runs no agent"},
		"a system prompt":   {func(h *Handler) { h.SystemPrompt = "be brief" }, "system_prompt", "nothing else"},
		"an input template": {func(h *Handler) { h.InputTemplate = "x" }, "input_template", "nothing else"},
	} {
		t.Run(name, func(t *testing.T) {
			d := triage()
			tc.edit(&d.States[1].Handler)
			var hit bool
			for _, is := range ValidateAll(d) {
				if is.Path == "states[1].handler."+tc.path && is.State == "triage" && strings.Contains(is.Msg, tc.want) {
					hit = true
				}
			}
			if !hit {
				t.Errorf("issues = %v, want one at states[1].handler.%s containing %q", ValidateAll(d), tc.path, tc.want)
			}
		})
	}
}

// A decision state's fields configure nothing on any other kind, so one that
// carries them is refused, not stored as a setting that does nothing.
func TestDecision_ItsFieldsAreRefusedOnAnotherKind(t *testing.T) {
	half := 0.5
	for field, edit := range map[string]func(*Handler){
		"about":     func(h *Handler) { h.About = json.RawMessage(`{}`) },
		"questions": func(h *Handler) { h.Questions = map[string]decisionq.Question{"q": {Type: "noul", Instructions: "x"}} },
		"route":     func(h *Handler) { h.Route = "q" },
		"threshold": func(h *Handler) { h.Threshold = &half },
		"model":     func(h *Handler) { h.Model = "decide" },
	} {
		d := triage()
		edit(&d.States[0].Handler)
		got := ValidateAll(d)
		if len(got) != 1 || got[0].Path != "states[0].handler."+field || !strings.Contains(got[0].Msg, "(decision only)") {
			t.Errorf("%s on an agent state: issues = %v, want one at states[0].handler.%s", field, got, field)
		}
	}
}

func TestDecision_AStateCarriesNoHooks(t *testing.T) {
	d := triage()
	raw, _ := json.Marshal(d)
	raw = []byte(strings.Replace(string(raw), `"kind":"decision"`, `"kind":"decision","hooks":{"run_end":["x"]}`, 1))
	withHooks, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var hit bool
	for _, is := range ValidateAll(withHooks) {
		if is.Path == "states[1].handler.hooks" && strings.Contains(is.Msg, "starts no run") {
			hit = true
		}
	}
	if !hit {
		t.Errorf("issues = %v, want the hooks refused on a state that starts no run", ValidateAll(withHooks))
	}
}

// The new fields are omitempty: a definition with no decision state is the
// bytes, and so the hash, it was before they existed.
func TestDecision_ADefinitionWithoutOneEncodesAsBefore(t *testing.T) {
	raw, err := json.Marshal(Handler{Kind: HandlerAgent, Agent: "reader"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"kind":"agent","agent":"reader"}`; got != want {
		t.Errorf("an agent handler encodes as %s, want %s", got, want)
	}
	d := triage()
	before := Sign("t", d)
	d.States[1].Handler.Route = "urgent"
	if Sign("t", d) == before {
		t.Error("changing a decision state's route did not change the definition's hash")
	}
}

func TestDecision_KeysAreReadAsWritten(t *testing.T) {
	raw, _ := json.Marshal(triage())
	if got := KeyIssues(raw, nil, nil); len(got) != 0 {
		t.Fatalf("a definition as encoded has key issues: %v", got)
	}
	bad := strings.Replace(string(raw), `"instructions":"Within the hour?"`, `"instruction":"Within the hour?"`, 1)
	got := KeyIssues([]byte(bad), nil, nil)
	if len(got) != 1 || got[0].Path != "states[1].handler.questions.urgent.instruction" || !strings.Contains(got[0].Msg, `did you mean "instructions"`) {
		t.Errorf("issues = %v, want the misspelt key reported with a suggestion", got)
	}
	// What a question's criteria and the state's `about` hold are the author's
	// own names: only an exact repeat is an issue.
	repeat := strings.Replace(string(raw), `"n":3`, `"n":3,"n":4`, 1)
	if got := KeyIssues([]byte(repeat), nil, nil); len(got) != 1 || got[0].Path != "states[1].handler.about.n" {
		t.Errorf("issues = %v, want the repeated key in `about` reported", got)
	}
}

func TestRouteAnswers(t *testing.T) {
	h := triage().States[1].Handler
	if got, ok := RouteAnswers(h); !ok || !reflect.DeepEqual(got, []string{"billing", "sales", "support"}) {
		t.Errorf("a choice routes on %v (%v)", got, ok)
	}
	h.Route = "urgent"
	if got, ok := RouteAnswers(h); !ok || !reflect.DeepEqual(got, []string{"false", "true"}) {
		t.Errorf("a yes/no routes on %v (%v)", got, ok)
	}
	for _, route := range []string{"", "detail", "nobody"} {
		h.Route = route
		if _, ok := RouteAnswers(h); ok {
			t.Errorf("route %q is reported as routable", route)
		}
	}
}

func TestRenderMermaid_ADecisionStateSaysWhatItRoutesOn(t *testing.T) {
	got := RenderMermaid("t", triage(), "")
	for _, want := range []string{"triage --> billing-desk: conditional:billing", "note right of triage\n    decision: routes on route\n  end note"} {
		if !strings.Contains(got, want) {
			t.Errorf("the diagram lacks %q:\n%s", want, got)
		}
	}
}
