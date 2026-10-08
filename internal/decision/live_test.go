// Live check of the Ollama decision driver against a real server.
//
// Skipped unless OLLAMA_TEST_BASE_URL is set (the variable the Ollama chat
// driver's live test uses), so `go test ./...` never calls a host. It makes
// three calls: one request with a question of each type, one for a model the
// host does not serve, and one prompt larger than the model's context (refused
// before any inference).
//
//	OLLAMA_TEST_BASE_URL=http://ollama.internal:11434 \
//	go test -run TestLive_Decision -v ./internal/decision/
//
// OLLAMA_TEST_DECISION_MODEL defaults to "nimble" (`ollama pull nimble`).

package decision

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLive_Decision(t *testing.T) {
	baseURL := os.Getenv("OLLAMA_TEST_BASE_URL")
	if baseURL == "" {
		t.Skip("OLLAMA_TEST_BASE_URL not set; skipping live test")
	}
	model := os.Getenv("OLLAMA_TEST_DECISION_MODEL")
	if model == "" {
		model = "nimble"
	}
	d, err := New("ollama", Options{ProviderID: "ollama-local", BaseURL: baseURL, Timeout: 90 * time.Second, MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	state := map[string]any{"ticket": map[string]any{
		"subject": "Charged twice for the March invoice",
		"body":    "I was billed two times on March 3rd. Order 1182. Please refund one charge today, my rent is due.",
	}}

	t.Run("one question of each type", func(t *testing.T) {
		resp, err := d.Decide(context.Background(), Request{Model: model, State: state, Questions: map[string]Question{
			"route": {Type: TypeChoice, Instructions: "Which team should handle this ticket?",
				Criteria: json.RawMessage(`{"billing":"charges, invoices and refunds","support":"the product does not work","sales":null}`)},
			"urgent": {Type: TypeNoul, Instructions: "Does the customer need an answer today?"},
			"detail": {Type: TypeScore, Instructions: "How much detail does the ticket give to act on?",
				Criteria: json.RawMessage(`["no detail","some detail","everything needed"]`)},
		}})
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		t.Logf("served by %s, usage in=%d out=%d", resp.Model, resp.Usage.InputTokens, resp.Usage.OutputTokens)
		for _, name := range []string{"route", "urgent", "detail"} {
			a, ok := resp.Answers[name]
			if !ok {
				t.Errorf("no answer for %q", name)
				continue
			}
			t.Logf("%s: %s", name, a.Raw)
		}
		if a := resp.Answers["route"]; a.Choice == nil || len(a.Probabilities) != 3 || a.Confidence == nil {
			t.Errorf("route = %+v, want a choice with a probability per option and a confidence", a)
		} else if _, own := a.Probabilities[*a.Choice]; !own {
			t.Errorf("route chose %q, which is not one of the caller's own option keys %v", *a.Choice, a.Probabilities)
		}
		if a := resp.Answers["urgent"]; a.Noul == nil {
			t.Errorf("urgent = %+v, want a noul probability", a)
		}
		if a := resp.Answers["detail"]; a.Score == nil || len(a.Legend) != 3 || len(a.Probabilities) != 3 {
			t.Errorf("detail = %+v, want a score with a 3-level legend and probabilities", a)
		}
		if resp.Usage.InputTokens == 0 {
			t.Errorf("usage = %+v, want input tokens reported", resp.Usage)
		}
	})

	t.Run("a model the host does not serve", func(t *testing.T) {
		_, err := d.Decide(context.Background(), Request{Model: "loomcycle-live-check-no-such-model", State: state,
			Questions: map[string]Question{"urgent": {Type: TypeNoul, Instructions: "Urgent?"}}})
		t.Logf("err: %v", err)
		if CodeOf(err) != CodeModelNotFound {
			t.Errorf("code = %q, want model_not_found", CodeOf(err))
		}
	})

	t.Run("a prompt larger than the model's context", func(t *testing.T) {
		// About 12,000 tokens in 54 KB: over a small decision model's context, and
		// under the endpoint's separate 64 KiB cap on the request text, which is
		// refused first and differently (HTTP 413).
		big := map[string]any{"text": strings.Repeat("The quick brown fox jumps over the lazy dog. ", 1200)}
		_, err := d.Decide(context.Background(), Request{Model: model, State: big,
			Questions: map[string]Question{"urgent": {Type: TypeNoul, Instructions: "Urgent?"}}})
		t.Logf("err: %v", err)
		var e *Error
		if !errors.As(err, &e) || e.Code != CodePromptTooLarge || e.Tokens == 0 || e.Limit == 0 || e.Tokens <= e.Limit {
			t.Errorf("err = %+v, want prompt_too_large carrying the prompt's tokens and the model's smaller limit", e)
		}
	})
}
