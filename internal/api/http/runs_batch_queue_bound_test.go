package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestWriteQuotaError_FullQueueNamesItsBound pins RFC GK D6 on the HTTP 429:
// a full queue's body carries queue_capacity and queued_now; a refusal that is
// not about a queue carries neither.
func TestWriteQuotaError_FullQueueNamesItsBound(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		wantCapacity  any
		wantQueuedNow any
	}{
		{"backpressure", &concurrency.BackpressureError{QueueCapacity: 3, QueuedNow: 3}, float64(3), float64(3)},
		{"per-user waiting runs", &concurrency.ErrPerUserQuotaExhausted{UserID: "u", Cap: 1, QueueCapacity: 2, QueuedNow: 2}, float64(2), float64(2)},
		{"no free slot", &concurrency.BackpressureError{}, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeQuotaError(rec, c.err)
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", rec.Code)
			}
			got := decodeErrorBody(t, rec.Body.Bytes())
			if got["queue_capacity"] != c.wantCapacity || got["queued_now"] != c.wantQueuedNow {
				t.Errorf("body = %v, want queue_capacity %v, queued_now %v", got, c.wantCapacity, c.wantQueuedNow)
			}
		})
	}
}

// TestSpawnRunBatch_QueueFullChildNamesTheBound: a batch larger than a user's
// share of the queue has its overflow refused "queue full for this user", and
// each refused child's slot names the bound and how many runs held it, in the
// envelope every transport serialises.
func TestSpawnRunBatch_QueueFullChildNamesTheBound(t *testing.T) {
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"r": {Model: "stub-model", SystemPrompt: "be brief"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 8},
	}
	cfg.Env.AuthToken = "" // open mode
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "qb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sem := concurrency.New(1, 8, 0).WithQueuedPerUserCap(1)
	s := New(cfg, &stubResolver{p: &latchProvider{dwell: 300 * time.Millisecond}}, []tools.Tool{}, sem, st)

	spawns := make([]connector.SpawnRunRequest, 4)
	for i := range spawns {
		spawns[i] = connector.SpawnRunRequest{Agent: "r", Segments: oneUserSeg("x")}
	}
	res, err := s.SpawnRunBatch(context.Background(), connector.BatchSpawnRequest{Spawns: spawns})
	if err != nil {
		t.Fatal(err)
	}
	refused := 0
	for i, r := range res.Results {
		if !strings.Contains(r.Error, "queue full for this user") {
			continue
		}
		refused++
		if r.QueueCapacity != 1 || r.QueuedNow != 1 {
			t.Errorf("child %d: queue_capacity %d queued_now %d, want 1 and 1", i, r.QueueCapacity, r.QueuedNow)
		}
		b, _ := json.Marshal(r)
		if !strings.Contains(string(b), `"queue_capacity":1`) || !strings.Contains(string(b), `"queued_now":1`) {
			t.Errorf("child %d envelope %s lacks the bound", i, b)
		}
	}
	// One runs, one waits, the rest are over the user's share of the queue.
	if refused != 2 {
		t.Fatalf("refused = %d, want 2; results %+v", refused, res.Results)
	}
}
