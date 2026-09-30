package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/dynvol"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// symlinkedRootFixture is volumeDefFixture with the dynamic root configured
// THROUGH a symlink to the real directory — /tmp on macOS (-> /private/tmp),
// or a symlinked NAS mount point. Returns the tool, ctx, the configured link
// and the real directory it points at.
func symlinkedRootFixture(t *testing.T) (*VolumeDef, context.Context, string, string, func()) {
	t.Helper()
	tool, ctx, target, cleanup := volumeDefFixture(t)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "root-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	tool.Cfg.Volumes["pool"] = config.Volume{Path: link, Mode: "rw", DynamicRoot: true}
	return tool, ctx, link, target, cleanup
}

// A dynamic root reached through a symlink works end to end: create
// provisions the directory in the real root and stores the path spelled under
// the root as configured, the stored path resolves and takes a write through
// the Write tool's fence, and purge removes the directory and the row.
func TestVolumeDefTool_ASymlinkedRootCreatesResolvesAndPurges(t *testing.T) {
	tool, ctx, link, target, cleanup := symlinkedRootFixture(t)
	defer cleanup()

	out, res := vdExec(t, tool, ctx, `{"op":"create","name":"repo","mode":"rw"}`)
	if res.IsError {
		t.Fatalf("create under a symlinked root: %s", res.Text)
	}
	if want := filepath.Join(link, "_shared", "repo"); out["path"] != want {
		t.Errorf("stored path = %v, want %v", out["path"], want)
	}
	volDir := filepath.Join(target, "_shared", "repo")
	if info, err := os.Stat(volDir); err != nil || !info.IsDir() {
		t.Fatalf("no directory in the target root: %v", err)
	}

	spec, ok := lookup.VolumeDef(ctx, tool.Cfg, tool.Store, "", "repo")
	if !ok {
		t.Fatal("the created volume does not resolve")
	}
	dst, err := resolveParentInsideRoot(spec.Path, "f.txt")
	if err != nil {
		t.Fatalf("a write into the volume is refused: %v", err)
	}
	if err := os.WriteFile(dst, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(volDir, "f.txt")); err != nil {
		t.Errorf("the write did not land in the target root: %v", err)
	}

	out, res = vdExec(t, tool, ctx, `{"op":"purge","name":"repo"}`)
	if res.IsError {
		t.Fatalf("purge under a symlinked root: %s", res.Text)
	}
	if out["files_removed"] != true {
		t.Errorf("purge did not remove files: %v", out)
	}
	if _, err := os.Stat(volDir); !os.IsNotExist(err) {
		t.Errorf("purge left the directory: err=%v", err)
	}
	if _, err := os.Stat(link); err != nil {
		t.Errorf("purge touched the root link itself: %v", err)
	}
}

