package migrations

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestMigrate_SameVersionIdentity — the common path: snapshot's
// section version equals CurrentVersion, migrator returns raw bytes
// unchanged.
func TestMigrate_SameVersionIdentity(t *testing.T) {
	for _, section := range []string{
		SectionAgentDefs, SectionAgentDefActive,
		SectionSkillDefs, SectionSkillDefActive,
		SectionTeamDefs, SectionTeamDefActive,
		SectionMemory,
		SectionChannels, SectionEvaluations, SectionPausedRuns,
		SectionInteractionHistory,
	} {
		t.Run(section, func(t *testing.T) {
			input := json.RawMessage(`{"version":"1.0","entries":[{"x":1}]}`)
			got, err := Migrate(section, "1.0", input)
			if err != nil {
				t.Fatalf("Migrate(%s, 1.0): %v", section, err)
			}
			if string(got) != string(input) {
				t.Errorf("identity migrator modified bytes:\n got: %s\nwant: %s", got, input)
			}
		})
	}
}

// TestMigrate_NewerVersionRejected — section version newer than the
// reader's CurrentVersion returns *ErrSnapshotVersionTooNew with
// both version strings + section name.
func TestMigrate_NewerVersionRejected(t *testing.T) {
	_, err := Migrate(SectionMemory, "9.99", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error on newer version, got nil")
	}
	var tooNew *ErrSnapshotVersionTooNew
	if !errors.As(err, &tooNew) {
		t.Errorf("err = %v, want *ErrSnapshotVersionTooNew", err)
	}
	if tooNew.Section != SectionMemory {
		t.Errorf("Section = %q, want %q", tooNew.Section, SectionMemory)
	}
	if tooNew.SnapshotVersion != "9.99" {
		t.Errorf("SnapshotVersion = %q, want %q", tooNew.SnapshotVersion, "9.99")
	}
	if tooNew.ReaderVersion != CurrentVersion {
		t.Errorf("ReaderVersion = %q, want %q", tooNew.ReaderVersion, CurrentVersion)
	}
}

// TestMigrate_UnknownSectionReturnsTypedError — section name not
// in the registry returns *ErrUnknownSectionVersion (distinct from
// version-too-new so callers can branch).
func TestMigrate_UnknownSectionReturnsTypedError(t *testing.T) {
	_, err := Migrate("not_a_section", "1.0", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error on unknown section, got nil")
	}
	var unknown *ErrUnknownSectionVersion
	if !errors.As(err, &unknown) {
		t.Errorf("err = %v, want *ErrUnknownSectionVersion", err)
	}
	if unknown.Section != "not_a_section" {
		t.Errorf("Section = %q", unknown.Section)
	}
}

// TestMigrate_UnknownVersionForKnownSectionRejected — section is
// registered but the snapshot's version string isn't in its map.
// Different from "newer than current" — this is "corrupted or
// pre-history snapshot from a version we never supported."
func TestMigrate_UnknownVersionForKnownSectionRejected(t *testing.T) {
	_, err := Migrate(SectionMemory, "0.1", json.RawMessage(`{}`))
	// "0.1" < "1.0" so we go down the "older known version" branch,
	// which today (no registered migrators for "0.1") surfaces as
	// either *ErrUnknownSectionVersion or the "not implemented"
	// error. Both are acceptable signals to the operator; just
	// verify it's NOT silently accepted.
	if err == nil {
		t.Fatal("missing-version-in-registry must fail")
	}
}

// TestErrSnapshotVersionTooNew_MessageContent — error message must
// name the section, both versions, and suggest a remediation. This
// is the operator's first contact with the failure mode; clarity
// matters.
func TestErrSnapshotVersionTooNew_MessageContent(t *testing.T) {
	err := &ErrSnapshotVersionTooNew{
		Section:         "memory",
		SnapshotVersion: "2.0",
		ReaderVersion:   "1.0",
	}
	msg := err.Error()
	for _, want := range []string{"memory", "2.0", "1.0", "upgrade"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing %q", msg, want)
		}
	}
}

// TestErrUnknownSectionVersion_MessageContent — error message names
// the section + version, with a "corrupted snapshot or unsupported
// source" hint that helps operators diagnose.
func TestErrUnknownSectionVersion_MessageContent(t *testing.T) {
	err := &ErrUnknownSectionVersion{Section: "memory", Version: "9.99"}
	msg := err.Error()
	for _, want := range []string{"memory", "9.99", "corrupted"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing %q", msg, want)
		}
	}
}
