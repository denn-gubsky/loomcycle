package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// RFC AL — Memory + Volume path: wiring.

func TestMemorySetWithPath_RegistersAndResolves(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	// set with a path registers a memory_entry dirent.
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"agent","key":"voice","value":{"style":"crisp"},"path":"/prefs/voice"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Text, `"path":"/prefs/voice"`) {
		t.Fatalf("set with path: %q", res.Text)
	}
	// The dirent exists in the agent tree (tenant "", scope agent, scope_id qa-agent).
	row, derr := tool.Store.DirentGet(context.Background(), "", "agent", "qa-agent", "/prefs/", "voice")
	if derr != nil {
		t.Fatalf("dirent not registered: %v", derr)
	}
	if row.Kind != "memory_entry" || !strings.Contains(string(row.ResourceRef), `"key":"voice"`) {
		t.Errorf("dirent = %+v", row)
	}

	// get BY PATH (no key) returns the value.
	res, err = tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"agent","path":"/prefs/voice"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Text, `"style":"crisp"`) {
		t.Errorf("get by path: %q", res.Text)
	}

	// get by a path that doesn't exist is a model-facing error.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"agent","path":"/prefs/missing"}`))
	if !res.IsError {
		t.Errorf("get of a missing path should error; got %q", res.Text)
	}
}

func TestMemorySetWithBadPath_RefusesAndDoesNotWrite(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	// A malformed path (..) must fail BEFORE the value is written.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"agent","key":"k","value":1,"path":"/a/../b"}`))
	if !res.IsError {
		t.Fatalf("set with a '..' path should refuse; got %q", res.Text)
	}
	// The value must NOT have been written (fail-fast).
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"agent","key":"k"}`))
	if strings.Contains(res.Text, `"value":1`) {
		t.Errorf("value was written despite the bad path: %q", res.Text)
	}
}

func TestVolumeDefCreate_RegistersMount(t *testing.T) {
	tool, ctx, _, cleanup := volumeDefFixture(t)
	defer cleanup()

	// Default mount at /vol/<name>.
	out, res := vdExec(t, tool, ctx, `{"op":"create","name":"repo-a","mode":"rw"}`)
	if res.IsError {
		t.Fatalf("create: %q", res.Text)
	}
	if out["mount_at"] != "/vol/repo-a" {
		t.Errorf("default mount_at = %v, want /vol/repo-a", out["mount_at"])
	}
	// A tenant-scoped volume_mount dirent exists at /vol/repo-a.
	row, derr := tool.Store.DirentGet(context.Background(), "", "tenant", "", "/vol/", "repo-a")
	if derr != nil {
		t.Fatalf("volume_mount dirent not registered: %v", derr)
	}
	if row.Kind != "volume_mount" || !strings.Contains(string(row.ResourceRef), `"volume_name":"repo-a"`) {
		t.Errorf("dirent = %+v", row)
	}

	// Custom mount_at.
	out, res = vdExec(t, tool, ctx, `{"op":"create","name":"repo-b","mode":"ro","mount_at":"/code/b"}`)
	if res.IsError || out["mount_at"] != "/code/b" {
		t.Errorf("custom mount_at: out=%v err=%q", out["mount_at"], res.Text)
	}
	if _, derr := tool.Store.DirentGet(context.Background(), "", "tenant", "", "/code/", "b"); derr != nil {
		t.Errorf("custom-mount dirent missing: %v", derr)
	}
}
