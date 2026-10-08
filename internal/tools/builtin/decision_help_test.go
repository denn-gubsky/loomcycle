package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// decisionHelpExample is one worked example of the Decision article: the
// request, and the result printed after it ("" when the article shows none).
type decisionHelpExample struct {
	line    int
	request string
	result  string
}

// decisionHelpExamples reads the article's `## Examples` section by the
// convention every tool article uses: a ```json fence is a call, and a
// ```json result fence is what the call before it returned.
func decisionHelpExamples(t *testing.T, topic *help.Topic) []decisionHelpExample {
	t.Helper()
	var (
		out       []decisionHelpExample
		inSection bool
		inFence   bool
		kind      string // "call", "result" or "" (a fence that is neither)
		start     int
		body      []string
	)
	for i, ln := range strings.Split(topic.Content, "\n") {
		trimmed := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(trimmed, "```") && !inFence:
			inFence, start, body, kind = true, i+1, nil, ""
			info := strings.Fields(strings.TrimPrefix(trimmed, "```"))
			if inSection && len(info) > 0 && info[0] == "json" {
				kind = "call"
				if len(info) > 1 && info[1] == "result" {
					kind = "result"
				}
			}
		case strings.HasPrefix(trimmed, "```"):
			inFence = false
			text := strings.Join(body, "\n")
			switch kind {
			case "call":
				out = append(out, decisionHelpExample{line: start, request: text})
			case "result":
				if len(out) == 0 || out[len(out)-1].result != "" {
					t.Fatalf("Decision article line %d: a result with no request before it", start)
				}
				out[len(out)-1].result = text
			}
		case inFence:
			body = append(body, ln)
		case strings.HasPrefix(ln, "## "):
			inSection = strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(ln, "## ")), "Examples")
		}
	}
	return out
}

func compactExample(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return buf.String()
}

