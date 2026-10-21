package grpc

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
)

// A gRPC Continue holds its session in every replica, as the HTTP
// continuation routes do: two replicas must not take turns on one session at
// once. A fresh Run has no session to hold.
func TestContinue_TakesTheSessionLockAcrossReplicas(t *testing.T) {
	fr := &fakeRunner{registered: registrationFrame{AgentID: "a", RunID: "r", SessionID: "s"}}
	client, cleanup := startTestServerWithRunner(t, fr)
	defer cleanup()

	cs, err := client.Continue(context.Background(), &loomcyclepb.ContinueRequest{SessionId: "s"})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, cs)
	if !fr.lastInput.SessionLockAcrossReplicas {
		t.Error("Continue: the RunInput does not take the session lock across replicas")
	}

	rs, err := client.Run(context.Background(), &loomcyclepb.RunRequest{Agent: "default"})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, rs)
	if fr.lastInput.SessionLockAcrossReplicas {
		t.Error("Run: a fresh run asks for a session lock it has no session for")
	}
}
