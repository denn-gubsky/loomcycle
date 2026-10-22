package lanegate

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// queryFakeEmbedder is fakeEmbedder with a query mode. It records the embedder
// lane's in-flight count at the moment its EmbedQuery runs, so a test can tell
// the call was made INSIDE the slot rather than beside it.
type queryFakeEmbedder struct {
	fakeEmbedder
	sched         *concurrency.CallScheduler
	queries       atomic.Int32
	activeAtQuery atomic.Int32
	queryErr      error
}

func (f *queryFakeEmbedder) EmbedQuery(_ context.Context, texts []string) ([][]float32, error) {
	f.queries.Add(1)
	f.activeAtQuery.Store(int32(kindStats(f.sched, config.ModelKindEmbedder).Active))
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return make([][]float32, len(texts)), nil
}

// TestEmbedQuery_ThroughLaneAndObserverReachesTheDriversQueryMode — the
// server's chain (driver, its provider's lane, the health observer) carries a
// query to the driver's query mode, inside one lane slot, and the observer
// counts it.
func TestEmbedQuery_ThroughLaneAndObserverReachesTheDriversQueryMode(t *testing.T) {
	sched := kindLane(config.ModelKindEmbedder, concurrency.LaneConfig{MaxConcurrent: 1})
	drv := &queryFakeEmbedder{sched: sched}
	obs := providers.ObserveEmbedder(WrapEmbedder("p", drv, sched))

	vecs, err := providers.EmbedQuery(context.Background(), obs, []string{"q"})
	if err != nil || len(vecs) != 1 {
		t.Fatalf("EmbedQuery = %d vectors, %v; want 1, nil", len(vecs), err)
	}
	if drv.queries.Load() != 1 || drv.calls.Load() != 0 {
		t.Fatalf("driver: queries=%d embeds=%d, want its query mode once and Embed never", drv.queries.Load(), drv.calls.Load())
	}
	if got := drv.activeAtQuery.Load(); got != 1 {
		t.Fatalf("embedder lane held %d slots during the query, want 1", got)
	}
	if st := kindStats(sched, config.ModelKindEmbedder); st.Grants != 1 || st.Active != 0 {
		t.Fatalf("embedder lane = %+v, want one grant, released", st)
	}
	if h := obs.Health(); h.Calls != 1 || h.Failures != 0 {
		t.Fatalf("observer = %+v, want one call, no failure", h)
	}

	// A failed query is booked as a failure, as a failed Embed is.
	drv.queryErr = errors.New("boom")
	if _, err := providers.EmbedQuery(context.Background(), obs, []string{"q"}); err == nil {
		t.Fatal("the driver's error did not come back")
	}
	if h := obs.Health(); h.Calls != 2 || h.Failures != 1 {
		t.Fatalf("observer after a failed query = %+v, want 2 calls, 1 failure", h)
	}
	if st := kindStats(sched, config.ModelKindEmbedder); st.Grants != 2 || st.Active != 0 {
		t.Fatalf("embedder lane after a failed query = %+v, want two grants, released", st)
	}
}

// TestEmbedQuery_ADriverWithoutQueryModeGetsEmbedThroughBothWrappers — the no
// behaviour change half: every embedder shipped before the split still embeds
// a query with Embed, in the same lane, counted the same way.
func TestEmbedQuery_ADriverWithoutQueryModeGetsEmbedThroughBothWrappers(t *testing.T) {
	sched := kindLane(config.ModelKindEmbedder, concurrency.LaneConfig{MaxConcurrent: 1})
	drv := &fakeEmbedder{}
	obs := providers.ObserveEmbedder(WrapEmbedder("p", drv, sched))

	if _, err := providers.EmbedQuery(context.Background(), obs, []string{"q"}); err != nil {
		t.Fatal(err)
	}
	if drv.calls.Load() != 1 {
		t.Fatalf("driver Embed calls = %d, want 1", drv.calls.Load())
	}
	if st := kindStats(sched, config.ModelKindEmbedder); st.Grants != 1 || st.Active != 0 {
		t.Fatalf("embedder lane = %+v, want one grant, released", st)
	}
	if h := obs.Health(); h.Calls != 1 {
		t.Fatalf("observer = %+v, want one call", h)
	}
}
