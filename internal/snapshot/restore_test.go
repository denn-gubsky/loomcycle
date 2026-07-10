package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestRoundTrip_SameInstance_EmptyStore — capture empty store, restore
// into a fresh store, capture again. The two captures must be
// structurally equivalent (created_at differs by design; everything
// else equal).
func TestRoundTrip_SameInstance_EmptyStore(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()

	// Capture from src.
	_, raw, err := Capture(context.Background(), src, CaptureOptions{})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// Restore into dst.
	result, err := Restore(context.Background(), dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if result.PausedRunsRestored != 0 {
		t.Errorf("PausedRunsRestored = %d, want 0 (empty source)", result.PausedRunsRestored)
	}
	if result.MemoryRestored != 0 {
		t.Errorf("MemoryRestored = %d, want 0", result.MemoryRestored)
	}

	// Re-capture from dst.
	_, raw2, err := Capture(context.Background(), dst, CaptureOptions{})
	if err != nil {
		t.Fatalf("re-Capture: %v", err)
	}

	// Compare envelope sections (ignoring created_at).
	if err := assertEnvelopesStructurallyEqual(raw, raw2); err != nil {
		t.Error(err)
	}
}

// TestRoundTrip_WithMemoryAndAgentDefs — populate the source store
// with memory rows + agent defs, capture, restore, verify the dest
// has the same row count + contents.
func TestRoundTrip_WithMemoryAndAgentDefs(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()

	ctx := context.Background()

	// Seed src.
	for i, key := range []string{"a", "b", "c"} {
		if err := src.MemorySet(ctx, store.MemoryScope("agent"), "agentX", key, []byte(`"v`+string(rune('0'+i))+`"`), 0); err != nil {
			t.Fatal(err)
		}
	}
	def, err := src.AgentDefCreate(ctx, store.AgentDefRow{
		DefID:                  "def_test_1",
		Name:                   "test-agent",
		Version:                1,
		Definition:             json.RawMessage(`{"system":"you are a test"}`),
		CreatedAt:              mustParseTime(t, "2026-05-01T00:00:00Z"),
		BootstrappedFromStatic: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = def

	// Capture + restore.
	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.MemoryRestored != 3 {
		t.Errorf("MemoryRestored = %d, want 3", result.MemoryRestored)
	}
	if result.AgentDefsRestored != 1 {
		t.Errorf("AgentDefsRestored = %d, want 1", result.AgentDefsRestored)
	}

	// Verify dst has the rows.
	mem, err := dst.SnapshotReadMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(mem) != 3 {
		t.Errorf("dst memory = %d, want 3", len(mem))
	}
	defs, err := dst.SnapshotReadAgentDefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].DefID != "def_test_1" {
		t.Errorf("dst agent_defs = %+v", defs)
	}
}

// TestRoundTrip_PreservesDefTenantID pins the RFC AP review #2 fix: a snapshot
// capture→restore must PRESERVE each def's owning tenant. Before the fix the
// snapshot DTO types dropped tenant_id, so every def (Agent/Skill/Team/MCPServer)
// collapsed to the shared "" tenant on restore — a cross-tenant disclosure +
// same-name active-pointer collisions. This seeds each family under a DISTINCT
// tenant and asserts the tenant round-trips, including the active pointer.
func TestRoundTrip_PreservesDefTenantID(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	def := json.RawMessage(`{"x":1}`)
	if _, err := src.AgentDefCreate(ctx, store.AgentDefRow{DefID: "ad1", Name: "a", Version: 1, TenantID: "acme", Definition: def}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.SkillDefCreate(ctx, store.SkillDefRow{DefID: "sd1", Name: "s", Version: 1, TenantID: "beta", Definition: def}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.TeamDefCreate(ctx, store.TeamDefRow{DefID: "td1", Name: "t", Version: 1, TenantID: "gamma", Definition: def}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.MCPServerDefCreate(ctx, store.MCPServerDefRow{DefID: "md1", Name: "m", Version: 1, TenantID: "delta", Definition: def}); err != nil {
		t.Fatal(err)
	}
	// An active pointer in a non-empty tenant — the active section must carry it too.
	if err := src.AgentDefSetActive(ctx, "acme", "a", "ad1", "agentX"); err != nil {
		t.Fatal(err)
	}

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, dst, raw, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}

	tenantOf := func(kind string) string {
		switch kind {
		case "agent":
			d, _ := dst.SnapshotReadAgentDefs(ctx)
			return d[0].TenantID
		case "skill":
			d, _ := dst.SnapshotReadSkillDefs(ctx)
			return d[0].TenantID
		case "team":
			d, _ := dst.SnapshotReadTeamDefs(ctx)
			return d[0].TenantID
		case "mcp":
			d, _ := dst.SnapshotReadMCPServerDefs(ctx)
			return d[0].TenantID
		}
		return ""
	}
	for _, tc := range []struct{ kind, want string }{
		{"agent", "acme"}, {"skill", "beta"}, {"team", "gamma"}, {"mcp", "delta"},
	} {
		if got := tenantOf(tc.kind); got != tc.want {
			t.Errorf("%s def restored under tenant %q, want %q (tenant dropped)", tc.kind, got, tc.want)
		}
	}
	// The active pointer's tenant must survive too.
	act, err := dst.SnapshotReadAgentDefActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(act) != 1 || act[0].TenantID != "acme" {
		t.Errorf("agent_def_active restored = %+v, want tenant acme", act)
	}
}

// TestRoundTrip_PreservesAgentDefSampling pins the v0.28.0 contract that a
// substrate-authored agent's per-agent LLM sampling block (temperature, top_p,
// seed, stop — the breeder/exp6 path) survives capture → Export (the external
// snapshot file) → Restore. Snapshot captures the agent_defs Definition as an
// opaque JSON blob, so sampling rides along for free — but this locks the
// invariant so a future change that re-serialises or normalises the definition
// can't silently drop it. Specifically guards the temperature: 0.0 edge:
// 0.0 is DETERMINISTIC, distinct from "unset" (nil → provider default), so a
// lossy round-trip that collapses the zero would be a real bug.
func TestRoundTrip_PreservesAgentDefSampling(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	zero, topP := 0.0, 0.95
	seed := 7
	samp := &config.Sampling{
		Temperature: &zero, // deterministic — the load-bearing edge
		TopP:        &topP,
		Seed:        &seed,
		Stop:        []string{"END"},
	}
	// The stored agent_defs Definition is the mergedDef JSON, which carries
	// sampling under the `sampling` key (config.Sampling's own json tags). We
	// mirror that exact shape here rather than depending on the unexported
	// mergedDef type from internal/tools/builtin.
	defBody := struct {
		Name     string           `json:"name"`
		Sampling *config.Sampling `json:"sampling,omitempty"`
	}{Name: "tuned-agent", Sampling: samp}
	defJSON, err := json.Marshal(defBody)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := src.AgentDefCreate(ctx, store.AgentDefRow{
		DefID:                  "def_tuned_1",
		Name:                   "tuned-agent",
		Version:                1,
		Definition:             json.RawMessage(defJSON),
		CreatedAt:              mustParseTime(t, "2026-06-11T00:00:00Z"),
		BootstrappedFromStatic: false, // runtime-created (breeder fork), not static yaml
	}); err != nil {
		t.Fatal(err)
	}

	// Capture → Export to the external-file byte form → Restore from those
	// bytes. This exercises BOTH "saved into snapshot" and "exported to
	// external snapshot file" in one chain.
	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("parse captured envelope: %v", err)
	}
	fileBytes, err := Export(&env)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, err := Restore(ctx, dst, fileBytes, RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	restored, err := dst.SnapshotReadAgentDefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0].DefID != "def_tuned_1" {
		t.Fatalf("dst agent_defs = %+v, want one def_tuned_1", restored)
	}

	var got struct {
		Sampling *config.Sampling `json:"sampling"`
	}
	if err := json.Unmarshal(restored[0].Definition, &got); err != nil {
		t.Fatalf("unmarshal restored definition: %v", err)
	}
	if got.Sampling == nil {
		t.Fatal("restored definition dropped the sampling block entirely")
	}
	if got.Sampling.Temperature == nil {
		t.Fatal("restored sampling.temperature is nil — the deterministic 0.0 was lost")
	}
	if *got.Sampling.Temperature != 0.0 {
		t.Errorf("restored temperature = %v, want 0.0", *got.Sampling.Temperature)
	}
	if got.Sampling.TopP == nil || *got.Sampling.TopP != topP {
		t.Errorf("restored top_p = %v, want %v", got.Sampling.TopP, topP)
	}
	if got.Sampling.Seed == nil || *got.Sampling.Seed != seed {
		t.Errorf("restored seed = %v, want %v", got.Sampling.Seed, seed)
	}
	if len(got.Sampling.Stop) != 1 || got.Sampling.Stop[0] != "END" {
		t.Errorf("restored stop = %v, want [END]", got.Sampling.Stop)
	}
}

// TestRoundTrip_WithMCPServerDefs — v0.9.x dynamic MCP server defs
// survive capture+restore round-trip with their lineage / definition /
// active-pointer intact. Counterpart to TestRoundTrip_WithMemoryAndAgentDefs
// pinning the new substrate's snapshot contract.
func TestRoundTrip_WithMCPServerDefs(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()

	ctx := context.Background()

	// Seed src with a single dynamic MCP server registration + promote.
	def, err := src.MCPServerDefCreate(ctx, store.MCPServerDefRow{
		DefID:       "mcpdef_n8n_mailgun_v1",
		Name:        "n8n-mailgun",
		Version:     1,
		Definition:  json.RawMessage(`{"transport":"streamable-http","url":"https://example.test/mcp","headers":{},"description":"n8n test"}`),
		Description: "n8n test",
		CreatedAt:   mustParseTime(t, "2026-05-22T00:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.MCPServerDefSetActive(ctx, "", def.Name, def.DefID, ""); err != nil {
		t.Fatal(err)
	}

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.MCPServerDefsRestored != 1 {
		t.Errorf("MCPServerDefsRestored = %d, want 1", result.MCPServerDefsRestored)
	}
	if result.MCPServerDefActiveRestored != 1 {
		t.Errorf("MCPServerDefActiveRestored = %d, want 1", result.MCPServerDefActiveRestored)
	}

	rows, err := dst.SnapshotReadMCPServerDefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].DefID != "mcpdef_n8n_mailgun_v1" {
		t.Errorf("dst mcp_server_defs = %+v", rows)
	}
	active, err := dst.SnapshotReadMCPServerDefActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].DefID != "mcpdef_n8n_mailgun_v1" {
		t.Errorf("dst mcp_server_def_active = %+v", active)
	}
}

// TestRestore_Idempotent_SecondCallNoDuplicates — restoring the same
// envelope twice produces the same end state. Catches a regression
// where SnapshotRestore* methods accidentally use plain INSERT
// instead of ON CONFLICT DO NOTHING.
func TestRestore_Idempotent_SecondCallNoDuplicates(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()

	ctx := context.Background()
	if err := src.MemorySet(ctx, store.MemoryScope("agent"), "a", "k", []byte(`"v"`), 0); err != nil {
		t.Fatal(err)
	}

	_, raw, _ := Capture(ctx, src, CaptureOptions{})

	// First restore.
	r1, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatalf("first Restore: %v", err)
	}
	if r1.MemoryRestored != 1 {
		t.Errorf("first MemoryRestored = %d, want 1", r1.MemoryRestored)
	}

	// Second restore — must succeed AND not change end state.
	r2, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatalf("second Restore: %v", err)
	}
	// SnapshotRestore* methods return (inserted bool, error) — on a
	// re-restore the INSERTs all hit ON CONFLICT DO NOTHING and report
	// rows_affected=0, so the counters reflect actual writes (0) and
	// not attempted writes (1). This is the PR #131 review fix.
	if r2.MemoryRestored != 0 {
		t.Errorf("second restore MemoryRestored = %d, want 0 (no new rows written)", r2.MemoryRestored)
	}

	mem, _ := dst.SnapshotReadMemory(ctx)
	if len(mem) != 1 {
		t.Errorf("after second restore: %d memory rows, want 1 (idempotent)", len(mem))
	}
}

// TestRestore_RejectsNewerVersion — a snapshot with a section version
// newer than CurrentVersion is refused with
// *migrations.ErrSnapshotVersionTooNew.
func TestRestore_RejectsNewerVersion(t *testing.T) {
	dst, dstClose := newTestStore(t)
	defer dstClose()

	// Hand-construct an envelope where memory.version = "9.99".
	envBytes := []byte(`{
		"schema_version": 1,
		"created_at": "2026-05-18T00:00:00Z",
		"sections": {
			"memory": {"version": "9.99", "entries": []}
		}
	}`)

	_, err := Restore(context.Background(), dst, envBytes, RestoreOptions{})
	if err == nil {
		t.Fatal("expected version-rejection error, got nil")
	}
	var tooNew *migrations.ErrSnapshotVersionTooNew
	if !errors.As(err, &tooNew) {
		t.Errorf("err = %v, want *ErrSnapshotVersionTooNew", err)
	}
	if tooNew.Section != "memory" {
		t.Errorf("Section = %q, want memory", tooNew.Section)
	}
	if tooNew.SnapshotVersion != "9.99" {
		t.Errorf("SnapshotVersion = %q", tooNew.SnapshotVersion)
	}
}

// TestRestore_RejectsNewerEnvelopeSchema — envelope-level
// schema_version > reader is refused before section decode.
func TestRestore_RejectsNewerEnvelopeSchema(t *testing.T) {
	dst, dstClose := newTestStore(t)
	defer dstClose()
	envBytes := []byte(`{"schema_version": 99, "created_at": "2026-05-18T00:00:00Z", "sections": {}}`)
	_, err := Restore(context.Background(), dst, envBytes, RestoreOptions{})
	if err == nil {
		t.Fatal("expected error on schema_version=99, got nil")
	}
	if !strings.Contains(err.Error(), "schema_version 99") {
		t.Errorf("err = %v, want 'schema_version 99' in message", err)
	}
}

// TestRestore_SynthesizesSessionForPausedRun — the load-bearing
// session FK synthesis test. A snapshot with a paused_run referencing
// a session_id NOT in the snapshot must NOT fail; restore creates
// a synthetic session and counts it in RestoreResult.
func TestRestore_SynthesizesSessionForPausedRun(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	// On src, create a session + run, flip the run to paused.
	sess, _ := src.CreateSession(ctx, "t", "qa", "user1")
	run, _ := src.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a1", UserID: "user1"})
	_ = src.SetRunPauseState(ctx, run.ID, store.PauseStatePaused)

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Restore into a fresh dst (no sessions exist there).
	result, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore failed (FK on session_id?): %v", err)
	}
	if result.PausedRunsRestored != 1 {
		t.Errorf("PausedRunsRestored = %d, want 1", result.PausedRunsRestored)
	}

	// The session was carried over by the envelope's session_id —
	// when the snapshot carries a real session_id (sess.ID), restore
	// uses it and synthesizes a session row with that ID. The
	// synthesized session row exists in dst.
	dstSess, err := dst.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("session %s not in dst: %v", sess.ID, err)
	}
	if dstSess.ID != sess.ID {
		t.Errorf("restored session ID = %q, want %q", dstSess.ID, sess.ID)
	}

	// And the run is present with pause_state='paused'.
	paused, _ := dst.ListPausedRuns(ctx)
	if len(paused) != 1 {
		t.Fatalf("dst paused runs = %d, want 1", len(paused))
	}
	if paused[0].ID != run.ID {
		t.Errorf("paused run id = %q, want %q", paused[0].ID, run.ID)
	}
}

