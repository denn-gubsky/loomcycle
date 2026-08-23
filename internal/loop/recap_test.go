package loop

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// recapProbeProvider records the last Request it was called with — including the
// token cap, which the package's other capturingProvider does not keep — and
// replies with scripted text, so a test can assert on what the summarize path
// actually sends AND on what it returns.
type recapProbeProvider struct {
	req   providers.Request
	calls int
	reply string
}

func (p *recapProbeProvider) ID() string                                   { return "capturing" }
func (p *recapProbeProvider) Probe(context.Context) error                  { return nil }
func (p *recapProbeProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *recapProbeProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *recapProbeProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.req = req
	p.calls++
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: p.reply}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

func recapConvo() []providers.Message {
	return []providers.Message{userMsg("port the board to SSE"), asstMsg("done, here is the diff")}
}

// TestRecap_AsksForATwoSentenceSummary: the recap prompt must ask for the short
// chat-list gist — NOT compactionPrompt's "roughly N% of the original length",
// which is what made stored recaps run to paragraphs.
func TestRecap_AsksForATwoSentenceSummary(t *testing.T) {
	p := &recapProbeProvider{reply: "Ported the board to SSE. The diff is under review."}
	if _, err := Recap(context.Background(), p, "m", recapConvo()); err != nil {
		t.Fatalf("Recap: %v", err)
	}
	if len(p.req.System) != 1 {
		t.Fatalf("want exactly one system block, got %d", len(p.req.System))
	}
	sys := p.req.System[0].Text
	if !strings.Contains(sys, "two sentences") {
		t.Errorf("recap prompt should ask for at most two sentences, got %q", sys)
	}
	if !strings.Contains(sys, strconv.Itoa(RecapMaxChars)) {
		t.Errorf("recap prompt should name the %d-char budget, got %q", RecapMaxChars, sys)
	}
	if strings.Contains(sys, "% of the original length") {
		t.Errorf("recap must not reuse the compaction prompt's length target: %q", sys)
	}
	if strings.Contains(sys, "context window") {
		t.Errorf("recap is not a compaction; prompt should not talk about the context window: %q", sys)
	}
}

// TestRecap_CapsMaxTokens: the prompt is an instruction a model may ignore, so
// the request also carries a hard token cap — with headroom over RecapMaxChars
// (~4 chars/token) so a compliant reply is never cut off mid-sentence.
func TestRecap_CapsMaxTokens(t *testing.T) {
	p := &recapProbeProvider{reply: "Short."}
	if _, err := Recap(context.Background(), p, "m", recapConvo()); err != nil {
		t.Fatalf("Recap: %v", err)
	}
	if p.req.MaxTokens != recapMaxTokens {
		t.Errorf("MaxTokens = %d, want %d", p.req.MaxTokens, recapMaxTokens)
	}
	if p.req.MaxTokens*4 <= RecapMaxChars {
		t.Errorf("token cap %d leaves no headroom over the %d-char budget", p.req.MaxTokens, RecapMaxChars)
	}
}

// TestRecap_SendsTheWholeConversationAndNoTools: same flattening contract as
// Summarize — every turn reaches the model in one user message, and no tools are
// offered, so the summarize call can never re-enter the tool machinery.
func TestRecap_SendsTheWholeConversationAndNoTools(t *testing.T) {
	p := &recapProbeProvider{reply: "ok"}
	if _, err := Recap(context.Background(), p, "m", recapConvo()); err != nil {
		t.Fatalf("Recap: %v", err)
	}
	if len(p.req.Messages) != 1 || p.req.Messages[0].Role != "user" {
		t.Fatalf("want one user message, got %+v", p.req.Messages)
	}
	body := p.req.Messages[0].Content[0].Text
	for _, want := range []string{"port the board to SSE", "done, here is the diff"} {
		if !strings.Contains(body, want) {
			t.Errorf("flattened conversation is missing %q: %q", want, body)
		}
	}
	if len(p.req.Tools) != 0 {
		t.Errorf("the recap call must offer no tools, got %d", len(p.req.Tools))
	}
}

// TestRecap_ReturnsTheModelText: the recap is stored verbatim — no server-side
// clipping — so whatever the model writes is what the chat list shows.
func TestRecap_ReturnsTheModelText(t *testing.T) {
	p := &recapProbeProvider{reply: "Ported the board to SSE; the diff is under review."}
	got, err := Recap(context.Background(), p, "m", recapConvo())
	if err != nil {
		t.Fatalf("Recap: %v", err)
	}
	if got != p.reply {
		t.Errorf("got %q, want the model text %q", got, p.reply)
	}
}

// TestSummarize_StillUsesTheCompactionPrompt: extracting summarizeWith must not
// have changed the compaction path — it keeps the percentage-target prompt and
// leaves MaxTokens at the provider default (a compaction summary is long).
func TestSummarize_StillUsesTheCompactionPrompt(t *testing.T) {
	p := &recapProbeProvider{reply: "SUMMARY"}
	if _, err := Summarize(context.Background(), p, "m", recapConvo(), 25); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	sys := p.req.System[0].Text
	if !strings.Contains(sys, fmt.Sprintf("roughly %d%%", 25)) {
		t.Errorf("compaction prompt should carry the percentage target, got %q", sys)
	}
	if !strings.Contains(sys, "context window") {
		t.Errorf("compaction prompt should still frame this as freeing context, got %q", sys)
	}
	if p.req.MaxTokens != 0 {
		t.Errorf("MaxTokens = %d, want 0 (provider default) for a compaction summary", p.req.MaxTokens)
	}
	if !strings.Contains(p.req.Messages[0].Content[0].Text, "Conversation to compact:") {
		t.Errorf("compaction lead-in changed: %q", p.req.Messages[0].Content[0].Text)
	}
}
