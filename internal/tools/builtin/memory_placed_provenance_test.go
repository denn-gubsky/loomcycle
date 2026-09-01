package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// placedProvenanceFixture is a Memory tool whose run is (tenant "acme", user "alice"),
// granted the consolidation ops and every scope — the shape a consolidation pass that has
// been enabled for placement actually runs as.
func placedProvenanceFixture(t *testing.T) (*Memory, context.Context) {
	t.Helper()
	m, ctx, _ := tenantMemFixture(t, "acme", "alice")
	// The tenant grant is what an operator adds to enable placement, so the fixture has
	// to carry it — grantedConsolidationCtx alone grants agent+user.
	ctx = tools.WithRunID(ctx, "run_consolidation_pass")
	return m, tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user", "tenant"},
		Consolidation: true,
	})
}

// TestMemorySet_PlacedWriteKeepsThePendingProvenance is a regression test for a silent
// provenance loss that appears only once facts are PLACED.
//
// A consolidation pass drains the queue in the USER scope, then — when the tenant ontology
// declares the fact's type shared — writes the fact to the TENANT scope, relaying the
// drained row's id as from_pending. from_pending is resolved against the tuple the write
// TARGETS, so the tenant-scope lookup missed the user-scope pending row; and a miss is
// silent by design, so the fact landed in the shared plane with no server-stamped origin
// and no source_session_id.
//
// source_session_id is what the erasure report uses to surface a placed fact as residue
// for the subject who produced it. Losing it means an erasure cannot see the fact at all —
// which is the guarantee the erasure policy rests on.
func TestMemorySet_PlacedWriteKeepsThePendingProvenance(t *testing.T) {
	m, ctx := placedProvenanceFixture(t)

	// Enqueued in the scope a pass drains: this user's own.
	if err := m.Store.MemoryPendingEnqueue(ctx, store.MemoryPendingRow{
		ID: "mp_placed", TenantID: "acme", Scope: store.MemoryScopeUser, ScopeID: "alice",
		Payload:         json.RawMessage(`{"messages":[]}`),
		Origin:          store.PendingOriginCompaction,
		SourceSessionID: "sess-abc", SourceRunID: "run-xyz",
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// The pass PLACES the fact in the tenant scope, relaying the id it drained.
	body := `{"op":"set","scope":"tenant","key":"memory/fact/two-approvals",` +
		`"value":"The checkout-api service requires two approvals.","from_pending":"mp_placed"}`
	if r := memExecJSON(t, m, ctx, body); r.IsError {
		t.Fatalf("placed write: %s", r.Text)
	}

	got, err := m.Store.MemoryProvenanceGet(ctx, "acme", store.MemoryScopeTenant, "", "memory/fact/two-approvals")
	if err != nil {
		t.Fatalf("read back the placed row's provenance: %v", err)
	}
	if got.SourceSessionID != "sess-abc" {
		t.Errorf("source_session_id = %q, want sess-abc — without it an erasure cannot see "+
			"this placed fact as residue for the subject who produced it", got.SourceSessionID)
	}
	if got.SourceRunID != "run-xyz" {
		t.Errorf("source_run_id = %q, want run-xyz", got.SourceRunID)
	}
	if got.Origin != store.PendingOriginCompaction {
		t.Errorf("origin = %q, want %q — a placed fact must not become indistinguishable "+
			"from one read out of an ordinary transcript", got.Origin, store.PendingOriginCompaction)
	}
}

// The fallback is built from the run's OWN identity, so it must not reach another
// principal's queue. A foreign id stays invisible and the write still succeeds, exactly as
// an unowned id always did.
func TestMemorySet_ThePendingFallbackStaysWithinTheCallersOwnQueue(t *testing.T) {
	m, ctx := placedProvenanceFixture(t)

	if err := m.Store.MemoryPendingEnqueue(ctx, store.MemoryPendingRow{
		ID: "mp_bob", TenantID: "acme", Scope: store.MemoryScopeUser, ScopeID: "bob",
		Payload: json.RawMessage(`{"messages":[]}`),
		Origin:  store.PendingOriginCompaction, SourceSessionID: "bob-sess",
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	body := `{"op":"set","scope":"tenant","key":"k","value":"v","from_pending":"mp_bob"}`
	if r := memExecJSON(t, m, ctx, body); r.IsError {
		t.Fatalf("the write must still succeed on an unowned id: %s", r.Text)
	}
	got, err := m.Store.MemoryProvenanceGet(ctx, "acme", store.MemoryScopeTenant, "", "k")
	if err == nil && got.SourceSessionID == "bob-sess" {
		t.Errorf("alice's write picked up bob's pending provenance — the fallback must only " +
			"ever read the caller's own queue")
	}
}

// TestMemoryPlacement_ConflictLookupUsesTheCallersScope: the inconsistent-subject guard
// asks whether the store already types this subject in a way that would place it
// elsewhere. That is a question about the partition the CALLER reads, and hardcoding the
// user scope left the guard silently unable to fire for an agent-scoped caller.
func TestMemoryPlacement_ConflictLookupUsesTheCallersScope(t *testing.T) {
	m, ctx := placementFixture(t, true)

	got := placements(t, m, ctx,
		`{"op":"placement","scope":"agent","items":[{"type":"service","subject":"checkout-api"}]}`)
	if len(got) != 1 {
		t.Fatalf("want 1 answer, got %v", got)
	}
	// The declaration still applies: the caller's scope changes where a CONFLICT is looked
	// up, not whether the type is declared.
	if got[0]["scope"] != "tenant" || got[0]["moved"] != true {
		t.Errorf("an agent-scoped caller should still be placed by the declaration: %v", got[0])
	}
	if r, _ := got[0]["reason"].(string); strings.Contains(r, "inconsistently") {
		t.Errorf("no conflict exists in the agent scope, so none should be reported: %q", r)
	}
}

var _ = tools.RunIdentity
