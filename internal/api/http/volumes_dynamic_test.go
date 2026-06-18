package http

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestVolumePolicyForAgent_ResolvesDynamicVolume is the RFC AH Phase 2a
// end-to-end: an agent bound (by name) to a volume that does NOT exist in
// static cfg.Volumes but WAS created via the VolumeDef tool resolves its
// runtime-derived root + mode through volumePolicyForAgent → the binding the
// file tools read. A static binding still resolves alongside (static-first).
//
// The resolved policy is then driven through the REAL Write tool: rw allows
// the write (file lands under the dynamic root), ro refuses it — the actual
// enforcement seam, not a flag inspection.
func TestVolumePolicyForAgent_ResolvesDynamicVolume(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Volumes: map[string]config.Volume{
		"pool":   {Path: root, Mode: "rw", DynamicRoot: true},
		"static": {Path: root, Mode: "ro"},
	}}

	// Create two dynamic volumes (one rw, one ro) for tenant "acme".
	tool := &builtin.VolumeDef{Store: st, Cfg: cfg, MaxNameLen: 64}
	toolCtx := tools.WithVolumeDefPolicy(
		tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme"}),
		tools.VolumeDefPolicyValue{Scopes: []string{"any"}},
	)
	for _, body := range []string{
		`{"op":"create","name":"repo-rw","mode":"rw"}`,
		`{"op":"create","name":"repo-ro","mode":"ro"}`,
	} {
		if res, _ := tool.Execute(toolCtx, json.RawMessage(body)); res.IsError {
			t.Fatalf("create %s: %s", body, res.Text)
		}
	}

	srv := &Server{store: st, cfg: cfg}
	runCtx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme"})
	agentDef := config.AgentDef{Volumes: []string{"repo-rw", "repo-ro", "static"}}

	pol := srv.volumePolicyForAgent(runCtx, agentDef)
	if !pol.Active {
		t.Fatal("policy should be active for a volume-bound agent")
	}
	byName := map[string]tools.VolumeBinding{}
	for _, b := range pol.Bindings {
		byName[b.Name] = b
	}
	if len(byName) != 3 {
		t.Fatalf("expected 3 bindings, got %d: %+v", len(byName), pol.Bindings)
	}
	if rw := byName["repo-rw"]; rw.Root != filepath.Join(root, "acme", "repo-rw") || rw.ReadOnly {
		t.Errorf("repo-rw binding wrong: %+v", rw)
	}
	if ro := byName["repo-ro"]; ro.Root != filepath.Join(root, "acme", "repo-ro") || !ro.ReadOnly {
		t.Errorf("repo-ro binding wrong: %+v", ro)
	}
	if s := byName["static"]; s.Root != root || !s.ReadOnly {
		t.Errorf("static binding (static-first) wrong: %+v", s)
	}

	// End-to-end through the REAL Write tool with the resolved policy on ctx.
	wCtx := tools.WithVolumePolicy(runCtx, pol)
	w := &builtin.Write{} // construction-time Root unset: only a volume can satisfy.

	rwBody, _ := json.Marshal(map[string]string{"path": "out.txt", "content": "hi", "volume": "repo-rw"})
	if res, _ := w.Execute(wCtx, rwBody); res.IsError {
		t.Fatalf("rw dynamic volume should allow write: %s", res.Text)
	}
	if _, err := os.Stat(filepath.Join(root, "acme", "repo-rw", "out.txt")); err != nil {
		t.Errorf("write did not land under the dynamic root: %v", err)
	}

	roBody, _ := json.Marshal(map[string]string{"path": "out.txt", "content": "hi", "volume": "repo-ro"})
	res, _ := w.Execute(wCtx, roBody)
	if !res.IsError || !strings.Contains(res.Text, "read-only") {
		t.Errorf("ro dynamic volume must refuse write, got Text=%q IsError=%v", res.Text, res.IsError)
	}
}
