package grpc

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func agentIDsOf(agents []*loomcyclepb.Agent) []string {
	out := make([]string, len(agents))
	for i, a := range agents {
		out[i] = a.GetAgentId()
	}
	slices.Sort(out)
	return out
}

// ListUserAgents' repeated statuses list a run in any of them, and
// awaited_states lists only the running runs blocked on one of the named waits
// — the semantics of HTTP's ?status=a,b and ?awaited_state=. The single status
// field reads as it always has.
func TestListUserAgents_StatusesAndAwaitedStatesFilter(t *testing.T) {
	client, _, st, cleanup := startTestServer(t, "")
	defer cleanup()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "", "default", "alice")
	if err != nil {
		t.Fatal(err)
	}
	seed := func(agentID string, final store.RunStatus, events ...[2]string) {
		t.Helper()
		r, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID, UserID: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if err := st.AppendEvent(ctx, r.ID, e[0], []byte(e[1])); err != nil {
				t.Fatal(err)
			}
		}
		if final != store.RunRunning {
			if err := st.FinishRun(ctx, r.ID, final, "end_turn", store.Usage{}, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	review := [2]string{"awaiting_review", `{"type":"awaiting_review","awaiting_review":{"round":1}}`}
	input := [2]string{"awaiting_input", `{"type":"awaiting_input","awaiting_input":{"since_turn":1}}`}
	seed("a_review", store.RunRunning, review)
	seed("a_input", store.RunRunning, input)
	seed("a_plain", store.RunRunning)
	seed("a_completed", store.RunCompleted, review)
	seed("a_failed", store.RunFailed)

	for _, tc := range []struct {
		name string
		req  *loomcyclepb.ListUserAgentsRequest
		want []string
	}{
		{"statuses_union", &loomcyclepb.ListUserAgentsRequest{Statuses: []string{"completed", "failed"}}, []string{"a_completed", "a_failed"}},
		{"statuses_all", &loomcyclepb.ListUserAgentsRequest{Statuses: []string{"all"}}, []string{"a_completed", "a_failed", "a_input", "a_plain", "a_review"}},
		{"legacy_status", &loomcyclepb.ListUserAgentsRequest{Status: "failed"}, []string{"a_failed"}},
		{"no_filter_is_every_status", &loomcyclepb.ListUserAgentsRequest{}, []string{"a_completed", "a_failed", "a_input", "a_plain", "a_review"}},
		{"awaited_review", &loomcyclepb.ListUserAgentsRequest{AwaitedStates: []string{"review"}}, []string{"a_review"}},
		{"awaited_review_input_running", &loomcyclepb.ListUserAgentsRequest{AwaitedStates: []string{"review", "input"}, Statuses: []string{"running"}}, []string{"a_input", "a_review"}},
		{"awaited_with_legacy_running", &loomcyclepb.ListUserAgentsRequest{AwaitedStates: []string{"input"}, Status: "running"}, []string{"a_input"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.UserId = "alice"
			resp, err := client.ListUserAgents(ctx, tc.req)
			if err != nil {
				t.Fatalf("ListUserAgents: %v", err)
			}
			if got := agentIDsOf(resp.GetAgents()); !slices.Equal(got, tc.want) {
				t.Errorf("listed %v, want %v", got, tc.want)
			}
			if resp.GetTruncated() {
				t.Error("a listing that read every run reported truncated")
			}
			for _, a := range resp.GetAgents() {
				if len(tc.req.AwaitedStates) > 0 && !slices.Contains(tc.req.AwaitedStates, a.GetAwaitedState()) {
					t.Errorf("%s listed with awaited_state %q", a.GetAgentId(), a.GetAwaitedState())
				}
			}
		})
	}
}

// An unknown status or awaited state, a non-running status with an awaited
// state, and both status fields at once are InvalidArgument.
func TestListUserAgents_RefusesUnknownOrConflictingFilters(t *testing.T) {
	client, _, _, cleanup := startTestServer(t, "")
	defer cleanup()
	for _, req := range []*loomcyclepb.ListUserAgentsRequest{
		{Statuses: []string{"runing"}},
		{Statuses: []string{"all", "running"}},
		{AwaitedStates: []string{"reviw"}},
		{AwaitedStates: []string{"review"}, Statuses: []string{"completed"}},
		{AwaitedStates: []string{"review"}, Statuses: []string{"all"}},
		{AwaitedStates: []string{"review"}, Status: "failed"},
		{Status: "running", Statuses: []string{"running"}},
	} {
		req.UserId = "alice"
		if _, err := client.ListUserAgents(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%v: err = %v, want InvalidArgument", req, err)
		}
	}
}
