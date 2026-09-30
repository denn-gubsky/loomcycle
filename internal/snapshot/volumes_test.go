package snapshot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/dynvol"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// volumeRoot is a dynamic root with no symlink in it (macOS t.TempDir sits
// under /var -> /private/var, and the provisioning fence compares against the
// resolved root).
func volumeRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// volumeOpts is what the production call sites pass for this section: the
// VolumeDef tool's own checks, and this host's root and static names.
func volumeOpts(root string, static ...string) RestoreOptions {
	v := passValidators()
	v[migrations.SectionVolumeDefs] = builtin.ValidateVolumeDefBody
	return RestoreOptions{Validators: v, VolumeRoot: root, StaticVolumeNames: static}
}

// plantVolume writes a volume row as the VolumeDef tool does on the source:
// the directory provisioned under srcRoot, the derived path in the body.
func plantVolume(t *testing.T, s store.Store, srcRoot, tenant, name, mode string) string {
	t.Helper()
	path, _, err := dynvol.Provision(srcRoot, tenant, name)
	if err != nil {
		t.Fatalf("provision %s/%s: %v", tenant, name, err)
	}
	def, _ := json.Marshal(dynvol.Body{Path: path, Mode: mode})
	if _, err := s.VolumeDefCreate(context.Background(), store.VolumeDefRow{TenantID: tenant, Name: name, Definition: def}); err != nil {
		t.Fatalf("plant volume %s/%s: %v", tenant, name, err)
	}
	return path
}

// storedVolume reads a restored row's body.
func storedVolume(t *testing.T, s store.Store, tenant, name string) (dynvol.Body, bool) {
	t.Helper()
	row, err := s.VolumeDefGetByName(context.Background(), tenant, name)
	if err != nil {
		return dynvol.Body{}, false
	}
	var b dynvol.Body
	if err := json.Unmarshal(row.Definition, &b); err != nil {
		t.Fatalf("stored body %s/%s: %v", tenant, name, err)
	}
	return b, true
}

func warningWith(warnings []string, parts ...string) bool {
	for _, w := range warnings {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(w, p)
		}
		if all {
			return true
		}
	}
	return false
}

