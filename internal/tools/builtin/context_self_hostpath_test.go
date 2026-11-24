package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Context op=self names each volume an agent may use, its mode and whether it
// is the default, and never the directory it lives in on the server. The
// agent addresses files relative to the volume, so the host directory tells
// it nothing it needs and tells the model and the run's transcript where the
// server keeps its files.
func TestContextSelf_ListsVolumesWithoutTheirHostDirectory(t *testing.T) {
	tool, ctx := contextFixture(t)
	root, hostForms := symlinkedVolume(t)
	ephDir := t.TempDir()
	ephRecorded := filepath.Join(t.TempDir(), "recorded")
	hostForms = append(hostForms, ephDir, ephRecorded)
	if resolved, err := filepath.EvalSymlinks(ephDir); err == nil {
		hostForms = append(hostForms, resolved)
	}

	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Volumes = tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{
			{Name: "work", Root: root, Default: true},
			{Name: "ref", Root: filepath.Join(root, "sub"), ReadOnly: true},
		}}
	})
	set := tools.NewEphemeralVolumeSet()
	set.Add("scratch", tools.EphemeralVolumeRef{Root: ephDir})
	ctx = tools.WithEphemeralVolumes(ephemeralWritable(ctx), set)
	ctx = tools.WithEphemeralRecords(ctx, func(context.Context) ([]tools.EphemeralRecord, error) {
		return []tools.EphemeralRecord{{Name: "recorded", Root: ephRecorded, ReadOnly: true}}, nil
	})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"self"}`))
	if res.IsError {
		t.Fatalf("self: %s", res.Text)
	}
	for _, h := range hostForms {
		if strings.Contains(res.Text, h) {
			t.Errorf("op=self names a host directory (%s): %s", h, res.Text)
		}
	}

	out := decodeResult(t, res.Text)
	vols, _ := out["volumes"].(map[string]any)
	bindings, _ := vols["bindings"].([]any)
	if len(bindings) != 2 {
		t.Fatalf("volumes.bindings = %v, want work and ref", vols["bindings"])
	}
	work, ref := bindings[0].(map[string]any), bindings[1].(map[string]any)
	if work["name"] != "work" || work["mode"] != "rw" || work["default"] != true {
		t.Errorf("work = %v, want rw and the default", work)
	}
	if ref["name"] != "ref" || ref["mode"] != "ro" || ref["default"] != false {
		t.Errorf("ref = %v, want ro and not the default", ref)
	}

	eph, _ := out["ephemeral_volumes"].(map[string]any)
	list, _ := eph["volumes"].([]any)
	modes := map[string]any{}
	for _, v := range list {
		m := v.(map[string]any)
		modes[m["name"].(string)] = m["mode"]
	}
	if len(list) != 2 || modes["scratch"] != "rw" || modes["recorded"] != "ro" {
		t.Errorf("ephemeral_volumes = %v, want scratch rw and recorded ro", eph["volumes"])
	}
}
