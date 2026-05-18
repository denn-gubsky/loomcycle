package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// newTestStore builds an in-memory SQLite store for snapshot tests.
func newTestStore(t *testing.T) (store.Store, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	return s, func() { _ = s.Close() }
}

// TestCapture_EmptyStoreReturnsAllSevenSections — a fresh store
// produces a valid envelope with every section present and empty.
// Pins the structural contract: section keys must exist even when
// there's nothing to serialise (so restore can rely on the shape).
func TestCapture_EmptyStoreReturnsAllSevenSections(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	row, jsonBytes, err := Capture(context.Background(), s, CaptureOptions{})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if row.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", row.SchemaVersion, SchemaVersion)
	}
	if row.ByteSize != int64(len(jsonBytes)) {
		t.Errorf("ByteSize = %d, want %d", row.ByteSize, len(jsonBytes))
	}
	if !strings.HasPrefix(row.ID, "snap_") {
		t.Errorf("ID = %q, want prefix snap_", row.ID)
	}

	// Decode the envelope and check every section is present.
	var env Envelope
	if err := json.Unmarshal(jsonBytes, &env); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if env.SchemaVersion != SchemaVersion {
		t.Errorf("envelope SchemaVersion = %d", env.SchemaVersion)
	}
	if env.Sections.AgentDefs.Version != SectionVersion {
		t.Errorf("AgentDefs.Version = %q", env.Sections.AgentDefs.Version)
	}
	if env.Sections.AgentDefActive.Version != SectionVersion {
		t.Errorf("AgentDefActive.Version = %q", env.Sections.AgentDefActive.Version)
	}
	if env.Sections.Memory.Version != SectionVersion {
		t.Errorf("Memory.Version = %q", env.Sections.Memory.Version)
	}
	if env.Sections.Channels.Version != SectionVersion {
		t.Errorf("Channels.Version = %q", env.Sections.Channels.Version)
	}
	if env.Sections.Evaluations.Version != SectionVersion {
		t.Errorf("Evaluations.Version = %q", env.Sections.Evaluations.Version)
	}
	if env.Sections.PausedRuns.Version != SectionVersion {
		t.Errorf("PausedRuns.Version = %q", env.Sections.PausedRuns.Version)
	}
	// InteractionHistory is omitted by default (opt-in).
	if env.Sections.InteractionHistory != nil {
		t.Errorf("InteractionHistory should be nil when IncludeHistory=false")
	}
}

