package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC GW P6, §11 "Checkpoint and fork", through the tool, the dispatcher and
// a real worker.

func (f *wsFixture) row(t *testing.T, ctx context.Context, name string) store.WorkspaceRow {
	t.Helper()
	rows, err := f.st.WorkspaceListByOwnerRun(context.Background(), "acme", tools.RunID(ctx))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("the run holds no workspace %q", name)
	return store.WorkspaceRow{}
}

func checkpointID(t *testing.T, res tools.Result) string {
	t.Helper()
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || out.ID == "" {
		t.Fatalf("checkpoint answered %s", res.Text)
	}
	return out.ID
}

// A restore brings back a deleted file; a fork is a new workspace of the
// caller, on the same worker, whose changes the original never sees.
func TestWorkspace_CheckpointRestoreAndForkRoundTrip(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	owner := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, owner, `{"op":"open","name":"work"}`)
	f.mustOK(t, owner, `{"op":"write","name":"work","path":"a.txt","content":"v1"}`)
	ck := checkpointID(t, f.mustOK(t, owner, `{"op":"checkpoint","name":"work","label":"first"}`))
	f.mustOK(t, owner, `{"op":"exec","name":"work","command":"rm a.txt && echo new > b.txt"}`)
	if res := f.mustOK(t, owner, `{"op":"checkpoints","name":"work"}`); !strings.Contains(res.Text, ck) || !strings.Contains(res.Text, `"label":"first"`) {
		t.Errorf("checkpoints = %s", res.Text)
	}
	f.mustOK(t, owner, `{"op":"restore","name":"work","checkpoint":"`+ck+`"}`)
	if res := f.mustOK(t, owner, `{"op":"read","name":"work","path":"a.txt"}`); res.Text != "v1" {
		t.Errorf("after the restore a.txt = %q", res.Text)
	}
	if res := f.call(t, owner, `{"op":"read","name":"work","path":"b.txt"}`); !res.IsError {
		t.Errorf("a file made after the checkpoint survived the restore: %s", res.Text)
	}
	// The session is made again on the restored folder.
	if res := f.mustOK(t, owner, `{"op":"exec","name":"work","command":"cat a.txt"}`); !strings.Contains(res.Text, "v1") {
		t.Errorf("a command after the restore = %s", res.Text)
	}

	res := f.mustOK(t, owner, `{"op":"fork","name":"work","as":"try"}`)
	if !strings.Contains(res.Text, `"forked_from":"work"`) {
		t.Errorf("fork = %s", res.Text)
	}
	f.mustOK(t, owner, `{"op":"write","name":"try","path":"a.txt","content":"v2"}`)
	if res := f.mustOK(t, owner, `{"op":"read","name":"work","path":"a.txt"}`); res.Text != "v1" {
		t.Errorf("the original sees the fork's write: %q", res.Text)
	}
	orig, fork := f.row(t, owner, "work"), f.row(t, owner, "try")
	if fork.OwnerRunID != tools.RunID(owner) || fork.WorkerID != orig.WorkerID || fork.ID == orig.ID {
		t.Errorf("the fork's row = %+v, want the caller's own, on the original's worker", fork)
	}
	if len(fork.Shares) != 0 {
		t.Errorf("the fork carried shares: %v", fork.Shares)
	}
	// From a checkpoint: the files as they were then.
	f.mustOK(t, owner, `{"op":"write","name":"work","path":"a.txt","content":"v3"}`)
	f.mustOK(t, owner, `{"op":"fork","name":"work","as":"then","checkpoint":"`+ck+`"}`)
	if res := f.mustOK(t, owner, `{"op":"read","name":"then","path":"a.txt"}`); res.Text != "v1" {
		t.Errorf("the fork from the checkpoint holds %q", res.Text)
	}
	if res := f.mustOK(t, owner, `{"op":"checkpoints","name":"then"}`); !strings.Contains(res.Text, `"checkpoints":[]`) {
		t.Errorf("a fork carried its source's checkpoints: %s", res.Text)
	}
}

