package loop

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// thinkingSummarizer is qwen3.6 on ollama-local, in miniature: it spends its
// whole output budget reasoning and only starts WRITING once the budget leaves
// room to. Below the threshold it emits thinking and no text — which is exactly
// what summarizeWith's text accumulator sees as an empty summary.
type thinkingSummarizer struct {
	mu        sync.Mutex
	maxTokens []int
	efforts   []string
	// needTokens is the budget below which this model never gets to the answer.
	needTokens int
	// failOnEffort makes the model reject the effort hint, the way a
	// non-reasoning OpenAI model rejects reasoning_effort.
	failOnEffort bool
}

func (p *thinkingSummarizer) ID() string                                   { return "thinker" }
func (p *thinkingSummarizer) Probe(context.Context) error                  { return nil }
func (p *thinkingSummarizer) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *thinkingSummarizer) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, SupportsThinking: true, MaxContextTokens: 40_000}
}
func (p *thinkingSummarizer) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	isSummary := false
	for _, b := range req.System {
		if strings.Contains(b.Text, "RECAP") || strings.Contains(b.Text, "summar") {
			isSummary = true
		}
	}
	if isSummary {
		p.mu.Lock()
		p.maxTokens = append(p.maxTokens, req.MaxTokens)
		p.efforts = append(p.efforts, req.Effort)
		p.mu.Unlock()
		if p.failOnEffort && req.Effort != "" {
			return nil, errors.New("400: this model does not support reasoning_effort")
		}
	}

	ch := make(chan providers.Event, 3)
	switch {
	case !isSummary:
		ch <- providers.Event{Type: providers.EventText, Text: "ok"}
	case req.MaxTokens >= p.needTokens:
		ch <- providers.Event{Type: providers.EventThinking, Text: "considering the span"}
		ch <- providers.Event{Type: providers.EventText, Text: "a genuine recap of the span"}
	default:
		// The reported failure: the budget went entirely to reasoning.
		ch <- providers.Event{Type: providers.EventThinking, Text: "considering the span"}
	}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn",
		Usage: &providers.Usage{InputTokens: 36_000, MaxContextTokens: 40_000}}
	close(ch)
	return ch, nil
}
func (p *thinkingSummarizer) seen() ([]int, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.maxTokens...), append([]string(nil), p.efforts...)
}

// longChat is a conversation the DEFAULT keep_last_n of 6 can still split: the
// standard six-message fixture is pinned whole, so a recap on it declines
// before the summarizer is ever called — which would make every assertion here
// vacuous for the wrong reason.
func longChat() []providers.Message {
	out := []providers.Message{userMsg("the task")}
	for i := 0; i < 7; i++ {
		out = append(out, asstMsg(bulky("a")), userMsg(bulky("q")))
	}
	return out
}

// recapOnce drives ONE recap with the DEFAULT context policy, because the
// default is what produced the reported failure — an agent that had set
// recap_max_chars would not have hit it.
func recapOnce(t *testing.T, prov providers.Provider) (bool, []providers.Event) {
	t.Helper()
	var evs []providers.Event
	_, did := maybeRecap(context.Background(), RunOptions{Provider: prov, Model: "qwen3.6"},
		longChat(), 36_000, 40_000,
		func(ev providers.Event) { evs = append(evs, ev) }, "auto")
	return did, evs
}

// ⚠️ THE OUTPUT BUDGET AND THE REQUESTED LENGTH WERE THE SAME NUMBER.
//
// recap_max_chars asks the PROMPT for a length; MaxTokens caps what the model
// may EMIT. At the 512-char default the cap was 512/4+64 = 192 tokens, and a
// reasoning model spends that thinking before it writes a word — so the
// summarizer "returned no text" on every attempt, and the advice the run gave
// its operator was to lengthen the summary in order to fix a budget.
func TestRecap_AThinkingModelGetsEnoughBudgetToAnswer(t *testing.T) {
	prov := &thinkingSummarizer{needTokens: 512}
	did, evs := recapOnce(t, prov)

	budgets, _ := prov.seen()
	if len(budgets) == 0 {
		t.Fatal("the summarizer was never called — this fixture proves nothing")
	}
	// Non-vacuity: the DERIVED budget must be below what this model needs, or
	// the floor is not what made the difference.
	derived := config.ContextDefaultRecapMaxChars/4 + 64
	if derived >= prov.needTokens {
		t.Fatalf("fixture is not the failing shape: the derived budget %d already covers "+
			"the model's need %d", derived, prov.needTokens)
	}
	if budgets[0] < distillMinOutputTokens {
		t.Errorf("summarize call got MaxTokens=%d, want at least the %d floor — the cap is "+
			"still being derived from the requested LENGTH", budgets[0], distillMinOutputTokens)
	}
	if !did {
		t.Errorf("recap declined on a model that only needed room to finish: %v", declineReasons(evs))
	}
}

// A recap is mechanical. Asking for reasoning costs the budget the answer needs,
// and on the two providers where an unset hint means "the model's own default"
// (Anthropic, Ollama) a thinking model will reason unless told not to.
func TestRecap_TheSummarizeCallAsksForTheLeastReasoning(t *testing.T) {
	prov := &thinkingSummarizer{needTokens: 512}
	if _, _ = recapOnce(t, prov); true {
		_, efforts := prov.seen()
		if len(efforts) == 0 {
			t.Fatal("the summarizer was never called")
		}
		if efforts[0] != "low" {
			t.Errorf("summarize call sent effort %q, want %q — an unset hint leaves a "+
				"reasoning model thinking by default", efforts[0], "low")
		}
	}
}

// ⚠️ THE HINT IS THE ONE PART OF THE REQUEST THE CALLER NEVER ASKED FOR, so it
// must not be able to break a provider that was working. OpenAI passes Effort
// through as reasoning_effort and a non-reasoning model rejects it; rather than
// guess per model, the call drops the hint and tries once more.
//
// Strictly better than what it replaces: any error here used to be a decline,
// so a second plain attempt can only turn a decline into a summary.
func TestRecap_AnEffortRejectionIsRetriedWithoutTheHint(t *testing.T) {
	prov := &thinkingSummarizer{needTokens: 512, failOnEffort: true}
	did, evs := recapOnce(t, prov)

	_, efforts := prov.seen()
	if len(efforts) < 2 {
		t.Fatalf("expected a second, hint-free attempt; efforts seen: %v", efforts)
	}
	if efforts[1] != "" {
		t.Errorf("the retry still carried effort %q — it must drop the hint", efforts[1])
	}
	if !did {
		t.Errorf("a provider that rejects only the effort hint still declined: %v", declineReasons(evs))
	}
}

func declineReasons(evs []providers.Event) []string {
	var out []string
	for _, e := range evs {
		if e.Type == providers.EventContextDistillDeclined && e.ContextDistill != nil {
			out = append(out, e.ContextDistill.Reason+": "+e.ContextDistill.Message)
		}
	}
	return out
}
