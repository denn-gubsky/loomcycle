package http

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// ephemeral_volumes: ro on a definition marks the run; a definition without it
// leaves an inherited mark alone.
func TestEphemeralReadOnlyFor_MarksAndNeverUnmarks(t *testing.T) {
	ctx := context.Background()
	if tools.EphemeralReadOnly(ephemeralReadOnlyFor(ctx, config.AgentDef{})) {
		t.Error("an agent without the setting was marked")
	}
	marked := ephemeralReadOnlyFor(ctx, config.AgentDef{EphemeralVolumes: "ro"})
	if !tools.EphemeralReadOnly(marked) {
		t.Fatal("ephemeral_volumes: ro did not mark the run")
	}
	// A child with no setting of its own, under a marked parent.
	if !tools.EphemeralReadOnly(ephemeralReadOnlyFor(marked, config.AgentDef{})) {
		t.Error("a child's definition without the setting cleared its parent's")
	}
}

// What a child inherits from its parent is written into its spawn record.
func TestSpawnRecord_CarriesInheritedEphemeralReadOnly(t *testing.T) {
	ctx := tools.WithEphemeralReadOnly(context.Background())
	rec := spawnRecordOf(tools.VolumePolicy(ctx), tools.FanoutCap(ctx), tools.EphemeralReadOnly(ctx))
	if !rec.EphemeralReadOnly {
		t.Error("the record did not take the parent's mark")
	}
	if spawnRecordOf(tools.VolumePolicyValue{}, 0, false).EphemeralReadOnly {
		t.Error("an unmarked parent produced a marked record")
	}
}

// A resumed child has no parent ctx. Whether it may write to the tree's
// ephemeral volume must come out the same as it did live: refused when its
// parent's definition, or its own, says ephemeral_volumes: ro, and allowed
// when neither does (the case that shows this test can fail).
func TestResumedChild_KeepsEphemeralVolumesReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name          string
		parent, child string
		wantRefused   bool
	}{
		{name: "the parent's definition says ro", parent: "ro", wantRefused: true},
		{name: "the child's own definition says ro", child: "ro", wantRefused: true},
		{name: "neither says ro", wantRefused: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, prov, _ := volumeCeilingSetup(t, nil,
				toolCallTurn("tu_write", "Write", `{"path":"out.txt","content":"x","volume":"scratchpad"}`))
			p, c := cfg.Agents["parent"], cfg.Agents["child"]
			p.EphemeralVolumes, c.EphemeralVolumes = tc.parent, tc.child
			cfg.Agents["parent"], cfg.Agents["child"] = p, c
			srv, st, ts := spawnCeilingServer(t, cfg, prov, []tools.Tool{&builtin.Write{}, &builtin.Read{}})

			child := spawnLiveChild(t, st, ts, "a_eph_parent", "")

			// The tree's ephemeral volume, recorded read-write, as a create
			// by any run of the tree would have left it.
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]string{"path": dir, "mode": "rw"})
			if _, err := st.EphemeralVolumeCreate(context.Background(), store.EphemeralVolumeDefRow{
				RootRunID: child.ParentRunID, Name: "scratchpad", Definition: body,
			}); err != nil {
				t.Fatal(err)
			}

			resumeAndFinish(t, srv, child)

			res := resumedToolResult(t, srv, child, "tu_write")
			_, statErr := os.Stat(filepath.Join(dir, "out.txt"))
			if tc.wantRefused {
				if !strings.Contains(res, `"is_error":true`) || !strings.Contains(res, "read-only") {
					t.Errorf("the resumed child's write was not refused as read-only: %s", res)
				}
				if statErr == nil {
					t.Error("the resumed child wrote to the volume")
				}
			} else if strings.Contains(res, `"is_error":true`) || statErr != nil {
				t.Errorf("with no setting anywhere the write should succeed: %s (stat: %v)", res, statErr)
			}
		})
	}
}
