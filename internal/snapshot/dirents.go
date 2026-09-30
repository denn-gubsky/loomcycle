package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The dirents section (RFC DP §4.9): the Path tree. A document's bodies travel
// in memory and its structure in sqlmem, but the NAME it is found by is a
// dirent; without this section a restored document is reachable only by id and
// is invisible in the Path and Library browsers.
//
// Restored LAST. A dirent names a document (memory + sqlmem), a memory entry
// (memory) or a volume (volume_defs), and sqlmem is the last of those to land.
// Every entry is checked against what the restore — or the target — actually
// holds, and a name whose target is not here is NOT restored: the tools never
// leave a dangling name behind (delete_document drops the names pointing at
// the document it deletes), and a restore does not introduce one. The skipped
// names are reported, grouped by tree and kind with a count, so the common case
// — a sqlmem section that did not restore, leaving every document name without
// its document — is one warning per tree rather than one per document. A later
// restore of the same snapshot into a host that has the documents brings the
// names back, since nothing was written for them.
//
// Directories are implicit: a name at /a/b/c makes /a and /a/b listable
// without a row for either, so restoring the leaves restores the directories.
// An explicit `directory` row (an empty folder made with mkdir) names nothing
// and always restores.
//
// The live row stands: a path already taken on the target is left alone. It is
// silent when the live entry names the same thing (a re-restore), and warns
// when it names something else.
//
// Every entry keeps its tree — tenant, scope, scope_id — verbatim, so a name
// never moves into another tenant's or user's tree; and resolution never
// widens anyway (a memory_entry resolves by key in the entry's own tree, a
// document by id inside the caller's scope, a mount by name in the caller's
// tenant).

// captureDirents reads every tree's entries, every tenant's.
func captureDirents(ctx context.Context, s store.Store, out *DirentsSection) error {
	out.Version = SectionVersion
	rows, err := s.SnapshotReadDirents(ctx)
	if err != nil {
		return fmt.Errorf("snapshot dirents: %w", err)
	}
	out.Entries = make([]DirentEntry, 0, len(rows))
	for _, r := range rows {
		out.Entries = append(out.Entries, DirentEntry{
			TenantID:    r.TenantID,
			Scope:       r.Scope,
			ScopeID:     r.ScopeID,
			ParentPath:  r.ParentPath,
			Name:        r.Name,
			Kind:        r.Kind,
			ResourceRef: r.ResourceRef,
			CreatedAt:   r.CreatedAt.UTC(),
			UpdatedAt:   r.UpdatedAt.UTC(),
		})
	}
	return nil
}

// direntGroup is the unit skipped names are reported in: one tree, one kind.
type direntGroup struct {
	tree, kind string
	// unchecked: the restore could not ask whether the document exists,
	// rather than asking and hearing no.
	unchecked bool
}

// restoreDirents inserts each name the target does not have whose target it
// does have.
func restoreDirents(ctx context.Context, s store.Store, sec *DirentsSection, opts RestoreOptions, result *RestoreResult) {
	if len(sec.Entries) == 0 {
		return
	}
	if opts.Validators[migrations.SectionDirents] == nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"dirents: %d path name(s) not restored: no dirents validator is wired on this restore, and a name is never restored unvalidated",
			len(sec.Entries)))
		return
	}
	static := make(map[string]bool, len(opts.StaticVolumeNames))
	for _, n := range opts.StaticVolumeNames {
		static[n] = true
	}
	skipped := map[direntGroup][]string{}
	for _, e := range sec.Entries {
		full := e.ParentPath + e.Name
		tree := direntTree(e)
		where := fmt.Sprintf("dirent %s (%s)", full, tree)
		body, err := json.Marshal(e)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: %v", where, err))
			continue
		}
		if !validRestoredBody(opts, migrations.SectionDirents, where, body, result) {
			continue
		}
		live, err := s.DirentGet(ctx, e.TenantID, e.Scope, e.ScopeID, e.ParentPath, e.Name)
		if err == nil {
			if !sameDirentTarget(live, e) {
				result.Warnings = append(result.Warnings, fmt.Sprintf(
					"%s: not restored: the path is taken here by a live %s entry that names something else, and it stands",
					where, live.Kind))
			}
			continue
		}
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: reading the live path: %v", where, err))
			continue
		}
		if e.Kind == "document" && opts.DocumentExists == nil {
			g := direntGroup{tree: tree, kind: e.Kind, unchecked: true}
			skipped[g] = append(skipped[g], full)
			continue
		}
		ok, err := direntTargetExists(ctx, s, opts, static, e)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%s: not restored: could not check that the %s it names exists here: %v", where, e.Kind, err))
			continue
		}
		if !ok {
			g := direntGroup{tree: tree, kind: e.Kind}
			skipped[g] = append(skipped[g], full)
			continue
		}
		inserted, err := s.SnapshotRestoreDirent(ctx, store.DirentRow{
			TenantID: e.TenantID, Scope: e.Scope, ScopeID: e.ScopeID,
			ParentPath: e.ParentPath, Name: e.Name, Kind: e.Kind, ResourceRef: e.ResourceRef,
			CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
		})
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: %v", where, err))
			continue
		}
		if !inserted {
			// Named on this host between the read above and the insert.
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: a live entry appeared at this path during the restore and stands", where))
			continue
		}
		result.DirentsRestored++
	}
	result.Warnings = append(result.Warnings, skippedDirentWarnings(skipped)...)
}

