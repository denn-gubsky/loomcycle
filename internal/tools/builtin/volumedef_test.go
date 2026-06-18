package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// volumeDefFixture builds a VolumeDef tool over in-memory SQLite + a config
// with a dynamic_root pointing at a real temp dir. Returns the tool, an
// "any"-scope ctx for the shared tenant, the resolved dynamic root path,
// and cleanup. scopes lets a test override the volume_def_scopes grant.
func volumeDefFixture(t *testing.T, scopes ...string) (*VolumeDef, context.Context, string, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	// EvalSymlinks so the derived/resolved paths match on macOS (/var ->
	// /private/var), where the purge fence's EvalSymlinks would otherwise
	// see a different real path than the configured one.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Volumes: map[string]config.Volume{
			"pool":   {Path: root, Mode: "rw", DynamicRoot: true},
			"static": {Path: root, Mode: "ro"}, // a non-root static volume (ground-truth collision test)
		},
	}
	tool := &VolumeDef{Store: s, Cfg: cfg, MaxNameLen: 64}
	if len(scopes) == 0 {
		scopes = []string{"any"}
	}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_admin"})
	ctx = tools.WithVolumeDefPolicy(ctx, tools.VolumeDefPolicyValue{Scopes: scopes})
	return tool, ctx, root, func() { _ = s.Close() }
}

func vdExec(t *testing.T, tool *VolumeDef, ctx context.Context, body string) (map[string]any, tools.Result) {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute(%s): %v", body, err)
	}
	if res.IsError {
		return nil, res
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode %q: %v", res.Text, err)
	}
	return out, res
}

// create derives <root>/_shared/<name> and mkdirs it.
func TestVolumeDefTool_CreateDerivesPathAndMkdirs(t *testing.T) {
	tool, ctx, root, cleanup := volumeDefFixture(t)
	defer cleanup()

	out, res := vdExec(t, tool, ctx, `{"op":"create","name":"repo-a","mode":"rw"}`)
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	want := filepath.Join(root, "_shared", "repo-a")
	if out["path"] != want {
		t.Errorf("path = %v, want %v", out["path"], want)
	}
	if out["mode"] != "rw" {
		t.Errorf("mode = %v, want rw", out["mode"])
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Errorf("directory not created at %q: err=%v", want, err)
	}
}

// A tenant create lands under <root>/<tenant>/<name>.
func TestVolumeDefTool_CreateTenantSegment(t *testing.T) {
	tool, baseCtx, root, cleanup := volumeDefFixture(t)
	defer cleanup()
	ctx := tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{AgentID: "a", TenantID: "acme"})
	ctx = tools.WithVolumeDefPolicy(ctx, tools.VolumeDefPolicyValue{Scopes: []string{"any"}})

	out, res := vdExec(t, tool, ctx, `{"op":"create","name":"work"}`)
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	want := filepath.Join(root, "acme", "work")
	if out["path"] != want {
		t.Errorf("path = %v, want %v", out["path"], want)
	}
	if out["mode"] != "rw" { // default mode
		t.Errorf("default mode = %v, want rw", out["mode"])
	}
}

// create refuses a bad name (path injection attempts).
func TestVolumeDefTool_CreateRefusesBadName(t *testing.T) {
	tool, ctx, _, cleanup := volumeDefFixture(t)
	defer cleanup()
	for _, bad := range []string{"../x", "a/b", ".x", "Repo", "with space", ""} {
		body := `{"op":"create","name":` + mustJSON(bad) + `}`
		_, res := vdExec(t, tool, ctx, body)
		if !res.IsError {
			t.Errorf("name %q should be refused", bad)
		}
	}
}

// create refuses a name colliding with a static volume (yaml is ground truth).
func TestVolumeDefTool_CreateRefusesStaticCollision(t *testing.T) {
	tool, ctx, _, cleanup := volumeDefFixture(t)
	defer cleanup()
	_, res := vdExec(t, tool, ctx, `{"op":"create","name":"static"}`)
	if !res.IsError || !strings.Contains(res.Text, "ground truth") {
		t.Errorf("static collision should be refused, got: %s", res.Text)
	}
}

