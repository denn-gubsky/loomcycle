package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// grantedConsolidationCtx layers the memory_consolidation grant onto the
// standard fixture ctx (which already carries AgentName + RunIdentity + the
// agent/user memory scopes).
func grantedConsolidationCtx(ctx context.Context) context.Context {
	return tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user"},
		Consolidation: true,
	})
}

// TestMemory_SchemaIsValidJSON guards the hand-written input schema after the
// consolidation-op additions — a malformed schema only fails at runtime.
func TestMemory_SchemaIsValidJSON(t *testing.T) {
	if !json.Valid([]byte(memoryInputSchema)) {
		t.Fatal("memoryInputSchema is not valid JSON")
	}
}

// TestMemory_CursorOpsRequireConsolidationGrant proves the SEPARATE gate: an
// agent with memory_scopes but WITHOUT the grant is refused every consolidation
// op, while a granted agent is admitted. Fails-before if consolidationGate is
// absent (the ops would run under scope-only authorization).
func TestMemory_CursorOpsRequireConsolidationGrant(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t) // fixture ctx grants scopes, NOT consolidation
	defer cleanup()

	denied := []string{
		`{"op":"cursor_get","scope":"agent"}`,
		`{"op":"cursor_lease","scope":"agent"}`,
		`{"op":"cursor_advance","scope":"agent","completed_at":"2026-07-24T00:00:00Z","session_id":"s1"}`,
		`{"op":"cursor_release","scope":"agent"}`,
		`{"op":"supersede","scope":"agent","key":"k"}`,
		`{"op":"pending_drain","scope":"agent"}`,
		`{"op":"pending_ack","scope":"agent","ids":["mp_x"]}`,
	}
	for _, in := range denied {
		res, _ := tool.Execute(ctx, json.RawMessage(in))
		if !res.IsError || !strings.Contains(res.Text, "memory_consolidation grant") {
			t.Errorf("%s: got (IsError=%v) %q, want a memory_consolidation refusal", in, res.IsError, res.Text)
		}
	}

	// Granted → admitted (cursor_get on a fresh target succeeds).
	gctx := grantedConsolidationCtx(ctx)
	res, _ := tool.Execute(gctx, json.RawMessage(`{"op":"cursor_get","scope":"agent"}`))
	if res.IsError {
		t.Fatalf("granted cursor_get refused: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"leased_by":""`) {
		t.Errorf("cursor_get default = %s, want an unleased zero-watermark row", res.Text)
	}
}

// TestMemory_SupersedeHidesFromReads: a superseded key vanishes from get + list
// (the store read-filter) while its sibling remains.
func TestMemory_SupersedeHidesFromReads(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)

	if res, _ := tool.Execute(gctx, json.RawMessage(`{"op":"set","scope":"agent","key":"raw-1","value":"one"}`)); res.IsError {
		t.Fatal(res.Text)
	}
	if res, _ := tool.Execute(gctx, json.RawMessage(`{"op":"set","scope":"agent","key":"raw-2","value":"two"}`)); res.IsError {
		t.Fatal(res.Text)
	}

	if res, _ := tool.Execute(gctx, json.RawMessage(`{"op":"supersede","scope":"agent","key":"raw-1"}`)); res.IsError {
		t.Fatalf("supersede: %s", res.Text)
	}

	// get(raw-1) → value:null (opaque miss).
	res, _ := tool.Execute(gctx, json.RawMessage(`{"op":"get","scope":"agent","key":"raw-1"}`))
	if res.IsError || !strings.Contains(res.Text, `"value":null`) {
		t.Errorf("get(superseded) = %q, want value:null", res.Text)
	}
	// list → only raw-2.
	res, _ = tool.Execute(gctx, json.RawMessage(`{"op":"list","scope":"agent"}`))
	if res.IsError || strings.Contains(res.Text, "raw-1") || !strings.Contains(res.Text, "raw-2") {
		t.Errorf("list after supersede = %q, want only raw-2", res.Text)
	}
}

