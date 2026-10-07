package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// memBackplane is an in-process cluster backplane: Publish fans a copy to
// every subscriber of the topic, the publisher's own included (the
// coordinators skip what is not theirs).
type memBackplane struct {
	mu   sync.Mutex
	subs map[string][]chan coord.Event
}

func (b *memBackplane) Publish(_ context.Context, topic string, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs[topic] {
		select {
		case ch <- coord.Event{Topic: topic, Payload: payload}:
		default:
		}
	}
	return nil
}

func (b *memBackplane) Subscribe(ctx context.Context, topic string) (<-chan coord.Event, error) {
	ch := make(chan coord.Event, 64)
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], ch)
	b.mu.Unlock()
	// Closed under the lock Publish sends under, so a send never meets a
	// closed channel; closing ends the subscriber goroutine.
	context.AfterFunc(ctx, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		subs := b.subs[topic]
		for i, c := range subs {
			if c == ch {
				b.subs[topic] = append(subs[:i:i], subs[i+1:]...)
				break
			}
		}
		close(ch)
	})
	return ch, nil
}

func (b *memBackplane) Close() error { return nil }

func (b *memBackplane) subscribers(topic string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[topic])
}

type aliveReplicas struct{}

func (aliveReplicas) IsReplicaAlive(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}

// replicaRowStore keeps the replica each run row was created with and reports
// it on read, as the shared Postgres database a cluster runs on does; SQLite
// has no replica column.
type replicaRowStore struct {
	store.Store
	mu      sync.Mutex
	replica map[string]string
}

func (s *replicaRowStore) CreateRun(ctx context.Context, sessionID string, identity store.RunIdentity) (store.Run, error) {
	run, err := s.Store.CreateRun(ctx, sessionID, identity)
	if err == nil {
		s.mu.Lock()
		s.replica[run.ID] = identity.ReplicaID
		s.mu.Unlock()
	}
	return run, err
}

func (s *replicaRowStore) GetRun(ctx context.Context, id string) (store.Run, error) {
	run, err := s.Store.GetRun(ctx, id)
	s.mu.Lock()
	run.ReplicaID = s.replica[id]
	s.mu.Unlock()
	return run, err
}

// walkCluster is the detach harness's server as replica r1 and a second
// server, r2, on the same database, joined by the turn-cancel and steer
// coordinators wired as a clustered boot wires them.
type walkCluster struct {
	h          *detachHarness
	a, b       *Server
	ackTimeout time.Duration
}

func newWalkCluster(t *testing.T) *walkCluster {
	t.Helper()
	h := newDetachHarness(t, false)
	shared := &replicaRowStore{Store: h.st, replica: map[string]string{}}
	a := h.srv
	a.store = shared
	b := New(a.cfg(), a.providers, []tools.Tool{}, concurrency.New(4, 4, time.Second), shared)
	c := &walkCluster{h: h, a: a, b: b, ackTimeout: 3 * time.Second}

	bp := &memBackplane{subs: map[string][]chan coord.Event{}}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	for _, r := range []struct {
		srv *Server
		id  string
	}{{a, "r1"}, {b, "r2"}} {
		r.srv.replicaID = r.id
		r.srv.SetSteerRegistry(steer.NewRegistry(0))
		tc, err := coord.NewTurnCancelCoordinator(coord.TurnCancelCoordinatorConfig{
			Backplane: bp, ReplicaID: r.id, Store: shared, ReplicaStore: aliveReplicas{}, AckTimeout: c.ackTimeout,
		})
		if err != nil {
			t.Fatal(err)
		}
		r.srv.TurnCancelRegistry().SetClusterCanceller(tc)
		go tc.RunTurnCancelSubscriber(ctx, r.srv.TurnCancelRegistry())
		go tc.RunTurnCancelAckSubscriber(ctx)
		sc, err := coord.NewSteerCoordinator(coord.SteerCoordinatorConfig{
			Backplane: bp, ReplicaID: r.id, Store: shared, ReplicaStore: aliveReplicas{}, AckTimeout: c.ackTimeout,
		})
		if err != nil {
			t.Fatal(err)
		}
		r.srv.SteerRegistry().SetClusterSteerer(sc)
		go sc.RunSteerSubscriber(ctx, r.srv.SteerRegistry())
		go sc.RunSteerAckSubscriber(ctx)
	}
	waitFor(t, "both replicas to subscribe", func() bool {
		for _, topic := range []string{"loomcycle.turncancel", "loomcycle.turncancel.ack", "loomcycle.steer", "loomcycle.steer.ack"} {
			if bp.subscribers(topic) != 2 {
				return false
			}
		}
		return true
	})
	return c
}

