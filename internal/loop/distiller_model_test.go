package loop

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// modelRecordingProvider notes which model each call named, and whether the
// call was the agent's own turn or a distillation.
type modelRecordingProvider struct {
	mu     sync.Mutex
	turns  []string
	recaps []string
}

func (p *modelRecordingProvider) ID() string                                   { return "rec" }
func (p *modelRecordingProvider) Probe(context.Context) error                  { return nil }
func (p *modelRecordingProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *modelRecordingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, MaxContextTokens: 40_000}
}
func (p *modelRecordingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	isSummary := false
	for _, b := range req.System {
		if strings.Contains(b.Text, "RECAP") || strings.Contains(b.Text, "summar") {
			isSummary = true
		}
	}
	p.mu.Lock()
	if isSummary {
		p.recaps = append(p.recaps, req.Model)
	} else {
		p.turns = append(p.turns, req.Model)
	}
	p.mu.Unlock()

	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "a genuine recap of the span"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

// summarizableChat is a conversation the DEFAULT keep_last_n of 6 can still
// split: the standard six-message fixture is pinned whole, so a recap on it
// declines before the summarizer is ever called and every assertion below would
// be vacuous for the wrong reason.
//
// (#1322 adds an identical longChat; collapse the two on whichever rebases.)
func summarizableChat() []providers.Message {
	out := []providers.Message{userMsg("the task")}
	for i := 0; i < 7; i++ {
		out = append(out, asstMsg(bulky("a")), userMsg(bulky("q")))
	}
	return out
}

// context.model runs the RECAP on a different model from the same provider —
// the knob compaction has had since it shipped and recap never got.
func TestRecap_UsesTheConfiguredSummarizerModel(t *testing.T) {
	summarizer := "qwen3.6-instruct"
	prov := &modelRecordingProvider{}
	if _, did := maybeRecap(context.Background(),
		RunOptions{Provider: prov, Model: "qwen3.6",
			Context: &config.Context{Mode: cptr(config.ContextModeRecap), Model: &summarizer}},
		summarizableChat(), 36_000, 40_000, func(providers.Event) {}, "auto"); !did {
		t.Fatal("recap declined — the fixture is not exercising the summarize call")
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.recaps) != 1 {
		t.Fatalf("expected exactly one summarize call, got %d", len(prov.recaps))
	}
	if prov.recaps[0] != summarizer {
		t.Errorf("recap ran on %q, want the configured %q", prov.recaps[0], summarizer)
	}
}

// Unset must leave the run's own model, or an agent that never configured this
// silently changes provider-side behaviour.
func TestRecap_WithoutAModelUsesTheRunsOwn(t *testing.T) {
	prov := &modelRecordingProvider{}
	if _, did := maybeRecap(context.Background(),
		RunOptions{Provider: prov, Model: "qwen3.6",
			Context: &config.Context{Mode: cptr(config.ContextModeRecap)}},
		summarizableChat(), 36_000, 40_000, func(providers.Event) {}, "auto"); !did {
		t.Fatal("recap declined")
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.recaps) != 1 || prov.recaps[0] != "qwen3.6" {
		t.Errorf("recap ran on %v, want the run's own model", prov.recaps)
	}
}

// ⚠️ A CONTENT-IDENTIFYING FIELD. sampling, compaction and the rest of context
// all change an agent's content hash, because two defs that run differently are
// not the same def. A field omitted from the signature would let a fork change
// how the agent behaves while claiming to be byte-identical.
func TestContextModel_ChangesTheAgentSignature(t *testing.T) {
	m := "cheap-summarizer"
	if (&config.Context{Model: &m}).IsZero() {
		t.Error("a context carrying only model reports IsZero — it would be dropped before it " +
			"ever reached the signature, the merge, or the wire")
	}
	base := &config.Context{Mode: cptr(config.ContextModeRecap)}
	merged := config.MergeContext(base, &config.Context{Model: &m})
	if merged.Model == nil || *merged.Model != m {
		t.Errorf("MergeContext dropped model: %+v", merged)
	}
	if merged.Mode == nil || *merged.Mode != config.ContextModeRecap {
		t.Errorf("MergeContext lost the base's mode while merging model: %+v", merged)
	}
	clone := base.Clone()
	clone2 := (&config.Context{Model: &m}).Clone()
	if clone2.Model == nil || *clone2.Model != m {
		t.Errorf("Clone dropped model: %+v", clone2)
	}
	if clone.Model != nil {
		t.Errorf("Clone invented a model: %+v", clone)
	}
}
