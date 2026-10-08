package decision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// stub is an Ollama /v1/systemone double: it records every request body and
// Authorization header and answers with reply.
type stub struct {
	mu     sync.Mutex
	bodies []map[string]any
	auth   []string
	reply  func(w http.ResponseWriter)
}

func (s *stub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func serve(t *testing.T, reply func(w http.ResponseWriter)) (*stub, string) {
	t.Helper()
	s := &stub{reply: reply}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost {
			t.Errorf("request %s %s, want POST /v1/systemone", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.mu.Unlock()
		s.reply(w)
	}))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func replyJSON(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func newDriver(t *testing.T, o Options) Driver {
	t.Helper()
	if o.ProviderID == "" {
		o.ProviderID = "ollama-local"
	}
	d, err := New("ollama", o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// threeQuestions is one question of each type, shaped as the endpoint takes
// them: a choice keyed by the caller's own words (one with no description), a
// noul with no criteria, and a score's levels lowest first.
func threeQuestions() map[string]Question {
	return map[string]Question{
		"route": {Type: TypeChoice, Instructions: "Which team owns this?",
			Criteria: json.RawMessage(`{"billing":"invoices and refunds","support":null,"sales":"new business"}`)},
		"urgent": {Type: TypeNoul, Instructions: "Is this urgent?"},
		"quality": {Type: TypeScore, Instructions: "How reproducible is the report?",
			Criteria: json.RawMessage(`["no detail","partial detail","fully reproducible"]`)},
	}
}

// The captured reply, with the noul at exactly 0: a certain "no" is an answer.
const threeAnswers = `{"model":"nimble","answers":{
  "route":{"type":"choice","choice":"billing","probabilities":{"billing":0.98,"support":0.007,"sales":0.005},"confidence":0.93},
  "urgent":{"type":"noul","noul":0},
  "quality":{"type":"score","score":1.14,"legend":{"0":"no detail","1":"partial detail","2":"fully reproducible"},"probabilities":{"0":0.29,"1":0.27,"2":0.43},"confidence":0.02,"later_field":7}},
 "usage":{"input_tokens":909,"output_tokens":4}}`

// TestOllama_EachQuestionTypeRoundTrips — the request reaches the endpoint in
// its wire shape, and every field of every answer comes back unchanged.
func TestOllama_EachQuestionTypeRoundTrips(t *testing.T) {
	s, url := serve(t, replyJSON(http.StatusOK, threeAnswers))
	d := newDriver(t, Options{BaseURL: url + "/"})
	resp, err := d.Decide(context.Background(), Request{
		Model:     "nimble",
		State:     map[string]any{"ticket": map[string]any{"subject": "refund", "tags": []any{"a", "b"}}, "n": 3},
		Questions: threeQuestions(),
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	body := s.bodies[0]
	if body["model"] != "nimble" {
		t.Errorf("model = %v, want nimble", body["model"])
	}
	wantState := map[string]any{"ticket": map[string]any{"subject": "refund", "tags": []any{"a", "b"}}, "n": float64(3)}
	if !reflect.DeepEqual(body["state"], wantState) {
		t.Errorf("state = %v, want %v (nested values intact)", body["state"], wantState)
	}
	qs := body["questions"].(map[string]any)
	route := qs["route"].(map[string]any)
	wantCriteria := map[string]any{"billing": "invoices and refunds", "support": nil, "sales": "new business"}
	if route["type"] != "choice" || route["instructions"] != "Which team owns this?" || !reflect.DeepEqual(route["criteria"], wantCriteria) {
		t.Errorf("route = %v, want the choice with the caller's own option keys", route)
	}
	urgent := qs["urgent"].(map[string]any)
	if _, sent := urgent["criteria"]; sent || urgent["type"] != "noul" {
		t.Errorf("urgent = %v, want a noul with no criteria key", urgent)
	}
	quality := qs["quality"].(map[string]any)
	if !reflect.DeepEqual(quality["criteria"], []any{"no detail", "partial detail", "fully reproducible"}) {
		t.Errorf("quality criteria = %v, want the levels in order", quality["criteria"])
	}

	if resp.Model != "nimble" || resp.Provider != "ollama-local" {
		t.Errorf("served by %s/%s, want ollama-local/nimble", resp.Provider, resp.Model)
	}
	r := resp.Answers["route"]
	if r.Type != "choice" || r.Choice == nil || *r.Choice != "billing" || r.Confidence == nil || *r.Confidence != 0.93 ||
		!reflect.DeepEqual(r.Probabilities, map[string]float64{"billing": 0.98, "support": 0.007, "sales": 0.005}) {
		t.Errorf("route answer = %+v, want choice billing with its probabilities and confidence", r)
	}
	u := resp.Answers["urgent"]
	if u.Type != "noul" || u.Noul == nil || *u.Noul != 0 {
		t.Errorf("urgent answer = %+v, want a noul of exactly 0 (not a missing field)", u)
	}
	q := resp.Answers["quality"]
	if q.Type != "score" || q.Score == nil || *q.Score != 1.14 || q.Confidence == nil || *q.Confidence != 0.02 ||
		!reflect.DeepEqual(q.Legend, map[string]string{"0": "no detail", "1": "partial detail", "2": "fully reproducible"}) ||
		!reflect.DeepEqual(q.Probabilities, map[string]float64{"0": 0.29, "1": 0.27, "2": 0.43}) {
		t.Errorf("quality answer = %+v, want the score with its legend, probabilities and confidence", q)
	}
	if !strings.Contains(string(q.Raw), `"later_field":7`) {
		t.Errorf("quality raw = %s, want the answer as received, unknown fields included", q.Raw)
	}
	want := providers.Usage{InputTokens: 909, OutputTokens: 4, Model: "nimble", Provider: "ollama-local", CredentialSource: "operator"}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
}

// TestOllama_ANoulMayDescribeItsSides — the optional noul criteria is sent as given.
func TestOllama_ANoulMayDescribeItsSides(t *testing.T) {
	s, url := serve(t, replyJSON(http.StatusOK, `{"answers":{"u":{"type":"noul","noul":0.46}}}`))
	_, err := newDriver(t, Options{BaseURL: url}).Decide(context.Background(), Request{Model: "nimble",
		Questions: map[string]Question{"u": {Type: TypeNoul, Instructions: "Urgent?",
			Criteria: json.RawMessage(`{"true":"needs a reply today","false":"can wait"}`)}}})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	got := s.bodies[0]["questions"].(map[string]any)["u"].(map[string]any)["criteria"]
	if !reflect.DeepEqual(got, map[string]any{"true": "needs a reply today", "false": "can wait"}) {
		t.Errorf("noul criteria = %v, want both sides as given", got)
	}
	if st, ok := s.bodies[0]["state"].(map[string]any); !ok || len(st) != 0 {
		t.Errorf("state = %v, want an empty object when the caller gave none", s.bodies[0]["state"])
	}
}

// TestOllama_RefusalsMapToTheirCodes — each refusal the endpoint was observed
// to give maps to its code, with the numbers it states.
func TestOllama_RefusalsMapToTheirCodes(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   Error
	}{
		{"too many questions", 400, `{"error":"questions must contain 1–64 fields"}`,
			Error{Code: CodeTooManyQuestions, Limit: 64}},
		{"option count", 400, `{"error":"question \"u\": criteria must contain 2–26 candidates"}`,
			Error{Code: CodeBadOptions, Question: "u", Min: 2, Max: 26}},
		{"unknown type", 400, `{"error":"question \"u\": type must be choice, noul, or score"}`,
			Error{Code: CodeBadQuestion, Question: "u"}},
		{"no instructions", 400, `{"error":"question \"r\": instructions must be a nonempty string, object, or array"}`,
			Error{Code: CodeBadQuestion, Question: "r"}},
		{"score criteria shape", 400, `{"error":"question \"quality\": score criteria must be an array of descriptions"}`,
			Error{Code: CodeBadQuestion, Question: "quality"}},
		{"choice criteria shape", 400, `{"error":"question \"route\": choice criteria must map option keys to descriptions or null"}`,
			Error{Code: CodeBadQuestion, Question: "route"}},
		{"noul criteria shape", 400, `{"error":"question \"u\": noul criteria must be an object of true/false descriptions"}`,
			Error{Code: CodeBadQuestion, Question: "u"}},
		{"prompt too large", 400, `{"error":"prompt 0 has 12144 tokens; expected 1–8194 (input is never truncated)"}`,
			Error{Code: CodePromptTooLarge, Tokens: 12144, Limit: 8194}},
		{"prompt too large, reworded", 400, `{"error":"too many tokens"}`,
			Error{Code: CodePromptTooLarge}},
		// The name is the caller's word: it must not be read as part of the fault.
		{"a question named tokens", 400, `{"error":"question \"tokens\": type must be choice, noul, or score"}`,
			Error{Code: CodeBadQuestion, Question: "tokens"}},
		{"model not found", 404, `{"error":"model \"nope-xyz\" not found, try pulling it first"}`,
			Error{Code: CodeModelNotFound}},
		// Seen on Ollama 0.40.0: a cap on the request's bytes, checked before the
		// model's token limit. It is an oversized input too, but it is not mapped to
		// prompt_too_large yet: the memory reranker retries shorter on that code, and
		// it never retried on this refusal. Pinned as it stands so the gap is visible.
		{"request over 64 KiB", 413, `{"error":"text and schema must not exceed 64 KiB"}`,
			Error{Code: CodeCallFailed}},
		{"no such endpoint", 404, `404 page not found`, Error{Code: CodeCallFailed}},
		{"another 400", 400, `{"error":"something else"}`, Error{Code: CodeCallFailed}},
		{"server error", 500, `boom`, Error{Code: CodeCallFailed}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, url := serve(t, replyJSON(c.status, c.body))
			_, err := newDriver(t, Options{BaseURL: url}).Decide(context.Background(), Request{Model: "nimble", Questions: threeQuestions()})
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want a *decision.Error", err)
			}
			got := Error{Code: e.Code, Question: e.Question, Limit: e.Limit, Min: e.Min, Max: e.Max, Tokens: e.Tokens}
			if got != c.want {
				t.Errorf("error = %+v, want %+v (message %q)", got, c.want, e.Message)
			}
			if CodeOf(err) != c.want.Code {
				t.Errorf("CodeOf = %q, want %q", CodeOf(err), c.want.Code)
			}
		})
	}
}

// TestOllama_AnUnreadableReplyIsACallFailure — a 200 that is not the reply.
func TestOllama_AnUnreadableReplyIsACallFailure(t *testing.T) {
	_, url := serve(t, replyJSON(http.StatusOK, "not json"))
	_, err := newDriver(t, Options{BaseURL: url}).Decide(context.Background(), Request{Model: "nimble", Questions: threeQuestions()})
	if CodeOf(err) != CodeCallFailed {
		t.Errorf("err = %v, want call_failed", err)
	}
}

func choiceOf(n int) json.RawMessage {
	opts := map[string]string{}
	for i := 0; i < n; i++ {
		opts[fmt.Sprintf("option-%02d", i)] = "d"
	}
	raw, _ := json.Marshal(opts)
	return raw
}

func scoreOf(n int) json.RawMessage {
	levels := make([]string, n)
	for i := range levels {
		levels[i] = fmt.Sprintf("level %d", i)
	}
	raw, _ := json.Marshal(levels)
	return raw
}

func questionsOf(n int) map[string]Question {
	qs := map[string]Question{}
	for i := 0; i < n; i++ {
		qs[fmt.Sprintf("q%02d", i)] = Question{Type: TypeNoul, Instructions: "?"}
	}
	return qs
}

// TestOllama_RefusesAnInvalidRequestBeforeCalling — a request the endpoint would
// refuse is refused here, with its code and the limit it broke, and no call is
// spent. Nothing is trimmed to fit.
func TestOllama_RefusesAnInvalidRequestBeforeCalling(t *testing.T) {
	one := func(q Question) map[string]Question { return map[string]Question{"q": q} }
	for _, c := range []struct {
		name string
		qs   map[string]Question
		want Error
	}{
		{"no questions", nil, Error{Code: CodeBadQuestion}},
		{"65 questions", questionsOf(65), Error{Code: CodeTooManyQuestions, Limit: 64}},
		{"choice with 1 option", one(Question{Type: TypeChoice, Instructions: "?", Criteria: choiceOf(1)}),
			Error{Code: CodeBadOptions, Question: "q", Min: 2, Max: 26}},
		{"choice with 27 options", one(Question{Type: TypeChoice, Instructions: "?", Criteria: choiceOf(27)}),
			Error{Code: CodeBadOptions, Question: "q", Min: 2, Max: 26}},
		{"score with 1 level", one(Question{Type: TypeScore, Instructions: "?", Criteria: scoreOf(1)}),
			Error{Code: CodeBadOptions, Question: "q", Min: 2, Max: 26}},
		{"score with 27 levels", one(Question{Type: TypeScore, Instructions: "?", Criteria: scoreOf(27)}),
			Error{Code: CodeBadOptions, Question: "q", Min: 2, Max: 26}},
		{"unknown type", one(Question{Type: "rank", Instructions: "?"}), Error{Code: CodeBadQuestion, Question: "q"}},
		{"no instructions", one(Question{Type: TypeNoul, Instructions: "  "}), Error{Code: CodeBadQuestion, Question: "q"}},
		{"choice without criteria", one(Question{Type: TypeChoice, Instructions: "?"}), Error{Code: CodeBadQuestion, Question: "q"}},
		{"choice criteria an array", one(Question{Type: TypeChoice, Instructions: "?", Criteria: scoreOf(3)}), Error{Code: CodeBadQuestion, Question: "q"}},
		{"choice description a number", one(Question{Type: TypeChoice, Instructions: "?", Criteria: json.RawMessage(`{"a":1,"b":"x"}`)}), Error{Code: CodeBadQuestion, Question: "q"}},
		{"score criteria an object", one(Question{Type: TypeScore, Instructions: "?", Criteria: choiceOf(3)}), Error{Code: CodeBadQuestion, Question: "q"}},
		{"score level a number", one(Question{Type: TypeScore, Instructions: "?", Criteria: json.RawMessage(`["low",2]`)}), Error{Code: CodeBadQuestion, Question: "q"}},
		{"noul criteria an array", one(Question{Type: TypeNoul, Instructions: "?", Criteria: scoreOf(2)}), Error{Code: CodeBadQuestion, Question: "q"}},
		{"noul criteria another key", one(Question{Type: TypeNoul, Instructions: "?", Criteria: json.RawMessage(`{"yes":"x"}`)}), Error{Code: CodeBadQuestion, Question: "q"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, url := serve(t, replyJSON(http.StatusOK, `{"answers":{}}`))
			_, err := newDriver(t, Options{BaseURL: url}).Decide(context.Background(), Request{Model: "nimble", Questions: c.qs})
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want a *decision.Error", err)
			}
			got := Error{Code: e.Code, Question: e.Question, Limit: e.Limit, Min: e.Min, Max: e.Max}
			if got != c.want {
				t.Errorf("error = %+v, want %+v (%v)", got, c.want, err)
			}
			if s.calls() != 0 {
				t.Errorf("the endpoint was called %d times for a request refused up front", s.calls())
			}
		})
	}
}

// TestOllama_AcceptsARequestAtItsLimits — the bounds are inclusive: 64 questions,
// 2 and 26 options, a null noul criteria. A check one off would refuse work the
// endpoint takes.
func TestOllama_AcceptsARequestAtItsLimits(t *testing.T) {
	s, url := serve(t, replyJSON(http.StatusOK, `{"answers":{}}`))
	d := newDriver(t, Options{BaseURL: url})
	qs := questionsOf(60)
	qs["c2"] = Question{Type: TypeChoice, Instructions: "?", Criteria: choiceOf(2)}
	qs["c26"] = Question{Type: TypeChoice, Instructions: "?", Criteria: choiceOf(26)}
	qs["s2"] = Question{Type: TypeScore, Instructions: "?", Criteria: scoreOf(2)}
	qs["n"] = Question{Type: TypeNoul, Instructions: "?", Criteria: json.RawMessage(`null`)}
	if len(qs) != 64 {
		t.Fatalf("fixture has %d questions, want 64", len(qs))
	}
	if _, err := d.Decide(context.Background(), Request{Model: "nimble", Questions: qs}); err != nil {
		t.Fatalf("Decide at the limits: %v", err)
	}
	sent := s.bodies[0]["questions"].(map[string]any)
	if len(sent) != 64 || len(sent["c26"].(map[string]any)["criteria"].(map[string]any)) != 26 {
		t.Errorf("sent %d questions; want all 64 with c26's 26 options intact", len(sent))
	}
	if _, has := sent["n"].(map[string]any)["criteria"]; has {
		t.Error("a null noul criteria was sent; want it left out")
	}
	if lim := d.Limits("nimble"); lim != (Limits{MaxQuestions: 64, MinOptions: 2, MaxOptions: 26}) {
		t.Errorf("limits = %+v, want 64 questions and 2 to 26 options", lim)
	}
}

// TestOllama_ARestrictedRunCannotSpendTheOperatorsKey — the key rule is the chat
// driver's: a keyed endpoint answers to a tenant's own key first, a run barred
// from the operator's key makes no call at all, and a keyless endpoint has no
// operator key to protect.
func TestOllama_ARestrictedRunCannotSpendTheOperatorsKey(t *testing.T) {
	keyed := Options{ProviderID: "ollama", APIKey: "test-operator-key", KeyEnvName: "OLLAMA_API_KEY"}
	restricted := providers.WithOperatorKeyAllowed(context.Background(), false)
	ownKey := providers.WithCredentialResolver(restricted, func(_ context.Context, name string) (providers.CredentialResolution, bool) {
		if name == "OLLAMA_API_KEY" {
			return providers.CredentialResolution{Value: "test-tenant-key", Scope: "tenant", ScopeID: "acme"}, true
		}
		return providers.CredentialResolution{}, false
	})
	req := Request{Model: "nimble", Questions: questionsOf(1)}
	reply := replyJSON(http.StatusOK, `{"answers":{"q00":{"type":"noul","noul":1}},"usage":{"input_tokens":5,"output_tokens":1}}`)

	t.Run("keyed, allowed: operator key sent", func(t *testing.T) {
		s, url := serve(t, reply)
		o := keyed
		o.BaseURL = url
		resp, err := newDriver(t, o).Decide(context.Background(), req)
		if err != nil || s.auth[0] != "Bearer test-operator-key" || resp.Usage.CredentialSource != "operator" {
			t.Errorf("err %v, auth %q, want the operator's key and an operator-paid usage", err, s.auth)
		}
	})
	t.Run("keyed, restricted: no call", func(t *testing.T) {
		s, url := serve(t, reply)
		o := keyed
		o.BaseURL = url
		_, err := newDriver(t, o).Decide(restricted, req)
		if !errors.Is(err, providers.ErrOperatorKeyForbidden) || CodeOf(err) != CodeCallFailed || s.calls() != 0 {
			t.Errorf("err %v after %d calls, want call_failed wrapping ErrOperatorKeyForbidden and no request", err, s.calls())
		}
	})
	t.Run("keyed, restricted, own key: tenant key sent", func(t *testing.T) {
		s, url := serve(t, reply)
		o := keyed
		o.BaseURL = url
		resp, err := newDriver(t, o).Decide(ownKey, req)
		if err != nil || s.auth[0] != "Bearer test-tenant-key" {
			t.Fatalf("err %v, auth %q, want the tenant's own key", err, s.auth)
		}
		if resp.Usage.CredentialSource != "tenant" || resp.Usage.CredentialScopeID != "acme" {
			t.Errorf("usage = %+v, want it booked to the tenant's key", resp.Usage)
		}
	})
	t.Run("keyless, restricted: allowed", func(t *testing.T) {
		s, url := serve(t, reply)
		_, err := newDriver(t, Options{BaseURL: url}).Decide(restricted, req)
		if err != nil || s.auth[0] != "" {
			t.Errorf("err %v, auth %q, want a call with no key", err, s.auth)
		}
	})
}

// TestOllama_TimesOut — a call that outlives the timeout, and one that spends it
// waiting for a slot, are both a timeout and both context.DeadlineExceeded.
func TestOllama_TimesOut(t *testing.T) {
	release := make(chan struct{})
	s, url := serve(t, func(w http.ResponseWriter) { <-release })
	// Registered after serve's own cleanup, so it runs first: the server's Close
	// waits for the handler this releases.
	t.Cleanup(func() { close(release) })
	d := newDriver(t, Options{BaseURL: url, Timeout: 80 * time.Millisecond, MaxConcurrent: 1})
	req := Request{Model: "nimble", Questions: questionsOf(1)}
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := d.Decide(context.Background(), req)
			errs <- err
		}()
	}
	for i := 0; i < 2; i++ {
		err := <-errs
		if CodeOf(err) != CodeTimeout || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want a timeout wrapping context.DeadlineExceeded", err)
		}
	}
	// One held the only slot for its whole timeout, so the other never called.
	if n := s.calls(); n != 1 {
		t.Errorf("the endpoint saw %d calls, want 1: the second waited for the slot and timed out", n)
	}
}

