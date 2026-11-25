package builtin

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func spawnWSCtx(pol tools.WorkspacePolicyValue) context.Context {
	ctx := tools.WithRunPolicy(context.Background(), tools.RunPolicy{Workspace: pol})
	return tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "acme", UserID: "bob"})
}

// A fork entry {fork, as?} (RFC GW §5.4) is carried to the sub-run with its
// name in the child, defaulting to the source's; it needs only the grant to
// open workspaces, not the one to share (D-P6-15, D-P6-19).
func TestSpawnWorkspaces_AForkNeedsOpenNotShareAndDefaultsItsName(t *testing.T) {
	ctx := spawnWSCtx(tools.WorkspacePolicyValue{Read: true, Open: true})
	got, res, ok := spawnWorkspaces(ctx, "workspaces", []spawnWorkspace{{Fork: "work"}, {Fork: "work", As: "work-b"}})
	if !ok {
		t.Fatalf("refused: %s", res.Text)
	}
	want := []tools.SpawnWorkspace{{Name: "work", Fork: true, As: "work"}, {Name: "work", Fork: true, As: "work-b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forks = %+v, want %+v", got, want)
	}
}

// Every malformed or ungranted fork entry is refused before any child is
// made, naming what is wrong.
func TestSpawnWorkspaces_RefusesABadOrUngrantedFork(t *testing.T) {
	open := spawnWSCtx(tools.WorkspacePolicyValue{Read: true, Write: true, Open: true, Share: true})
	noOpen := spawnWSCtx(tools.WorkspacePolicyValue{Read: true, Write: true, Share: true})
	for _, tc := range []struct {
		why  string
		ctx  context.Context
		in   []spawnWorkspace
		want string
	}{
		{"share and fork in one entry", open, []spawnWorkspace{{Name: "work", Access: "read", Fork: "work"}}, "not both"},
		{"as without fork", open, []spawnWorkspace{{As: "copy"}}, "fork names the workspace"},
		{"a shared name", open, []spawnWorkspace{{Fork: "work@ag_other"}}, "is not one of your workspaces"},
		{"a bad as", open, []spawnWorkspace{{Fork: "work", As: "../x"}}, "as \"../x\""},
		{"two forks of one name", open, []spawnWorkspace{{Fork: "work"}, {Fork: "other", As: "work"}}, "already gets a fork"},
		{"no open grant", noOpen, []spawnWorkspace{{Fork: "work"}}, "workspace.open"},
	} {
		_, res, ok := spawnWorkspaces(tc.ctx, "workspaces", tc.in)
		if ok || !res.IsError || !strings.Contains(res.Text, tc.want) {
			t.Errorf("%s: ok=%v %s, want a refusal naming %q", tc.why, ok, res.Text, tc.want)
		}
	}
	// A share beside a fork still needs the share grant.
	onlyOpen := spawnWSCtx(tools.WorkspacePolicyValue{Read: true, Open: true})
	if _, res, ok := spawnWorkspaces(onlyOpen, "workspaces", []spawnWorkspace{{Fork: "work"}, {Name: "work", Access: "read"}}); ok || !strings.Contains(res.Text, "workspace.share") {
		t.Errorf("a share without the share grant beside a fork = ok %v %s", ok, res.Text)
	}
}
