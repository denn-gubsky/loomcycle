package http

import (
	"fmt"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/redact"
)

// TestServer_RedactorKeepsALiveRunsCredential: the server answers the
// redactor's liveness from its cancel registry, by run id and by agent id, so
// a credential a run in flight registered survives a flood from its own
// tenant, and is evictable once the run deregisters.
func TestServer_RedactorKeepsALiveRunsCredential(t *testing.T) {
	s := &Server{cancelReg: cancel.NewRegistry()}
	if err := s.cancelReg.Register(cancel.Entry{AgentID: "agent-1", RunID: "run-1", StartedAt: time.Now()}, func(error) {}); err != nil {
		t.Fatal(err)
	}
	live := s.liveRunIDs()
	if !live("agent-1") || !live("run-1") || live("run-2") || live("") {
		t.Fatal("liveness does not match the cancel registry")
	}

	rd := redact.New(nil, false)
	rd.SetLiveness(s.liveRunIDs)
	rd.RegisterCredential(redact.Holder{Tenant: "t", Runs: []string{"run-1"}}, "live-run-credential")
	flood := func() {
		for i := 0; i < 2*redact.DefaultDynamicCap; i++ {
			rd.RegisterCredential(redact.Holder{Tenant: "t"}, fmt.Sprintf("flood-credential-%05d", i))
		}
	}
	flood()
	if got := rd.String("x live-run-credential y"); got != "x [redacted:credential] y" {
		t.Fatalf("a live run's credential was evicted: %q", got)
	}
	s.cancelReg.Deregister("agent-1")
	flood()
	if got := rd.String("x live-run-credential y"); got == "x [redacted:credential] y" {
		t.Error("the credential stayed pinned after its run deregistered")
	}
}