// TestRoundTrip_PreservesParentContext pins the v0.12.x contract that a
// paused run's opaque tracking lineage survives pause→snapshot→restore.
// Regression: the PausedRunEntry carried no ParentContext field, so the
// SnapshotRestoreRun parent_context write was dead and the lineage was
// lost on restore — exactly the long-running runs the feature targets.
func TestRoundTrip_PreservesParentContext(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	pc := &store.ParentContext{RootAgentRunID: "run_root", FunctionKey: "cv-batch", TierAtRun: "pro"}
	sess, _ := src.CreateSession(ctx, "t", "qa", "user1")
	run, _ := src.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a1", UserID: "user1", ParentContext: pc})
	_ = src.SetRunPauseState(ctx, run.ID, store.PauseStatePaused)

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, dst, raw, RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got, err := dst.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun on dst: %v", err)
	}
	if got.ParentContext == nil || *got.ParentContext != *pc {
		t.Errorf("restored ParentContext = %+v, want %+v", got.ParentContext, pc)
	}
}

// TestRoundTrip_PreservesInteractiveFlag pins F42 / RFC X Phase 2: a paused
// interactive run's `interactive` flag survives pause→snapshot→restore, so the
// run re-dispatches with the correct park-at-end_turn (vs run-to-completion)
// semantics on the target instance. Without the flag captured + restored, a
// resumed interactive session would silently become a batch run.
func TestRoundTrip_PreservesInteractiveFlag(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	sess, _ := src.CreateSession(ctx, "t", "qa", "user1")
	run, err := src.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_int", UserID: "user1", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, dst, raw, RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got, err := dst.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun on dst: %v", err)
	}
	if !got.Interactive {
		t.Error("interactive flag lost across pause→snapshot→restore")
	}
}

