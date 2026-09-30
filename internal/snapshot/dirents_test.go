package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// direntDocs is a fake document check: the documents "on this host", keyed by
// the dirent coordinates a restore asks with.
type direntDocs map[string]bool

func direntDocKey(tenant, scope, scopeID, id string) string {
	return tenant + "|" + scope + "|" + scopeID + "|" + id
}

func (d direntDocs) exists(_ context.Context, tenant, scope, scopeID, id string) (bool, error) {
	return d[direntDocKey(tenant, scope, scopeID, id)], nil
}

// direntRestoreOptions is what the production call sites pass for this
// section — the real Path-entry validator and a document check — plus a
// dynamic volume root, so a restored volume is there for its mount's name.
func direntRestoreOptions(t *testing.T, docs func(context.Context, string, string, string, string) (bool, error)) RestoreOptions {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return RestoreOptions{
		Validators: map[string]func(json.RawMessage) error{
			migrations.SectionDirents:    builtin.ValidateDirentEntry,
			migrations.SectionVolumeDefs: builtin.ValidateVolumeDefBody,
		},
		DocumentExists: docs,
		VolumeRoot:     root,
	}
}

var direntT0 = time.Date(2026, 7, 3, 4, 5, 6, 0, time.UTC)

func docRef(id string) json.RawMessage { return json.RawMessage(`{"document_id":"` + id + `"}`) }

