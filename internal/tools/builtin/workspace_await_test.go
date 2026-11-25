package builtin

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC GW §5.3, P9 plan §3.4, D-P9-66: Workspace op=await returns the next
// update the workspace's application posted to LOOMCYCLE_UPDATE_URL, once,
// as untrusted output; never the address.

const updatePoster = `import os, urllib.request, sys, time
u = os.environ["` + workspace.UpdateURLEnv + `"]
for m in sys.argv[1:]:
    urllib.request.urlopen(urllib.request.Request(u, data=m.encode(), method="POST"))
print("posted", flush=True)
time.sleep(600)
`

func TestWorkspaceAwait_ReturnsWhatTheApplicationPostedOnce(t *testing.T) {
	needProgram(t, "python3")
	f := newWSFixture(t, lifecycle.Limits{})
	ctx := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, ctx, `{"op":"open","name":"web"}`)
	src, _ := json.Marshal(updatePoster)
	f.mustOK(t, ctx, fmt.Sprintf(`{"op":"write","name":"web","path":"poster.py","content":%s}`, src))
	start := f.mustOK(t, ctx, `{"op":"start","name":"web","command":"python3 poster.py '{\"clicked\":1}' second"}`)
	if strings.Contains(start.Text, workspace.UpdateURLEnv) || strings.Contains(start.Text, "127.0.0.1") {
		t.Errorf("start answers the update address: %s", start.Text)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(f.mustOK(t, ctx, `{"op":"await","name":"web","timeout_s":15}`).Text), &first); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range first {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if first["update"] != `{"clicked":1}` || strings.Join(keys, ",") != "name,pending,update" {
		t.Errorf("first await = %v", first)
	}
	if second := f.mustOK(t, ctx, `{"op":"await","name":"web","timeout_s":15}`); !strings.Contains(second.Text, `"update":"second"`) {
		t.Errorf("second await = %s", second.Text)
	}
	if third := f.mustOK(t, ctx, `{"op":"await","name":"web","timeout_s":1}`); !strings.Contains(third.Text, `"timed_out":true`) {
		t.Errorf("await with nothing posted = %s", third.Text)
	}
}

// await is a server's (exec), the workspace's owner's alone: not through a
// share, not over the MCP server, not another run's; its timeout is bounded.
func TestWorkspaceAwait_NeedsExecAndThePin(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	owner := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, owner, `{"op":"open","name":"web"}`)
	in := `{"op":"await","name":"web","timeout_s":1}`
	readWrite := tools.WithRunPolicy(owner, tools.RunPolicy{Tools: []string{"Workspace"}, Workspace: tools.WorkspacePolicyValue{Read: true, Write: true, Open: true}})
	if c := refusalClass(f.call(t, readWrite, in)); c != "not_granted" {
		t.Errorf("await without exec = %q, want not_granted", c)
	}
	other := f.runCtx(t, "alice", "", wsFull)
	if c := refusalClass(f.call(t, other, in)); c != "not_found" {
		t.Errorf("await on another run's workspace = %q, want not_found", c)
	}
	for _, bad := range []string{`{"op":"await","name":"web","timeout_s":-1}`, fmt.Sprintf(`{"op":"await","name":"web","timeout_s":%d}`, workspace.MaxAwaitSeconds+1)} {
		if c := refusalClass(f.call(t, owner, bad)); c != "invalid_argument" {
			t.Errorf("%s = %q, want invalid_argument", bad, c)
		}
	}
	if slicesContains(WorkspaceMCPOps, "await") {
		t.Error("await is offered over the MCP server")
	}
	sf := newShareFixture(t, wsFull)
	sf.ws.mustOK(t, sf.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read_write"}`)
	if c := refusalClass(sf.wsCall(t, sf.peer, `{"op":"await","name":"w@a_alice","timeout_s":1}`)); c != "not_shared_op" {
		t.Errorf("await through a share = %q, want not_shared_op", c)
	}
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
