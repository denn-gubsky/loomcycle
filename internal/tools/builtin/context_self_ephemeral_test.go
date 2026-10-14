package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// op=self lists the run's ephemeral volumes from the record as well as from
// this process's set: one a run of the tree made on another replica is
// listed, and one whose files are not on this server says so instead of
// giving a path.
func TestContextSelf_ListsEphemeralVolumesTheRecordHas(t *testing.T) {
	set := tools.NewEphemeralVolumeSet()
	set.Add("local", tools.EphemeralVolumeRef{Root: "/vol/local"})
	ctx := tools.WithEphemeralVolumes(context.Background(), set)
	ctx = tools.WithEphemeralRecords(ctx, func(context.Context) ([]tools.EphemeralRecord, error) {
		return []tools.EphemeralRecord{
			{Name: "local", Root: "/vol/local"},
			{Name: "shared", Root: "/vol/shared", ReadOnly: true},
			{Name: "remote", Elsewhere: true},
		}, nil
	})
	res, _ := (&Context{}).Execute(ctx, json.RawMessage(`{"op":"self"}`))
	if res.IsError {
		t.Fatalf("self: %s", res.Text)
	}
	var out struct {
		Eph struct {
			Volumes []map[string]any `json:"volumes"`
		} `json:"ephemeral_volumes"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for _, v := range out.Eph.Volumes {
		got[v["name"].(string)] = v
	}
	if len(out.Eph.Volumes) != 3 {
		t.Fatalf("listed %v, want local, shared and remote once each", out.Eph.Volumes)
	}
	if v := got["shared"]; v["path"] != "/vol/shared" || v["mode"] != "ro" {
		t.Errorf("shared = %v", v)
	}
	if v := got["remote"]; v["path"] != nil || v["note"] == nil {
		t.Errorf("remote = %v, want no path and a note", v)
	}
}
