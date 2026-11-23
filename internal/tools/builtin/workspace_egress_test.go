package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// rowEgress is the egress recorded on the run's workspace of this name.
func (f *wsFixture) rowEgress(t *testing.T, ctx context.Context, name string) []string {
	t.Helper()
	r, err := f.st.WorkspaceGetByName(context.Background(), "acme", tools.RunID(ctx), name)
	if err != nil {
		t.Fatal(err)
	}
	return r.Egress
}

func polEgress(pol tools.WorkspacePolicyValue, routes ...string) tools.WorkspacePolicyValue {
	pol.Egress = routes
	return pol
}

// open with no egress gets every route of the run's grant (O4); with a list,
// that subset.
func TestWorkspaceEgress_OpenWithoutEgressGetsTheWholeGrant(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", polEgress(wsFull, "pypi", "pypi-files", "npm"))
	res := f.mustOK(t, ctx, `{"op":"open","name":"all"}`)
	if got := f.rowEgress(t, ctx, "all"); !reflect.DeepEqual(got, []string{"pypi", "pypi-files", "npm"}) {
		t.Errorf("open with no egress recorded %v, want the whole grant", got)
	}
	var out struct {
		Egress []string `json:"egress"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || len(out.Egress) != 3 {
		t.Errorf("open answered %s; want the routes named", res.Text)
	}
	f.mustOK(t, ctx, `{"op":"open","name":"some","egress":["npm","npm"]}`)
	if got := f.rowEgress(t, ctx, "some"); !reflect.DeepEqual(got, []string{"npm"}) {
		t.Errorf("open with egress [npm, npm] recorded %v, want [npm]", got)
	}
	f.mustOK(t, ctx, `{"op":"open","name":"none","egress":[]}`)
	if got := f.rowEgress(t, ctx, "none"); len(got) != 0 {
		t.Errorf("open with egress [] recorded %v, want none", got)
	}
}

// A route the run's grant does not hold is refused at open, and nothing is
// opened; so is a name no route could have.
func TestWorkspaceEgress_OpenWithARouteOutsideTheGrantIsRefused(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", polEgress(wsFull, "pypi"))
	if c := refusalClass(f.call(t, ctx, `{"op":"open","name":"w","egress":["pypi","crm"]}`)); c != "not_granted" {
		t.Errorf("a route outside the grant: class %q, want not_granted", c)
	}
	if c := refusalClass(f.call(t, ctx, `{"op":"open","name":"w","egress":["Bad/Name"]}`)); c != "invalid_argument" {
		t.Errorf("a malformed route name: class %q, want invalid_argument", c)
	}
	none := f.runCtx(t, "alice", "", wsFull)
	if c := refusalClass(f.call(t, none, `{"op":"open","name":"w","egress":["pypi"]}`)); c != "not_granted" {
		t.Errorf("a route with no egress grant: class %q, want not_granted", c)
	}
	if rows, _ := f.st.WorkspaceListByOwnerRun(context.Background(), "acme", tools.RunID(ctx)); len(rows) != 0 {
		t.Fatalf("a refused open left %d rows", len(rows))
	}
}

// A sub-agent's grant is its own narrowed by its parent's (PR 1's
// intersection): what it opens with no egress is that narrower set.
func TestWorkspaceEgress_ASubAgentsNarrowerGrantNarrowsItsOpen(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	parent := polEgress(wsFull, "pypi", "pypi-files")
	child := tools.NarrowWorkspace(parent, polEgress(wsFull, "pypi", "npm"))
	ctx := f.runCtx(t, "alice", "", child)
	f.mustOK(t, ctx, `{"op":"open","name":"w"}`)
	if got := f.rowEgress(t, ctx, "w"); !reflect.DeepEqual(got, []string{"pypi"}) {
		t.Errorf("the child's open recorded %v, want [pypi]", got)
	}
	if c := refusalClass(f.call(t, ctx, `{"op":"open","name":"x","egress":["npm"]}`)); c != "not_granted" {
		t.Errorf("a route only the child's own definition names: class %q, want not_granted", c)
	}
}

// list and status name a workspace's routes.
func TestWorkspaceEgress_ListAndStatusNameTheRoutes(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", polEgress(wsFull, "pypi", "npm"))
	f.mustOK(t, ctx, `{"op":"open","name":"w","egress":["npm"]}`)
	var list struct {
		Workspaces []struct {
			Egress []string `json:"egress"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(f.mustOK(t, ctx, `{"op":"list"}`).Text), &list); err != nil || len(list.Workspaces) != 1 ||
		!reflect.DeepEqual(list.Workspaces[0].Egress, []string{"npm"}) {
		t.Errorf("list = %+v, %v", list, err)
	}
	var st struct {
		Egress []string `json:"egress"`
	}
	if err := json.Unmarshal([]byte(f.mustOK(t, ctx, `{"op":"status","name":"w"}`).Text), &st); err != nil || !reflect.DeepEqual(st.Egress, []string{"npm"}) {
		t.Errorf("status = %+v, %v", st, err)
	}
}