// direntTargetExists reports whether what the entry names is on this host,
// where the entry's tools would look for it.
func direntTargetExists(ctx context.Context, s store.Store, opts RestoreOptions, static map[string]bool, e DirentEntry) (bool, error) {
	var ref struct {
		DocumentID string `json:"document_id"`
		Key        string `json:"key"`
		VolumeName string `json:"volume_name"`
	}
	if len(e.ResourceRef) > 0 {
		if err := json.Unmarshal(e.ResourceRef, &ref); err != nil {
			return false, fmt.Errorf("resource_ref: %w", err)
		}
	}
	var nf *store.ErrNotFound
	switch e.Kind {
	case "directory":
		return true, nil // names nothing
	case "document":
		return opts.DocumentExists(ctx, e.TenantID, e.Scope, e.ScopeID, ref.DocumentID)
	case "memory_entry":
		// The Memory tool resolves a path's key in the entry's own tree.
		_, err := s.MemoryGet(ctx, e.TenantID, store.MemoryScope(e.Scope), e.ScopeID, ref.Key)
		if errors.As(err, &nf) {
			return false, nil
		}
		return err == nil, err
	case "volume_mount":
		// Resolution tries this host's static volumes first, then the
		// tenant's dynamic ones (restored earlier in this call).
		if static[ref.VolumeName] {
			return true, nil
		}
		_, err := s.VolumeDefGetByName(ctx, e.TenantID, ref.VolumeName)
		if errors.As(err, &nf) {
			return false, nil
		}
		return err == nil, err
	default:
		return false, fmt.Errorf("unknown kind %q", e.Kind)
	}
}

// sameDirentTarget reports whether the live entry names what the snapshot
// entry names. The refs are compared as JSON values: postgres stores jsonb,
// which does not keep the key order or spacing the snapshot carries.
func sameDirentTarget(live store.DirentRow, e DirentEntry) bool {
	if live.Kind != e.Kind {
		return false
	}
	decode := func(raw json.RawMessage) any {
		if len(raw) == 0 {
			raw = json.RawMessage("{}")
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return string(raw)
		}
		return v
	}
	return reflect.DeepEqual(decode(live.ResourceRef), decode(e.ResourceRef))
}

// direntTree names the tree an entry belongs to, for a warning.
func direntTree(e DirentEntry) string {
	tenant := "tenant " + e.TenantID
	if e.TenantID == "" {
		tenant = "the operator tenant"
	}
	if e.Scope == "tenant" {
		return tenant + ", tenant scope"
	}
	return fmt.Sprintf("%s, %s %s", tenant, e.Scope, e.ScopeID)
}

// maxNamedPaths bounds how many paths one skipped-names warning spells out;
// the count covers the rest.
const maxNamedPaths = 5

// skippedDirentWarnings renders one warning per (tree, kind), in a stable
// order.
func skippedDirentWarnings(skipped map[direntGroup][]string) []string {
	groups := make([]direntGroup, 0, len(skipped))
	for g := range skipped {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if a.tree != b.tree {
			return a.tree < b.tree
		}
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		return !a.unchecked && b.unchecked
	})
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		paths := skipped[g]
		named := paths
		more := ""
		if len(named) > maxNamedPaths {
			named = named[:maxNamedPaths]
			more = fmt.Sprintf(" and %d more", len(paths)-maxNamedPaths)
		}
		why := fmt.Sprintf("the %s each one names is not on this host, and a name is never restored pointing at nothing", g.kind)
		if g.unchecked {
			why = "this restore cannot check that the documents they name exist here (SQL Memory is not enabled on this host), and a name is never restored pointing at nothing"
		}
		out = append(out, fmt.Sprintf("dirents: %d %s name(s) in %s not restored: %s (%s%s)",
			len(paths), g.kind, g.tree, why, strings.Join(named, ", "), more))
	}
	return out
}
