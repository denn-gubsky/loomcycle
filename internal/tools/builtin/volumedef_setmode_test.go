package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// fileCall runs one file tool against the named volume and returns its result.
func fileCall(t *testing.T, ctx context.Context, tool tools.Tool, args map[string]any) tools.Result {
	t.Helper()
	in, _ := json.Marshal(args)
	res, err := tool.Execute(ctx, in)
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return res
}

// set_mode takes write access from every run that shares the volume: the
// caller, and a run that never called it. Reads go on working.
func TestVolumeDefTool_SetModeMakesTheVolumeReadOnlyForTheWholeRunTree(t *testing.T) {
	tool, base, root, done := volumeDefFixture(t)
	defer done()
	parent, set := ephemeralCtx(base, "run-1")
	// A child shares the set and has its own, different, identity.
	child := tools.WithEphemeralVolumes(
		tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a_child", RootRunID: "run-1"}), set)

	if _, res := vdExec(t, tool, parent, `{"op":"create","name":"repo","ephemeral":true}`); res.IsError {
		t.Fatal(res.Text)
	}
	write := map[string]any{"volume": "repo", "path": "a.txt", "content": "one\n"}
	if res := fileCall(t, parent, &Write{}, write); res.IsError {
		t.Fatalf("write before set_mode: %s", res.Text)
	}

	out, res := vdExec(t, tool, child, `{"op":"set_mode","name":"repo","mode":"ro"}`)
	if res.IsError {
		t.Fatalf("set_mode: %s", res.Text)
	}
	if out["mode"] != "ro" || out["ephemeral"] != true {
		t.Errorf("set_mode answer = %v", out)
	}

	for who, ctx := range map[string]context.Context{"the caller": child, "another run of the tree": parent} {
		write["content"] = "two\n"
		if res := fileCall(t, ctx, &Write{}, write); !res.IsError || !strings.Contains(res.Text, "read-only") {
			t.Errorf("%s can still write: error=%v %q", who, res.IsError, res.Text)
		}
		edit := map[string]any{"volume": "repo", "path": "a.txt", "old_string": "one", "new_string": "two"}
		if res := fileCall(t, ctx, &Edit{}, edit); !res.IsError || !strings.Contains(res.Text, "read-only") {
			t.Errorf("%s can still edit: error=%v %q", who, res.IsError, res.Text)
		}
		if res := fileCall(t, ctx, &Read{}, map[string]any{"volume": "repo", "path": "a.txt"}); res.IsError || res.Text != "one\n" {
			t.Errorf("%s cannot read, or the file changed: error=%v %q", who, res.IsError, res.Text)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "_ephemeral", "run-1", "repo", "a.txt")); string(b) != "one\n" {
		t.Errorf("the file on disk changed after set_mode: %q", b)
	}

	// The row a resumed run rebuilds from says the same.
	rows, err := tool.Store.EphemeralVolumeListByRun(context.Background(), "run-1")
	if err != nil || len(rows) != 1 || !strings.Contains(string(rows[0].Definition), `"ro"`) {
		t.Errorf("stored row = %+v err=%v, want mode ro", rows, err)
	}
	// Calling it again is not an error.
	if _, res := vdExec(t, tool, parent, `{"op":"set_mode","name":"repo","mode":"ro"}`); res.IsError {
		t.Errorf("a second set_mode ro: %s", res.Text)
	}
}

// There is no way back, from set_mode or from create.
func TestVolumeDefTool_SetModeNeverMakesAVolumeWritable(t *testing.T) {
	tool, base, _, done := volumeDefFixture(t)
	defer done()
	ctx, set := ephemeralCtx(base, "run-1")
	if _, res := vdExec(t, tool, ctx, `{"op":"create","name":"repo","mode":"ro","ephemeral":true}`); res.IsError {
		t.Fatal(res.Text)
	}
	for _, call := range []string{
		`{"op":"set_mode","name":"repo","mode":"rw"}`,
		`{"op":"set_mode","name":"repo"}`,
		`{"op":"create","name":"repo","mode":"rw","ephemeral":true}`,
	} {
		if _, res := vdExec(t, tool, ctx, call); !res.IsError {
			t.Errorf("%s was accepted", call)
		}
		if ref, _ := set.Get("repo"); !ref.ReadOnly {
			t.Fatalf("after %s the volume is writable", call)
		}
	}
}

func TestVolumeDefTool_SetModeRefusals(t *testing.T) {
	tool, base, _, done := volumeDefFixture(t)
	defer done()
	ctx, _ := ephemeralCtx(base, "run-1")
	// A persistent volume is not an ephemeral one.
	if _, res := vdExec(t, tool, ctx, `{"op":"create","name":"data"}`); res.IsError {
		t.Fatal(res.Text)
	}
	if _, res := vdExec(t, tool, ctx, `{"op":"create","name":"repo","ephemeral":true}`); res.IsError {
		t.Fatal(res.Text)
	}
	other, _ := ephemeralCtx(base, "run-2")
	noScope := tools.WithVolumeDefPolicy(ctx, tools.VolumeDefPolicyValue{})
	onlyOther := tools.WithVolumeDefPolicy(ctx, tools.VolumeDefPolicyValue{Scopes: []string{"named:scratch"}})
	for _, c := range []struct {
		name string
		ctx  context.Context
		call string
		want string
	}{
		{"a persistent volume", ctx, `{"op":"set_mode","name":"data","mode":"ro"}`, "ephemeral volumes only"},
		{"a name this run does not have", ctx, `{"op":"set_mode","name":"nope","mode":"ro"}`, "no ephemeral volume"},
		{"another run's volume of the same name", other, `{"op":"set_mode","name":"repo","mode":"ro"}`, "no ephemeral volume"},
		{"outside a run", base, `{"op":"set_mode","name":"repo","mode":"ro"}`, "only inside a run"},
		{"an agent with no volume_def_scopes", noScope, `{"op":"set_mode","name":"repo","mode":"ro"}`, "volume_def_scopes"},
		{"an agent granted another name", onlyOther, `{"op":"set_mode","name":"repo","mode":"ro"}`, "volume_def_scopes"},
	} {
		if _, res := vdExec(t, tool, c.ctx, c.call); !res.IsError || !strings.Contains(res.Text, c.want) {
			t.Errorf("%s: error=%v %q, want a refusal naming %q", c.name, res.IsError, res.Text, c.want)
		}
	}
	// None of the refusals changed the volume.
	if res := fileCall(t, ctx, &Write{}, map[string]any{"volume": "repo", "path": "a.txt", "content": "x"}); res.IsError {
		t.Errorf("a refused set_mode still made the volume read-only: %s", res.Text)
	}
}

// Context op=self names the run's ephemeral volumes with the mode the agent
// gets, also for an agent that is bound to no volume of its own.
func TestContextTool_SelfListsEphemeralVolumesWithTheirMode(t *testing.T) {
	tool, ctx := contextFixture(t)
	set := tools.NewEphemeralVolumeSet()
	set.Add("repo", tools.EphemeralVolumeRef{Root: "/pool/_ephemeral/r/repo"})
	set.Add("notes", tools.EphemeralVolumeRef{Root: "/pool/_ephemeral/r/notes"})
	set.SetReadOnly("repo")
	ctx = tools.WithEphemeralVolumes(ctx, set)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"self"}`))
	if res.IsError {
		t.Fatal(res.Text)
	}
	out := decodeResult(t, res.Text)
	eph, _ := out["ephemeral_volumes"].(map[string]any)
	list, _ := eph["volumes"].([]any)
	if len(list) != 2 {
		t.Fatalf("ephemeral_volumes = %v, want two volumes", out["ephemeral_volumes"])
	}
	got := map[string]string{}
	for _, v := range list {
		m := v.(map[string]any)
		got[m["name"].(string)] = m["mode"].(string)
	}
	if got["repo"] != "ro" || got["notes"] != "rw" {
		t.Errorf("modes = %v, want repo ro and notes rw", got)
	}
	if fs, _ := out["filesystem"].(string); !strings.Contains(fs, "none") {
		t.Errorf("an agent bound to no volume should still be told so: %q", fs)
	}

	// A run with no ephemeral volume gets no such block.
	plain, _ := contextFixture(t)
	_, plainCtx := contextFixture(t)
	res, _ = plain.Execute(tools.WithEphemeralVolumes(plainCtx, tools.NewEphemeralVolumeSet()), json.RawMessage(`{"op":"self"}`))
	if _, ok := decodeResult(t, res.Text)["ephemeral_volumes"]; ok {
		t.Error("an empty set was reported")
	}
}

// A run whose set does not know the tree's volume (a resumed run that could
// not read its volumes back) must not get it back writable by creating it
// again. The create is refused, and the volume is taken into the set with the
// mode that is recorded.
func TestVolumeDefTool_CreateOverARecordedReadOnlyVolumeIsRefusedAndStaysReadOnly(t *testing.T) {
	tool, base, _, done := volumeDefFixture(t)
	defer done()
	first, _ := ephemeralCtx(base, "run-1")
	if _, res := vdExec(t, tool, first, `{"op":"create","name":"repo","ephemeral":true}`); res.IsError {
		t.Fatal(res.Text)
	}
	if _, res := vdExec(t, tool, first, `{"op":"set_mode","name":"repo","mode":"ro"}`); res.IsError {
		t.Fatal(res.Text)
	}

	// The same tree, an empty set.
	again, set := ephemeralCtx(base, "run-1")
	if _, res := vdExec(t, tool, again, `{"op":"create","name":"repo","mode":"rw","ephemeral":true}`); !res.IsError || !strings.Contains(res.Text, "already exists") {
		t.Fatalf("create over a recorded volume: error=%v %q", res.IsError, res.Text)
	}
	ref, ok := set.Get("repo")
	if !ok || !ref.ReadOnly {
		t.Fatalf("after the refused create the set holds %+v ok=%v, want the volume, read-only", ref, ok)
	}
	if res := fileCall(t, again, &Write{}, map[string]any{"volume": "repo", "path": "a.txt", "content": "x"}); !res.IsError || !strings.Contains(res.Text, "read-only") {
		t.Errorf("the run can write to it: error=%v %q", res.IsError, res.Text)
	}
	rows, _ := tool.Store.EphemeralVolumeListByRun(context.Background(), "run-1")
	if len(rows) != 1 || !strings.Contains(string(rows[0].Definition), `"ro"`) {
		t.Errorf("stored row = %+v, want one row, mode ro", rows)
	}
}