// TestRestore_DropsEmbeddingFieldOnPhase1 — restoring a hand-crafted
// envelope where memory.entries[].embedding is populated (simulating
// a Phase-2-captured snapshot) on a Phase-1 reader silently drops
// the embedding payload. The memory row IS restored; only the
// embedding side is skipped.
func TestRestore_DropsEmbeddingFieldOnPhase1(t *testing.T) {
	dst, dstClose := newTestStore(t)
	defer dstClose()

	envBytes := []byte(`{
		"schema_version": 1,
		"created_at": "2026-05-18T00:00:00Z",
		"sections": {
			"memory": {
				"version": "1.0",
				"entries": [
					{
						"scope": "agent",
						"scope_id": "a1",
						"key": "k",
						"value": "v",
						"created_at": "2026-05-18T00:00:00Z",
						"updated_at": "2026-05-18T00:00:00Z",
						"embedding": {
							"provider": "openai",
							"model":    "text-embedding-3-large",
							"dimension": 1536,
							"vector":   "AAAA",
							"embed_text": "hello",
							"created_at": "2026-05-18T00:00:00Z"
						}
					}
				]
			}
		}
	}`)

	ctx := context.Background()
	result, err := Restore(ctx, dst, envBytes, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if result.MemoryRestored != 1 {
		t.Errorf("MemoryRestored = %d, want 1", result.MemoryRestored)
	}
	mem, _ := dst.SnapshotReadMemory(ctx)
	if len(mem) != 1 {
		t.Errorf("dst memory rows = %d, want 1", len(mem))
	}
	// Embedding silently dropped — Phase 1 has no MemoryEmbedSet
	// method. Phase 2 will pick up the field from re-deserialised
	// memory.entries[].embedding and write to memory_embeddings.
}

// TestRestore_MissingSectionsTolerated — an envelope with only
// a subset of sections restores those sections + leaves the rest
// empty.
func TestRestore_MissingSectionsTolerated(t *testing.T) {
	dst, dstClose := newTestStore(t)
	defer dstClose()
	// Only memory, nothing else.
	envBytes := []byte(`{
		"schema_version": 1,
		"created_at": "2026-05-18T00:00:00Z",
		"sections": {
			"memory": {
				"version": "1.0",
				"entries": [
					{"scope":"agent","scope_id":"a","key":"k","value":"v","created_at":"2026-05-18T00:00:00Z","updated_at":"2026-05-18T00:00:00Z","embedding":null}
				]
			}
		}
	}`)
	result, err := Restore(context.Background(), dst, envBytes, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.MemoryRestored != 1 {
		t.Errorf("MemoryRestored = %d, want 1", result.MemoryRestored)
	}
	if result.AgentDefsRestored != 0 || result.PausedRunsRestored != 0 {
		t.Errorf("non-zero counts for missing sections: %+v", result)
	}
}

// TestRestore_InteractionHistorySkippedByDefault — when
// IncludeHistory=false (default) any interaction_history section in
// the envelope is skipped with a warning.
func TestRestore_InteractionHistorySkippedByDefault(t *testing.T) {
	dst, dstClose := newTestStore(t)
	defer dstClose()
	envBytes := []byte(`{
		"schema_version": 1,
		"created_at": "2026-05-18T00:00:00Z",
		"sections": {
			"interaction_history": {
				"version": "1.0",
				"since_ts": "2026-05-17T00:00:00Z",
				"events": []
			}
		}
	}`)
	result, err := Restore(context.Background(), dst, envBytes, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.InteractionHistoryRestored != 0 {
		t.Errorf("InteractionHistoryRestored = %d, want 0 (default skip)", result.InteractionHistoryRestored)
	}
	// Warning surfaced for operator visibility.
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "interaction_history") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("warnings do not mention interaction_history skip: %v", result.Warnings)
	}
}

// TestRestore_ForceProbeInvoked — when ForceProbe is set, restore
// calls it after section writes.
func TestRestore_ForceProbeInvoked(t *testing.T) {
	dst, dstClose := newTestStore(t)
	defer dstClose()

	called := false
	opts := RestoreOptions{
		ForceProbe: func(ctx context.Context) {
			called = true
		},
	}
	envBytes := []byte(`{"schema_version": 1, "created_at": "2026-05-18T00:00:00Z", "sections": {}}`)
	_, err := Restore(context.Background(), dst, envBytes, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("ForceProbe not invoked")
	}
}

// TestExport_RoundTripsThroughCanonical — Export produces canonical
// JSON bytes that re-parse into the same Envelope struct.
func TestExport_RoundTripsThroughCanonical(t *testing.T) {
	env := &Envelope{
		SchemaVersion: 1,
		CreatedAt:     mustParseTime(t, "2026-05-18T10:00:00Z"),
		Sections: Sections{
			Memory: MemorySection{Version: "1.0", Entries: []MemoryEntry{}},
		},
	}
	bytes, err := Export(env)
	if err != nil {
		t.Fatal(err)
	}
	var rt Envelope
	if err := json.Unmarshal(bytes, &rt); err != nil {
		t.Fatalf("round-trip Unmarshal: %v", err)
	}
	if rt.SchemaVersion != env.SchemaVersion {
		t.Errorf("SchemaVersion mismatch")
	}
}

// TestExport_NilEnvelopeRefused — defensive.
func TestExport_NilEnvelopeRefused(t *testing.T) {
	_, err := Export(nil)
	if err == nil {
		t.Error("Export(nil) accepted")
	}
}

// TestRestore_NilStoreRefused — defensive.
func TestRestore_NilStoreRefused(t *testing.T) {
	_, err := Restore(context.Background(), nil, []byte(`{}`), RestoreOptions{})
	if err == nil {
		t.Error("Restore(nil store) accepted")
	}
}

// TestRestore_EmptyBytesRefused — defensive.
func TestRestore_EmptyBytesRefused(t *testing.T) {
	dst, cleanup := newTestStore(t)
	defer cleanup()
	_, err := Restore(context.Background(), dst, nil, RestoreOptions{})
	if err == nil {
		t.Error("Restore(empty bytes) accepted")
	}
}

// assertEnvelopesStructurallyEqual compares two raw JSON envelopes
// for structural equality, ignoring fields that differ by design
// (created_at + snapshot ids). Returns an error describing the
// first difference found.
func assertEnvelopesStructurallyEqual(a, b []byte) error {
	var envA, envB Envelope
	if err := json.Unmarshal(a, &envA); err != nil {
		return err
	}
	if err := json.Unmarshal(b, &envB); err != nil {
		return err
	}
	if envA.SchemaVersion != envB.SchemaVersion {
		return errors.New("schema_version differs")
	}
	if len(envA.Sections.Memory.Entries) != len(envB.Sections.Memory.Entries) {
		return errors.New("memory entries count differs")
	}
	if len(envA.Sections.AgentDefs.Entries) != len(envB.Sections.AgentDefs.Entries) {
		return errors.New("agent_defs entries count differs")
	}
	return nil
}

// mustParseTime parses an RFC3339 timestamp or fails the test.
func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("time.Parse(%q): %v", s, err)
	}
	return tt
}