// create refuses when no dynamic_root is configured.
func TestVolumeDefTool_CreateRefusesNoDynamicRoot(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	cfg := &config.Config{Volumes: map[string]config.Volume{
		"plain": {Path: root, Mode: "rw"}, // no dynamic_root
	}}
	tool := &VolumeDef{Store: s, Cfg: cfg}
	ctx := tools.WithVolumeDefPolicy(tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{}), tools.VolumeDefPolicyValue{Scopes: []string{"any"}})
	_, res := vdExec(t, tool, ctx, `{"op":"create","name":"x"}`)
	if !res.IsError || !strings.Contains(res.Text, "no dynamic volume root configured") {
		t.Errorf("expected no-dynamic-root refusal, got: %s", res.Text)
	}
}

// Idempotent: identical re-create is a no-op-OK; a different mode updates it.
func TestVolumeDefTool_CreateIdempotentAndUpdatesMode(t *testing.T) {
	tool, ctx, _, cleanup := volumeDefFixture(t)
	defer cleanup()
	if _, res := vdExec(t, tool, ctx, `{"op":"create","name":"data","mode":"rw"}`); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	out, res := vdExec(t, tool, ctx, `{"op":"create","name":"data","mode":"rw"}`)
	if res.IsError {
		t.Fatalf("re-create identical: %s", res.Text)
	}
	if out["mode"] != "rw" {
		t.Errorf("mode = %v", out["mode"])
	}
	// Different mode updates.
	out, res = vdExec(t, tool, ctx, `{"op":"create","name":"data","mode":"ro"}`)
	if res.IsError || out["mode"] != "ro" {
		t.Errorf("mode update failed: %v %s", out, res.Text)
	}
	// Still exactly one row.
	listOut, _ := vdExec(t, tool, ctx, `{"op":"list"}`)
	if vols, _ := listOut["volumes"].([]any); len(vols) != 1 {
		t.Errorf("update minted a new row: %d volumes", len(vols))
	}
}