// TestCapture_IncludeHistoryAddsSection — opt-in flag adds the
// interaction_history section to the envelope.
func TestCapture_IncludeHistoryAddsSection(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	row, jsonBytes, err := Capture(context.Background(), s, CaptureOptions{
		IncludeHistory:      true,
		IncludeHistorySince: time.Now().Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	_ = row

	var env Envelope
	if err := json.Unmarshal(jsonBytes, &env); err != nil {
		t.Fatal(err)
	}
	if env.Sections.InteractionHistory == nil {
		t.Fatal("InteractionHistory missing when IncludeHistory=true")
	}
	if env.Sections.InteractionHistory.Version != SectionVersion {
		t.Errorf("InteractionHistory.Version = %q", env.Sections.InteractionHistory.Version)
	}
}

// TestCapture_MemoryEmbeddingIsNull_Phase1 pins the Phase-1
// forward-compat shape: every memory row's `embedding` field
// serialises as JSON null (not omitted, not empty object) so the
// Phase-2 vector ops can land without a v1.0 → v1.1 migration.
func TestCapture_MemoryEmbeddingIsNull_Phase1(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	// Seed two memory rows in different scopes.
	if err := s.MemorySet(ctx, store.MemoryScope("agent"), "agentA", "k1", json.RawMessage(`"v1"`), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.MemorySet(ctx, store.MemoryScope("user"), "userB", "k2", json.RawMessage(`"v2"`), 0); err != nil {
		t.Fatal(err)
	}

	_, jsonBytes, err := Capture(ctx, s, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Verify the raw JSON contains "embedding":null for both rows.
	// Counting occurrences pins both rows + the explicit-null
	// (not omitted) contract.
	occurrences := strings.Count(string(jsonBytes), `"embedding":null`)
	if occurrences != 2 {
		t.Errorf(`got %d occurrences of "embedding":null, want 2 (one per memory row); envelope:\n%s`,
			occurrences, jsonBytes)
	}
}

// TestCapture_SizeCapEnforced — when MaxBytes is set very low,
// Capture returns *ErrSnapshotTooLarge with concrete numbers.
func TestCapture_SizeCapEnforced(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	_, _, err := Capture(context.Background(), s, CaptureOptions{MaxBytes: 1})
	if err == nil {
		t.Fatal("Capture accepted with cap=1; expected ErrSnapshotTooLarge")
	}
	var tooLarge *ErrSnapshotTooLarge
	if !errors.As(err, &tooLarge) {
		t.Errorf("err = %v, want *ErrSnapshotTooLarge", err)
	}
	if tooLarge.MaxBytes != 1 {
		t.Errorf("MaxBytes = %d, want 1", tooLarge.MaxBytes)
	}
	if tooLarge.SizeBytes <= 1 {
		t.Errorf("SizeBytes = %d, want > 1", tooLarge.SizeBytes)
	}
}

// TestCapture_PausedRunsAndTranscript — when paused runs exist with
// transcript events, the section captures both and filters events by
// run_id (events from other runs in the same session are NOT mixed).
func TestCapture_PausedRunsAndTranscript(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	sess, _ := s.CreateSession(ctx, "t", "qa-agent", "user1")
	// Two runs in the same session: r1 will be paused, r2 will not.
	r1, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a1", UserID: "user1"})
	r2, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a2", UserID: "user1"})
	_ = s.SetRunPauseState(ctx, r1.ID, store.PauseStatePaused)

	// Append events to both runs. capturePausedRuns filters by
	// run_id so r2's events must NOT appear in r1's transcript_events.
	if err := s.AppendEvent(ctx, r1.ID, "text", []byte(`{"text":"r1-evt-1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, r2.ID, "text", []byte(`{"text":"r2-evt-1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, r1.ID, "tool_call", []byte(`{"tool":"WebFetch"}`)); err != nil {
		t.Fatal(err)
	}

	_, jsonBytes, err := Capture(ctx, s, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(jsonBytes, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Sections.PausedRuns.Entries) != 1 {
		t.Fatalf("paused_runs entries = %d, want 1 (only r1 is paused)", len(env.Sections.PausedRuns.Entries))
	}
	entry := env.Sections.PausedRuns.Entries[0]
	if entry.RunID != r1.ID {
		t.Errorf("paused run id = %q, want %q", entry.RunID, r1.ID)
	}
	if entry.PauseState != store.PauseStatePaused {
		t.Errorf("PauseState = %q, want %q", entry.PauseState, store.PauseStatePaused)
	}
	// Two events for r1 (text + tool_call); r2's event must be absent.
	if len(entry.TranscriptEvents) != 2 {
		t.Errorf("transcript_events = %d, want 2 (r1 events only, r2 excluded)", len(entry.TranscriptEvents))
	}
	// Verify the events ARE r1's (defensive — confirms the filter
	// works, not just the count).
	for _, e := range entry.TranscriptEvents {
		if !strings.Contains(string(e.Payload), "r1-evt") && !strings.Contains(string(e.Payload), "WebFetch") {
			t.Errorf("transcript event payload looks like r2: %s", e.Payload)
		}
	}
}

// TestCapture_NilStoreReturnsError — defensive: a caller passing nil
// gets a clean error rather than a panic.
func TestCapture_NilStoreReturnsError(t *testing.T) {
	_, _, err := Capture(context.Background(), nil, CaptureOptions{})
	if err == nil {
		t.Fatal("expected error on nil store")
	}
}

// TestCapture_ChannelConfigPassthrough — operator yaml channel
// config flows through to the envelope. Restore on a different
// machine sees the original-declaration shape.
func TestCapture_ChannelConfigPassthrough(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	cfg := []ChannelConfigEntry{
		{Name: "_system/heartbeat", Scope: "agent", TTLSeconds: 60},
		{Name: "verdicts", Scope: "user", MaxMessages: 1000},
	}
	_, jsonBytes, err := Capture(context.Background(), s, CaptureOptions{Channels: cfg})
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(jsonBytes, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Sections.Channels.Config) != 2 {
		t.Fatalf("Channels.Config = %d, want 2", len(env.Sections.Channels.Config))
	}
	if env.Sections.Channels.Config[0].Name != "_system/heartbeat" {
		t.Errorf("first config name = %q", env.Sections.Channels.Config[0].Name)
	}
}

// TestCapture_IDFormat — snap_<unix_ms>_<8hex>; the unix_ms prefix
// matches CreatedAt's millisecond.
func TestCapture_IDFormat(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	row, _, err := Capture(context.Background(), s, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(row.ID, "_")
	if len(parts) != 3 {
		t.Fatalf("ID = %q, want snap_<ms>_<hex> (3 parts)", row.ID)
	}
	if parts[0] != "snap" {
		t.Errorf("prefix = %q, want snap", parts[0])
	}
	if len(parts[2]) != 8 {
		t.Errorf("hex suffix len = %d, want 8", len(parts[2]))
	}
}

// TestCapture_DeterministicOrderingAcrossCalls — two captures of an
// unchanged store produce the same logical envelope (created_at +
// id differ, but section contents are byte-equal when re-marshalled
// from a deterministic-ordering perspective).
func TestCapture_DeterministicOrderingAcrossCalls(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	// Seed in a deliberately scrambled order so ordering matters.
	for _, k := range []string{"z", "a", "m"} {
		if err := s.MemorySet(ctx, store.MemoryScope("agent"), "agentX", k, json.RawMessage(`"v"`), 0); err != nil {
			t.Fatal(err)
		}
	}
	_, json1, err := Capture(ctx, s, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, json2, err := Capture(ctx, s, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var env1, env2 Envelope
	_ = json.Unmarshal(json1, &env1)
	_ = json.Unmarshal(json2, &env2)
	// Compare memory entries — must be in identical order across
	// captures.
	if len(env1.Sections.Memory.Entries) != len(env2.Sections.Memory.Entries) {
		t.Fatalf("entry counts differ: %d vs %d", len(env1.Sections.Memory.Entries), len(env2.Sections.Memory.Entries))
	}
	for i := range env1.Sections.Memory.Entries {
		if env1.Sections.Memory.Entries[i].Key != env2.Sections.Memory.Entries[i].Key {
			t.Errorf("entry[%d] key differs: %q vs %q",
				i, env1.Sections.Memory.Entries[i].Key, env2.Sections.Memory.Entries[i].Key)
		}
	}
}
