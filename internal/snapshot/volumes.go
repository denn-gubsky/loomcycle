package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/dynvol"
	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The volume_defs section (RFC DP §4.5): persistent dynamic volumes.
//
// A volume row's definition is {path, mode}, and lookup hands that path to
// the file and exec tools verbatim. Carrying it would make a snapshot an
// arbitrary filesystem grant — a hand-edited "/etc" would come back as a
// volume an agent could write to. So an entry carries the name and mode only,
// and restore derives the path under the TARGET's dynamic root through the
// same fenced helper the VolumeDef tool's create uses, which also creates the
// empty directory (0700). A row is written only once its directory exists,
// so the table never names a volume with nothing behind it.
//
// Contents never travel: a restored volume is an empty directory, and each
// restored volume says so in a warning.
//
// Each entry is skipped, with a warning, when:
//   - this host has no dynamic root (the whole section, once);
//   - the injected validator is missing or refuses it (name, mode, tenant);
//   - the name is a static yaml volume here, which is ground truth and would
//     shadow it at resolution anyway, so no dormant row is written;
//   - the directory cannot be provisioned (fence refused, mkdir failed).
//
// The live row stands: a volume already on (tenant, name) keeps its row and
// its directory is not touched. It is silent when the live row is what this
// restore would have written, and warns when it differs.

// captureVolumeDefs reads every tenant's volume rows and keeps each one's
// mode. The stored path is read and dropped.
func captureVolumeDefs(ctx context.Context, s store.Store, out *VolumeDefsSection) error {
	out.Version = SectionVersion
	rows, err := s.SnapshotReadVolumeDefs(ctx)
	if err != nil {
		return fmt.Errorf("snapshot volume_defs: %w", err)
	}
	out.Entries = make([]VolumeDefEntry, 0, len(rows))
	for _, r := range rows {
		out.Entries = append(out.Entries, VolumeDefEntry{
			TenantID:  r.TenantID,
			Name:      r.Name,
			Mode:      capturedVolumeMode(r.Definition),
			CreatedAt: r.CreatedAt.UTC(),
			UpdatedAt: r.UpdatedAt.UTC(),
		})
	}
	return nil
}

// capturedVolumeMode is the mode the source resolves the volume with: lookup
// reads an empty mode as rw. A body that does not decode is unresolvable on
// the source, so it travels with no mode and the restore validator refuses it
// rather than bringing back a volume the source could not use.
func capturedVolumeMode(def json.RawMessage) string {
	var b dynvol.Body
	if err := json.Unmarshal(def, &b); err != nil {
		return ""
	}
	if b.Mode == "" {
		return "rw"
	}
	return b.Mode
}

// restoreVolumeDefs provisions and inserts each volume the target does not
// have, under the target's own dynamic root.
func restoreVolumeDefs(ctx context.Context, s store.Store, sec *VolumeDefsSection, opts RestoreOptions, result *RestoreResult) {
	if len(sec.Entries) == 0 {
		return
	}
	if opts.VolumeRoot == "" {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"volume_defs: %d dynamic volume(s) not restored: this host has no dynamic volume root (a static volume marked dynamic_root: true)",
			len(sec.Entries)))
		return
	}
	static := make(map[string]bool, len(opts.StaticVolumeNames))
	for _, n := range opts.StaticVolumeNames {
		static[n] = true
	}
	for _, e := range sec.Entries {
		where := "volume_def " + qualifiedName(e.TenantID, e.Name)
		// The validator sees the entry as decoded: a path a hand-edited
		// envelope added is already gone.
		entry, err := json.Marshal(e)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: %v", where, err))
			continue
		}
		if !validRestoredBody(opts, migrations.SectionVolumeDefs, where, entry, result) {
			continue
		}
		if static[e.Name] {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%s: not restored: this host declares a static volume %q, which takes precedence over a dynamic one of that name", where, e.Name))
			continue
		}
		derived := dynvol.DerivedPath(opts.VolumeRoot, e.TenantID, e.Name)
		live, err := s.VolumeDefGetByName(ctx, e.TenantID, e.Name)
		if err == nil {
			// The live row stands and its directory is not touched.
			if w := liveVolumeDiffers(where, live, derived, e.Mode, opts.VolumeRoot); w != "" {
				result.Warnings = append(result.Warnings, w)
			}
			continue
		}
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: reading the live volume: %v", where, err))
			continue
		}
		path, created, err := dynvol.Provision(opts.VolumeRoot, e.TenantID, e.Name)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%s: not restored: its directory could not be provisioned: %s", where, withoutVolumeRoot(err.Error(), opts.VolumeRoot)))
			continue
		}
		if created {
			result.VolumeDirsCreated++
		}
		def, err := json.Marshal(dynvol.Body{Path: path, Mode: e.Mode})
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: %v", where, err))
			continue
		}
		inserted, err := s.SnapshotRestoreVolumeDef(ctx, store.VolumeDefRow{
			TenantID: e.TenantID, Name: e.Name, Definition: def, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
		})
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: %v", where, err))
			continue
		}
		if !inserted {
			// Created on this host between the read above and the insert.
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: a live volume of that name appeared during the restore and stands", where))
			continue
		}
		result.VolumeDefsRestored++
		at := withoutVolumeRoot(path, opts.VolumeRoot)
		if created {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%s: restored as an empty directory at %s; volume contents are not carried in a snapshot", where, at))
		} else {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%s: restored onto the directory already at %s, as found; volume contents are not carried in a snapshot", where, at))
		}
	}
}

// liveVolumeDiffers explains how the live row on the snapshot's key differs
// from the one this restore would have written, or returns "" when it is the
// same volume (a re-restore).
func liveVolumeDiffers(where string, live store.VolumeDefRow, derived, mode, root string) string {
	var b dynvol.Body
	_ = json.Unmarshal(live.Definition, &b)
	liveMode := b.Mode
	if liveMode == "" {
		liveMode = "rw"
	}
	var diffs []string
	if liveMode != mode {
		diffs = append(diffs, fmt.Sprintf("mode %s here, %s in the snapshot", liveMode, mode))
	}
	if !sameVolumePath(root, b.Path, derived) {
		diffs = append(diffs, "its directory is not the one this host derives for it")
	}
	if len(diffs) == 0 {
		return ""
	}
	return fmt.Sprintf("%s: not restored: a live volume of that name stands (%s)", where, strings.Join(diffs, "; "))
}

// sameVolumePath reports whether a live row's path and the path this host
// derives name one volume. A row written while the root was configured by its
// real path, and the path derived under a symlink to that root, are the same
// volume; comparing the spellings alone called them different. Only the
// root's own symlink is followed: a row naming a symlink below the root still
// differs.
func sameVolumePath(root, live, derived string) bool {
	if filepath.Clean(live) == filepath.Clean(derived) {
		return true
	}
	l, errL := dynvol.UnderResolvedRoot(root, live)
	d, errD := dynvol.UnderResolvedRoot(root, derived)
	return errL == nil && errD == nil && l == d
}

// withoutVolumeRoot names a path under the dynamic root relative to it, so a
// warning does not spell out the host's filesystem layout.
func withoutVolumeRoot(msg, root string) string {
	const label = "<dynamic root>"
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		msg = strings.ReplaceAll(msg, resolved, label)
	}
	return strings.ReplaceAll(msg, filepath.Clean(root), label)
}