// A fork is the caller's: refused for a workspace of another run or user
// (not found, as for a name nobody uses), for a workspace shared with it,
// for a name it already holds, and past max_per_run.
func TestWorkspace_ForkIsPinnedToTheCallerAndBounded(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{MaxPerRun: 2})
	owner := f.runCtx(t, "alice", "", tools.WorkspacePolicyValue{Read: true, Write: true, Exec: true, Open: true, Share: true})
	f.mustOK(t, owner, `{"op":"open","name":"work"}`)
	f.mustOK(t, owner, `{"op":"write","name":"work","path":"a.txt","content":"mine"}`)
	sibling := f.runCtx(t, "alice", tools.RunIdentity(owner).RootRunID, wsFull)
	other := f.runCtx(t, "bob", "", wsFull)
	for who, ctx := range map[string]context.Context{"sibling": sibling, "another user": other} {
		if c := refusalClass(f.call(t, ctx, `{"op":"fork","name":"work","as":"stolen"}`)); c != "not_found" {
			t.Errorf("%s: fork of another run's workspace = %q, want not_found", who, c)
		}
		if c := refusalClass(f.call(t, ctx, `{"op":"checkpoint","name":"work"}`)); c != "not_found" {
			t.Errorf("%s: checkpoint of another run's workspace = %q, want not_found", who, c)
		}
	}
	// A shared workspace is reached by name@agent; no checkpoint op takes it.
	peer := f.runCtx(t, "alice", tools.RunIdentity(owner).RootRunID, wsFull)
	for _, in := range []string{
		`{"op":"fork","name":"work@a_alice","as":"mine"}`, `{"op":"checkpoint","name":"work@a_alice"}`,
		`{"op":"restore","name":"work@a_alice","checkpoint":"ck_0000000000000000"}`, `{"op":"checkpoints","name":"work@a_alice"}`,
	} {
		if c := refusalClass(f.call(t, peer, in)); c != "not_shared_op" {
			t.Errorf("peer: %s = %q, want not_shared_op", in, c)
		}
	}
	if c := refusalClass(f.call(t, owner, `{"op":"fork","name":"work","as":"work"}`)); c != "exists" {
		t.Errorf("a fork onto a name the run holds = %q, want exists", c)
	}
	f.mustOK(t, owner, `{"op":"fork","name":"work","as":"two"}`)
	if c := refusalClass(f.call(t, owner, `{"op":"fork","name":"work","as":"three"}`)); c != "limit" {
		t.Errorf("a fork past max_per_run = %q, want limit", c)
	}
	rows, _ := f.st.WorkspaceListByOwnerRun(context.Background(), "acme", tools.RunID(owner))
	if len(rows) != 2 {
		t.Errorf("the run holds %d workspaces, want 2", len(rows))
	}
	for _, ctx := range []context.Context{sibling, other} {
		if res := f.mustOK(t, ctx, `{"op":"list"}`); res.Text != `{"workspaces":[]}` {
			t.Errorf("another run lists %s", res.Text)
		}
	}
}

// The grants: checkpoint, restore and delete_checkpoint need write;
// checkpoints needs read; fork needs open.
func TestWorkspace_CheckpointOpsCheckTheirGrants(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	owner := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, owner, `{"op":"open","name":"work"}`)
	ck := checkpointID(t, f.mustOK(t, owner, `{"op":"checkpoint","name":"work"}`))
	readOnly := tools.WithRunPolicy(owner, tools.RunPolicy{Tools: []string{"Workspace"}, Workspace: tools.WorkspacePolicyValue{Read: true, Write: true}})
	noWrite := tools.WithRunPolicy(owner, tools.RunPolicy{Tools: []string{"Workspace"}, Workspace: tools.WorkspacePolicyValue{Read: true, Open: true}})
	for _, in := range []string{`{"op":"checkpoint","name":"work"}`, `{"op":"restore","name":"work","checkpoint":"` + ck + `"}`, `{"op":"delete_checkpoint","name":"work","checkpoint":"` + ck + `"}`} {
		if c := refusalClass(f.call(t, noWrite, in)); c != "not_granted" {
			t.Errorf("without write: %s = %q, want not_granted", in, c)
		}
	}
	if c := refusalClass(f.call(t, readOnly, `{"op":"fork","name":"work","as":"b"}`)); c != "not_granted" {
		t.Errorf("without open: fork = %q, want not_granted", c)
	}
	f.mustOK(t, noWrite, `{"op":"checkpoints","name":"work"}`)
	none := tools.WithRunPolicy(owner, tools.RunPolicy{Tools: []string{"Workspace"}, Workspace: tools.WorkspacePolicyValue{Write: true}})
	if c := refusalClass(f.call(t, none, `{"op":"checkpoints","name":"work"}`)); c != "not_granted" {
		t.Errorf("without read: checkpoints = %q, want not_granted", c)
	}
	f.mustOK(t, noWrite, `{"op":"fork","name":"work","as":"b"}`)
	// An agent that requires the container engine does not fork a workspace
	// on the host engine: the fork would run its commands there.
	strict := tools.WithRunPolicy(owner, tools.RunPolicy{Tools: []string{"Workspace"}, Workspace: tools.WorkspacePolicyValue{Read: true, Open: true, Engine: "container"}})
	if c := refusalClass(f.call(t, strict, `{"op":"fork","name":"work","as":"c"}`)); c != "engine" {
		t.Errorf("a fork onto a weaker engine than required = %q, want engine", c)
	}
}

