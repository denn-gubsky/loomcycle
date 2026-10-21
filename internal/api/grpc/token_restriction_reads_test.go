package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedAgentRun files a run of agent for (acme, dev) in a session of its own,
// with a line of transcript. parentRunID makes it a sub-run of that run.
func seedAgentRun(t *testing.T, st store.Store, agent, agentID, parentRunID string) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "acme", agent, "dev")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID, UserID: "dev", TenantID: "acme", ParentRunID: parentRunID})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, run.ID, "text", []byte(`{"type":"text","text":"said by `+agent+`"}`)); err != nil {
		t.Fatal(err)
	}
	return run
}

// The gRPC reads a token limited to certain agents may call answer a run or
// session of another agent in its tenant with the NotFound an unknown one
// gets, and leave such runs out of their listings; its own agent's runs, and
// everything for the tenant's unrestricted token, read as before.
func TestGrpcReads_RestrictedTokenCannotReadAnotherAgentsRuns(t *testing.T) {
	adapter, _, st, _ := ownershipGRPC(t)
	other := seedAgentRun(t, st, "default", "a_other", "")
	own := seedAgentRun(t, st, "agentx", "a_own", "")
	spawned := seedAgentRun(t, st, "default", "a_spawned", own.ID)
	underOther := seedAgentRun(t, st, "agentx", "a_under_other", other.ID)

	restricted := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
		Restrictions: auth.Restrictions{Agents: []string{"agentx"}},
	})
	free := scopedCtx("acme", "dev", auth.ScopeRunsCreate, auth.ScopeRunsRead)

	reads := map[string]func(context.Context, store.Run) error{
		"GetAgent": func(ctx context.Context, r store.Run) error {
			_, err := adapter.GetAgent(ctx, &loomcyclepb.GetAgentRequest{AgentId: r.AgentID})
			return err
		},
		"GetRun": func(ctx context.Context, r store.Run) error {
			_, err := adapter.GetRun(ctx, &loomcyclepb.GetRunRequest{RunId: r.ID})
			return err
		},
		"GetTranscript": func(ctx context.Context, r store.Run) error {
			_, err := adapter.GetTranscript(ctx, &loomcyclepb.GetTranscriptRequest{SessionId: r.SessionID})
			return err
		},
		"ListWalkRuns": func(ctx context.Context, r store.Run) error {
			_, err := adapter.ListWalkRuns(ctx, &loomcyclepb.ListWalkRunsRequest{WalkId: r.ID})
			return err
		},
		"ListChildRuns": func(ctx context.Context, r store.Run) error {
			_, err := adapter.ListChildRuns(ctx, &loomcyclepb.ListChildRunsRequest{ParentRunId: r.ID})
			return err
		},
	}
	for name, read := range reads {
		for _, r := range []store.Run{other, underOther} {
			if err := read(restricted, r); status.Code(err) != codes.NotFound {
				t.Errorf("restricted %s of %s = %v, want NotFound", name, r.AgentID, err)
			}
			if err := read(free, r); err != nil {
				t.Errorf("unrestricted %s of %s = %v, want it read", name, r.AgentID, err)
			}
		}
		for _, r := range []store.Run{own, spawned} {
			if err := read(restricted, r); err != nil {
				t.Errorf("restricted %s of its own %s = %v, want it read", name, r.AgentID, err)
			}
		}
	}

	listed := func(ctx context.Context) map[string]bool {
		t.Helper()
		resp, err := adapter.ListUserAgents(ctx, &loomcyclepb.ListUserAgentsRequest{UserId: "dev"})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, a := range resp.GetAgents() {
			out[a.GetRunId()] = true
		}
		return out
	}
	if got := listed(restricted); len(got) != 2 || !got[own.ID] || !got[spawned.ID] {
		t.Errorf("restricted ListUserAgents = %v, want exactly its own run and the run it spawned", got)
	}
	if got := listed(free); len(got) != 4 {
		t.Errorf("unrestricted ListUserAgents = %v, want all four runs", got)
	}
}
