package grpc

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A gRPC run read names the agent, echoes the run's lineage and says which
// provider served it — what the HTTP read has carried all along, and what a
// gRPC client needs to attribute a sub-agent's cost without a second fetch.
func TestGetAgent_CarriesAgentNameLineageAndProvider(t *testing.T) {
	client, _, st, cleanup := startTestServer(t, "")
	defer cleanup()
	ctx := context.Background()

	sess, err := st.CreateSession(ctx, "t", "qa-agent", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_lineage", UserID: "alice", Interactive: true,
		ParentContext: &store.ParentContext{FunctionKey: "fk-1", WalkID: "walk-1", WaveID: "wave-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn",
		store.Usage{Model: "m-1", Provider: "deepseek"}, ""); err != nil {
		t.Fatal(err)
	}

	got, err := client.GetAgent(ctx, &loomcyclepb.GetAgentRequest{AgentId: "a_lineage"})
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.GetAgent() != "qa-agent" {
		t.Errorf("agent = %q, want qa-agent", got.GetAgent())
	}
	if got.GetUsage().GetProvider() != "deepseek" {
		t.Errorf("usage.provider = %q, want deepseek", got.GetUsage().GetProvider())
	}
	if !got.GetInteractive() {
		t.Error("interactive = false, want true")
	}
	pc := got.GetParentContext()
	if pc == nil || pc.GetFunctionKey() != "fk-1" || pc.GetWalkId() != "walk-1" || pc.GetWaveId() != "wave-1" {
		t.Errorf("parent_context = %+v, want the run's lineage", pc)
	}
}

// A run that carried no lineage reads with parent_context UNSET, not as an
// empty message a client would mistake for a walk with a blank id.
func TestGetAgent_RunWithoutLineageHasNoParentContext(t *testing.T) {
	client, _, st, cleanup := startTestServer(t, "")
	defer cleanup()
	ctx := context.Background()
	sess, _ := st.CreateSession(ctx, "t", "qa-agent", "alice")
	if _, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_nolineage", UserID: "alice"}); err != nil {
		t.Fatal(err)
	}
	got, err := client.GetAgent(ctx, &loomcyclepb.GetAgentRequest{AgentId: "a_nolineage"})
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.ParentContext != nil {
		t.Errorf("parent_context = %+v, want unset", got.ParentContext)
	}
}

// A running run parked on a channel, an interruption, a review hold or for the
// operator's next turn reads with awaited_state saying so — on the single read
// AND the listing, the same answer the HTTP read gives for the same run.
func TestAgentReads_ReportWhatARunningRunIsBlockedOn(t *testing.T) {
	cases := []struct {
		name, evType, payload string
		wantState, wantOn     string
	}{
		{"channel", "tool_call",
			`{"type":"tool_call","tool_use":{"id":"tu_1","name":"Channel","input":{"op":"subscribe","channel":"findings"}}}`,
			"channel", "findings"},
		{"interrupted", "tool_call",
			`{"type":"tool_call","tool_use":{"id":"tu_2","name":"Interruption","input":{"op":"ask","question":"go?"}}}`,
			"interrupted", "question"},
		{"review", "awaiting_review",
			`{"type":"awaiting_review","awaiting_review":{"round":1}}`,
			"review", ""},
		{"review_by_hook", "awaiting_review",
			`{"type":"awaiting_review","awaiting_review":{"round":1,"held_by":"ops/gate"}}`,
			"review", "ops/gate"},
		{"input", "awaiting_input",
			`{"type":"awaiting_input","awaiting_input":{"since_turn":1}}`,
			"input", ""},
		{"progressing", "text", `{"type":"text","text":"working"}`, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, _, st, cleanup := startTestServer(t, "")
			defer cleanup()
			ctx := context.Background()
			sess, _ := st.CreateSession(ctx, "t", "qa-agent", "alice")
			run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_parked", UserID: "alice"})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.AppendEvent(ctx, run.ID, tc.evType, []byte(tc.payload)); err != nil {
				t.Fatal(err)
			}

			got, err := client.GetAgent(ctx, &loomcyclepb.GetAgentRequest{AgentId: "a_parked"})
			if err != nil {
				t.Fatalf("GetAgent: %v", err)
			}
			if got.GetAwaitedState() != tc.wantState || got.GetAwaitedOn() != tc.wantOn {
				t.Errorf("GetAgent awaited = (%q,%q), want (%q,%q)",
					got.GetAwaitedState(), got.GetAwaitedOn(), tc.wantState, tc.wantOn)
			}

			list, err := client.ListUserAgents(ctx, &loomcyclepb.ListUserAgentsRequest{UserId: "alice"})
			if err != nil || len(list.GetAgents()) != 1 {
				t.Fatalf("ListUserAgents = %+v (%v), want the one run", list, err)
			}
			if a := list.GetAgents()[0]; a.GetAwaitedState() != tc.wantState || a.GetAwaitedOn() != tc.wantOn {
				t.Errorf("ListUserAgents awaited = (%q,%q), want (%q,%q)",
					a.GetAwaitedState(), a.GetAwaitedOn(), tc.wantState, tc.wantOn)
			}
		})
	}
}

// A run that has ended is not blocked on anything, whatever its last event.
func TestGetAgent_EndedRunHasNoAwaitedState(t *testing.T) {
	client, _, st, cleanup := startTestServer(t, "")
	defer cleanup()
	ctx := context.Background()
	sess, _ := st.CreateSession(ctx, "t", "qa-agent", "alice")
	run, _ := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_ended", UserID: "alice"})
	_ = st.AppendEvent(ctx, run.ID, "tool_call",
		[]byte(`{"type":"tool_call","tool_use":{"id":"tu_1","name":"Channel","input":{"op":"subscribe","channel":"c"}}}`))
	if err := st.FinishRun(ctx, run.ID, store.RunCancelled, "cancelled", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	got, err := client.GetAgent(ctx, &loomcyclepb.GetAgentRequest{AgentId: "a_ended"})
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.GetAwaitedState() != "" {
		t.Errorf("awaited_state = %q on a cancelled run, want empty", got.GetAwaitedState())
	}
}

// A gRPC run read names the run that spawned it; a top-level run's is empty.
func TestGetAgent_CarriesTheParentRunID(t *testing.T) {
	client, _, st, cleanup := startTestServer(t, "")
	defer cleanup()
	ctx := context.Background()
	sess, _ := st.CreateSession(ctx, "t", "qa-agent", "alice")
	top, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_top", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_child", ParentAgentID: "a_top", ParentRunID: top.ID, UserID: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	child, err := client.GetAgent(ctx, &loomcyclepb.GetAgentRequest{AgentId: "a_child"})
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if child.GetParentRunId() != top.ID {
		t.Errorf("parent_run_id = %q, want %q", child.GetParentRunId(), top.ID)
	}
	parent, err := client.GetAgent(ctx, &loomcyclepb.GetAgentRequest{AgentId: "a_top"})
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if parent.GetParentRunId() != "" {
		t.Errorf("top-level run's parent_run_id = %q, want empty", parent.GetParentRunId())
	}
}
