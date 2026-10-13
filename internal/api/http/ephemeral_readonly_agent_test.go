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

// A spawn pinned to a stored version runs with that version's setting, and
// can only gain it: the base's is never dropped by a version without one.
func TestPinnedSubAgentDef_KeepsAndAddsEphemeralVolumesReadOnly(t *testing.T) {
	added := pinnedSubAgentDef(config.AgentDef{SystemPrompt: "base"}, json.RawMessage(`{"ephemeral_volumes":"ro"}`))
	if added.EphemeralVolumes != "ro" {
		t.Errorf("a pinned read-only version ran without the setting: %q", added.EphemeralVolumes)
	}
	kept := pinnedSubAgentDef(config.AgentDef{SystemPrompt: "base", EphemeralVolumes: "ro"}, json.RawMessage(`{"system_prompt":"v2"}`))
	if kept.EphemeralVolumes != "ro" {
		t.Errorf("a pinned version without the setting removed the base's: %q", kept.EphemeralVolumes)
	}
	if got := pinnedSubAgentDef(config.AgentDef{}, json.RawMessage(`{"system_prompt":"v2"}`)).EphemeralVolumes; got != "" {
		t.Errorf("a version that never set it reads as %q", got)
	}
}

// A resumed child has no parent ctx. Whether it may write to the tree's
// ephemeral volume must come out the same as it did live: refused when its
// parent's definition, or its own, says ephemeral_volumes: ro, and allowed
// when neither does (the case that shows this test can fail).
func TestResumedChild_KeepsEphemeralVolumesReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name            string
		parent, child   string
		dropWhilePaused bool
		wantRefused     bool
	}{
		{name: "the parent's definition says ro", parent: "ro", wantRefused: true},
		{name: "the child's own definition says ro", child: "ro", wantRefused: true},
		{name: "the parent's says ro, and loses it while the child is paused", parent: "ro", dropWhilePaused: true, wantRefused: true},
		{name: "the child's own says ro, and loses it while the child is paused", child: "ro", dropWhilePaused: true, wantRefused: true},
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

			if tc.dropWhilePaused {
				// The definitions lose the setting while the run is parked.
				p, c := cfg.Agents["parent"], cfg.Agents["child"]
				p.EphemeralVolumes, c.EphemeralVolumes = "", ""
				cfg.Agents["parent"], cfg.Agents["child"] = p, c
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