// TestMemory_CursorLeaseAndAdvance: lease → advance → the watermark is
// observable via cursor_get, and a non-lease-holder cannot advance.
func TestMemory_CursorLeaseAndAdvance(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)

	// Lease the agent-scope target (owner = agent name "qa-agent").
	res, _ := tool.Execute(gctx, json.RawMessage(`{"op":"cursor_lease","scope":"agent","lease_ttl_ms":60000}`))
	if res.IsError || !strings.Contains(res.Text, `"acquired":true`) {
		t.Fatalf("cursor_lease = %q, want acquired:true", res.Text)
	}

	// Advance the watermark.
	adv := `{"op":"cursor_advance","scope":"agent","completed_at":"2026-07-24T10:30:00Z","session_id":"sess-42"}`
	if res, _ := tool.Execute(gctx, json.RawMessage(adv)); res.IsError {
		t.Fatalf("cursor_advance: %s", res.Text)
	}

	// cursor_get reflects the advanced watermark.
	res, _ = tool.Execute(gctx, json.RawMessage(`{"op":"cursor_get","scope":"agent"}`))
	if res.IsError {
		t.Fatal(res.Text)
	}
	if !strings.Contains(res.Text, `"watermark_session_id":"sess-42"`) ||
		!strings.Contains(res.Text, "2026-07-24T10:30:00") {
		t.Errorf("cursor_get after advance = %q, want the sess-42 / 2026-07-24T10:30 watermark", res.Text)
	}

	// A backward advance is a monotonic no-op (not an error); the watermark holds.
	back := `{"op":"cursor_advance","scope":"agent","completed_at":"2026-07-24T09:00:00Z","session_id":"sess-01"}`
	if res, _ := tool.Execute(gctx, json.RawMessage(back)); res.IsError {
		t.Fatalf("backward advance should be a no-op, got: %s", res.Text)
	}
	res, _ = tool.Execute(gctx, json.RawMessage(`{"op":"cursor_get","scope":"agent"}`))
	if !strings.Contains(res.Text, `"watermark_session_id":"sess-42"`) {
		t.Errorf("watermark moved backward: %q", res.Text)
	}

	// Release, then a fresh cursor_get shows an unleased target.
	if res, _ := tool.Execute(gctx, json.RawMessage(`{"op":"cursor_release","scope":"agent"}`)); res.IsError {
		t.Fatalf("cursor_release: %s", res.Text)
	}
	res, _ = tool.Execute(gctx, json.RawMessage(`{"op":"cursor_get","scope":"agent"}`))
	if !strings.Contains(res.Text, `"leased_by":""`) {
		t.Errorf("cursor_get after release = %q, want leased_by empty", res.Text)
	}
}

// TestMemory_PendingDrainAck seeds the queue via the store (the tool exposes no
// enqueue op in PR1), then drains + acks through the tool: drain returns rows
// oldest-first, and acked rows are excluded from the next drain.
func TestMemory_PendingDrainAck(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)

	// The tool's agent-scope target resolves to (tenant "", scope agent,
	// scope_id "qa-agent") — seed the store there directly.
	base := time.Now().UTC().Truncate(time.Second)
	seed := func(id string, ageSecs int) {
		if err := tool.Store.MemoryPendingEnqueue(context.Background(), store.MemoryPendingRow{
			ID:        id,
			Scope:     store.MemoryScopeAgent,
			ScopeID:   "qa-agent",
			Payload:   json.RawMessage(`{"turn":"` + id + `"}`),
			CreatedAt: base.Add(time.Duration(ageSecs) * time.Second),
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("mp_older", 0)
	seed("mp_newer", 5)

	// Drain via the tool → both rows, oldest-first.
	res, _ := tool.Execute(gctx, json.RawMessage(`{"op":"pending_drain","scope":"agent"}`))
	if res.IsError {
		t.Fatalf("pending_drain: %s", res.Text)
	}
	var drained struct {
		Pending []struct {
			ID string `json:"id"`
		} `json:"pending"`
	}
	if err := json.Unmarshal([]byte(res.Text), &drained); err != nil {
		t.Fatalf("decode drain result: %v (%s)", err, res.Text)
	}
	if len(drained.Pending) != 2 || drained.Pending[0].ID != "mp_older" || drained.Pending[1].ID != "mp_newer" {
		t.Fatalf("drain order = %+v, want [mp_older, mp_newer]", drained.Pending)
	}

	// Ack the older row → the next drain returns only the newer.
	if res, _ := tool.Execute(gctx, json.RawMessage(`{"op":"pending_ack","scope":"agent","ids":["mp_older"]}`)); res.IsError {
		t.Fatalf("pending_ack: %s", res.Text)
	}
	res, _ = tool.Execute(gctx, json.RawMessage(`{"op":"pending_drain","scope":"agent"}`))
	if strings.Contains(res.Text, "mp_older") || !strings.Contains(res.Text, "mp_newer") {
		t.Errorf("drain after ack = %q, want only mp_newer", res.Text)
	}
}
