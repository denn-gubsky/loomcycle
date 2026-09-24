package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// spawn_run with review holds the child and returns only once an operator has
// ruled on its answer — here, approved it.
func TestSpawnRun_ReviewReturnsOnceTheAnswerIsApproved(t *testing.T) {
	srv, _, st := configuredMCP(t)
	httpSrv := srv.cfg.Connector.(interface {
		SetSteerRegistry(*steer.Registry)
		ReviewRun(ctx context.Context, runID, decision, feedback, source string) (bool, error)
	})
	httpSrv.SetSteerRegistry(steer.NewRegistry(0))

	type out struct{ res connector.SpawnRunResult }
	done := make(chan out, 1)
	go func() {
		res := callTool(t, srv, "spawn_run", map[string]any{"agent": "agent", "user_id": "u1", "review": true, "segments": segment("go")})
		var r connector.SpawnRunResult
		_ = json.Unmarshal([]byte(res.Content[0].Text), &r)
		done <- out{r}
	}()

	var runID string
	deadline := time.Now().Add(3 * time.Second)
	for runID == "" && time.Now().Before(deadline) {
		runs, _ := st.ListActiveRunsByUser(context.Background(), "u1", store.RunRunning)
		for _, r := range runs {
			if ev, err := st.GetLastEventForRun(context.Background(), r.ID); err == nil && ev.Type == string(providers.EventAwaitingReview) {
				runID = r.ID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runID == "" {
		t.Fatal("the spawned child was never held")
	}
	select {
	case <-done:
		t.Fatal("spawn_run returned before the held answer was ruled on")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := httpSrv.ReviewRun(context.Background(), runID, "approve", "", "api"); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-done:
		if o.res.FinalText != "finished" {
			t.Errorf("final text = %q", o.res.FinalText)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("spawn_run did not return after approval")
	}
	if run, _ := st.GetRun(context.Background(), runID); run.Status != store.RunCompleted {
		t.Errorf("status = %q", run.Status)
	}
}