// The checkpoint ops take only their own arguments, and the ones they need.
func TestWorkspace_CheckpointOpsRefuseForeignOrMissingArguments(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{})
	owner := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, owner, `{"op":"open","name":"work"}`)
	for _, in := range []string{
		`{"op":"checkpoint","name":"work","checkpoint":"x"}`,
		`{"op":"restore","name":"work"}`,
		`{"op":"delete_checkpoint","name":"work"}`,
		`{"op":"fork","name":"work"}`,
		`{"op":"fork","name":"work","as":"../x"}`,
		`{"op":"checkpoints","name":"work","label":"x"}`,
	} {
		if c := refusalClass(f.call(t, owner, in)); c != "invalid_argument" {
			t.Errorf("%s = %q, want invalid_argument", in, c)
		}
	}
	if c := refusalClass(f.call(t, owner, `{"op":"restore","name":"work","checkpoint":"ck_0000000000000000"}`)); c != "checkpoint_not_found" {
		t.Errorf("restore of an unknown checkpoint = %q", c)
	}
}

// Checkpoints count against the workspace's disk quota through the whole
// path: a second checkpoint that would pass it is refused, and deleting one
// makes room.
func TestWorkspace_CheckpointsCountAgainstTheDiskQuota(t *testing.T) {
	f := newWSFixture(t, lifecycle.Limits{DiskQuotaBytes: 1000})
	owner := f.runCtx(t, "alice", "", wsFull)
	f.mustOK(t, owner, `{"op":"open","name":"work"}`)
	f.mustOK(t, owner, `{"op":"write","name":"work","path":"a","content":"`+strings.Repeat("x", 400)+`"}`)
	ck := checkpointID(t, f.mustOK(t, owner, `{"op":"checkpoint","name":"work"}`))
	if c := refusalClass(f.call(t, owner, `{"op":"checkpoint","name":"work"}`)); c != "workspace_over_quota" {
		t.Errorf("a second checkpoint past the quota = %q", c)
	}
	f.mustOK(t, owner, `{"op":"delete_checkpoint","name":"work","checkpoint":"`+ck+`"}`)
	f.mustOK(t, owner, `{"op":"checkpoint","name":"work"}`)
	// A fork is a workspace with the operator's quota: a source over it is
	// refused and leaves no row.
	f.mustOK(t, owner, `{"op":"exec","name":"work","command":"head -c 1200 /dev/zero > big"}`)
	if c := refusalClass(f.call(t, owner, `{"op":"fork","name":"work","as":"b"}`)); c != "workspace_over_quota" {
		t.Errorf("a fork over a new workspace's quota = %q", c)
	}
	rows, _ := f.st.WorkspaceListByOwnerRun(context.Background(), "acme", tools.RunID(owner))
	if len(rows) != 1 {
		t.Errorf("a refused fork left %d rows", len(rows))
	}
	ents, _ := os.ReadDir(f.h.Root)
	for _, e := range ents {
		if e.IsDir() && e.Name() != rows[0].ID && !strings.HasPrefix(e.Name(), ".") {
			t.Errorf("a refused fork left the folder %s", filepath.Join(f.h.Root, e.Name()))
		}
	}
}
