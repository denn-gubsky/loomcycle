package grpc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedWalk writes a walk's own run and n members under (tenant, user); it
// returns the walk id and every run id listed for it.
func seedWalk(t *testing.T, st store.Store, tenant, user string, n int) (string, map[string]bool) {
	t.Helper()
	ctx := context.Background()
	walkID := seedRunID(t, st, tenant, user, "team:triage")
	sess, err := st.CreateSession(ctx, tenant, "writer", user)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{walkID: true}
	for i := 0; i < n; i++ {
		r, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{
			AgentID: fmt.Sprintf("m_%d", i), UserID: user, TenantID: tenant,
			ParentContext: &store.ParentContext{WalkID: walkID, State: "a", StateVisit: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[r.ID] = true
	}
	return walkID, ids
}

func TestGrpcListWalkRuns_PagesEveryRunOfTheWalkOnce(t *testing.T) {
	adapter, st := tenantTestServer(t)
	walkID, want := seedWalk(t, st, "acme", "alice", 250)
	op := scopedCtx("acme", "op", auth.ScopeTenant)

	seen := map[string]int{}
	cursor, pages := "", 0
	for {
		resp, err := adapter.ListWalkRuns(op, &loomcyclepb.ListWalkRunsRequest{WalkId: walkID, Limit: 100, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, a := range resp.GetAgents() {
			seen[a.GetRunId()]++
		}
		if resp.GetNextCursor() == "" {
			break
		}
		if pages > 10 {
			t.Fatal("the listing never ended")
		}
		cursor = resp.GetNextCursor()
	}
	if pages != 3 || len(seen) != len(want) {
		t.Errorf("%d pages, %d distinct runs; want 3 pages and %d runs", pages, len(seen), len(want))
	}
	for id := range want {
		if seen[id] != 1 {
			t.Errorf("run %s listed %d times, want once", id, seen[id])
		}
	}
}

func TestGrpcListWalkRuns_AWalkTheCallerCannotReadIsNotFound(t *testing.T) {
	adapter, st := tenantTestServer(t)
	walkID, _ := seedWalk(t, st, "acme", "alice", 2)
	for name, ctx := range map[string]context.Context{
		"another tenant":                         scopedCtx("evil", "mallory", auth.ScopeTenant),
		"an isolated member not owning the walk": scopedCtx("acme", "bob", auth.ScopeUser),
	} {
		if _, err := adapter.ListWalkRuns(ctx, &loomcyclepb.ListWalkRunsRequest{WalkId: walkID}); status.Code(err) != codes.NotFound {
			t.Errorf("%s: code=%s, want NotFound", name, status.Code(err))
		}
	}
	resp, err := adapter.ListWalkRuns(scopedCtx("acme", "alice", auth.ScopeUser), &loomcyclepb.ListWalkRunsRequest{WalkId: walkID})
	if err != nil || len(resp.GetAgents()) != 3 {
		t.Errorf("the owning isolated member: %d runs, err %v; want 3", len(resp.GetAgents()), err)
	}
}

func TestGrpcListWalkRuns_RefusesMalformedArguments(t *testing.T) {
	adapter, _ := tenantTestServer(t)
	for name, req := range map[string]*loomcyclepb.ListWalkRunsRequest{
		"malformed walk_id": {WalkId: "team:triage"},
		"limit over cap":    {WalkId: "r_1", Limit: 1001},
		"foreign cursor":    {WalkId: "r_1", Cursor: "cur_0"},
	} {
		if _, err := adapter.ListWalkRuns(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: code=%s, want InvalidArgument", name, status.Code(err))
		}
	}
	if _, err := adapter.ListWalkRuns(context.Background(), &loomcyclepb.ListWalkRunsRequest{WalkId: "r_nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown walk: code=%s, want NotFound", status.Code(err))
	}
}

func TestRequiredScopeForRPC_ListWalkRunsIsARunsRead(t *testing.T) {
	if got := requiredScopeForRPC(grpcMethodPrefix + "ListWalkRuns"); got != auth.ScopeRunsRead {
		t.Errorf("ListWalkRuns scope = %q, want %q (an unlisted RPC defaults to admin)", got, auth.ScopeRunsRead)
	}
}

// The walk's own row carries its result — the end state it reached — as on
// HTTP; GetRun reads the same; member rows stay without one.
func TestGrpcListWalkRuns_WalksOwnRowCarriesTheEndStateReached(t *testing.T) {
	adapter, st := tenantTestServer(t)
	walkID, _ := seedWalk(t, st, "acme", "alice", 1)
	if err := st.FinishRun(context.Background(), walkID, store.RunCompleted, "",
		store.Usage{Result: []byte(`{"final_text":"no","terminal":"abandoned"}`)}, ""); err != nil {
		t.Fatal(err)
	}
	op := scopedCtx("acme", "op", auth.ScopeTenant)

	resp, err := adapter.ListWalkRuns(op, &loomcyclepb.ListWalkRunsRequest{WalkId: walkID})
	if err != nil {
		t.Fatalf("ListWalkRuns: %v", err)
	}
	if len(resp.GetAgents()) != 2 {
		t.Fatalf("listed %d runs, want the walk and its member", len(resp.GetAgents()))
	}
	for _, a := range resp.GetAgents() {
		if a.GetRunId() == walkID {
			if !strings.Contains(string(a.GetResult()), `"terminal":"abandoned"`) {
				t.Errorf("the walk's own row result = %s, want its terminal", a.GetResult())
			}
		} else if len(a.GetResult()) != 0 {
			t.Errorf("member row %s carries a result %s, want none", a.GetRunId(), a.GetResult())
		}
	}
	got, err := adapter.GetRun(op, &loomcyclepb.GetRunRequest{RunId: walkID})
	if err != nil || !strings.Contains(string(got.GetResult()), `"terminal":"abandoned"`) {
		t.Errorf("GetRun result = %s (%v), want its terminal", got.GetResult(), err)
	}
}
