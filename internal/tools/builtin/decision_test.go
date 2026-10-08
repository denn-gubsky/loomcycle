package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// decisionDouble is a /v1/systemone double. reply, when set, answers a call;
// otherwise every call gets `answers` back, served as the model it asked for.
type decisionDouble struct {
	mu      sync.Mutex
	bodies  [][]byte
	answers string
	reply   func(w http.ResponseWriter, model string)
	url     string
}

func newDecisionDouble(t *testing.T) *decisionDouble {
	t.Helper()
	d := &decisionDouble{answers: `{"q":{"type":"noul","noul":0.5}}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		d.mu.Lock()
		d.bodies = append(d.bodies, raw)
		reply, answers := d.reply, d.answers
		d.mu.Unlock()
		if reply != nil {
			reply(w, body.Model)
			return
		}
		_, _ = io.WriteString(w, `{"model":"`+body.Model+`:served","answers":`+answers+`,"usage":{"input_tokens":1116,"output_tokens":4}}`)
	}))
	t.Cleanup(srv.Close)
	d.url = srv.URL
	return d
}

func (d *decisionDouble) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.bodies)
}

// lastModel is the model the provider was last asked for.
func (d *decisionDouble) lastModel(t *testing.T) string {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.bodies) == 0 {
		t.Fatal("the provider was never called")
	}
	var body struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(d.bodies[len(d.bodies)-1], &body)
	return body.Model
}

// decisionTool is a Decision tool over d, with the operator's list
// decide (default) = nimble, deep = clef, lit = nimble-lit. opts adjusts the
// driver (a key, a timeout).
func decisionTool(t *testing.T, d *decisionDouble, opts ...func(*decision.Options)) *Decision {
	t.Helper()
	o := decision.Options{ProviderID: "ollama-local", BaseURL: d.url}
	for _, f := range opts {
		f(&o)
	}
	drv, err := decision.New("ollama", o)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := decision.NewService("decide", []decision.ModelSpec{
		{Name: "decide", Provider: "ollama-local", Model: "nimble", Driver: drv},
		{Name: "deep", Provider: "ollama-local", Model: "clef", Driver: drv},
		{Name: "lit", Provider: "ollama-local", Model: "nimble-lit", Driver: drv},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Decision{Service: svc}
}

func decide(t *testing.T, tool *Decision, ctx context.Context, input string) tools.Result {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute returned a Go error: %v", err)
	}
	return res
}

// inRun is the context of a call made from inside a run: the tool answers no
// other.
func inRun() context.Context { return tools.WithRunID(context.Background(), "run-1") }

const oneNoul = `"state":{"x":1},"questions":{"q":{"type":"noul","instructions":"Is it?"}}`

// TestDecision_ReturnsEveryAnswerFieldUnchanged — the tool's result carries
// each answer byte for byte as the model gave it: a noul of 0 (a certain "no",
// not a missing field), every probability, and a field this code has never
// heard of. The request reaches the provider as written, a null option
// description included.
func TestDecision_ReturnsEveryAnswerFieldUnchanged(t *testing.T) {
	d := newDecisionDouble(t)
	answers := map[string]string{
		"route":  `{"type":"choice","choice":"billing","probabilities":{"billing":0.971,"support":0.018,"sales":0.010},"confidence":0.865,"margin":0.953}`,
		"urgent": `{"type":"noul","noul":0}`,
		"detail": `{"type":"score","score":1.51,"legend":{"0":"no detail","1":"some detail","2":"everything needed"},"probabilities":{"0":0.097,"1":0.295,"2":0.608},"confidence":0.190}`,
	}
	d.answers = `{"route":` + answers["route"] + `,"urgent":` + answers["urgent"] + `,"detail":` + answers["detail"] + `}`
	tool := decisionTool(t, d)

	res := decide(t, tool, inRun(), `{
		"state": {"ticket": "My invoice for March was charged twice.", "tags": ["billing", {"tier": "gold"}]},
		"questions": {
			"route":  {"type": "choice", "instructions": "Which team?", "criteria": {"billing": "invoices, refunds", "support": "bugs, outages", "sales": null}},
			"urgent": {"type": "noul", "instructions": "Reply within the hour?"},
			"detail": {"type": "score", "instructions": "How complete?", "criteria": ["no detail", "some detail", "everything needed"]}}}`)
	if res.IsError {
		t.Fatalf("result: %s", res.Text)
	}
	var out struct {
		Model       string                     `json:"model"`
		Provider    string                     `json:"provider"`
		ServedModel string                     `json:"served_model"`
		Answers     map[string]json.RawMessage `json:"answers"`
		Usage       map[string]int             `json:"usage"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("the result is not JSON: %v\n%s", err, res.Text)
	}
	if out.Model != "decide" || out.Provider != "ollama-local" || out.ServedModel != "nimble:served" {
		t.Errorf("model %q provider %q served_model %q, want decide / ollama-local / nimble:served", out.Model, out.Provider, out.ServedModel)
	}
	if out.Usage["input_tokens"] != 1116 || out.Usage["output_tokens"] != 4 {
		t.Errorf("usage = %v, want 1116 in and 4 out", out.Usage)
	}
	for name, want := range answers {
		if got := string(out.Answers[name]); got != want {
			t.Errorf("answer %q changed:\n got %s\nwant %s", name, got, want)
		}
	}
	// What the provider received: the null description as null, the nested state whole.
	var sent struct {
		State     map[string]any `json:"state"`
		Questions map[string]struct {
			Criteria json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(d.bodies[0], &sent); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(sent.Questions["route"].Criteria, []byte(`"sales":null`)) {
		t.Errorf("the choice criteria reached the provider as %s, want sales: null kept", sent.Questions["route"].Criteria)
	}
	if tags, _ := sent.State["tags"].([]any); len(tags) != 2 {
		t.Errorf("the state reached the provider as %v, want its nested array whole", sent.State)
	}
}

// TestDecision_PicksTheModel — a call naming no model gets the default, a
// listed name gets that model, and a name outside the list is refused without
// a call, naming the names the caller may use. An agent's own `decision` block
// narrows all three.
func TestDecision_PicksTheModel(t *testing.T) {
	d := newDecisionDouble(t)
	tool := decisionTool(t, d)
	narrowed := tools.WithDecisionPolicy(inRun(), &config.AgentDecision{Default: "lit", Models: []string{"deep", "lit"}})

	for _, c := range []struct {
		name      string
		ctx       context.Context
		model     string
		wantAsked string // the model the provider is asked for
		wantName  string // "model" in the result
	}{
		{"none named: the operator's default", inRun(), "", "nimble", "decide"},
		{"a listed name", inRun(), "deep", "clef", "deep"},
		{"narrowed, none named: the agent's default", narrowed, "", "nimble-lit", "lit"},
		{"narrowed, a name inside the agent's list", narrowed, "deep", "clef", "deep"},
	} {
		res := decide(t, tool, c.ctx, `{"model":"`+c.model+`",`+oneNoul+`}`)
		if res.IsError {
			t.Errorf("%s: %s", c.name, res.Text)
			continue
		}
		if got := d.lastModel(t); got != c.wantAsked || !strings.Contains(res.Text, `"model":"`+c.wantName+`"`) {
			t.Errorf("%s: asked the provider for %q and reported %s; want %q reported as %q", c.name, got, res.Text, c.wantAsked, c.wantName)
		}
	}

	before := d.calls()
	for _, c := range []struct {
		name  string
		ctx   context.Context
		model string
		names string // the allowed names the refusal lists
	}{
		// "nimble" is the model's own name: a caller reaches a model only by the
		// name the operator listed it under.
		{"outside the operator's list", inRun(), "nimble", "decide, deep, lit"},
		{"in the operator's list, outside the agent's", narrowed, "decide", "deep, lit"},
	} {
		res := decide(t, tool, c.ctx, `{"model":"`+c.model+`",`+oneNoul+`}`)
		if !res.IsError || !strings.HasPrefix(res.Text, "Decision: model_not_allowed: ") ||
			res.Error == nil || res.Error.Category != tools.CategoryValidation ||
			!strings.Contains(res.Error.Description, "name one of: "+c.names+".") {
			t.Errorf("%s: result %q %+v, want model_not_allowed listing %q", c.name, res.Text, res.Error, c.names)
		}
	}
	if d.calls() != before {
		t.Errorf("a refused model reached the provider (%d calls)", d.calls()-before)
	}
}

// TestDecision_EachFaultIsAClassifiedErrorSayingWhatToDo — every code reaches
// the caller as "Decision: <code>: …" with a category, whether resending can
// work, and a next step the caller can act on.
func TestDecision_EachFaultIsAClassifiedErrorSayingWhatToDo(t *testing.T) {
	refuse := func(status int, msg string) func(http.ResponseWriter, string) {
		return func(w http.ResponseWriter, _ string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
		}
	}
	many := `{"state":{},"questions":{`
	for i := 0; i < 65; i++ {
		many += `"q` + string(rune('A'+i/26)) + string(rune('a'+i%26)) + `":{"type":"noul","instructions":"Is it?"},`
	}
	many = strings.TrimSuffix(many, ",") + `}}`

	for _, c := range []struct {
		name      string
		reply     func(http.ResponseWriter, string)
		input     string
		code      string
		category  tools.ErrorCategory
		retryable bool
		next      string // a phrase of the next step
		wantCalls int
	}{
		{"too many questions", nil, many,
			"too_many_questions", tools.CategoryValidation, false, "at most 64 questions: split them", 0},
		{"a choice with one option", nil, `{"state":{},"questions":{"q":{"type":"choice","instructions":"Which?","criteria":{"only":null}}}}`,
			"bad_options", tools.CategoryValidation, false, "A choice takes 2 to 26 options", 0},
		{"an unknown type", nil, `{"state":{},"questions":{"q":{"type":"rank","instructions":"Which?"}}}`,
			"bad_question", tools.CategoryValidation, false, "Each question needs a type (choice, noul or score)", 0},
		{"missing instructions", nil, `{"state":{},"questions":{"q":{"type":"noul"}}}`,
			"bad_question", tools.CategoryValidation, false, "instructions", 0},
		{"no state", nil, `{"questions":{"q":{"type":"noul","instructions":"Is it?"}}}`,
			"invalid_input", tools.CategoryValidation, false, "Pass `state`", 0},
		{"a prompt over the model's context", refuse(400, "prompt 0 has 12144 tokens; expected 1–8194 (input is never truncated)"), `{` + oneNoul + `}`,
			"prompt_too_large", tools.CategoryBusiness, false, "Shorten the state or the criteria, or ask fewer questions", 1},
		{"a request over the byte cap", refuse(413, "text and schema must not exceed 64 KiB"), `{` + oneNoul + `}`,
			"prompt_too_large", tools.CategoryBusiness, false, "Nothing is shortened for you", 1},
		{"a model the provider does not serve", refuse(404, `model "nimble" not found`), `{` + oneNoul + `}`,
			"model_not_found", tools.CategoryBusiness, false, "Name another of: decide, deep, lit.", 1},
		{"the provider failing", refuse(500, "boom"), `{` + oneNoul + `}`,
			"call_failed", tools.CategoryTransient, true, "Send the same call once more", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newDecisionDouble(t)
			d.reply = c.reply
			res := decide(t, decisionTool(t, d), inRun(), c.input)
			if !res.IsError || !strings.HasPrefix(res.Text, "Decision: "+c.code+": ") {
				t.Fatalf("result = %q, want a failure coded %s", res.Text, c.code)
			}
			if res.Error == nil || res.Error.Category != c.category || res.Error.Retryable != c.retryable {
				t.Errorf("classified %+v, want %s retryable=%v", res.Error, c.category, c.retryable)
			} else if !strings.Contains(res.Error.Description, c.next) {
				t.Errorf("next step %q does not say %q", res.Error.Description, c.next)
			}
			if d.calls() != c.wantCalls {
				t.Errorf("%d calls reached the provider, want %d", d.calls(), c.wantCalls)
			}
		})
	}

	t.Run("the prompt's size is reported", func(t *testing.T) {
		d := newDecisionDouble(t)
		d.reply = refuse(400, "prompt 0 has 12144 tokens; expected 1–8194 (input is never truncated)")
		res := decide(t, decisionTool(t, d), inRun(), `{`+oneNoul+`}`)
		if !strings.Contains(res.Text, "12144 tokens") || !strings.Contains(res.Text, "at most 8194") {
			t.Errorf("result = %q, want the request's size and the model's limit", res.Text)
		}
	})
	t.Run("a timeout", func(t *testing.T) {
		d := newDecisionDouble(t)
		release := make(chan struct{})
		defer close(release)
		d.reply = func(http.ResponseWriter, string) { <-release }
		tool := decisionTool(t, d, func(o *decision.Options) { o.Timeout = 50 * time.Millisecond })
		res := decide(t, tool, inRun(), `{`+oneNoul+`}`)
		if !strings.HasPrefix(res.Text, "Decision: timeout: ") || res.Error == nil ||
			res.Error.Category != tools.CategoryTransient || !res.Error.Retryable {
			t.Errorf("result = %q %+v, want a retryable timeout", res.Text, res.Error)
		}
	})
	t.Run("an unreachable provider does not name its host", func(t *testing.T) {
		d := newDecisionDouble(t)
		tool := decisionTool(t, d, func(o *decision.Options) { o.BaseURL = "http://127.0.0.1:1" })
		res := decide(t, tool, inRun(), `{`+oneNoul+`}`)
		if !strings.HasPrefix(res.Text, "Decision: call_failed: ") || strings.Contains(res.Text, "127.0.0.1") {
			t.Errorf("result = %q, want call_failed without the endpoint's address", res.Text)
		}
	})
	t.Run("a run barred from the operator's key", func(t *testing.T) {
		d := newDecisionDouble(t)
		tool := decisionTool(t, d, func(o *decision.Options) { o.APIKey, o.KeyEnvName = "test-operator-key", "OLLAMA_API_KEY" })
		res := decide(t, tool, providers.WithOperatorKeyAllowed(inRun(), false), `{`+oneNoul+`}`)
		if !strings.HasPrefix(res.Text, "Decision: operator_key_restricted: ") || res.Error == nil ||
			res.Error.Category != tools.CategoryPermission || res.Error.Retryable ||
			!strings.Contains(res.Error.Description, "supply the tenant's own provider credential") {
			t.Errorf("result = %q %+v, want the operator-key refusal", res.Text, res.Error)
		}
		if strings.Contains(res.Text, "test-operator-key") || d.calls() != 0 {
			t.Errorf("a restricted run: text %q after %d calls, want no key and no call", res.Text, d.calls())
		}
	})
	t.Run("no decision models", func(t *testing.T) {
		res := decide(t, &Decision{}, inRun(), `{`+oneNoul+`}`)
		if !strings.HasPrefix(res.Text, "Decision: decision_not_configured: ") || res.Error == nil ||
			res.Error.Category != tools.CategoryBusiness || res.Error.Retryable {
			t.Errorf("result = %q %+v, want a business refusal coded decision_not_configured", res.Text, res.Error)
		}
	})
}

// TestDecision_ACallWithNoRunIsRefused — a run's context is what holds a call
// to the caller's key rule and charges its tokens. A call dispatched with no
// run on its context would be held to neither (the key rule is fail-open when
// nothing stamped it), so the tool refuses it before a key is resolved: a
// restricted caller on such a path gets no call, like any other.
func TestDecision_ACallWithNoRunIsRefused(t *testing.T) {
	d := newDecisionDouble(t)
	tool := decisionTool(t, d, func(o *decision.Options) { o.APIKey, o.KeyEnvName = "test-operator-key", "OLLAMA_API_KEY" })
	res := decide(t, tool, context.Background(), `{`+oneNoul+`}`)
	if !res.IsError || !strings.HasPrefix(res.Text, "Decision: no_run: ") ||
		res.Error == nil || res.Error.Category != tools.CategoryBusiness || res.Error.Retryable {
		t.Errorf("result = %q %+v, want a refusal coded no_run", res.Text, res.Error)
	}
	if d.calls() != 0 {
		t.Errorf("a call with no run reached the provider %d times", d.calls())
	}
	if res := decide(t, tool, inRun(), `{`+oneNoul+`}`); res.IsError || d.calls() != 1 {
		t.Errorf("the same call inside a run: %q after %d calls, want an answer", res.Text, d.calls())
	}
}

// TestDecision_ModelVisibleTextNamesNoDesignDocument — the description and the
// schema are read by models on every turn.
func TestDecision_ModelVisibleTextNamesNoDesignDocument(t *testing.T) {
	tool := &Decision{}
	text := tool.Description() + string(tool.InputSchema())
	if strings.Contains(text, "RFC") {
		t.Error("the Decision tool's description or schema cites a design document")
	}
	for _, want := range []string{"choice", "noul", "score", "NOT calibrated", "Do NOT use it", "topic=Decision"} {
		if !strings.Contains(tool.Description(), want) {
			t.Errorf("the description does not mention %q", want)
		}
	}
	if len(tool.Description()) > 1200 {
		t.Errorf("the description is %d bytes: every granted agent pays for it on every turn", len(tool.Description()))
	}
}

// TestDecisionPolicy_IsSetWhereverTheMemoryPolicyIs — every place a run's
// context is built sets the agent's decision block, the empty one included: a
// sub-agent's context derives from its parent's, so a site that skipped it
// would run the child under the parent's narrowing. Counted against the memory
// policy, which every such site sets.
func TestDecisionPolicy_IsSetWhereverTheMemoryPolicyIs(t *testing.T) {
	for _, f := range []string{"../../api/http/server.go", "../../api/http/resume.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		sibling := strings.Count(src, "RecallIncludeTurns: ")
		if sibling == 0 {
			t.Fatalf("%s builds no run memory policy — this guard would assert nothing", f)
		}
		if mine := strings.Count(src, "tools.WithDecisionPolicy("); mine != sibling {
			t.Errorf("%s builds a run's memory policy %d times but sets its decision policy %d times", f, sibling, mine)
		}
	}
}
