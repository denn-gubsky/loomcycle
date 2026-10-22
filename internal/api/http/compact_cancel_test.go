package http

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
)

// cutSummaryProvider streams the start of a summary and then ends the call's
// caller (cancel), holding the stream open until the ctx ends: the request
// behind a manual compact hung up mid-summary.
type cutSummaryProvider struct{ cancel func() }

func (p *cutSummaryProvider) ID() string                  { return "scripted" }
func (p *cutSummaryProvider) Probe(context.Context) error { return nil }
func (p *cutSummaryProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *cutSummaryProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *cutSummaryProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 2)
	go func() {
		defer close(ch)
		ch <- providers.Event{Type: providers.EventText, Text: "TRUNCATED"}
		p.cancel()
		<-ctx.Done()
	}()
	return ch, nil
}

// A manual compact whose summary call is cut by its request's cancel fails
// like any other summary failure (502 "summarize: ..."), and nothing is
// applied: no compact control reaches the parked run and no marker is written.
// It used to push the truncated summary into the run as its new history.
func TestCompactRun_SummaryCutByCancelAppliesNothing(t *testing.T) {
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"compactor": {Provider: "scripted", Model: "stub-model", Tools: []string{}}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8},
	}
	cfg.Env.AuthToken = ""
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, _ := makeServer(t, &cutSummaryProvider{cancel: cancel}, cfg)
	srv.SetSteerRegistry(steer.NewRegistry(8))
	sessID, runID := seedConversation(t, srv, false)
	q, dereg := srv.steerReg.Register(steer.Entry{RunID: runID, SessionID: sessID, UserID: "alice"})
	defer dereg()
	srv.steerReg.SetParked(runID, true)

	res, err := srv.CompactRun(ctx, runID)
	var ce *compactErr
	if !errors.As(err, &ce) || ce.HTTPStatus() != 502 || res.Compacted {
		t.Fatalf("CompactRun = (%+v, %v), want a 502 summary failure", res, err)
	}
	select {
	case m := <-q:
		t.Fatalf("a compact control reached the run (%q): a cut summary was applied", m.Text)
	default:
	}
	events, _ := srv.store.GetTranscript(context.Background(), sessID)
	for _, e := range events {
		if e.Type == string(providers.EventContextCompaction) {
			t.Fatalf("a context_compaction marker was persisted: %s", e.Payload)
		}
	}
}