// restampSectionChecksum recomputes the I4 integrity checksum over a
// (possibly mutated) but structurally valid snapshot document and writes it
// back, so the document is self-consistent again. Used by tests that
// deliberately mutate section CONTENT (e.g. a malformed embedding) and want
// Restore to reach the downstream warning path rather than reject on the
// digest. Lives here so package-internal tests can share it.
func restampSectionChecksum(t *testing.T, doc string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("restamp: parse: %v", err)
	}
	cs, err := json.Marshal(sectionChecksum(m["sections"]))
	if err != nil {
		t.Fatalf("restamp: marshal checksum: %v", err)
	}
	m["checksum"] = cs
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("restamp: marshal: %v", err)
	}
	return string(out)
}

// mkChecksumEnvelope builds a minimal envelope with one agent def, carrying a
// recognisable description for tamper tests.
func mkChecksumEnvelope(t *testing.T) Envelope {
	t.Helper()
	return Envelope{
		SchemaVersion: SchemaVersion,
		CreatedAt:     mustParseTime(t, "2026-01-01T00:00:00Z"),
		Sections: Sections{
			AgentDefs: AgentDefsSection{
				Version: SectionVersion,
				Entries: []AgentDefEntry{{
					DefID:       "d1",
					Name:        "alpha",
					Version:     1,
					Definition:  json.RawMessage(`{"model":"x"}`),
					Description: "ORIGINAL_DESCRIPTION_TOKEN",
					CreatedAt:   mustParseTime(t, "2026-01-01T00:00:00Z"),
				}},
			},
		},
	}
}

