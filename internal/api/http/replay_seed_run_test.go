package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A replay's seed run holds the carried transcript; no loop runs it. It is
// finished when the seeding returns, so no replica treats it as live. Before,
// it stayed running with the creating replica's id: that replica had no steer
// entry for it, but another replica's remote gate admitted a steer or retune.
func TestReplaySession_SeedRunIsTerminalAndNotSteerableFromAnotherReplica(t *testing.T) {
	srv, cleanup := clusterFixture(t)
	defer cleanup()
	reg := steer.NewRegistry(2)
	cluster := &fakeCluster{}
	reg.SetClusterSteerer(cluster)
	srv.SetSteerRegistry(reg)
	ctx := context.Background()

	src, err := srv.store.CreateSession(ctx, "", "writer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	srcRun, err := srv.store.CreateRun(ctx, src.ID, store.RunIdentity{AgentID: "a_src", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, srcRun.ID, "user_input", userSeg("hello"))
	res, err := srv.ReplaySession(ctx, connector.ReplaySessionRequest{SourceSessionID: src.ID, Agent: "writer"})
	if err != nil {
		t.Fatalf("ReplaySession: %v", err)
	}
	seed, err := srv.store.GetRun(ctx, res.SeedRunID)
	if err != nil {
		t.Fatal(err)
	}
	if seed.Status == store.RunRunning {
		t.Errorf("the seed run is %q after the replay returned; no loop runs it", seed.Status)
	}

	// The same row, read from the other replica of the cluster: stamped with
	// the replica that created it, as a Postgres row is.
	srv.store.(*replicaStore).owner[res.SeedRunID] = srv.replicaID
	srv.replicaID = "r2"
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/runs/" + res.SeedRunID + "/input", `{"text":"x"}`},
		{"POST", "/v1/runs/" + res.SeedRunID + "/retune", `{"max_tokens":512}`},
	} {
		if rec := doJSON(t, srv, c.method, c.path, c.body); rec.Code != 404 {
			t.Errorf("%s %s on the seed run from another replica: status %d, want 404: %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	cluster.mu.Lock()
	sent := len(cluster.sent)
	cluster.mu.Unlock()
	if sent != 0 {
		t.Errorf("pushed %d message(s) to the seed run's replica, want 0", sent)
	}
}
