package loop

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// thinkingSteerProvider is a steerProvider that, like the DeepSeek driver, has a
// non-thinking sibling for its thinking model, and records the model and effort
// of every call.
type thinkingSteerProvider struct {
	steerProvider
	callMu  sync.Mutex
	models  []string
	efforts []string
}

func (p *thinkingSteerProvider) NonThinkingSibling(model string) (string, bool) {
	if model == "thinker-pro" {
		return "plain-chat", true
	}
	return "", false
}

func (p *thinkingSteerProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.callMu.Lock()
	p.models = append(p.models, req.Model)
	p.efforts = append(p.efforts, req.Effort)
	p.callMu.Unlock()
	return p.steerProvider.Call(ctx, req)
}

// A compaction leaves an assistant turn in the history that no model produced,
// so it carries no reasoning. A thinking model that wants its reasoning passed
// back (DeepSeek's) answers the next call with 400 "reasoning_content in the
// thinking mode must be passed back", and the run fails on the first turn after
// the compaction. The run must continue on the non-thinking sibling instead,
// with the effort hint dropped, and say so.
func TestRun_CompactionOnAThinkingModelContinuesOnTheNonThinkingSibling(t *testing.T) {
	q := make(chan steer.Message, 4)
	parked := make(chan struct{}, 8)
	compacted := make(chan struct{}, 8)
	var evMu sync.Mutex
	var downgrades []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := &thinkingSteerProvider{}
	go func() {
		_, _ = Run(ctx, RunOptions{
			Provider:    prov,
			Model:       "thinker-pro",
			Effort:      "high",
			Tools:       []tools.Tool{noopTool{}},
			Dispatcher:  tools.NewDispatcher([]tools.Tool{noopTool{}}),
			Segments:    steerSegs(),
			SteerQueue:  q,
			Interactive: true,
			OnEvent: func(ev providers.Event) {
				switch ev.Type {
				case providers.EventAwaitingInput:
					parked <- struct{}{}
				case providers.EventContextCompaction:
					compacted <- struct{}{}
				case providers.EventModelDowngraded:
					evMu.Lock()
					downgrades = append(downgrades, ev.Text)
					evMu.Unlock()
				}
			},
		})
	}()
	waitOn := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: timed out", what)
		}
	}

	waitOn(parked, "initial park")
	q <- steer.Message{Kind: steer.KindCompact, Text: "SUMMARY"}
	waitOn(compacted, "compaction applied")
	q <- steer.Message{Text: "continue"}
	waitOn(parked, "park after the turn that follows the compaction")

	prov.callMu.Lock()
	models := append([]string(nil), prov.models...)
	efforts := append([]string(nil), prov.efforts...)
	prov.callMu.Unlock()
	if len(models) != 2 {
		t.Fatalf("provider calls = %d, want 2 (one before the compaction, one after): %v", len(models), models)
	}
	if models[0] != "thinker-pro" || efforts[0] != "high" {
		t.Errorf("call before the compaction used %q effort %q, want the thinking model untouched", models[0], efforts[0])
	}
	if models[1] != "plain-chat" {
		t.Errorf("call after the compaction used %q, want the non-thinking sibling plain-chat", models[1])
	}
	if efforts[1] != "" {
		t.Errorf("call after the compaction kept effort %q; it turns thinking back on and must be dropped", efforts[1])
	}
	evMu.Lock()
	defer evMu.Unlock()
	if len(downgrades) != 1 || !strings.Contains(downgrades[0], "thinker-pro") || !strings.Contains(downgrades[0], "plain-chat") {
		t.Errorf("model_downgraded events = %v, want exactly one naming both models", downgrades)
	}
}

// Only the runtime's own turns count. A thinking model may answer without a
// reasoning trace, and its provider accepts that turn back, so an ordinary
// reasoning-less assistant turn must not cost a run its model.
func TestHasSyntheticAssistantTurn(t *testing.T) {
	text := func(role, s, reasoning string) providers.Message {
		return providers.Message{Role: role, Reasoning: reasoning,
			Content: []providers.ContentBlock{{Type: "text", Text: s}}}
	}
	compactedHist := CompactionMessages("task", "summary", []providers.Message{text("user", "next", "")})
	recapHist := RecapMessages("task", "what I did", []providers.Message{text("user", "next", "")})
	cases := []struct {
		name string
		msgs []providers.Message
		want bool
	}{
		{"a compacted history", compactedHist, true},
		{"a recap history", recapHist, true},
		{"an ordinary answer with no reasoning", []providers.Message{text("user", "hi", ""), text("assistant", "hello", "")}, false},
		{"an answer with reasoning", []providers.Message{text("user", "hi", ""), text("assistant", "hello", "thought")}, false},
		{"the ack text said by a user", []providers.Message{text("user", compactionAckText, "")}, false},
		{"no assistant turn at all", []providers.Message{text("user", "hi", "")}, false},
	}
	for _, tc := range cases {
		if got := hasSyntheticAssistantTurn(tc.msgs); got != tc.want {
			t.Errorf("%s: hasSyntheticAssistantTurn = %v, want %v", tc.name, got, tc.want)
		}
	}
}
