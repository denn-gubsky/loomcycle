package http

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channelhooks"
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

// clusterSubRun is clusterRun for a sub-run of parent: pc nil is the Agent
// tool's child, a pc with a walk id is a team-walk member.
func clusterSubRun(t *testing.T, srv *Server, parent, replica string, pc *store.ParentContext) string {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "writer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "sub_" + replica, ParentRunID: parent, UserID: "alice", ReplicaID: replica, ParentContext: pc,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.store.(*replicaStore).owner[run.ID] = replica
	return run.ID
}

// A child the Agent tool spawned takes a verdict and nothing else on the
// replica that owns it (its steer entry is VerdictsOnly). Another replica must
// refuse it the same way: before, the remote gate admitted any running run, so
// from there a retune changed the model or budget of a child its parent drives,
// and its config was readable, while the owner answered 404. Its verdict still
// routes, and a walk member and a top-level run stay reachable.
func TestSteer_ARemoteAgentToolChildTakesOnlyAVerdict(t *testing.T) {
	srv, cleanup := clusterFixture(t)
	defer cleanup()
	reg := steer.NewRegistry(2)
	cluster := &fakeCluster{}
	reg.SetClusterSteerer(cluster)
	srv.SetSteerRegistry(reg)
	parent := clusterRun(t, srv, "", "r2")
	child := clusterSubRun(t, srv, parent, "r2", nil)

	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/runs/" + child + "/retune", `{"max_tokens":512}`},
		{"POST", "/v1/runs/" + child + "/input", `{"text":"focus on auth"}`},
		{"GET", "/v1/runs/" + child + "/config", ``},
	} {
		if rec := doJSON(t, srv, c.method, c.path, c.body); rec.Code != 404 {
			t.Errorf("%s %s on another replica's Agent-tool child: status %d, want 404: %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	cluster.mu.Lock()
	sent := len(cluster.sent)
	cluster.mu.Unlock()
	if sent != 0 {
		t.Fatalf("pushed %d message(s) to the owner of a verdicts-only child, want 0", sent)
	}

	if err := srv.store.AppendEvent(context.Background(), child, "awaiting_review", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if rec := doJSON(t, srv, "POST", "/v1/runs/"+child+"/review", `{"decision":"approve"}`); rec.Code != 200 {
		t.Errorf("verdict on another replica's held child: status %d, want 200: %s", rec.Code, rec.Body.String())
	}

	member := clusterSubRun(t, srv, parent, "r2", &store.ParentContext{WalkID: "w1", State: "a", StateVisit: 1})
	for _, id := range []string{member, parent} {
		if rec := doJSON(t, srv, "POST", "/v1/runs/"+id+"/retune", `{"max_tokens":512}`); rec.Code != 200 {
			t.Errorf("retune of %s on another replica: status %d, want 200: %s", id, rec.Code, rec.Body.String())
		}
	}
}

// clusterRunAs is clusterRun under a given agent label (and a session of the
// given agent), for the runs filed under a label of their own.
func clusterRunAs(t *testing.T, srv *Server, sessAgent, agentID, replica string, interactive bool) string {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", sessAgent, "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: agentID, UserID: "alice", ReplicaID: replica, Interactive: interactive,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.store.(*replicaStore).owner[run.ID] = replica
	return run.ID
}

// A team walk's own run and a channel hook's run have no steer entry on the
// replica that owns them, so the owner answers a steer, retune or config read
// 404. Another replica must answer the same. A walk started from the substrate
// plane has no parent run, so the verdicts-only derivation does not catch it;
// the row's agent id does. A top-level interactive run stays reachable.
//
// These rows carry their owner's replica id; the walk and hook paths do not
// stamp one today, which the remote gate refuses on its own. This pins the
// refusal to what the run is, not to that omission.
func TestSteer_ARemoteWalkOrHookRunIsRefusedLikeOnItsOwner(t *testing.T) {
	srv, cleanup := clusterFixture(t)
	defer cleanup()
	reg := steer.NewRegistry(2)
	cluster := &fakeCluster{}
	reg.SetClusterSteerer(cluster)
	srv.SetSteerRegistry(reg)
	walk := clusterRunAs(t, srv, teamWalkAgentPrefix+"triage", teamWalkAgentPrefix+"triage", "r2", false)
	hook := clusterRunAs(t, srv, channelhooks.HookAgentPrefix+"gate", channelhooks.HookAgentPrefix+"gate", "r2", false)

	calls := func(id string) []struct{ method, path, body string } {
		return []struct{ method, path, body string }{
			{"POST", "/v1/runs/" + id + "/retune", `{"max_tokens":512}`},
			{"POST", "/v1/runs/" + id + "/input", `{"text":"focus on auth"}`},
			{"GET", "/v1/runs/" + id + "/config", ``},
		}
	}
	for _, id := range []string{walk, hook} {
		for _, c := range calls(id) {
			if rec := doJSON(t, srv, c.method, c.path, c.body); rec.Code != 404 {
				t.Errorf("%s %s from another replica: status %d, want 404: %s", c.method, c.path, rec.Code, rec.Body.String())
			}
		}
	}
	cluster.mu.Lock()
	sent := len(cluster.sent)
	cluster.mu.Unlock()
	if sent != 0 {
		t.Fatalf("pushed %d message(s) to the owner of a walk or hook run, want 0", sent)
	}

	// Its owner: no entry in the registry, the row names this replica.
	srv.replicaID = "r2"
	for _, c := range calls(walk) {
		if rec := doJSON(t, srv, c.method, c.path, c.body); rec.Code != 404 {
			t.Errorf("%s %s on the owning replica: status %d, want 404", c.method, c.path, rec.Code)
		}
	}
	srv.replicaID = "r1"

	top := clusterRunAs(t, srv, "writer", "a_top", "r2", true)
	for _, c := range calls(top) {
		if rec := doJSON(t, srv, c.method, c.path, c.body); rec.Code != 200 {
			t.Errorf("%s %s of a top-level interactive run on another replica: status %d, want 200: %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}