// delete removes the row but LEAVES files on disk.
func TestVolumeDefTool_DeleteLeavesFiles(t *testing.T) {
	tool, ctx, root, cleanup := volumeDefFixture(t)
	defer cleanup()
	vdExec(t, tool, ctx, `{"op":"create","name":"keep","mode":"rw"}`)
	dir := filepath.Join(root, "_shared", "keep")
	marker := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(marker, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, res := vdExec(t, tool, ctx, `{"op":"delete","name":"keep"}`)
	if res.IsError {
		t.Fatalf("delete: %s", res.Text)
	}
	if out["files_removed"] != false {
		t.Errorf("delete should not remove files: %v", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("delete removed the files (should leave them): %v", err)
	}
	// Row is gone.
	if _, res := vdExec(t, tool, ctx, `{"op":"get","name":"keep"}`); !res.IsError {
		t.Error("row still present after delete")
	}
}

// purge removes the row AND the directory.
func TestVolumeDefTool_PurgeRemovesRowAndFiles(t *testing.T) {
	tool, ctx, root, cleanup := volumeDefFixture(t)
	defer cleanup()
	vdExec(t, tool, ctx, `{"op":"create","name":"gone","mode":"rw"}`)
	dir := filepath.Join(root, "_shared", "gone")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, res := vdExec(t, tool, ctx, `{"op":"purge","name":"gone"}`)
	if res.IsError {
		t.Fatalf("purge: %s", res.Text)
	}
	if out["files_removed"] != true {
		t.Errorf("purge should remove files: %v", out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("purge left the directory: err=%v", err)
	}
	if _, res := vdExec(t, tool, ctx, `{"op":"get","name":"gone"}`); !res.IsError {
		t.Error("row still present after purge")
	}
}

// THE FENCE TEST. A row whose stored definition.path was tampered to escape
// the dynamic root must NOT cause purge to delete the tampered target —
// purge RE-DERIVES the path from (root, tenant, name) and only ever
// RemoveAll's the derived path. We assert the escape target survives.
//
// FAIL-BEFORE: if purge trusted the stored path (os.RemoveAll(body.Path)),
// the escape directory below would be deleted and this test would fail.
func TestVolumeDefTool_PurgeReDerivesIgnoringTamperedPath(t *testing.T) {
	tool, ctx, root, cleanup := volumeDefFixture(t)
	defer cleanup()

	// A directory OUTSIDE the dynamic root that an attacker would target.
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	escapeFile := filepath.Join(outside, "precious.txt")
	if err := os.WriteFile(escapeFile, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Create the volume legitimately, then tamper the stored path directly
	// in the store to point at `outside`.
	vdExec(t, tool, ctx, `{"op":"create","name":"evil","mode":"rw"}`)
	tampered, _ := json.Marshal(volumeDefBody{Path: outside, Mode: "rw"})
	if _, err := tool.Store.VolumeDefCreate(ctx, store.VolumeDefRow{TenantID: "", Name: "evil", Definition: tampered}); err != nil {
		t.Fatal(err)
	}

	// The derived (real) directory still exists from create.
	derived := filepath.Join(root, "_shared", "evil")
	if _, err := os.Stat(derived); err != nil {
		t.Fatalf("derived dir missing: %v", err)
	}

	out, res := vdExec(t, tool, ctx, `{"op":"purge","name":"evil"}`)
	if res.IsError {
		t.Fatalf("purge: %s", res.Text)
	}
	// The DERIVED dir is gone; the TAMPERED escape target is untouched.
	if _, err := os.Stat(derived); !os.IsNotExist(err) {
		t.Errorf("purge did not remove the derived dir: %v", err)
	}
	if _, err := os.Stat(escapeFile); err != nil {
		t.Errorf("FENCE BREACH: purge followed the tampered path and deleted %q: %v", escapeFile, err)
	}
	_ = out
}

// Tenant isolation: tenant A cannot get/delete/purge tenant B's volume.
func TestVolumeDefTool_TenantIsolationOpaque404(t *testing.T) {
	tool, baseCtx, _, cleanup := volumeDefFixture(t)
	defer cleanup()
	ctxA := tools.WithVolumeDefPolicy(tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{TenantID: "a"}), tools.VolumeDefPolicyValue{Scopes: []string{"any"}})
	ctxB := tools.WithVolumeDefPolicy(tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{TenantID: "b"}), tools.VolumeDefPolicyValue{Scopes: []string{"any"}})

	vdExec(t, tool, ctxA, `{"op":"create","name":"secret","mode":"rw"}`)

	// B cannot get / delete / purge A's volume → opaque not-found.
	for _, op := range []string{`{"op":"get","name":"secret"}`, `{"op":"delete","name":"secret"}`, `{"op":"purge","name":"secret"}`} {
		_, res := vdExec(t, tool, ctxB, op)
		if !res.IsError || !strings.Contains(res.Text, "not found") {
			t.Errorf("B op %s should be opaque-404, got: %s", op, res.Text)
		}
	}
	// A's volume still resolves for A.
	if _, res := vdExec(t, tool, ctxA, `{"op":"get","name":"secret"}`); res.IsError {
		t.Errorf("A lost its own volume: %s", res.Text)
	}
}

// Capability gate: default-deny on empty scopes; named:x allows only x; any allows all.
func TestVolumeDefTool_CapabilityGate(t *testing.T) {
	// Empty scopes → default-deny on create/delete/purge.
	tool, baseCtx, _, cleanup := volumeDefFixture(t)
	defer cleanup()
	denyCtx := tools.WithVolumeDefPolicy(tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{}), tools.VolumeDefPolicyValue{})
	for _, op := range []string{`{"op":"create","name":"x"}`, `{"op":"delete","name":"x"}`, `{"op":"purge","name":"x"}`} {
		_, res := vdExec(t, tool, denyCtx, op)
		if !res.IsError || !strings.Contains(res.Text, "default-deny") {
			t.Errorf("empty scopes should default-deny %s, got: %s", op, res.Text)
		}
	}

	// named:allowed permits "allowed" but refuses "other".
	namedCtx := tools.WithVolumeDefPolicy(tools.WithRunIdentity(baseCtx, tools.RunIdentityValue{}), tools.VolumeDefPolicyValue{Scopes: []string{"named:allowed"}})
	if _, res := vdExec(t, tool, namedCtx, `{"op":"create","name":"allowed"}`); res.IsError {
		t.Errorf("named:allowed should permit create allowed: %s", res.Text)
	}
	if _, res := vdExec(t, tool, namedCtx, `{"op":"create","name":"other"}`); !res.IsError {
		t.Error("named:allowed should refuse create other")
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
