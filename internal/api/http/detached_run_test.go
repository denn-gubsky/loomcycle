package http

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// cancelledCallProvider's call blocks until the run is cancelled, then fails
// with the cancellation, as a driver's HTTP request does.
type cancelledCallProvider struct{ started chan struct{} }

func (p *cancelledCallProvider) ID() string                  { return "stub" }
func (p *cancelledCallProvider) Probe(context.Context) error { return nil }
func (p *cancelledCallProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *cancelledCallProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *cancelledCallProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// A detached run cancelled mid-turn records its failure once. The loop records
// the error it ends with, and the detached path used to record its own copy
// of the same error, so the transcript (and every client re-attaching to it)
// showed "context canceled" twice.
func TestDetachedRun_CancelledMidTurnRecordsItsErrorOnce(t *testing.T) {
	prov := &cancelledCallProvider{started: make(chan struct{}, 1)}
	srv, st := newStubProviderServer(t, prov)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	g := &goldenServer{t: t, srv: srv, ts: ts, st: st}

	r := g.post("/v1/runs", goldenRunBody("default", "hi", `"interactive":true`))
	agentID, runID, _ := r.ids(t)
	select {
	case <-prov.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run's turn never started")
	}

	g.post("/v1/agents/"+agentID+"/cancel", "").drain(t)
	r.drain(t)
	waitRunStatus(t, st, runID, store.RunCancelled)

	evs, err := st.GetRunEventsSince(context.Background(), runID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var errs []string
	for _, ev := range evs {
		if ev.Type == "error" {
			errs = append(errs, string(ev.Payload))
		}
	}
	if len(errs) != 1 {
		t.Errorf("stored %d error events, want 1: %v", len(errs), errs)
	}
}