// TestExportRestore_ChecksumRoundTrip pins exp7 I4: Export stamps a
// sha256:<hex> checksum over the section bytes and Restore accepts the
// round-trip.
func TestExportRestore_ChecksumRoundTrip(t *testing.T) {
	env := mkChecksumEnvelope(t)
	raw, err := Export(&env)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !strings.Contains(string(raw), `"checksum":"sha256:`) {
		t.Fatalf("Export output carries no sha256 checksum: %s", raw)
	}
	// Export must NOT mutate the caller's envelope.
	if env.Checksum != "" {
		t.Errorf("Export mutated caller's envelope Checksum = %q, want empty", env.Checksum)
	}

	dst, dstClose := newTestStore(t)
	defer dstClose()
	res, err := Restore(context.Background(), dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore of a valid checksummed snapshot failed: %v", err)
	}
	if res.AgentDefsRestored != 1 {
		t.Errorf("AgentDefsRestored = %d, want 1", res.AgentDefsRestored)
	}
}

// TestExportPretty_ClearsChecksumSoRestoreSucceeds is the exp7 regression for
// the pretty-export path. json.MarshalIndent re-indents the nested "sections"
// object, so a checksum stamped over the COMPACT section bytes (which every
// Capture/Export envelope carries) no longer matches the indented bytes —
// Restore hashes the document's section bytes and would reject a pretty doc as
// "truncated or tampered". ExportPretty therefore clears the checksum.
// Fail-before: with the checksum propagated into the pretty doc, this Restore
// fails with "checksum mismatch".
func TestExportPretty_ClearsChecksumSoRestoreSucceeds(t *testing.T) {
	env := mkChecksumEnvelope(t)
	// Simulate a fetched, stored snapshot: it carries the compact checksum
	// because Capture routes through Export, which stamps it.
	raw, err := Export(&env)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	var stored Envelope
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal stored: %v", err)
	}
	if stored.Checksum == "" {
		t.Fatal("precondition: stored envelope should carry a checksum")
	}

	pretty, err := ExportPretty(&stored)
	if err != nil {
		t.Fatalf("ExportPretty: %v", err)
	}
	if strings.Contains(string(pretty), `"checksum"`) {
		t.Errorf("pretty export still carries a checksum (cannot survive re-indent): %s", pretty)
	}
	// Must not mutate the caller's envelope.
	if stored.Checksum == "" {
		t.Errorf("ExportPretty cleared the caller's Checksum; it must copy")
	}

	dst, dstClose := newTestStore(t)
	defer dstClose()
	res, err := Restore(context.Background(), dst, pretty, RestoreOptions{})
	if err != nil {
		t.Fatalf("ExportPretty -> Restore failed (stale checksum survived?): %v", err)
	}
	if res.AgentDefsRestored != 1 {
		t.Errorf("AgentDefsRestored = %d, want 1", res.AgentDefsRestored)
	}
}

