package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestRecapSession_ProducesSummary: the server-injected History op=recap
// summarizer (Server.RecapSession — the off-loop twin of the compaction summary
// step) replays a chat's whole session transcript and returns the provider's
// summary text. Reuses the compaction fixture's scripted provider, which returns
// "COMPACTED SUMMARY" for any summarize call.
func TestRecapSession_ProducesSummary(t *testing.T) {
	srv, _ := compactFixture(t)
	sessID, _ := seedConversation(t, srv, true)

	summary, err := srv.RecapSession(context.Background(), sessID)
	if err != nil {
		t.Fatalf("RecapSession: %v", err)
	}
	if summary != "COMPACTED SUMMARY" {
		t.Errorf("summary = %q, want the scripted provider's summary", summary)
	}
}

// TestRecapSession_EmptyTranscriptErrors: a chat with no transcript yet is a
// clean error, never a panic or an empty-string "success" the tool would persist.
func TestRecapSession_EmptyTranscriptErrors(t *testing.T) {
	srv, _ := compactFixture(t)
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "compactor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RecapSession(ctx, sess.ID); err == nil {
		t.Fatal("recap of a chat with no transcript must return an error")
	}
}

// promptCapturingProvider answers like compactFixture's scripted provider but
// keeps the system prompt, so a test can assert WHICH summarization prompt the
// recap path sent.
type promptCapturingProvider struct {
	sys string
}

func (p *promptCapturingProvider) ID() string                                   { return "scripted" }
func (p *promptCapturingProvider) Probe(context.Context) error                  { return nil }
func (p *promptCapturingProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *promptCapturingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *promptCapturingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	if len(req.System) > 0 {
		p.sys = req.System[0].Text
	}
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "Short recap."}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

// TestRecapSession_UsesTheShortRecapPrompt: op=recap must summarize for a chat
// LIST — two sentences — not reuse compactionPrompt's "N% of the original
// length", which is sized to free a context window and yields the paragraphs
// that made stored recaps unusable as a title subtitle.
func TestRecapSession_UsesTheShortRecapPrompt(t *testing.T) {
	srv, _ := compactFixture(t)
	sessID, _ := seedConversation(t, srv, true)

	// Swap the fixture's scripted provider for one that keeps the system prompt.
	// Same-package test, so the unexported resolver field is reachable directly —
	// compactFixture hard-wires its own provider and takes no injection point.
	prov := &promptCapturingProvider{}
	srv.providers = &stubResolver{p: prov}

	if _, err := srv.RecapSession(context.Background(), sessID); err != nil {
		t.Fatalf("RecapSession: %v", err)
	}
	if !strings.Contains(prov.sys, "two sentences") {
		t.Errorf("recap should ask for two sentences, sent: %q", prov.sys)
	}
	if strings.Contains(prov.sys, "% of the original length") {
		t.Errorf("recap must not send the compaction prompt, sent: %q", prov.sys)
	}
	if !strings.Contains(prov.sys, "256") {
		t.Errorf("recap prompt should name the %d-char budget, sent: %q", loop.RecapMaxChars, prov.sys)
	}
}