// plantDirentTrees writes, on the source, one name of every kind across the
// operator tenant and two tenants, in agent, user and tenant trees — each
// with what it names — and returns the names plus the documents they point at
// (a fake document check stands in for SQL Memory here).
func plantDirentTrees(t *testing.T, s store.Store) ([]store.DirentRow, direntDocs) {
	t.Helper()
	ctx := context.Background()
	docs := direntDocs{}
	rows := []store.DirentRow{
		{TenantID: "", Scope: "user", ScopeID: "alice", ParentPath: "/documents/", Name: "launch", Kind: "document", ResourceRef: docRef("doc-op-alice")},
		{TenantID: "acme", Scope: "user", ScopeID: "alice", ParentPath: "/projects/q3/specs/", Name: "api", Kind: "document", ResourceRef: docRef("doc-acme-alice")},
		{TenantID: "acme", Scope: "tenant", ScopeID: "", ParentPath: "/handbook/", Name: "onboarding", Kind: "document", ResourceRef: docRef("doc-acme-tenant")},
		{TenantID: "beta", Scope: "agent", ScopeID: "helper", ParentPath: "/", Name: "notes", Kind: "document", ResourceRef: docRef("doc-beta-helper")},
		{TenantID: "acme", Scope: "user", ScopeID: "alice", ParentPath: "/prefs/", Name: "tea", Kind: "memory_entry",
			ResourceRef: json.RawMessage(`{"scope":"user","scope_id":"alice","key":"pref.tea","facet":"kv"}`)},
		{TenantID: "beta", Scope: "tenant", ScopeID: "", ParentPath: "/policy/", Name: "shipping", Kind: "memory_entry",
			ResourceRef: json.RawMessage(`{"scope":"tenant","scope_id":"","key":"policy.ship","facet":"kv"}`)},
		{TenantID: "acme", Scope: "tenant", ScopeID: "", ParentPath: "/vol/", Name: "data", Kind: "volume_mount",
			ResourceRef: json.RawMessage(`{"volume_name":"data","mode":"rw"}`)},
		{TenantID: "acme", Scope: "user", ScopeID: "bob", ParentPath: "/", Name: "empty-folder", Kind: "directory", ResourceRef: json.RawMessage(`{}`)},
	}
	for i := range rows {
		rows[i].CreatedAt = direntT0.Add(time.Duration(i) * time.Minute)
		rows[i].UpdatedAt = rows[i].CreatedAt.Add(time.Hour)
		if _, err := s.SnapshotRestoreDirent(ctx, rows[i]); err != nil {
			t.Fatalf("plant %s%s: %v", rows[i].ParentPath, rows[i].Name, err)
		}
		if rows[i].Kind == "document" {
			var ref struct {
				DocumentID string `json:"document_id"`
			}
			_ = json.Unmarshal(rows[i].ResourceRef, &ref)
			docs[direntDocKey(rows[i].TenantID, rows[i].Scope, rows[i].ScopeID, ref.DocumentID)] = true
		}
	}
	// What the memory_entry and volume_mount names point at. Memory travels in
	// its own section; the volume is a dynamic volume def.
	if err := s.MemorySet(ctx, "acme", store.MemoryScopeUser, "alice", "pref.tea", json.RawMessage(`"green"`), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.MemorySet(ctx, "beta", store.MemoryScopeTenant, "", "policy.ship", json.RawMessage(`"fridays"`), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VolumeDefCreate(ctx, store.VolumeDefRow{TenantID: "acme", Name: "data", Definition: json.RawMessage(`{"path":"/x","mode":"rw"}`)}); err != nil {
		t.Fatal(err)
	}
	return rows, docs
}

func direntWarnings(res RestoreResult) []string {
	var out []string
	for _, w := range res.Warnings {
		if strings.HasPrefix(w, "dirent") {
			out = append(out, w)
		}
	}
	return out
}

func sameDirent(a, b store.DirentRow) bool {
	return a.TenantID == b.TenantID && a.Scope == b.Scope && a.ScopeID == b.ScopeID && a.ParentPath == b.ParentPath &&
		a.Name == b.Name && a.Kind == b.Kind && sameDirentTarget(a, DirentEntry{Kind: b.Kind, ResourceRef: b.ResourceRef}) &&
		a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt)
}

// TestDirents_RoundTripKeepsEveryNameInItsOwnTree: every name comes back at
// its full coordinate — tenant, scope, scope id, path — with its kind, ref and
// both timestamps, is counted, and appears in no other tree: not another
// tenant's, not the operator tenant's, not another user's.
func TestDirents_RoundTripKeepsEveryNameInItsOwnTree(t *testing.T) {
	for _, b := range coverageBackends() {
		t.Run(b.name, func(t *testing.T) {
			src, dst := b.open(t), b.open(t)
			ctx := context.Background()
			want, docs := plantDirentTrees(t, src)

			_, raw, err := Capture(ctx, src, CaptureOptions{})
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			res, err := Restore(ctx, dst, raw, direntRestoreOptions(t, docs.exists))
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if res.DirentsRestored != len(want) || res.Counts()["dirents"] != len(want) {
				t.Errorf("dirents restored = %d (Counts %d), want %d; warnings %v",
					res.DirentsRestored, res.Counts()["dirents"], len(want), direntWarnings(res))
			}
			if w := direntWarnings(res); len(w) != 0 {
				t.Errorf("a clean restore warned: %v", w)
			}
			got, err := dst.SnapshotReadDirents(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Errorf("target holds %d names, want %d: %+v", len(got), len(want), got)
			}
			for _, w := range want {
				r, err := dst.DirentGet(ctx, w.TenantID, w.Scope, w.ScopeID, w.ParentPath, w.Name)
				if err != nil {
					t.Errorf("%s|%s|%s %s%s: not on the target: %v", w.TenantID, w.Scope, w.ScopeID, w.ParentPath, w.Name, err)
					continue
				}
				if !sameDirent(r, w) {
					t.Errorf("restored as %+v, want %+v", r, w)
				}
				// No other tree holds it.
				for _, other := range []struct{ tenant, scope, scopeID string }{
					{"", w.Scope, w.ScopeID}, {"acme", w.Scope, w.ScopeID}, {"beta", w.Scope, w.ScopeID},
					{w.TenantID, w.Scope, "mallory"},
				} {
					if other.tenant == w.TenantID && other.scopeID == w.ScopeID {
						continue
					}
					if _, err := dst.DirentGet(ctx, other.tenant, other.scope, other.scopeID, w.ParentPath, w.Name); err == nil {
						t.Errorf("%s%s of %s|%s|%s is also in tree %s|%s|%s", w.ParentPath, w.Name, w.TenantID, w.Scope, w.ScopeID,
							other.tenant, other.scope, other.scopeID)
					}
				}
			}
		})
	}
}

// orderedSqlMem is a SQL Memory stand-in that records which scopes a restore
// has written, so a document "exists" only once its scope has been restored.
type orderedSqlMem struct {
	scopes   []sqlmem.ScopeKey
	restored map[sqlmem.ScopeKey]bool
}

func (o *orderedSqlMem) Tier() string { return "sqlite" }
func (o *orderedSqlMem) ListScopes(context.Context) ([]sqlmem.ScopeKey, error) {
	return o.scopes, nil
}
func (o *orderedSqlMem) ExportScope(context.Context, sqlmem.ScopeKey) (*sqlmem.ScopeDump, error) {
	return &sqlmem.ScopeDump{}, nil
}
func (o *orderedSqlMem) RestoreScope(_ context.Context, key sqlmem.ScopeKey, _ *sqlmem.ScopeDump) error {
	o.restored[key] = true
	return nil
}

// TestDirents_RestoredAfterTheDocumentsTheyName: a document's structure lands
// with the sqlmem section, and its name must be restored after it — the only
// order in which the check can find the document. A document name restored any
// earlier would find nothing and be skipped.
func TestDirents_RestoredAfterTheDocumentsTheyName(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()
	if _, err := src.SnapshotRestoreDirent(ctx, store.DirentRow{TenantID: "acme", Scope: "user", ScopeID: "alice",
		ParentPath: "/documents/", Name: "launch", Kind: "document", ResourceRef: docRef("d1")}); err != nil {
		t.Fatal(err)
	}
	key := sqlmem.ScopeKey{Tenant: "acme", Scope: "user", ScopeID: "alice"}
	_, raw, err := Capture(ctx, src, CaptureOptions{SqlMem: &orderedSqlMem{scopes: []sqlmem.ScopeKey{key}}})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	target := &orderedSqlMem{restored: map[sqlmem.ScopeKey]bool{}}
	opts := direntRestoreOptions(t, func(_ context.Context, tenant, scope, scopeID, id string) (bool, error) {
		return target.restored[sqlmem.ScopeKey{Tenant: tenant, Scope: scope, ScopeID: scopeID}] && id == "d1", nil
	})
	opts.SqlMem = target
	res, err := Restore(ctx, dst, raw, opts)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.DirentsRestored != 1 {
		t.Fatalf("dirents restored = %d, want 1 (the name after its document); warnings %v", res.DirentsRestored, res.Warnings)
	}
	if _, err := dst.DirentGet(ctx, "acme", "user", "alice", "/documents/", "launch"); err != nil {
		t.Errorf("the document's name is not on the target: %v", err)
	}
}

// TestDirents_NameWhoseTargetIsMissingIsSkippedWithAWarning: a name whose
// document, memory entry or volume is not on the target is not restored — no
// dangling name — and each (tree, kind) says so once, with a count and the
// paths. A directory names nothing and still restores. Without SQL Memory no
// document can be here, which is its own warning.
func TestDirents_NameWhoseTargetIsMissingIsSkippedWithAWarning(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	ctx := context.Background()
	plant := func(r store.DirentRow) {
		t.Helper()
		if _, err := src.SnapshotRestoreDirent(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 7; i++ {
		plant(store.DirentRow{TenantID: "acme", Scope: "user", ScopeID: "alice", ParentPath: "/docs/",
			Name: fmt.Sprintf("gone-%d", i), Kind: "document", ResourceRef: docRef(fmt.Sprintf("missing-%d", i))})
	}
	plant(store.DirentRow{TenantID: "acme", Scope: "user", ScopeID: "alice", ParentPath: "/docs/", Name: "kept",
		Kind: "document", ResourceRef: docRef("present")})
	plant(store.DirentRow{TenantID: "acme", Scope: "user", ScopeID: "alice", ParentPath: "/prefs/", Name: "tea",
		Kind: "memory_entry", ResourceRef: json.RawMessage(`{"scope":"user","scope_id":"alice","key":"no.such.key","facet":"kv"}`)})
	plant(store.DirentRow{TenantID: "acme", Scope: "tenant", ParentPath: "/vol/", Name: "gone",
		Kind: "volume_mount", ResourceRef: json.RawMessage(`{"volume_name":"gone","mode":"rw"}`)})
	plant(store.DirentRow{TenantID: "acme", Scope: "tenant", ParentPath: "/vol/", Name: "repo",
		Kind: "volume_mount", ResourceRef: json.RawMessage(`{"volume_name":"repo","mode":"ro"}`)})
	plant(store.DirentRow{TenantID: "acme", Scope: "user", ScopeID: "alice", ParentPath: "/", Name: "folder",
		Kind: "directory", ResourceRef: json.RawMessage(`{}`)})
	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	dst, dstClose := newTestStore(t)
	defer dstClose()
	docs := direntDocs{direntDocKey("acme", "user", "alice", "present"): true}
	opts := direntRestoreOptions(t, docs.exists)
	opts.StaticVolumeNames = []string{"repo"} // resolves from this host's yaml
	res, err := Restore(ctx, dst, raw, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.DirentsRestored != 3 {
		t.Errorf("dirents restored = %d, want 3 (the present document, the static mount, the folder)", res.DirentsRestored)
	}
	all, _ := dst.SnapshotReadDirents(ctx)
	for _, r := range all {
		if strings.HasPrefix(r.Name, "gone") || r.Name == "tea" {
			t.Errorf("%s%s was restored pointing at nothing", r.ParentPath, r.Name)
		}
	}
	w := direntWarnings(res)
	if len(w) != 3 {
		t.Fatalf("dirent warnings = %d, want one per (tree, kind): %v", len(w), w)
	}
	has := func(parts ...string) bool {
		for _, x := range w {
			ok := true
			for _, p := range parts {
				ok = ok && strings.Contains(x, p)
			}
			if ok {
				return true
			}
		}
		return false
	}
	if !has("7 document name(s)", "tenant acme, user alice", "/docs/gone-0", "and 2 more") {
		t.Errorf("no grouped warning for the seven missing documents: %v", w)
	}
	if !has("1 memory_entry name(s)", "/prefs/tea") {
		t.Errorf("no warning for the missing memory entry: %v", w)
	}
	if !has("1 volume_mount name(s)", "tenant acme, tenant scope", "/vol/gone") {
		t.Errorf("no warning for the missing volume: %v", w)
	}

	// No SQL Memory here: every document name is skipped, and the warning
	// says the restore could not check rather than that the documents are gone.
	dst2, dst2Close := newTestStore(t)
	defer dst2Close()
	res, err = Restore(ctx, dst2, raw, direntRestoreOptions(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	w = direntWarnings(res)
	if !has("8 document name(s)", "cannot check", "SQL Memory is not enabled") {
		t.Errorf("no warning that document names could not be checked: %v", w)
	}
	if rows, _ := dst2.SnapshotReadDirents(ctx); len(rows) != 1 || rows[0].Name != "folder" {
		t.Errorf("without SQL Memory the target holds %+v, want only the folder", rows)
	}
}

// TestDirents_LiveEntryStands: a path already taken on the target keeps its
// entry. One naming something else is reported; one naming the same thing — a
// re-restore — is silent, and a second restore of the same envelope writes and
// says nothing.
func TestDirents_LiveEntryStands(t *testing.T) {
	for _, b := range coverageBackends() {
		t.Run(b.name, func(t *testing.T) {
			src, dst := b.open(t), b.open(t)
			ctx := context.Background()
			want, docs := plantDirentTrees(t, src)
			_, raw, err := Capture(ctx, src, CaptureOptions{})
			if err != nil {
				t.Fatal(err)
			}
			first, err := Restore(ctx, dst, raw, direntRestoreOptions(t, docs.exists))
			if err != nil || first.DirentsRestored != len(want) {
				t.Fatalf("first restore: %d restored, err %v", first.DirentsRestored, err)
			}
			again, err := Restore(ctx, dst, raw, direntRestoreOptions(t, docs.exists))
			if err != nil {
				t.Fatal(err)
			}
			if again.DirentsRestored != 0 || len(direntWarnings(again)) != 0 {
				t.Errorf("an identical re-restore restored %d and warned %v; want nothing", again.DirentsRestored, direntWarnings(again))
			}

			// Re-point one live name at another document on the target.
			moved := want[1]
			live, err := dst.DirentCreate(ctx, store.DirentRow{TenantID: moved.TenantID, Scope: moved.Scope, ScopeID: moved.ScopeID,
				ParentPath: moved.ParentPath, Name: moved.Name, Kind: "document", ResourceRef: docRef("doc-live")})
			if err != nil {
				t.Fatal(err)
			}
			res, err := Restore(ctx, dst, raw, direntRestoreOptions(t, docs.exists))
			if err != nil {
				t.Fatal(err)
			}
			w := direntWarnings(res)
			if res.DirentsRestored != 0 || len(w) != 1 || !strings.Contains(w[0], moved.ParentPath+moved.Name) ||
				!strings.Contains(w[0], "names something else") {
				t.Errorf("restore onto a re-pointed name: restored %d, warnings %v; want 0 and one warning naming %s%s",
					res.DirentsRestored, w, moved.ParentPath, moved.Name)
			}
			got, err := dst.DirentGet(ctx, moved.TenantID, moved.Scope, moved.ScopeID, moved.ParentPath, moved.Name)
			if err != nil || !sameDirentTarget(got, DirentEntry{Kind: "document", ResourceRef: docRef("doc-live")}) ||
				!got.UpdatedAt.Equal(live.UpdatedAt) {
				t.Errorf("the live name after restore = %+v (err %v); want it untouched, naming doc-live", got, err)
			}
		})
	}
}

// TestDirents_ImplicitParentDirectoriesAreListable: only leaves travel, and
// the Path tool lists the directories above a restored name from the name
// itself — ls of the root and of each ancestor shows the next segment.
func TestDirents_ImplicitParentDirectoriesAreListable(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()
	want, docs := plantDirentTrees(t, src)
	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := Restore(ctx, dst, raw, direntRestoreOptions(t, docs.exists)); err != nil || res.DirentsRestored != len(want) {
		t.Fatalf("restore: %d restored, err %v", res.DirentsRestored, err)
	}

	p := &builtin.Path{Store: dst}
	runCtx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "alice", TenantID: "acme"})
	ls := func(path string) map[string]string {
		t.Helper()
		in, _ := json.Marshal(map[string]any{"op": "ls", "scope": "user", "path": path})
		res, err := p.Execute(runCtx, in)
		if err != nil || res.IsError {
			t.Fatalf("ls %s: %v %s", path, err, res.Text)
		}
		var out struct {
			Entries []struct{ Name, Kind string } `json:"entries"`
		}
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, e := range out.Entries {
			m[e.Name] = e.Kind
		}
		return m
	}
	// acme/alice holds /projects/q3/specs/api and /prefs/tea.
	for path, child := range map[string]string{"/": "projects", "/projects": "q3", "/projects/q3": "specs"} {
		if got := ls(path); got[child] != "directory" {
			t.Errorf("ls %s = %v, want the implicit directory %s", path, got, child)
		}
	}
	if got := ls("/projects/q3/specs"); got["api"] != "document" {
		t.Errorf("ls /projects/q3/specs = %v, want the restored document api", got)
	}
	if got := ls("/"); got["prefs"] != "directory" || len(got) != 2 {
		t.Errorf("ls / = %v, want exactly projects and prefs (no other user's or tenant's names)", got)
	}
}

// TestDirents_RefusedAndUnvalidatedEntriesAreNotRestored: an entry the Path
// rules refuse is skipped with a warning naming it, and with no validator
// wired no name is restored at all.
func TestDirents_RefusedAndUnvalidatedEntriesAreNotRestored(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()
	for _, r := range []store.DirentRow{
		{TenantID: "acme", Scope: "user", ScopeID: "alice", ParentPath: "/", Name: "ok", Kind: "directory"},
		// Rows no tool writes: an escaping parent, and a tenant-scope row keyed
		// the way SQL Memory keys it (invisible to the Path tool).
		{TenantID: "acme", Scope: "user", ScopeID: "alice", ParentPath: "/../etc/", Name: "passwd", Kind: "directory"},
		{TenantID: "acme", Scope: "tenant", ScopeID: "acme", ParentPath: "/", Name: "legacy", Kind: "directory"},
	} {
		if _, err := src.SnapshotRestoreDirent(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if w := direntWarnings(res); res.DirentsRestored != 0 || len(w) != 1 || !strings.Contains(w[0], "no dirents validator") {
		t.Errorf("unwired: restored %d, warnings %v; want none restored and one warning", res.DirentsRestored, w)
	}

	res, err = Restore(ctx, dst, raw, direntRestoreOptions(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	w := direntWarnings(res)
	if res.DirentsRestored != 1 || len(w) != 2 {
		t.Fatalf("wired: restored %d, warnings %v; want only /ok, and two refusals", res.DirentsRestored, w)
	}
	for _, name := range []string{"/../etc/passwd", "/legacy"} {
		found := false
		for _, x := range w {
			found = found || (strings.Contains(x, name) && strings.Contains(x, "fails the validation"))
		}
		if !found {
			t.Errorf("no refusal names %s: %v", name, w)
		}
	}
}

// TestRestore_WithoutDirentsSectionRestoresAsBefore: a snapshot taken before
// the section existed restores everything else exactly as before, with no
// name and no warning about names.
func TestRestore_WithoutDirentsSectionRestoresAsBefore(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()
	_, docs := plantDirentTrees(t, src)
	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	old := withoutSection(t, raw, migrations.SectionDirents)
	res, err := Restore(ctx, dst, old, direntRestoreOptions(t, docs.exists))
	if err != nil {
		t.Fatal(err)
	}
	if res.DirentsRestored != 0 || len(direntWarnings(res)) != 0 {
		t.Errorf("old snapshot: restored %d names, warnings %v; want neither", res.DirentsRestored, direntWarnings(res))
	}
	if rows, _ := dst.SnapshotReadDirents(ctx); len(rows) != 0 {
		t.Errorf("old snapshot left names on the target: %+v", rows)
	}
	if res.MemoryRestored != 2 || res.VolumeDefsRestored != 1 {
		// What the names point at still travels, exactly as before.
		t.Errorf("the other sections changed: memory %d, volumes %d", res.MemoryRestored, res.VolumeDefsRestored)
	}
}
