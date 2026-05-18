// Package migrations holds per-section cross-version migrators for
// the v0.8.17 snapshot envelope. The locked design (see
// doc-internal/rfcs/pause-resume-snapshot.md § "Format migration
// rule") evolves each section's schema independently: bumping the
// outer envelope's schema_version is reserved for structurally
// breaking changes; per-section sub-schema changes use version
// strings keyed in this registry.
//
// Today every section is at "1.0" so the registry's Get() returns
// identity migrators for the matching case (no transformation
// needed). The framework is in place so when a section bumps to
// "1.1" (e.g., adding a required field), the new migrator from
// "1.0" → "1.1" lands here without touching the snapshot package
// itself.
package migrations

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// CurrentVersion is the reader's declared section version. Each
// section uses the same string today; future per-section divergence
// is allowed (the registry is per-section).
const CurrentVersion = "1.0"

// Section names — wire-stable strings used as keys in
// Envelope.Sections JSON. Mirrored from snapshot.Envelope's struct
// tags; we redeclare here to avoid an import cycle (migrations
// imported by snapshot/restore.go, NOT the other way around).
const (
	SectionAgentDefs          = "agent_defs"
	SectionAgentDefActive     = "agent_def_active"
	SectionMemory             = "memory"
	SectionChannels           = "channels"
	SectionEvaluations        = "evaluations"
	SectionPausedRuns         = "paused_runs"
	SectionInteractionHistory = "interaction_history"
)

// Migrator transforms a section's raw JSON bytes from one version
// to the next. Identity migrators (fromVersion == toVersion) pass
// through unchanged. A migrator chain (e.g. "0.9" → "1.0" → "1.1")
// is applied in sequence by Migrate().
type Migrator func(raw json.RawMessage) (json.RawMessage, error)

// ErrSnapshotVersionTooNew is returned by Migrate when a section's
// declared version is newer than the reader's CurrentVersion.
// Operators get the section name + both version strings so they
// can decide to upgrade the reader or capture a fresh snapshot
// from the source instance.
type ErrSnapshotVersionTooNew struct {
	Section         string
	SnapshotVersion string
	ReaderVersion   string
}

func (e *ErrSnapshotVersionTooNew) Error() string {
	return fmt.Sprintf("snapshot: section %q is version %s but reader supports %s — upgrade loomcycle or re-capture from source",
		e.Section, e.SnapshotVersion, e.ReaderVersion)
}

// ErrUnknownSectionVersion is returned when a section's version
// string isn't in the migration registry at all (not even as an
// older known version). Distinguishes a corrupted / hand-edited
// snapshot from a forward-version situation.
type ErrUnknownSectionVersion struct {
	Section string
	Version string
}

func (e *ErrUnknownSectionVersion) Error() string {
	return fmt.Sprintf("snapshot: section %q has unknown version %q — corrupted snapshot or unsupported source",
		e.Section, e.Version)
}

// registry is the per-section map of from-version → migrator. Each
// section's entry produces the version listed in CurrentVersion when
// chained from oldest known version forward. v0.8.17 ships with
// only "1.0" entries (identity migrators) — the framework's first
// real use will be a "1.1" addition when a section's required-field
// shape changes.
//
// IMPORTANT: when adding a new version (e.g. "1.1"), register the
// "1.0" → "1.1" migrator AND bump CurrentVersion to "1.1". The
// registry walks from the snapshot's declared version forward to
// CurrentVersion; intermediate versions need their own migrators
// in the chain.
var registry = map[string]map[string]Migrator{
	SectionAgentDefs:          {"1.0": identityMigrator},
	SectionAgentDefActive:     {"1.0": identityMigrator},
	SectionMemory:             {"1.0": identityMigrator},
	SectionChannels:           {"1.0": identityMigrator},
	SectionEvaluations:        {"1.0": identityMigrator},
	SectionPausedRuns:         {"1.0": identityMigrator},
	SectionInteractionHistory: {"1.0": identityMigrator},
}

// identityMigrator is the no-op migrator. Used when fromVersion ==
// CurrentVersion (the common case in v0.8.17).
func identityMigrator(raw json.RawMessage) (json.RawMessage, error) {
	return raw, nil
}

// Migrate transforms a section's raw JSON from snapshotVersion up to
// CurrentVersion. Returns:
//   - The raw unchanged when snapshotVersion == CurrentVersion.
//   - *ErrSnapshotVersionTooNew when snapshotVersion > CurrentVersion.
//   - *ErrUnknownSectionVersion when snapshotVersion isn't registered.
//   - The migrated raw + nil when one or more migrators ran cleanly.
//
// The walk is deterministic and version-string-based; today every
// section has only "1.0" so the only valid input is "1.0". Future
// additions (e.g. "1.1") chain forward from older versions.
func Migrate(section, snapshotVersion string, raw json.RawMessage) (json.RawMessage, error) {
	sectionRegistry, ok := registry[section]
	if !ok {
		return nil, &ErrUnknownSectionVersion{Section: section, Version: snapshotVersion}
	}
	// Same version → identity. Cheap and common.
	if snapshotVersion == CurrentVersion {
		mig, ok := sectionRegistry[snapshotVersion]
		if !ok {
			return nil, &ErrUnknownSectionVersion{Section: section, Version: snapshotVersion}
		}
		return mig(raw)
	}
	// Newer than reader → operator-actionable error.
	if compareVersions(snapshotVersion, CurrentVersion) > 0 {
		return nil, &ErrSnapshotVersionTooNew{
			Section:         section,
			SnapshotVersion: snapshotVersion,
			ReaderVersion:   CurrentVersion,
		}
	}
	// Older known version → would walk migrator chain. v0.8.17 has
	// only "1.0" so this branch is unreachable today; ship the
	// framework with a clear "not implemented" so a future schema
	// change can land its migrator without changing the dispatch
	// logic.
	if _, ok := sectionRegistry[snapshotVersion]; !ok {
		return nil, &ErrUnknownSectionVersion{Section: section, Version: snapshotVersion}
	}
	// When a real migration chain exists, walk it here: iterate
	// from snapshotVersion forward to CurrentVersion, applying each
	// registered migrator in sequence.
	return nil, fmt.Errorf("snapshot: section %q version %q is older than current %q but no migration chain registered yet",
		section, snapshotVersion, CurrentVersion)
}

// compareVersions does a numeric per-component compare on
// dot-separated version strings. Returns -1, 0, or +1.
//
// Components that fail to parse as int compare as 0 (e.g. "1.x" vs
// "1.0" tie on the second component, which is the cautious behaviour
// — we don't want a malformed version to silently sort above or
// below a real one and flip the ErrSnapshotVersionTooNew gate).
//
// Per-component numeric ordering matters: lexicographic "1.10" <
// "1.9" would make a reader at v1.10 wrongly reject a valid older
// snapshot at v1.9 as "too new" once minor versions reach 10.
func compareVersions(a, b string) int {
	if a == b {
		return 0
	}
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	n := len(aParts)
	if len(bParts) > n {
		n = len(bParts)
	}
	for i := 0; i < n; i++ {
		var ax, bx int
		if i < len(aParts) {
			ax, _ = strconv.Atoi(aParts[i])
		}
		if i < len(bParts) {
			bx, _ = strconv.Atoi(bParts[i])
		}
		if ax < bx {
			return -1
		}
		if ax > bx {
			return 1
		}
	}
	return 0
}
