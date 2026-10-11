package grpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedChildren writes a run and n children of it under (tenant, user), plus a
// grandchild; it returns the parent id and the children's ids.
func seedChildren(t *testing.T, st store.Store, tenant, user string, n int) (string, map[string]bool) {
	t.Helper()
	ctx := context.Background()
	parent := seedRunID(t, st, tenant, user, "lead")
	sess, err := st.CreateSession(ctx, tenant, "writer", user)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	last := ""
	for i := 0; i < n; i++ {
		r, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: fmt.Sprintf("c_%d", i), ParentRunID: parent, UserID: user, TenantID: tenant})
		if err != nil {
			t.Fatal(err)
		}
		ids[r.ID], last = true, r.ID
	}
	if _, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "grandchild", ParentRunID: last, UserID: user, TenantID: tenant}); err != nil {
		t.Fatal(err)
	}
	return parent, ids
}

func TestGrpcListChildRuns_PagesEveryChildOnce(t *testing.T) {
	adapter, st := tenantTestServer(t)
	parent, want := seedChildren(t, st, "acme", "alice", 250)
	op := scopedCtx("acme", "op", auth.ScopeTenant)

	seen := map[string]int{}
	cursor, pages := "", 0
	for {
		resp, err := adapter.ListChildRuns(op, &loomcyclepb.ListChildRunsRequest{ParentRunId: parent, Limit: 100, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, a := range resp.GetAgents() {
			seen[a.GetRunId()]++
			if a.GetParentRunId() != parent {
				t.Errorf("listed %s, whose parent run is %q", a.GetRunId(), a.GetParentRunId())
			}
		}
		if resp.GetNextCursor() == "" || pages > 10 {
			break
		}
		cursor = resp.GetNextCursor()
	}
	if pages != 3 || len(seen) != len(want) {
		t.Errorf("%d pages, %d distinct runs; want 3 pages and the %d children", pages, len(seen), len(want))
	}
	for id := range want {
		if seen[id] != 1 {
			t.Errorf("child %s listed %d times, want once", id, seen[id])
		}
	}
}

func TestGrpcListChildRuns_AParentTheCallerCannotReadIsNotFound(t *testing.T) {
	adapter, st := tenantTestServer(t)
	parent, _ := seedChildren(t, st, "acme", "alice", 2)
	for name, ctx := range map[string]context.Context{
		"another tenant":                        scopedCtx("evil", "mallory", auth.ScopeTenant),
		"an isolated member not owning the run": scopedCtx("acme", "bob", auth.ScopeUser),
	} {
		if _, err := adapter.ListChildRuns(ctx, &loomcyclepb.ListChildRunsRequest{ParentRunId: parent}); status.Code(err) != codes.NotFound {
			t.Errorf("%s: code=%s, want NotFound", name, status.Code(err))
		}
	}
	resp, err := adapter.ListChildRuns(scopedCtx("acme", "alice", auth.ScopeUser), &loomcyclepb.ListChildRunsRequest{ParentRunId: parent})
	if err != nil || len(resp.GetAgents()) != 2 {
		t.Errorf("the owning isolated member: %d runs, err %v; want 2", len(resp.GetAgents()), err)
	}
}

func TestGrpcListChildRuns_RefusesMalformedArguments(t *testing.T) {
	adapter, _ := tenantTestServer(t)
	for name, req := range map[string]*loomcyclepb.ListChildRunsRequest{
		"malformed parent_run_id": {ParentRunId: "team:triage"},
		"limit over cap":          {ParentRunId: "r_1", Limit: 1001},
		"foreign cursor":          {ParentRunId: "r_1", Cursor: "cur_0"},
	} {
		if _, err := adapter.ListChildRuns(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: code=%s, want InvalidArgument", name, status.Code(err))
		}
	}
	if _, err := adapter.ListChildRuns(context.Background(), &loomcyclepb.ListChildRunsRequest{ParentRunId: "r_nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown run: code=%s, want NotFound", status.Code(err))
	}
}

func TestRequiredScopeForRPC_ListChildRunsIsARunsRead(t *testing.T) {
	if got := requiredScopeForRPC(grpcMethodPrefix + "ListChildRuns"); got != auth.ScopeRunsRead {
		t.Errorf("ListChildRuns scope = %q, want %q (an unlisted RPC defaults to admin)", got, auth.ScopeRunsRead)
	}
}

// The stream's parent_run_id must reach the connector: an unforwarded filter
// does not error, it streams every run, which looks like a busy parent.
func TestGrpcStreamUserRunStates_ForwardsTheParentRunFilter(t *testing.T) {
	mc := &n8nMock{}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()
	stream, err := client.StreamUserRunStates(context.Background(), &loomcyclepb.StreamUserRunStatesRequest{
		UserId: "user-a", ParentRunId: "r_parent_1",
	})
	if err != nil {
		t.Fatalf("StreamUserRunStates: %v", err)
	}
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("Recv: %v", err)
		}
	}
	if mc.lastStreamReq.ParentRunID != "r_parent_1" {
		t.Errorf("connector received ParentRunID %q, want r_parent_1", mc.lastStreamReq.ParentRunID)
	}
}