// Every volume comes back under its own tenant with its mode and timestamps,
// at the path the TARGET derives under its own root — not the source's —
// with a fresh empty 0700 directory there; the source's files do not travel;
// the restored volume resolves at once through the resolver the file tools
// use; each one says its contents were not carried; and a re-restore writes
// nothing and warns nothing.
func TestVolumeDefs_RoundTripDerivesThePathUnderTheTargetRoot(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	srcRoot, dstRoot := volumeRoot(t), volumeRoot(t)

	acmeSrc := plantVolume(t, src, srcRoot, "acme", "data", "rw")
	plantVolume(t, src, srcRoot, "", "shared", "ro")
	plantVolume(t, src, srcRoot, "beta", "logs", "rw")
	if err := os.WriteFile(filepath.Join(acmeSrc, "notes.txt"), []byte("source contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	raw := mustCapture(t, src)
	if strings.Contains(string(raw), srcRoot) {
		t.Fatal("the envelope carries the source's volume root")
	}
	res := mustRestore(t, dst, raw, volumeOpts(dstRoot))
	c := res.Counts()
	if c["volume_defs"] != 3 || c["volume_dirs_created"] != 3 {
		t.Fatalf("counts volume_defs=%d volume_dirs_created=%d, want 3/3 (warnings %v)", c["volume_defs"], c["volume_dirs_created"], res.Warnings)
	}

	for _, v := range []struct{ tenant, seg, name, mode string }{
		{"acme", "acme", "data", "rw"}, {"", "_shared", "shared", "ro"}, {"beta", "beta", "logs", "rw"},
	} {
		want := filepath.Join(dstRoot, v.seg, v.name)
		b, ok := storedVolume(t, dst, v.tenant, v.name)
		if !ok || b.Path != want || b.Mode != v.mode {
			t.Errorf("%s/%s restored as %+v (ok=%v), want path %s mode %s", v.tenant, v.name, b, ok, want, v.mode)
		}
		info, err := os.Stat(want)
		if err != nil || !info.IsDir() {
			t.Fatalf("%s/%s: no directory at the derived path: %v", v.tenant, v.name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s/%s: directory mode %o, want 700", v.tenant, v.name, perm)
		}
		if entries, _ := os.ReadDir(want); len(entries) != 0 {
			t.Errorf("%s/%s: restored directory is not empty: %d entries", v.tenant, v.name, len(entries))
		}
		if !warningWith(res.Warnings, "volume_def "+qualifiedName(v.tenant, v.name), "empty directory", "contents are not carried") {
			t.Errorf("%s/%s: no contents-not-carried warning: %v", v.tenant, v.name, res.Warnings)
		}
		spec, ok := lookup.VolumeDef(context.Background(), &config.Config{}, dst, v.tenant, v.name)
		if !ok || spec.Path != want || spec.Mode != v.mode {
			t.Errorf("%s/%s resolves (ok=%v) to %+v, want %s %s", v.tenant, v.name, ok, spec, want, v.mode)
		}
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, dstRoot) || strings.Contains(w, srcRoot) {
			t.Errorf("a warning spells out a host volume root: %s", w)
		}
	}

	// Timestamps travel.
	srcRows, _ := src.SnapshotReadVolumeDefs(context.Background())
	dstRows, _ := dst.SnapshotReadVolumeDefs(context.Background())
	if len(srcRows) != len(dstRows) {
		t.Fatalf("restored %d rows, source has %d", len(dstRows), len(srcRows))
	}
	for i := range srcRows {
		if !srcRows[i].CreatedAt.Equal(dstRows[i].CreatedAt) || !srcRows[i].UpdatedAt.Equal(dstRows[i].UpdatedAt) {
			t.Errorf("%s/%s timestamps %v/%v, source %v/%v", dstRows[i].TenantID, dstRows[i].Name,
				dstRows[i].CreatedAt, dstRows[i].UpdatedAt, srcRows[i].CreatedAt, srcRows[i].UpdatedAt)
		}
	}

	again := mustRestore(t, dst, raw, volumeOpts(dstRoot))
	if again.Counts()["volume_defs"] != 0 || again.Counts()["volume_dirs_created"] != 0 {
		t.Errorf("re-restore counted %v", again.Counts())
	}
	if warningWith(again.Warnings, "volume_def") {
		t.Errorf("re-restore of the same volumes warned: %v", again.Warnings)
	}
}

// The VolumeDef tool's create and a restore land a volume at the same path:
// they derive through one helper. The tool creates acme/data on one store;
// a restore of that name onto another store over the same root names the
// identical path, and binds the directory the tool made, as found.
func TestVolumeDefs_ToolCreateAndRestoreDeriveTheSamePath(t *testing.T) {
	toolStore, closeTool := newTestStore(t)
	defer closeTool()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	root := volumeRoot(t)

	tool := &builtin.VolumeDef{Store: toolStore, Cfg: &config.Config{Volumes: map[string]config.Volume{
		"pool": {Path: root, Mode: "rw", DynamicRoot: true},
	}}, MaxNameLen: 64}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a", TenantID: "acme"})
	ctx = tools.WithVolumeDefPolicy(ctx, tools.VolumeDefPolicyValue{Scopes: []string{"any"}})
	if res, err := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"data","mode":"ro"}`)); err != nil || res.IsError {
		t.Fatalf("tool create: %v %s", err, res.Text)
	}
	byTool, _ := storedVolume(t, toolStore, "acme", "data")

	res := mustRestore(t, dst, mustCapture(t, toolStore), volumeOpts(root, "pool"))
	byRestore, ok := storedVolume(t, dst, "acme", "data")
	if !ok || byRestore != byTool {
		t.Fatalf("restore stored %+v (ok=%v), the tool stored %+v", byRestore, ok, byTool)
	}
	if res.Counts()["volume_defs"] != 1 || res.Counts()["volume_dirs_created"] != 0 {
		t.Errorf("counts %v, want one volume restored onto the existing directory", res.Counts())
	}
	if !warningWith(res.Warnings, "volume_def acme/data", "already at <dynamic root>/acme/data", "as found") {
		t.Errorf("no as-found warning: %v", res.Warnings)
	}
}

// A hand-edited envelope that adds a path — at the entry, in a definition
// body, under another path-like key — is ignored: the row names the path
// derived under the target root, and nothing is created at the supplied one.
func TestVolumeDefs_HandEditedPathIsIgnored(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	srcRoot, dstRoot, outside := volumeRoot(t), volumeRoot(t), volumeRoot(t)
	plantVolume(t, src, srcRoot, "acme", "data", "rw")
	plantVolume(t, src, srcRoot, "acme", "logs", "rw")

	raw := editSection(t, mustCapture(t, src), migrations.SectionVolumeDefs, func(sec map[string]any) {
		for _, e := range sec["entries"].([]any) {
			m := e.(map[string]any)
			if m["name"] == "data" {
				m["path"] = "/etc"
				m["definition"] = map[string]any{"path": "/etc", "mode": "rw"}
			} else {
				m["path"] = outside
				m["host_path"] = outside
				m["root"] = outside
			}
		}
	})
	res := mustRestore(t, dst, raw, volumeOpts(dstRoot))
	if res.Counts()["volume_defs"] != 2 {
		t.Fatalf("restored %d volumes, want 2 (warnings %v)", res.Counts()["volume_defs"], res.Warnings)
	}
	for _, name := range []string{"data", "logs"} {
		b, _ := storedVolume(t, dst, "acme", name)
		if want := filepath.Join(dstRoot, "acme", name); b.Path != want {
			t.Errorf("acme/%s stored path %q, want the derived %q", name, b.Path, want)
		}
		spec, ok := lookup.VolumeDef(context.Background(), nil, dst, "acme", name)
		if !ok || spec.Path == "/etc" || spec.Path == outside || !strings.HasPrefix(spec.Path, dstRoot+string(filepath.Separator)) {
			t.Errorf("acme/%s resolves (ok=%v) to %q, want a path under the target root", name, ok, spec.Path)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("the restore created %d entries at the supplied path", len(entries))
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "/etc") || strings.Contains(w, outside) {
			t.Errorf("a warning names the supplied path: %s", w)
		}
	}
}

// A name that would escape the root, or a derived path already occupied by a
// symlink out of it, is refused by the provisioning fence itself — even with
// a validator that lets everything through. No row, a warning, and nothing
// created outside the root.
func TestVolumeDefs_TraversalAndSymlinkAreRefusedByTheFence(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	srcRoot, dstRoot, outside := volumeRoot(t), volumeRoot(t), volumeRoot(t)
	plantVolume(t, src, srcRoot, "acme", "data", "rw")
	plantVolume(t, src, srcRoot, "beta", "logs", "rw")

	raw := editSection(t, mustCapture(t, src), migrations.SectionVolumeDefs, func(sec map[string]any) {
		for _, e := range sec["entries"].([]any) {
			if m := e.(map[string]any); m["name"] == "data" {
				m["name"] = "../../escape"
			}
		}
	})
	// beta's tenant segment on the target is a symlink out of the root.
	if err := os.Symlink(outside, filepath.Join(dstRoot, "beta")); err != nil {
		t.Fatal(err)
	}
	opts := volumeOpts(dstRoot)
	opts.Validators[migrations.SectionVolumeDefs] = func(json.RawMessage) error { return nil }

	res := mustRestore(t, dst, raw, opts)
	if res.Counts()["volume_defs"] != 0 || res.Counts()["volume_dirs_created"] != 0 {
		t.Fatalf("counts %v, want nothing restored (warnings %v)", res.Counts(), res.Warnings)
	}
	if !warningWith(res.Warnings, "volume_def acme/../../escape", "could not be provisioned") {
		t.Errorf("no refusal for the traversal name: %v", res.Warnings)
	}
	if !warningWith(res.Warnings, "volume_def beta/logs", "could not be provisioned", "symlink") {
		t.Errorf("no refusal for the symlinked segment: %v", res.Warnings)
	}
	rows, _ := dst.SnapshotReadVolumeDefs(context.Background())
	if len(rows) != 0 {
		t.Errorf("refused volumes wrote %d rows", len(rows))
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("the restore created %d entries through the symlink", len(entries))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dstRoot), "escape")); !os.IsNotExist(err) {
		t.Errorf("the traversal name created a directory beside the root (err=%v)", err)
	}
}

// When the directory cannot be created, no row is written — the table never
// names a volume with no directory — and the warning names the location
// relative to the root, not the host path.
func TestVolumeDefs_DirectoryCreateFailureWritesNoRow(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	srcRoot, dstRoot := volumeRoot(t), volumeRoot(t)
	plantVolume(t, src, srcRoot, "acme", "data", "rw")
	// A regular file where the tenant segment directory would go.
	if err := os.WriteFile(filepath.Join(dstRoot, "acme"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := mustRestore(t, dst, mustCapture(t, src), volumeOpts(dstRoot))
	if _, ok := storedVolume(t, dst, "acme", "data"); ok {
		t.Error("a volume whose directory could not be created was written")
	}
	if res.Counts()["volume_defs"] != 0 {
		t.Errorf("counted %d", res.Counts()["volume_defs"])
	}
	if !warningWith(res.Warnings, "volume_def acme/data", "not restored", "could not be provisioned", "<dynamic root>/acme") {
		t.Errorf("no provisioning warning: %v", res.Warnings)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, dstRoot) {
			t.Errorf("a warning spells out the host volume root: %s", w)
		}
	}
}

// A restored name that is a static yaml volume here is skipped with a
// warning: no row, no directory.
func TestVolumeDefs_StaticNameCollisionIsSkipped(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	srcRoot, dstRoot := volumeRoot(t), volumeRoot(t)
	plantVolume(t, src, srcRoot, "acme", "repo", "rw")
	plantVolume(t, src, srcRoot, "acme", "data", "rw")

	res := mustRestore(t, dst, mustCapture(t, src), volumeOpts(dstRoot, "pool", "repo"))
	if res.Counts()["volume_defs"] != 1 {
		t.Fatalf("restored %d, want only acme/data (warnings %v)", res.Counts()["volume_defs"], res.Warnings)
	}
	if _, ok := storedVolume(t, dst, "acme", "repo"); ok {
		t.Error("a volume shadowed by a static one was written")
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "acme", "repo")); !os.IsNotExist(err) {
		t.Errorf("a skipped volume's directory was created (err=%v)", err)
	}
	if !warningWith(res.Warnings, "volume_def acme/repo", "not restored", "static volume") {
		t.Errorf("no static-collision warning: %v", res.Warnings)
	}
}

// A volume the target already has keeps its row and its directory, and the
// restore says how the snapshot's differs.
func TestVolumeDefs_LiveRowStands(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	srcRoot, dstRoot := volumeRoot(t), volumeRoot(t)
	plantVolume(t, src, srcRoot, "acme", "data", "rw")
	livePath := plantVolume(t, dst, dstRoot, "acme", "data", "ro")
	if err := os.WriteFile(filepath.Join(livePath, "live.txt"), []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := mustRestore(t, dst, mustCapture(t, src), volumeOpts(dstRoot))
	if res.Counts()["volume_defs"] != 0 || res.Counts()["volume_dirs_created"] != 0 {
		t.Errorf("counts %v, want nothing written", res.Counts())
	}
	if b, _ := storedVolume(t, dst, "acme", "data"); b.Mode != "ro" || b.Path != livePath {
		t.Errorf("live volume became %+v, want ro at %s", b, livePath)
	}
	if got, err := os.ReadFile(filepath.Join(livePath, "live.txt")); err != nil || string(got) != "live" {
		t.Errorf("live volume's contents changed: %q %v", got, err)
	}
	if !warningWith(res.Warnings, "volume_def acme/data", "live volume", "stands", "mode ro here, rw in the snapshot") {
		t.Errorf("no live-row warning: %v", res.Warnings)
	}
}

// With no validator wired, or one that refuses, a volume is never restored,
// and a target with no dynamic root skips the section once.
func TestVolumeDefs_UnwiredOrFailingValidatorAndNoRootSkip(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	srcRoot := volumeRoot(t)
	plantVolume(t, src, srcRoot, "acme", "data", "rw")
	raw := mustCapture(t, src)

	t.Run("unwired", func(t *testing.T) {
		dst, dstClose := newTestStore(t)
		defer dstClose()
		root := volumeRoot(t)
		res := mustRestore(t, dst, raw, RestoreOptions{Validators: passValidators(), VolumeRoot: root})
		if _, ok := storedVolume(t, dst, "acme", "data"); ok {
			t.Error("restored with no validator wired")
		}
		if !warningWith(res.Warnings, "volume_def acme/data", "no volume_defs validator") {
			t.Errorf("no unwired-validator warning: %v", res.Warnings)
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Errorf("an unvalidated volume created %d entries", len(entries))
		}
	})
	t.Run("failing", func(t *testing.T) {
		dst, dstClose := newTestStore(t)
		defer dstClose()
		edited := editSection(t, raw, migrations.SectionVolumeDefs, func(sec map[string]any) {
			sec["entries"].([]any)[0].(map[string]any)["mode"] = "rwx"
		})
		res := mustRestore(t, dst, edited, volumeOpts(volumeRoot(t)))
		if _, ok := storedVolume(t, dst, "acme", "data"); ok {
			t.Error("restored a mode the tool refuses")
		}
		if !warningWith(res.Warnings, "volume_def acme/data", "fails the validation", "invalid mode") {
			t.Errorf("no validator warning: %v", res.Warnings)
		}
	})
	t.Run("no root", func(t *testing.T) {
		dst, dstClose := newTestStore(t)
		defer dstClose()
		res := mustRestore(t, dst, raw, volumeOpts(""))
		if _, ok := storedVolume(t, dst, "acme", "data"); ok {
			t.Error("restored with no dynamic root")
		}
		if !warningWith(res.Warnings, "volume_defs: 1 dynamic volume(s) not restored", "no dynamic volume root") {
			t.Errorf("no missing-root warning: %v", res.Warnings)
		}
	})
}

// A snapshot taken before this section existed restores as it always did.
func TestVolumeDefs_WithoutSectionRestoresAsBefore(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	dstRoot := volumeRoot(t)
	plantVolume(t, src, volumeRoot(t), "acme", "data", "rw")
	raw := withoutSection(t, mustCapture(t, src), migrations.SectionVolumeDefs)
	var env struct {
		Sections map[string]json.RawMessage `json:"sections"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.Sections[migrations.SectionVolumeDefs]; ok {
		t.Fatal("the old-format fixture still has the volume_defs section")
	}
	res := mustRestore(t, dst, raw, volumeOpts(dstRoot))
	if res.Counts()["volume_defs"] != 0 || len(res.Warnings) != 0 {
		t.Errorf("old-format restore counted %v, warned %v", res.Counts(), res.Warnings)
	}
	if entries, _ := os.ReadDir(dstRoot); len(entries) != 0 {
		t.Errorf("old-format restore created %d entries under the root", len(entries))
	}
}
