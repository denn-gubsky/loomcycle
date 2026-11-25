package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC GW §5.3, §7: expose and unexpose through the Workspace tool.

var wsExposer = tools.WorkspacePolicyValue{Read: true, Write: true, Exec: true, Open: true, Expose: true}

func exposeFixture(t *testing.T, enabled bool) *wsFixture {
	t.Helper()
	f := newWSFixture(t, lifecycle.Limits{})
	f.tool.Manager.SetExpose(lifecycle.ExposeLimits{Enabled: enabled, MaxPerWorkspace: 2})
	return f
}

// expose returns the page's id and base_path; a second expose of the port is
// refused, a third port past the bound too, and unexpose closes it.
func TestWorkspaceExpose_PublishesAndWithdrawsAPort(t *testing.T) {
	f := exposeFixture(t, true)
	ctx := f.runCtx(t, "alice", "", wsExposer)
	f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	res := f.mustOK(t, ctx, `{"op":"expose","name":"web","port":8000,"title":"Chart","kind":"panel"}`)
	var out struct {
		ID       string `json:"exposure_id"`
		BasePath string `json:"base_path"`
		Kind     string `json:"kind"`
		Title    string `json:"title"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || len(out.ID) != 24 ||
		out.BasePath != "/v1/workspaces/exposures/"+out.ID+"/" || out.Kind != "panel" || out.Title != "Chart" {
		t.Fatalf("expose = %s (%v)", res.Text, err)
	}
	if c := refusalClass(f.call(t, ctx, `{"op":"expose","name":"web","port":8000}`)); c != "exists" {
		t.Errorf("a port exposed twice = %q, want exists", c)
	}
	f.mustOK(t, ctx, `{"op":"expose","name":"web","port":8001}`)
	if c := refusalClass(f.call(t, ctx, `{"op":"expose","name":"web","port":8002}`)); c != "limit" {
		t.Errorf("a third port past max 2 = %q, want limit", c)
	}
	if res := f.mustOK(t, ctx, `{"op":"unexpose","name":"web","port":8000}`); !strings.Contains(res.Text, `"closed":true`) {
		t.Errorf("unexpose = %s", res.Text)
	}
	if res := f.mustOK(t, ctx, `{"op":"unexpose","name":"web","port":8000}`); !strings.Contains(res.Text, `"closed":false`) {
		t.Errorf("unexpose again = %s", res.Text)
	}
	if _, _, err := f.tool.Manager.OpenExposure(context.Background(), "acme", "alice", out.ID); err == nil {
		t.Error("the withdrawn exposure still opens")
	}
}

// expose needs its own grant (exec alone is not enough), the operator's
// switch, and the pin; a share never carries it.
func TestWorkspaceExpose_NeedsTheGrantTheSwitchAndThePin(t *testing.T) {
	f := exposeFixture(t, true)
	owner := f.runCtx(t, "alice", "", wsExposer)
	f.mustOK(t, owner, `{"op":"open","name":"web"}`)
	noExpose := tools.WithRunPolicy(owner, tools.RunPolicy{Tools: []string{"Workspace"}, Workspace: wsFull})
	for _, in := range []string{`{"op":"expose","name":"web","port":8000}`, `{"op":"unexpose","name":"web","port":8000}`} {
		if c := refusalClass(f.call(t, noExpose, in)); c != "not_granted" {
			t.Errorf("%s without the expose grant = %q, want not_granted", in, c)
		}
	}
	other := f.runCtx(t, "bob", "", wsExposer)
	if c := refusalClass(f.call(t, other, `{"op":"expose","name":"web","port":8000}`)); c != "not_found" {
		t.Errorf("another run = %q, want not_found", c)
	}
	if res := f.call(t, owner, `{"op":"expose","name":"web@a_alice","port":8000}`); !res.IsError {
		t.Errorf("expose on a shared name = %s, want refused", res.Text)
	}
	off := exposeFixture(t, false)
	ctx := off.runCtx(t, "alice", "", wsExposer)
	off.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	if c := refusalClass(off.call(t, ctx, `{"op":"expose","name":"web","port":8000}`)); c != "expose_off" {
		t.Errorf("with workspace.expose off = %q, want expose_off", c)
	}
}

// A narrowed child holds expose only when both it and its parent do.
func TestWorkspaceExpose_NarrowsAtSpawn(t *testing.T) {
	if tools.NarrowWorkspace(wsExposer, wsFull).Expose || tools.NarrowWorkspace(wsFull, wsExposer).Expose {
		t.Error("a child holds expose that one side lacks")
	}
	if !tools.NarrowWorkspace(wsExposer, wsExposer).Expose {
		t.Error("a child of two expose grants lost it")
	}
}

// Bad arguments are refused before anything is written.
func TestWorkspaceExpose_StrictArguments(t *testing.T) {
	f := exposeFixture(t, true)
	ctx := f.runCtx(t, "alice", "", wsExposer)
	f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	for _, in := range []string{
		`{"op":"expose","name":"web"}`,
		`{"op":"expose","name":"web","port":0}`,
		`{"op":"expose","name":"web","port":8000,"kind":"popup"}`,
		`{"op":"expose","name":"web","port":8000,"title":"a\nb"}`,
		`{"op":"expose","name":"web","port":8000,"command":"x"}`,
		`{"op":"unexpose","name":"web","port":8000,"title":"x"}`,
	} {
		if c := refusalClass(f.call(t, ctx, in)); c != "invalid_argument" {
			t.Errorf("%s = %q, want invalid_argument", in, c)
		}
	}
}