// An ephemeral volume under a symlinked root is created, and its run tree is
// purged by the same fence the run-completion purge and the sweeper use.
func TestVolumeDefTool_ASymlinkedRootCreatesAndPurgesAnEphemeralVolume(t *testing.T) {
	tool, base, link, target, cleanup := symlinkedRootFixture(t)
	defer cleanup()
	ctx, set := ephemeralCtx(base, "run-sym")

	out, res := vdExec(t, tool, ctx, `{"op":"create","name":"scratch","ephemeral":true}`)
	if res.IsError {
		t.Fatalf("ephemeral create under a symlinked root: %s", res.Text)
	}
	if want := filepath.Join(link, "_ephemeral", "run-sym", "scratch"); out["path"] != want {
		t.Errorf("path = %v, want %v", out["path"], want)
	}
	if _, ok := set.Get("scratch"); !ok {
		t.Error("the ephemeral volume was not registered on the run")
	}
	removed, err := PurgeEphemeralRunTree(link, "run-sym", "test")
	if err != nil || !removed {
		t.Fatalf("ephemeral purge: removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(target, "_ephemeral", "run-sym")); !os.IsNotExist(err) {
		t.Errorf("the run tree survived the purge: %v", err)
	}
}

// Trusting a symlinked ROOT does not trust a symlink INSIDE it. On create, a
// planted leaf is refused and writes no row. On purge, a volume whose
// directory was swapped for a symlink out of the root is refused, and the
// target survives; so is an ephemeral run dir swapped the same way.
func TestVolumeDefTool_ASymlinkedRootStillRefusesAnInnerSymlinkOnCreateAndPurge(t *testing.T) {
	tool, ctx, link, target, cleanup := symlinkedRootFixture(t)
	defer cleanup()
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(outside, "precious.txt")
	if err := os.WriteFile(precious, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(target, "_shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "_shared", "planted")); err != nil {
		t.Fatal(err)
	}
	if _, res := vdExec(t, tool, ctx, `{"op":"create","name":"planted"}`); !res.IsError {
		t.Errorf("create over a planted symlink was accepted: %s", res.Text)
	}
	if _, err := tool.Store.VolumeDefGetByName(ctx, "", "planted"); err == nil {
		t.Error("a refused create still wrote a volume row")
	}

	// A row for "evil" whose directory is a symlink out of the root. The row
	// is written directly: create would refuse the directory.
	if err := os.Symlink(outside, filepath.Join(target, "_shared", "evil")); err != nil {
		t.Fatal(err)
	}
	def, _ := json.Marshal(dynvol.Body{Path: filepath.Join(link, "_shared", "evil"), Mode: "rw"})
	if _, err := tool.Store.VolumeDefCreate(ctx, store.VolumeDefRow{Name: "evil", Definition: def}); err != nil {
		t.Fatal(err)
	}
	if _, res := vdExec(t, tool, ctx, `{"op":"purge","name":"evil"}`); !res.IsError {
		t.Errorf("purge through an inner symlink was accepted: %s", res.Text)
	}

	if err := os.MkdirAll(filepath.Join(target, "_ephemeral"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "_ephemeral", "run-evil")); err != nil {
		t.Fatal(err)
	}
	if removed, err := PurgeEphemeralRunTree(link, "run-evil", "test"); removed || err == nil {
		t.Errorf("ephemeral purge through an inner symlink: removed=%v err=%v", removed, err)
	}

	if _, err := os.Stat(precious); err != nil {
		t.Errorf("FENCE BREACH: a purge followed an inner symlink and deleted %q: %v", precious, err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Errorf("something was created through an inner symlink: %d entries outside", len(entries))
	}
}

// A row created while the root was configured by its real path keeps working
// after the operator reaches the same directory through a symlink: it still
// resolves, and purge (which re-derives under the new spelling) removes it.
func TestVolumeDefTool_ARowFromTheRealRootStillResolvesAndPurgesUnderALink(t *testing.T) {
	tool, ctx, link, target, cleanup := symlinkedRootFixture(t)
	defer cleanup()
	tool.Cfg.Volumes["pool"] = config.Volume{Path: target, Mode: "rw", DynamicRoot: true}
	out, res := vdExec(t, tool, ctx, `{"op":"create","name":"legacy"}`)
	if res.IsError {
		t.Fatalf("create under the target root: %s", res.Text)
	}
	stored := out["path"]
	if want := filepath.Join(target, "_shared", "legacy"); stored != want {
		t.Fatalf("stored path = %v, want %v", stored, want)
	}

	tool.Cfg.Volumes["pool"] = config.Volume{Path: link, Mode: "rw", DynamicRoot: true}
	spec, ok := lookup.VolumeDef(ctx, tool.Cfg, tool.Store, "", "legacy")
	if !ok || spec.Path != stored {
		t.Errorf("resolves (ok=%v) to %q, want the stored %v", ok, spec.Path, stored)
	}
	if _, err := resolveParentInsideRoot(spec.Path, "f.txt"); err != nil {
		t.Errorf("a write into the existing volume is refused: %v", err)
	}
	if out, res := vdExec(t, tool, ctx, `{"op":"purge","name":"legacy"}`); res.IsError || out["files_removed"] != true {
		t.Errorf("purge of the existing row under the link: %v %s", out, res.Text)
	}
	if _, err := os.Stat(filepath.Join(target, "_shared", "legacy")); !os.IsNotExist(err) {
		t.Errorf("purge left the directory: err=%v", err)
	}
}