// TestDecisionHelp_EveryExampleIsWhatTheToolTakesAndReturns — the article
// cannot drift from the tool. Each example request is run through the tool
// itself (its own input check, the model list, the driver's limits) against a
// provider that answers with the example's own result, and the tool's output
// must be the result the article prints, byte for byte once whitespace is
// dropped. Each printed answer must also be a well-formed answer of its
// question's type, about the options the request gave.
func TestDecisionHelp_EveryExampleIsWhatTheToolTakesAndReturns(t *testing.T) {
	topic, ok := bundledHelp(t).ToolArticle("Decision")
	if !ok {
		t.Fatal("the Decision tool has no help article")
	}
	examples := decisionHelpExamples(t, topic)

	// The article must teach every type alone and all of them together, with
	// the result of each.
	seen := map[string]bool{}
	withResult := 0
	for _, ex := range examples {
		in, _, err := parseDecisionInput(json.RawMessage(ex.request))
		if err != nil {
			t.Fatalf("line %d: the tool's own input check refuses this example: %v", ex.line, err)
		}
		var types []string
		for _, q := range in.Questions {
			types = append(types, q.Type)
		}
		sort.Strings(types)
		if ex.result != "" {
			seen[strings.Join(types, "+")] = true
			withResult++
		}
	}
	for _, want := range []string{"choice", "noul", "score", "choice+noul+score"} {
		if !seen[want] {
			t.Errorf("the article has no example with a result for: %s", want)
		}
	}
	if withResult < 4 {
		t.Fatalf("only %d examples carry a result; the check would assert little", withResult)
	}

	for _, ex := range examples {
		in, _, _ := parseDecisionInput(json.RawMessage(ex.request))

		// The provider's reply: the example's own answers and usage, or a plain
		// noul per question for an example that prints no result.
		var printed struct {
			Model       string                     `json:"model"`
			Provider    string                     `json:"provider"`
			ServedModel string                     `json:"served_model"`
			Answers     map[string]json.RawMessage `json:"answers"`
			Usage       json.RawMessage            `json:"usage"`
		}
		if ex.result != "" {
			dec := json.NewDecoder(strings.NewReader(ex.result))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&printed); err != nil {
				t.Errorf("line %d: the printed result is not the tool's output shape: %v", ex.line, err)
				continue
			}
		} else {
			printed.Answers = map[string]json.RawMessage{}
			for q := range in.Questions {
				printed.Answers[q] = json.RawMessage(`{"type":"noul","noul":0.5}`)
			}
			printed.Usage = json.RawMessage(`{"input_tokens":1,"output_tokens":1}`)
		}
		d := newDecisionDouble(t)
		d.reply = func(w http.ResponseWriter, model string) {
			_ = json.NewEncoder(w).Encode(map[string]any{"model": model, "answers": printed.Answers, "usage": printed.Usage})
		}
		drv, err := decision.New("ollama", decision.Options{ProviderID: "ollama-local", BaseURL: d.url})
		if err != nil {
			t.Fatal(err)
		}
		svc, err := decision.NewService("decide", []decision.ModelSpec{
			{Name: "decide", Provider: "ollama-local", Model: "nimble", Driver: drv},
			{Name: "decide-deep", Provider: "ollama-local", Model: "clef", Driver: drv},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Through a dispatcher that serves the article, so the argument check a
		// run applies (no undeclared argument) is applied to the example too.
		disp := tools.NewDispatcher([]tools.Tool{&Decision{Service: svc}, &Context{Help: bundledHelp(t)}})
		res, err := disp.Call(context.Background(), "Decision", json.RawMessage(ex.request))
		if err != nil || res.IsError {
			t.Errorf("line %d: the tool refuses this example: %v %s", ex.line, err, res.Text)
			continue
		}
		if ex.result == "" {
			continue
		}
		if got, want := res.Text, compactExample(t, ex.result); got != want {
			t.Errorf("line %d: the tool returns something other than the article prints:\n tool: %s\nprint: %s", ex.line, got, want)
		}
		for name, q := range in.Questions {
			raw, ok := printed.Answers[name]
			if !ok {
				t.Errorf("line %d: the printed result has no answer for question %q", ex.line, name)
				continue
			}
			checkPrintedAnswer(t, ex.line, name, q, raw)
		}
		if len(printed.Answers) != len(in.Questions) {
			t.Errorf("line %d: %d answers printed for %d questions", ex.line, len(printed.Answers), len(in.Questions))
		}
	}
}

// checkPrintedAnswer holds one printed answer to the answer type: only its
// fields, the ones its question's type carries, about the options the request
// gave, with numbers that agree with each other.
func checkPrintedAnswer(t *testing.T, line int, name string, q decision.Question, raw json.RawMessage) {
	t.Helper()
	fail := func(format string, a ...any) {
		t.Helper()
		t.Errorf("line %d, answer %q: "+format, append([]any{line, name}, a...)...)
	}
	var a decision.Answer
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		fail("not an answer: %v", err)
		return
	}
	if a.Type != q.Type {
		fail("type %q, but the question is a %s", a.Type, q.Type)
		return
	}
	sum := func() float64 {
		s := 0.0
		for _, p := range a.Probabilities {
			s += p
		}
		return s
	}
	switch q.Type {
	case decision.TypeNoul:
		if a.Noul == nil || *a.Noul < 0 || *a.Noul > 1 || a.Choice != nil || a.Score != nil || a.Probabilities != nil {
			fail("a noul answer is {type, noul} with noul in 0..1: %s", raw)
		}
	case decision.TypeChoice:
		var options map[string]json.RawMessage
		_ = json.Unmarshal(q.Criteria, &options)
		if a.Choice == nil || a.Confidence == nil || a.Noul != nil || a.Score != nil {
			fail("a choice answer carries choice, probabilities and confidence: %s", raw)
			return
		}
		if _, ok := options[*a.Choice]; !ok {
			fail("choice %q is not one of the request's options", *a.Choice)
		}
		if len(a.Probabilities) != len(options) {
			fail("%d probabilities for %d options", len(a.Probabilities), len(options))
		}
		for opt, p := range a.Probabilities {
			if _, ok := options[opt]; !ok {
				fail("a probability for %q, which is not an option", opt)
			}
			if p > a.Probabilities[*a.Choice] {
				fail("option %q is likelier than the choice %q", opt, *a.Choice)
			}
		}
		if math.Abs(sum()-1) > 0.01 {
			fail("the probabilities sum to %.3f", sum())
		}
	case decision.TypeScore:
		var levels []string
		_ = json.Unmarshal(q.Criteria, &levels)
		if a.Score == nil || a.Confidence == nil || a.Noul != nil || a.Choice != nil {
			fail("a score answer carries score, legend, probabilities and confidence: %s", raw)
			return
		}
		if len(a.Legend) != len(levels) || len(a.Probabilities) != len(levels) {
			fail("%d legend entries and %d probabilities for %d levels", len(a.Legend), len(a.Probabilities), len(levels))
		}
		expected := 0.0
		for i, level := range levels {
			key := string(rune('0' + i))
			if a.Legend[key] != level {
				fail("legend[%s] = %q, want the request's level %q", key, a.Legend[key], level)
			}
			expected += float64(i) * a.Probabilities[key]
		}
		if math.Abs(sum()-1) > 0.01 || math.Abs(expected-*a.Score) > 0.01 {
			fail("score %.3f, but the probabilities sum to %.3f and put the expected position at %.3f", *a.Score, sum(), expected)
		}
	}
}

