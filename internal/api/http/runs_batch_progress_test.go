package http

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// TestSpawnRunBatch_ReportsEachChildAsItSettles: OnChildSettled is called once
// per child, with the child's index and the result its slot holds, and every
// call lands before SpawnRunBatch returns — what the MCP join's progress
// notifications are built on (RFC GK D7).
func TestSpawnRunBatch_ReportsEachChildAsItSettles(t *testing.T) {
	s := newBatchTestServer(t, &latchProvider{dwell: 20 * time.Millisecond}, 4)
	var (
		mu       sync.Mutex
		seen     = map[int]string{}
		returned bool
		late     int
	)
	req := connector.BatchSpawnRequest{
		Spawns: []connector.SpawnRunRequest{
			{Agent: "r", Segments: oneUserSeg("a")},
			{Agent: "r", Segments: oneUserSeg("b")},
			{Agent: "r", Segments: oneUserSeg("c")},
		},
		OnChildSettled: func(i int, r connector.SpawnRunResult) {
			mu.Lock()
			defer mu.Unlock()
			if returned {
				late++
			}
			if _, dup := seen[i]; dup {
				t.Errorf("child %d reported twice", i)
			}
			seen[i] = r.RunID
		},
	}
	res, err := s.SpawnRunBatch(context.Background(), req)
	mu.Lock()
	returned = true
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if late != 0 {
		t.Errorf("%d reports after the call returned", late)
	}
	if len(seen) != len(res.Results) {
		t.Fatalf("reported %d children, want %d", len(seen), len(res.Results))
	}
	for i, r := range res.Results {
		if seen[i] != r.RunID || r.RunID == "" {
			t.Errorf("child %d reported run %q, slot holds %q", i, seen[i], r.RunID)
		}
	}
}
