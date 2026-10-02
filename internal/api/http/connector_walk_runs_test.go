package http

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// connector.ListWalkRuns is what MCP list_runs answers a walk_id from. It goes
// through the HTTP listing's gate and carries a running row's awaited state.
func TestConnectorListWalkRuns_GatesLikeHTTPAndCarriesAwaitedState(t *testing.T) {
	srv, _ := runReadServer(t)
	walkID, finish := walkAs(t, srv, "acme", "alice", "triage")
	defer finish(builtin.WalkEnd{})
	var member string
	for id := range addWalkMembers(t, srv.store, "acme", "alice", walkID, 1) {
		member = id
	}
	if err := srv.store.AppendEvent(context.Background(), member, "tool_call",
		[]byte(`{"type":"tool_call","tool_use":{"id":"tu_1","name":"Channel","input":{"op":"subscribe","channel":"findings"}}}`)); err != nil {
		t.Fatal(err)
	}

	got, err := srv.ListWalkRuns(principalCtx("acme", "alice", auth.ScopeUser), walkID, 0, "")
	if err != nil {
		t.Fatalf("ListWalkRuns as the walk's owner: %v", err)
	}
	if len(got.Runs) != 2 || got.NextCursor != "" {
		t.Fatalf("listed %d runs (next %q), want the walk and its member", len(got.Runs), got.NextCursor)
	}
	for _, r := range got.Runs {
		if r.RunID == member && (r.AwaitedState != "channel" || r.AwaitedOn != "findings") {
			t.Errorf("member awaited = (%q, %q), want (channel, findings)", r.AwaitedState, r.AwaitedOn)
		}
	}

	for name, ctx := range map[string]context.Context{
		"another tenant":                principalCtx("other", "op", auth.ScopeTenant),
		"an isolated member not owning": principalCtx("acme", "bob", auth.ScopeUser),
	} {
		var nf *store.ErrNotFound
		if _, err := srv.ListWalkRuns(ctx, walkID, 0, ""); !errors.As(err, &nf) {
			t.Errorf("%s: err = %v, want not-found", name, err)
		}
	}
	if _, err := srv.ListWalkRuns(context.Background(), walkID, 0, "cur_0"); !errors.Is(err, store.ErrInvalidRunCursor) {
		t.Errorf("foreign cursor: err = %v, want ErrInvalidRunCursor", err)
	}
}