// TestDecisionHelp_IsServedByTheHelpOp — the article is in the topic index and
// is what a model gets from the help call the tool's description points at;
// and a run that holds both tools is sent to it from Decision's description.
func TestDecisionHelp_IsServedByTheHelpOp(t *testing.T) {
	set := bundledHelp(t)
	c := &Context{Help: set}
	idx, _ := callContext(t, c, context.Background(), `{"op":"help"}`)
	listed := false
	for _, e := range idx["topics"].([]any) {
		listed = listed || e.(map[string]any)["name"] == "Decision"
	}
	if !listed {
		t.Error("the help index does not list Decision")
	}
	_, res := callContext(t, c, context.Background(), `{"op":"help","topic":"Decision"}`)
	if res.IsError {
		t.Fatalf("op=help topic=Decision: %s", res.Text)
	}
	for _, want := range []string{"## Arguments", "## Returns", "## Reading the numbers", "## Limits", "## Errors", "## Examples", "not calibrated", `"type": "score"`} {
		if !strings.Contains(res.Text, want) && !strings.Contains(strings.ReplaceAll(res.Text, `\"`, `"`), want) {
			t.Errorf("the served article lacks %q", want)
		}
	}

	// Every code the tool can return is in the article's table, so a model that
	// reads a code can look up what to do.
	topic, _ := set.ToolArticle("Decision")
	for _, code := range []string{
		decision.CodeTooManyQuestions, decision.CodeBadOptions, decision.CodeBadQuestion, decision.CodePromptTooLarge,
		decision.CodeModelNotFound, decision.CodeModelNotAllowed, decision.CodeTimeout, decision.CodeCallFailed,
		decisionNotConfigured, decisionInvalidInput, decisionKeyRestricted,
	} {
		if !strings.Contains(topic.Content, "| `"+code+"` |") {
			t.Errorf("the article's error table has no row for %s", code)
		}
	}

	tool := &Decision{}
	d := tools.NewDispatcher([]tools.Tool{tool, c})
	for _, spec := range d.SpecsFor(context.Background(), []tools.Tool{tool, c}) {
		if spec.Name == "Decision" && !strings.Contains(spec.Description, `{"op":"help","topic":"Decision"}`) {
			t.Errorf("Decision's description in a run does not carry the help call: %s", spec.Description)
		}
	}
}