// runWalkOnA starts a detached walk on r1 and waits for its member to be in
// its model call.
func (c *walkCluster) runWalkOnA(t *testing.T) (walkID string, member store.Run) {
	t.Helper()
	walkID = c.h.runDetached(c.h.startAgent(nil, false))
	member = c.h.memberUp(walkID)
	if row, err := c.b.store.GetRun(context.Background(), walkID); err != nil || row.ReplicaID != "r1" {
		t.Fatalf("walk row = %+v, %v; want it running on r1", row, err)
	}
	return walkID, member
}

// call drives one route's handler on srv as a tenant operator of tenant.
func call(srv *Server, tenant, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.SetPathValue("run_id", strings.Split(path, "/")[3])
	req = req.WithContext(tenantOperatorCtx(tenant))
	rec := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(path, "/cancel"):
		srv.handleCancelTurn(rec, req)
	case strings.HasSuffix(path, "/input"):
		srv.handleRunInput(rec, req)
	}
	return rec
}

// A walk is cancelled from any replica, not only the one running it. The
// replica the cancel lands on does not have the walk, so it routes the cancel
// to the owner by the walk's row; the owner's listener looked only for an
// armed turn, which a walk never has, so the cancel waited out the ack timeout
// and answered "no in-flight run" while the walk ran on.
func TestTeamWalkCluster_ACancelFromAnotherReplicaStopsTheWalkAndItsMember(t *testing.T) {
	c := newWalkCluster(t)
	walkID, member := c.runWalkOnA(t)

	start := time.Now()
	rec := call(c.b, "acme", "/v1/runs/"+walkID+"/cancel", `{"reason":"stop from r2"}`)
	elapsed := time.Since(start)
	if rec.Code != 200 {
		t.Fatalf("cancel via r2: status %d after %v: %s", rec.Code, elapsed, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["stopped"] != true || body["parked"] != false {
		t.Errorf("body = %v, want stopped:true parked:false, as the owner answers", body)
	}
	if elapsed >= c.ackTimeout {
		t.Errorf("the cancel took %v, the whole ack timeout: the owner never acknowledged it", elapsed)
	}
	if walk := c.h.waitEnded(walkID); walk.Status != store.RunCancelled || walk.StopReason != "stop from r2" {
		t.Errorf("walk = %s (%q), want cancelled with the caller's reason", walk.Status, walk.StopReason)
	}
	if got := c.h.waitEnded(member.ID); got.Status != store.RunCancelled {
		t.Errorf("member = %s (%s), want cancelled with its walk", got.Status, got.ErrorMsg)
	}
}

// What another replica answers about a walk is what its owner answers: a
// stranger's cancel is the same opaque 404 there, and it reaches nothing — the
// walk is later stopped with its owner's reason, not the stranger's; a steer is
// refused the same way on both, since a walk takes no operator turns.
func TestTeamWalkCluster_AnotherReplicaAnswersAsTheOwnerDoes(t *testing.T) {
	c := newWalkCluster(t)
	walkID, _ := c.runWalkOnA(t)

	for _, srv := range []*Server{c.a, c.b} {
		if rec := call(srv, "globex", "/v1/runs/"+walkID+"/cancel", `{"reason":"stranger"}`); rec.Code != 404 {
			t.Errorf("stranger cancel on %s: status %d, want 404: %s", srv.replicaID, rec.Code, rec.Body)
		}
		start := time.Now()
		if rec := call(srv, "acme", "/v1/runs/"+walkID+"/input", `{"text":"focus"}`); rec.Code != 404 {
			t.Errorf("steer on %s: status %d, want 404: %s", srv.replicaID, rec.Code, rec.Body)
		}
		if d := time.Since(start); d >= c.ackTimeout {
			t.Errorf("steer on %s took %v, the whole ack timeout", srv.replicaID, d)
		}
	}
	if rec := call(c.a, "acme", "/v1/runs/"+walkID+"/cancel", `{"reason":"owner stop"}`); rec.Code != 200 {
		t.Fatalf("owner's cancel: status %d: %s", rec.Code, rec.Body)
	}
	if walk := c.h.waitEnded(walkID); walk.StopReason != "owner stop" {
		t.Errorf("walk stopped with %q, want the owner's reason: a refused cancel reached it", walk.StopReason)
	}
}