// TestRestore_RejectsTamperedBodyWithStaleChecksum pins the integrity gate: a
// body mutated AFTER Export (without re-stamping) is rejected before any
// decode/insert.
func TestRestore_RejectsTamperedBodyWithStaleChecksum(t *testing.T) {
	env := mkChecksumEnvelope(t)
	raw, err := Export(&env)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// Flip a section byte without recomputing the checksum.
	tampered := strings.Replace(string(raw), "ORIGINAL_DESCRIPTION_TOKEN", "TAMPERED_DESCRIPTION_XXXX", 1)
	if tampered == string(raw) {
		t.Fatal("tamper no-op — token not found in export")
	}

	dst, dstClose := newTestStore(t)
	defer dstClose()
	_, err = Restore(context.Background(), dst, []byte(tampered), RestoreOptions{})
	if err == nil {
		t.Fatal("Restore accepted a tampered body with a stale checksum")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("err = %v, want a checksum mismatch", err)
	}
}

// TestRestore_LegacySnapshotWithoutChecksumStillRestores pins backward-compat:
// a snapshot captured before I4 (no checksum field) restores unchanged.
func TestRestore_LegacySnapshotWithoutChecksumStillRestores(t *testing.T) {
	env := mkChecksumEnvelope(t)
	// Marshal directly (NOT via Export) so the document carries no checksum —
	// the pre-I4 producer shape (env.Checksum == "" → omitempty drops it).
	legacy, err := json.Marshal(&env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(legacy), `"checksum"`) {
		t.Fatalf("legacy fixture unexpectedly carries a checksum: %s", legacy)
	}

	dst, dstClose := newTestStore(t)
	defer dstClose()
	res, err := Restore(context.Background(), dst, legacy, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore of a checksum-less (legacy) snapshot failed: %v", err)
	}
	if res.AgentDefsRestored != 1 {
		t.Errorf("AgentDefsRestored = %d, want 1", res.AgentDefsRestored)
	}
}