// TestOllama_BoundsCallsInFlight — max_concurrent is the most calls the driver
// has at the endpoint at once.
func TestOllama_BoundsCallsInFlight(t *testing.T) {
	var inFlight, peak atomic.Int64
	_, url := serve(t, func(w http.ResponseWriter) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		_, _ = io.WriteString(w, `{"answers":{}}`)
	})
	d := newDriver(t, Options{BaseURL: url, MaxConcurrent: 2})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.Decide(context.Background(), Request{Model: "nimble", Questions: questionsOf(1)}); err != nil {
				t.Errorf("Decide: %v", err)
			}
		}()
	}
	wg.Wait()
	if p := peak.Load(); p != 2 {
		t.Errorf("peak calls in flight = %d, want exactly 2 (bounded, and the bound used)", p)
	}
}

// TestNew_KnowsWhichDriversServeDecisionModels — the registry is keyed by the
// provider DRIVER name; a driver with no decision endpoint is an error naming
// the ones that have one.
func TestNew_KnowsWhichDriversServeDecisionModels(t *testing.T) {
	if got := Registered(); len(got) != 1 || got[0] != "ollama" {
		t.Errorf("Registered = %v, want [ollama]", got)
	}
	if _, err := New("openai", Options{BaseURL: "http://x"}); err == nil || !strings.Contains(err.Error(), "ollama") {
		t.Errorf("New(openai) err = %v, want a refusal naming the drivers that serve decision models", err)
	}
	if _, err := New("ollama", Options{ProviderID: "ollama-local"}); err == nil || !strings.Contains(err.Error(), "base URL") {
		t.Errorf("New(ollama) with no base URL: err = %v, want a refusal", err)
	}
}
