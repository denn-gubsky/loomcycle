package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
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

// heldRun waits for u1's run to be held at the given round and returns it.
func heldRun(t *testing.T, st store.Store, round int) store.Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := st.ListActiveRunsByUser(context.Background(), "u1", store.RunRunning)
		for _, r := range runs {
			ev, err := st.GetLastEventForRun(context.Background(), r.ID)
			if err != nil || ev.Type != string(providers.EventAwaitingReview) {
				continue
			}
			var p providers.Event
			if json.Unmarshal(ev.Payload, &p) == nil && p.AwaitingReview != nil && p.AwaitingReview.Round == round {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no run held at round %d", round)
	return store.Run{}
}

// The review_run tool drives the whole loop: reject with feedback, the
// revision is held again, approve — and the blocked spawn returns.
func TestReviewRunTool_RejectThenApprove(t *testing.T) {
	srv, prov, st := configuredMCP(t)
	srv.cfg.Connector.(interface{ SetSteerRegistry(*steer.Registry) }).SetSteerRegistry(steer.NewRegistry(0))

	done := make(chan loommcp.CallToolResult, 1)
	go func() {
		done <- callTool(t, srv, "spawn_run", map[string]any{"agent": "agent", "user_id": "u1", "review": true, "segments": segment("go")})
	}()
	run := heldRun(t, st, 1)
	// The reviewer is a second MCP client of the same deployment. One server
	// drives one session at a time, and the spawn's session is blocked on the
	// hold being reviewed.
	reviewer := New(srv.cfg)

	for name, args := range map[string]map[string]any{
		"unknown decision":        {"agent_id": run.AgentID, "decision": "maybe"},
		"feedback on an approval": {"agent_id": run.AgentID, "decision": "approve", "feedback": "nice"},
		"no agent_id":             {"decision": "approve"},
	} {
		res := callTool(t, reviewer, "review_run", args)
		if !res.IsError || !strings.Contains(string(res.StructuredContent), `"validation"`) {
			t.Errorf("%s = isError %v %s, want a validation refusal", name, res.IsError, res.StructuredContent)
		}
	}

	if res := callTool(t, reviewer, "review_run", map[string]any{"agent_id": run.AgentID, "decision": "reject", "feedback": "cover the rollback"}); res.IsError {
		t.Fatalf("reject: %v", res.Content)
	}
	heldRun(t, st, 2)
	if prov.last == nil || len(prov.last.Messages) == 0 {
		t.Fatal("the revision never reached the provider")
	}
	if last := prov.last.Messages[len(prov.last.Messages)-1]; last.Content[0].Text != "cover the rollback" {
		t.Errorf("revision's last user turn = %q, want the feedback", last.Content[0].Text)
	}
	if res := callTool(t, reviewer, "review_run", map[string]any{"agent_id": run.AgentID, "decision": "approve"}); res.IsError {
		t.Fatalf("approve: %v", res.Content)
	}
	select {
	case res := <-done:
		if res.IsError {
			t.Errorf("spawn_run = %v", res.Content)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("spawn_run did not return after approval")
	}
	// Finished: a later verdict is refused.
	if res := callTool(t, reviewer, "review_run", map[string]any{"agent_id": run.AgentID, "decision": "approve"}); !res.IsError {
		t.Error("a verdict on a finished run was accepted")
	}
}

// Delivering a verdict is available to a tenant-confined session: the
// connector gates it on the run's tenant and owner.
func TestReviewRunTool_IsTenantConfinable(t *testing.T) {
	if !tenantConfinableTools["review_run"] {
		t.Error("review_run is not tenant-confinable: a tenant operator could not review its own runs")
	}
}
