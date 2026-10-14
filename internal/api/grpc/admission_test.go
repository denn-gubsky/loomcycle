package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A queued / admitted frame must reach a TYPED gRPC client with its payload,
// not just its type: without it a client sees that a run waits but not why,
// where, or for how long.
func TestEventToProto_CarriesTheAdmissionPayload(t *testing.T) {
	q := eventToProto(providers.Event{Type: providers.EventQueued,
		Admission: &providers.AdmissionInfo{Reason: "user_cap", Position: 3}})
	if q.GetType() != "queued" {
		t.Fatalf("type = %q, want queued", q.GetType())
	}
	if a := q.GetAdmission(); a == nil || a.GetReason() != "user_cap" || a.GetPosition() != 3 || a.GetWaitMs() != 0 {
		t.Fatalf("queued payload = %+v, want reason user_cap, position 3", a)
	}

	ad := eventToProto(providers.Event{Type: providers.EventAdmitted,
		Admission: &providers.AdmissionInfo{WaitMs: 4200}})
	if ad.GetType() != "admitted" {
		t.Fatalf("type = %q, want admitted", ad.GetType())
	}
	if a := ad.GetAdmission(); a == nil || a.GetWaitMs() != 4200 {
		t.Fatalf("admitted payload = %+v, want wait_ms 4200", a)
	}
}

// An event with no admission payload must not grow an empty one — a client
// that checks presence would read every frame as an admission report.
func TestEventToProto_LeavesOtherEventsWithoutAnAdmission(t *testing.T) {
	if got := eventToProto(providers.Event{Type: providers.EventText, Text: "hello"}); got.Admission != nil {
		t.Error("a plain text frame carries an admission payload")
	}
}

// admission reaches the runner from Run and a batch child / configured run
// alike.
func TestRun_AdmissionReachesTheRunner(t *testing.T) {
	fr := &fakeRunner{registered: registrationFrame{AgentID: "a", RunID: "r", SessionID: "s"}}
	client, cleanup := startTestServerWithRunner(t, fr)
	defer cleanup()
	stream, err := client.Run(context.Background(), &loomcyclepb.RunRequest{Agent: "default", Admission: "reject"})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, stream)
	if fr.lastInput.Admission != "reject" {
		t.Errorf("Run: RunInput.Admission = %q, want reject", fr.lastInput.Admission)
	}
	if r := spawnRequestFromProto(&loomcyclepb.RunRequest{Admission: "wait"}); r.Admission != "wait" {
		t.Errorf("spawnRequestFromProto: Admission = %q, want wait", r.Admission)
	}
}

// An unknown admission value is refused on every fresh-run RPC rather than
// read as the default: a caller that misspelt "reject" must not be queued.
func TestRun_RefusesAnUnknownAdmission(t *testing.T) {
	fr := &fakeRunner{registered: registrationFrame{AgentID: "a", RunID: "r", SessionID: "s"}}
	client := startTestServerWithRunnerAndConnector(t, fr, &interactiveMock{})

	stream, err := client.Run(context.Background(), &loomcyclepb.RunRequest{Agent: "default", Admission: "rejected"})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("Run: code = %v (%v), want InvalidArgument", status.Code(err), err)
	}
	if fr.lastInput.Agent != "" {
		t.Error("Run started a run with an unknown admission value")
	}

	_, err = client.SpawnRunBatch(context.Background(), &loomcyclepb.BatchSpawnRequest{
		Spawns: []*loomcyclepb.RunRequest{{Agent: "default"}, {Agent: "default", Admission: "later"}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("SpawnRunBatch: code = %v (%v), want InvalidArgument", status.Code(err), err)
	}

	_, err = client.CreateConfiguredRun(context.Background(), &loomcyclepb.RunRequest{Agent: "default", Admission: "queue"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateConfiguredRun: code = %v (%v), want InvalidArgument", status.Code(err), err)
	}
}

// A queued run's single-run read carries where it waits; the position lives
// in the replica's memory, so it is asked of the injected source and only for
// a queued run.
func TestGetRun_ReportsAQueuedRunsPlace(t *testing.T) {
	st := newTestStore(t)
	asked := map[string]bool{}
	adapter := New(Config{Store: st, CancelReg: cancel.NewRegistry(),
		// The queue knows a run by its agent id, the name it waits under.
		QueuePosition: func(agentID string) (int, string, bool) {
			asked[agentID] = true
			if agentID != "a_q" {
				return 0, "", false
			}
			return 2, "global_cap", true
		}})
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "acme", "default", "alice")
	if err != nil {
		t.Fatal(err)
	}
	queued, err := st.CreateQueuedRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_q", UserID: "alice", TenantID: "acme"})
	if err != nil {
		t.Fatalf("CreateQueuedRun: %v", err)
	}
	running := seedRunID(t, st, "acme", "alice", "a_r")

	op := scopedCtx("acme", "op", auth.ScopeTenant)
	got, err := adapter.GetRun(op, &loomcyclepb.GetRunRequest{RunId: queued.ID})
	if err != nil {
		t.Fatalf("GetRun(queued): %v", err)
	}
	if got.GetStatus() != string(store.RunQueued) || got.GetQueuePosition() != 2 || got.GetQueueReason() != "global_cap" {
		t.Errorf("queued run read = (status %q, position %d, reason %q), want (queued, 2, global_cap)",
			got.GetStatus(), got.GetQueuePosition(), got.GetQueueReason())
	}

	got, err = adapter.GetRun(op, &loomcyclepb.GetRunRequest{RunId: running})
	if err != nil {
		t.Fatalf("GetRun(running): %v", err)
	}
	if got.GetQueuePosition() != 0 || got.GetQueueReason() != "" || asked["a_r"] {
		t.Errorf("a running run reports a queue place (%d, %q) or was looked up", got.GetQueuePosition(), got.GetQueueReason())
	}
}
