package loop

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// CompactionMessages is the single source of truth for the compacted history
// shape (the live loop AND replayTranscript both build it). It must start on a
// user turn and end on an assistant turn so the operator's next input alternates
// cleanly (Anthropic et al. require start-with-user + role alternation).
func TestCompactionMessages_Shape(t *testing.T) {
	tail := []providers.Message{
		{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "recent question"}}},
		{Role: "assistant", Content: []providers.ContentBlock{{Type: "text", Text: "recent answer"}}},
	}
	msgs := CompactionMessages("THE TASK", "THE SUMMARY", tail)
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4 (summary pair + 2 kept tail)", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("first role = %q, want user (providers require start-with-user)", msgs[0].Role)
	}
	if !strings.Contains(msgs[0].Content[0].Text, "THE SUMMARY") || !strings.Contains(msgs[0].Content[0].Text, "THE TASK") {
		t.Errorf("pinned task + summary not both embedded in the user turn: %q", msgs[0].Content[0].Text)
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msg[1] role = %q, want assistant", msgs[1].Role)
	}
	if msgs[2].Role != "user" || msgs[2].Content[0].Text != "recent question" {
		t.Errorf("kept tail not appended verbatim: %+v", msgs[2])
	}
}

// A steer.KindCompact control delivered to a PARKED interactive run replaces the
// in-memory conversation with the summary pair and RE-parks (no provider call),
// and the next real operator turn's request carries ONLY the compacted history.
// Fail-before: pre-feature, a compact-kind message was appended as a user turn
// (history kept growing) and triggered a provider call.
func TestRun_Interactive_CompactReplacesHistory(t *testing.T) {
	q := make(chan steer.Message, 4)
	parked := make(chan struct{}, 8)
	compacted := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := &steerProvider{} // inject nil → always end_turn; records every request
	done := make(chan struct{})
	go func() {
		_, _ = Run(ctx, RunOptions{
			Provider:    prov,
			Model:       "x",
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
				}
			},
		})
		close(done)
	}()

	waitOn := func(ch <-chan struct{}, what string) {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: timed out", what)
		}
	}
	calls := func() int { prov.mu.Lock(); defer prov.mu.Unlock(); return len(prov.requests) }

	waitOn(parked, "initial end_turn park")
	if got := calls(); got != 1 {
		t.Fatalf("calls=%d before compact, want 1", got)
	}

	// Compact while parked → emits context_compaction + re-parks, NO provider call.
	q <- steer.Message{Kind: steer.KindCompact, Text: "SUMMARY-X"}
	waitOn(compacted, "compaction applied")
	if got := calls(); got != 1 {
		t.Errorf("compact triggered a provider call (calls=%d, want 1 — must just re-park)", got)
	}

	// A real operator turn now → exactly one provider call on the compacted history.
	q <- steer.Message{Text: "continue"}
	waitOn(parked, "re-park after the real turn")
	if got := calls(); got != 2 {
		t.Fatalf("calls=%d after the real turn, want 2", got)
	}
	prov.mu.Lock()
	last := append([]providers.Message(nil), prov.requests[len(prov.requests)-1]...)
	prov.mu.Unlock()

	txt := func(m providers.Message) string {
		var b strings.Builder
		for _, c := range m.Content {
			b.WriteString(c.Text)
		}
		return b.String()
	}
	if len(last) != 3 {
		t.Fatalf("compacted request has %d messages, want 3 (summary pair + the new turn): %+v", len(last), last)
	}
	if last[0].Role != "user" || !strings.Contains(txt(last[0]), "SUMMARY-X") {
		t.Errorf("msg[0] = %q (%s); want the summary user turn", txt(last[0]), last[0].Role)
	}
	if last[1].Role != "assistant" {
		t.Errorf("msg[1] role = %q, want assistant", last[1].Role)
	}
	if last[2].Role != "user" || txt(last[2]) != "continue" {
		t.Errorf("msg[2] = %q (%s); want user 'continue'", txt(last[2]), last[2].Role)
	}
	for _, m := range last {
		if txt(m) == "go" {
			t.Errorf("the original pre-compaction turn survived: %+v", m)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not terminate after cancel")
	}
}
