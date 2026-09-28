package http

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakeCluster stands in for the steer coordinator: it records what it is
// asked to deliver to another replica.
type fakeCluster struct {
	mu   sync.Mutex
	sent []steer.Message
}

func (c *fakeCluster) PushRemote(_ context.Context, _ string, m steer.Message) (bool, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return true, true, nil
}

// clusterRun is a running run owned by replica, in tenant, not in this
// replica's steer registry.
func clusterRun(t *testing.T, srv *Server, tenant, replica string) string {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, tenant, "writer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_" + replica, UserID: "alice", TenantID: tenant, ReplicaID: replica})
	if err != nil {
		t.Fatal(err)
	}
	srv.store.(*replicaStore).owner[run.ID] = replica
	return run.ID
}

// replicaStore reports each run's owning replica as a Postgres row would; the
// SQLite store has no replica column (a cluster needs a shared database).
type replicaStore struct {
	store.Store
	owner map[string]string
}

func (r *replicaStore) GetRun(ctx context.Context, id string) (store.Run, error) {
	run, err := r.Store.GetRun(ctx, id)
	run.ReplicaID = r.owner[id]
	return run, err
}

func clusterFixture(t *testing.T) (*Server, func()) {
	srv, cleanup := channelFanFixture(t)
	srv.store = &replicaStore{Store: srv.store, owner: map[string]string{}}
	cfg := *srv.cfg()
	cfg.Agents = map[string]config.AgentDef{"writer": {Model: "stub-model"}}
	srv.cfgHolder = config.NewHolder(&cfg)
	srv.replicaID = "r1"
	return srv, cleanup
}

// In a cluster, a run is steered, retuned and read from any replica, not only
// the one that owns it: the local steer registry misses, the run's row says
// another replica is running it, and the push routes there. Before, every
// such call answered 404.
func TestSteer_ARunOnAnotherReplicaIsReachable(t *testing.T) {
	srv, cleanup := clusterFixture(t)
	defer cleanup()
	reg := steer.NewRegistry(2)
	cluster := &fakeCluster{}
	reg.SetClusterSteerer(cluster)
	srv.SetSteerRegistry(reg)
	runID := clusterRun(t, srv, "", "r2")

	if rec := doJSON(t, srv, "POST", "/v1/runs/"+runID+"/input", `{"text":"focus on auth"}`); rec.Code != 200 {
		t.Fatalf("steer: status %d: %s", rec.Code, rec.Body.String())
	}
	cluster.mu.Lock()
	sent := len(cluster.sent)
	cluster.mu.Unlock()
	if sent != 1 {
		t.Fatalf("pushed to the owning replica %d time(s), want 1", sent)
	}
	if rec := doJSON(t, srv, "POST", "/v1/runs/"+runID+"/retune", `{"max_tokens":512}`); rec.Code != 200 {
		t.Fatalf("retune: status %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, "GET", "/v1/runs/"+runID+"/config", ``); rec.Code != 200 {
		t.Fatalf("config: status %d: %s", rec.Code, rec.Body.String())
	}
}

// The store gate is no wider than the registry's: a run this replica owns but
// does not have is not live anywhere; without a cluster nothing routes; and
// another tenant's run is the same opaque not-in-flight.
func TestSteer_TheRemoteGateAdmitsOnlyALiveOwnedRun(t *testing.T) {
	srv, cleanup := clusterFixture(t)
	defer cleanup()
	clustered := steer.NewRegistry(2)
	clustered.SetClusterSteerer(&fakeCluster{})
	srv.SetSteerRegistry(clustered)

	if rec := doJSON(t, srv, "POST", "/v1/runs/"+clusterRun(t, srv, "", "r1")+"/input", `{"text":"x"}`); rec.Code != 404 {
		t.Errorf("a run stamped with this replica but not registered: status %d, want 404", rec.Code)
	}
	if _, err := srv.SteerRun(tenantCtx("globex"), clusterRun(t, srv, "acme", "r2"), "x", "api"); !errors.Is(err, connector.ErrRunNotInFlight) {
		t.Errorf("another tenant's run: err %v, want not in flight", err)
	}
	srv.SetSteerRegistry(steer.NewRegistry(2))
	if rec := doJSON(t, srv, "POST", "/v1/runs/"+clusterRun(t, srv, "", "r2")+"/input", `{"text":"x"}`); rec.Code != 404 {
		t.Errorf("no cluster: status %d, want 404", rec.Code)
	}
}
