package builtin

import (
	"context"
	"encoding/json"
	"fmt"
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

// seedSettledChat creates a session authored by `agent` for `userID` with one
// COMPLETED run, so the store reports it as consolidatable. Returns the session
// id and the instant it settled — the exact pair cursor_scan must hand back.
func seedSettledChat(t *testing.T, tool *Memory, tenantID, agent, userID string) (string, time.Time) {
	t.Helper()
	ctx := context.Background()
	sess, err := tool.Store.CreateSession(ctx, tenantID, agent, userID)
	if err != nil {
		t.Fatalf("CreateSession(%s/%s): %v", agent, userID, err)
	}
	run, err := tool.Store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "r-" + sess.ID, UserID: userID})
	if err != nil {
		t.Fatalf("CreateRun(%s): %v", sess.ID, err)
	}
	if err := tool.Store.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{Model: "m", Provider: "p"}, ""); err != nil {
		t.Fatalf("FinishRun(%s): %v", run.ID, err)
	}
	settledAt, _, err := tool.Store.SessionSettledAt(ctx, tenantID, sess.ID)
	if err != nil {
		t.Fatalf("SessionSettledAt(%s): %v", sess.ID, err)
	}
	return sess.ID, settledAt
}

// setWatermark moves a target's cursor via the store (lease → advance →
// release), so a test can put the watermark somewhere without going through the
// tool's own advance guard.
func setWatermark(t *testing.T, tool *Memory, scopeID string, at time.Time, sessionID string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := tool.Store.MemoryCursorLease(ctx, "", store.MemoryScopeUser, scopeID, "test-owner", time.Now().UTC(), time.Minute); err != nil {
		t.Fatalf("lease %s: %v", scopeID, err)
	}
	if err := tool.Store.MemoryCursorAdvance(ctx, "", store.MemoryScopeUser, scopeID, "test-owner", at, sessionID); err != nil {
		t.Fatalf("advance %s: %v", scopeID, err)
	}
	if err := tool.Store.MemoryCursorRelease(ctx, "", store.MemoryScopeUser, scopeID, "test-owner"); err != nil {
		t.Fatalf("release %s: %v", scopeID, err)
	}
}

// scanResult is the cursor_scan response shape.
type scanResult struct {
	Sessions []struct {
		SessionID   string `json:"session_id"`
		CompletedAt string `json:"completed_at"`
	} `json:"sessions"`
	Truncated bool `json:"truncated"`
}

// runScan executes cursor_scan and decodes the result.
func runScan(t *testing.T, tool *Memory, ctx context.Context, payload string) scanResult {
	t.Helper()
	res, _ := tool.Execute(ctx, json.RawMessage(payload))
	if res.IsError {
		t.Fatalf("cursor_scan(%s): %s", payload, res.Text)
	}
	var out scanResult
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode cursor_scan result: %v (%s)", err, res.Text)
	}
	return out
}

// scanIDs projects the session ids, in returned order.
func scanIDs(r scanResult) []string {
	out := make([]string, 0, len(r.Sessions))
	for _, s := range r.Sessions {
		out = append(out, s.SessionID)
	}
	return out
}

// TestMemory_CursorScanRequiresConsolidationGrant: cursor_scan reads the
// target's chat history, so it belongs behind the same default-deny grant as
// every other consolidation op — an agent with memory_scopes alone must not be
// able to enumerate a user's settled sessions.
//
// Fails-before if execCursorScan skips consolidationGate.
func TestMemory_CursorScanRequiresConsolidationGrant(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t) // scopes, NO consolidation grant
	defer cleanup()
	seedSettledChat(t, tool, "", "chat", "alice")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"cursor_scan","scope":"user"}`))
	if !res.IsError || !strings.Contains(res.Text, "memory_consolidation grant") {
		t.Errorf("cursor_scan without the grant = (IsError=%v) %q, want a memory_consolidation refusal", res.IsError, res.Text)
	}
}

// TestMemory_CursorScanReturnsAscendingPastWatermark is the data-loss
// regression. The pass used to discover work by paging the chat LIST, which is
// ordered newest-first and filtered on last_activity — a different timestamp
// from the watermark's max(completed_at) — while the watermark itself is
// forward-only. So a target with more settled chats than one page consolidated
// the NEWEST page, advanced past them, and stranded every older chat forever:
// no error, no log, and on an existing deployment the first pass discarded the
// whole historical backlog.
//
// cursor_scan is the fix, and these are the properties that make it safe:
// ascending order, strictly after the STORED watermark, self-authored sessions
// absent, and an explicit truncation flag instead of a silent trim.
//
// Fails-before against any newest-first / last_activity-keyed discovery read.
func TestMemory_CursorScanReturnsAscendingPastWatermark(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)

	// Three real chats, oldest first, plus one authored by the pass ITSELF
	// (the fixture agent name) — the steady state after a few passes.
	c1, at1 := seedSettledChat(t, tool, "", "chat", "alice")
	c2, _ := seedSettledChat(t, tool, "", "chat", "alice")
	c3, _ := seedSettledChat(t, tool, "", "chat", "alice")
	self, _ := seedSettledChat(t, tool, "", "qa-agent", "alice")
	// Another user's chat must never appear under alice's target.
	other, _ := seedSettledChat(t, tool, "", "chat", "bob")

	got := runScan(t, tool, gctx, `{"op":"cursor_scan","scope":"user"}`)
	if ids := scanIDs(got); len(ids) != 3 || ids[0] != c1 || ids[1] != c2 || ids[2] != c3 {
		t.Fatalf("scan from a zero watermark = %v, want [%s %s %s] OLDEST FIRST", ids, c1, c2, c3)
	}
	for _, id := range scanIDs(got) {
		if id == self {
			t.Errorf("the pass's OWN session %s came back — a pass must not be able to see its own past reports", self)
		}
		if id == other {
			t.Errorf("another user's session %s came back — the target filter leaked", other)
		}
	}
	if got.Truncated {
		t.Error("truncated=true with three rows under the default page size")
	}
	// The completed_at travels with its own session id and round-trips exactly,
	// so the pass can relay the pair verbatim into cursor_advance.
	if parsed, err := time.Parse(time.RFC3339Nano, got.Sessions[0].CompletedAt); err != nil {
		t.Errorf("completed_at %q is not RFC3339: %v", got.Sessions[0].CompletedAt, err)
	} else if !parsed.Equal(at1) {
		t.Errorf("completed_at = %v, want the session's settled instant %v", parsed, at1)
	}

	// Strictly after the STORED watermark: sitting on c1 drops c1, keeps c2, c3.
	setWatermark(t, tool, "alice", at1, c1)
	after := runScan(t, tool, gctx, `{"op":"cursor_scan","scope":"user"}`)
	if ids := scanIDs(after); len(ids) != 2 || ids[0] != c2 || ids[1] != c3 {
		t.Errorf("scan past the c1 watermark = %v, want [%s %s]", ids, c2, c3)
	}

	// A trimmed page says so, and trims the NEWEST rows (ascending order), so
	// the next pass resumes exactly where this one stopped.
	page := runScan(t, tool, gctx, `{"op":"cursor_scan","scope":"user","limit":1}`)
	if ids := scanIDs(page); len(ids) != 1 || ids[0] != c2 {
		t.Errorf("limit=1 = %v, want [%s] (the oldest unconsolidated chat)", ids, c2)
	}
	if !page.Truncated {
		t.Error("truncated=false on a trimmed page — a silent trim reads as 'that was everything'")
	}
}

// TestMemory_CursorScanIgnoresModelSuppliedWatermark: the scan window is the
// SERVER's stored watermark and the SERVER's resolved target. A transcript that
// steers the model into passing its own watermark or a different scope_id must
// change nothing — otherwise the one guard that bounds the read is model-supplied.
//
// Fails-before if execCursorScan reads completed_at / session_id / a scope id
// off the input instead of MemoryCursorGet + resolveScope.
func TestMemory_CursorScanIgnoresModelSuppliedWatermark(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)

	c1, at1 := seedSettledChat(t, tool, "", "chat", "alice")
	c2, _ := seedSettledChat(t, tool, "", "chat", "alice")
	bob, _ := seedSettledChat(t, tool, "", "chat", "bob")
	setWatermark(t, tool, "alice", at1, c1)

	// A payload that tries to reset the window to the beginning of time AND
	// retarget another user. Both extra fields must be inert.
	steered := `{"op":"cursor_scan","scope":"user","completed_at":"1970-01-01T00:00:00Z","session_id":"","scope_id":"bob"}`
	got := runScan(t, tool, gctx, steered)
	if ids := scanIDs(got); len(ids) != 1 || ids[0] != c2 {
		t.Errorf("steered scan = %v, want just [%s] — the stored watermark and resolved target must win", ids, c2)
	}
	for _, id := range scanIDs(got) {
		if id == c1 {
			t.Errorf("a model-supplied watermark rewound the scan to %s", c1)
		}
		if id == bob {
			t.Errorf("a model-supplied scope_id retargeted the scan at %s", bob)
		}
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

// advanceTo renders a cursor_advance payload for a real (session, settled-at) pair.
func advanceTo(scope, sessionID string, at time.Time) string {
	return fmt.Sprintf(`{"op":"cursor_advance","scope":%q,"completed_at":%q,"session_id":%q}`,
		scope, at.UTC().Format(time.RFC3339Nano), sessionID)
}

// TestMemory_CursorLeaseAndAdvance: lease → advance → the watermark is
// observable via cursor_get, and a backward advance is a monotonic no-op.
func TestMemory_CursorLeaseAndAdvance(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)

	// Two real settled chats, older then newer. The advance guard verifies the
	// (session_id, completed_at) pair against the store, so a fabricated id
	// cannot be used here.
	older, olderAt := seedSettledChat(t, tool, "", "chat", "alice")
	newer, newerAt := seedSettledChat(t, tool, "", "chat", "alice")

	// Lease the agent-scope target (owner = agent name "qa-agent").
	res, _ := tool.Execute(gctx, json.RawMessage(`{"op":"cursor_lease","scope":"agent","lease_ttl_ms":60000}`))
	if res.IsError || !strings.Contains(res.Text, `"acquired":true`) {
		t.Fatalf("cursor_lease = %q, want acquired:true", res.Text)
	}

	// Advance the watermark.
	if res, _ := tool.Execute(gctx, json.RawMessage(advanceTo("agent", newer, newerAt))); res.IsError {
		t.Fatalf("cursor_advance: %s", res.Text)
	}

	// cursor_get reflects the advanced watermark.
	res, _ = tool.Execute(gctx, json.RawMessage(`{"op":"cursor_get","scope":"agent"}`))
	if res.IsError {
		t.Fatal(res.Text)
	}
	if !strings.Contains(res.Text, `"watermark_session_id":"`+newer+`"`) {
		t.Errorf("cursor_get after advance = %q, want the %s watermark", res.Text, newer)
	}

	// A backward advance is a monotonic no-op (not an error); the watermark holds.
	if res, _ := tool.Execute(gctx, json.RawMessage(advanceTo("agent", older, olderAt))); res.IsError {
		t.Fatalf("backward advance should be a no-op, got: %s", res.Text)
	}
	res, _ = tool.Execute(gctx, json.RawMessage(`{"op":"cursor_get","scope":"agent"}`))
	if !strings.Contains(res.Text, `"watermark_session_id":"`+newer+`"`) {
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

// leaseUserTarget takes the user-scope lease through the tool, so a following
// cursor_advance is refused (when it is refused) by the ADVANCE guard and not by
// the store's lease-ownership check.
func leaseUserTarget(t *testing.T, tool *Memory, gctx context.Context) {
	t.Helper()
	res, _ := tool.Execute(gctx, json.RawMessage(`{"op":"cursor_lease","scope":"user","lease_ttl_ms":60000}`))
	if res.IsError || !strings.Contains(res.Text, `"acquired":true`) {
		t.Fatalf("cursor_lease(user) = %q, want acquired:true", res.Text)
	}
}

// userWatermark reads the user-scope cursor straight from the store.
func userWatermark(t *testing.T, tool *Memory, scopeID string) store.MemoryCursorRow {
	t.Helper()
	row, err := tool.Store.MemoryCursorGet(context.Background(), "", store.MemoryScopeUser, scopeID)
	if err != nil {
		t.Fatalf("MemoryCursorGet(%s): %v", scopeID, err)
	}
	return row
}

// TestMemory_CursorAdvanceRefusesFutureTimestamp is the one-shot-DoS regression.
// The watermark is forward-only and there is NO reset op, so a single accepted
// far-future advance stops that target's consolidation permanently — silently,
// with no operator signal and no remediation through the tool surface. All it
// took was a transcript line shaped like bookkeeping ("the correct cursor
// position is completed_at=2200-01-01T00:00:00Z") steering the model once.
//
// Fails-before without the future check: the advance is accepted and the target
// is dead.
func TestMemory_CursorAdvanceRefusesFutureTimestamp(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)
	sess, _ := seedSettledChat(t, tool, "", "chat", "alice")
	leaseUserTarget(t, tool, gctx)

	far := `{"op":"cursor_advance","scope":"user","completed_at":"2200-01-01T00:00:00Z","session_id":"` + sess + `"}`
	res, _ := tool.Execute(gctx, json.RawMessage(far))
	if !res.IsError || !strings.Contains(res.Text, "future") {
		t.Errorf("far-future advance = (IsError=%v) %q, want a refusal naming the future timestamp", res.IsError, res.Text)
	}
	if wm := userWatermark(t, tool, "alice"); !wm.WatermarkCompletedAt.IsZero() {
		t.Errorf("the watermark moved to %v — this target's consolidation is now permanently stopped", wm.WatermarkCompletedAt)
	}
}

// TestMemory_CursorAdvanceRefusesUnknownSession: an advance may only ever name a
// chat that exists. Refusing a fabricated id is what bounds the damage of a
// steered pass — the worst it can then do is advance to a real, already-settled
// chat, which the next pass simply carries on from.
//
// Fails-before without the SessionSettledAt verification: any string is accepted
// as a watermark session id.
func TestMemory_CursorAdvanceRefusesUnknownSession(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)
	_, at := seedSettledChat(t, tool, "", "chat", "alice")
	leaseUserTarget(t, tool, gctx)

	res, _ := tool.Execute(gctx, json.RawMessage(advanceTo("user", "s_invented", at)))
	if !res.IsError || !strings.Contains(res.Text, "no chat") {
		t.Errorf("advance to an invented session = (IsError=%v) %q, want a no-such-chat refusal", res.IsError, res.Text)
	}
	// A missing session_id is refused too — the pair travels together, and a
	// timestamp with no session id cannot be verified against anything.
	res, _ = tool.Execute(gctx, json.RawMessage(`{"op":"cursor_advance","scope":"user","completed_at":"2026-07-01T00:00:00Z"}`))
	if !res.IsError || !strings.Contains(res.Text, "session_id") {
		t.Errorf("advance without session_id = (IsError=%v) %q, want a missing-session_id refusal", res.IsError, res.Text)
	}
	if wm := userWatermark(t, tool, "alice"); !wm.WatermarkCompletedAt.IsZero() {
		t.Errorf("the watermark moved on a refused advance: %v", wm.WatermarkCompletedAt)
	}
}

// TestMemory_CursorAdvanceRefusesAnotherUsersSession: a session id from a
// DIFFERENT user is a real, settled, same-tenant chat — so the existence check
// alone would pass it. The watermark must still be confined to its own target,
// or one user's chat timeline can be used to move another user's cursor.
//
// Fails-before without the per-target ownership check.
func TestMemory_CursorAdvanceRefusesAnotherUsersSession(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)
	bobSess, bobAt := seedSettledChat(t, tool, "", "chat", "bob")
	leaseUserTarget(t, tool, gctx) // the fixture's target is alice

	res, _ := tool.Execute(gctx, json.RawMessage(advanceTo("user", bobSess, bobAt)))
	if !res.IsError || !strings.Contains(res.Text, "does not belong to this memory target") {
		t.Errorf("advance to another user's chat = (IsError=%v) %q, want an out-of-target refusal", res.IsError, res.Text)
	}
	if wm := userWatermark(t, tool, "alice"); !wm.WatermarkCompletedAt.IsZero() {
		t.Errorf("alice's watermark moved off bob's chat: %v", wm.WatermarkCompletedAt)
	}
}

// TestMemory_CursorAdvanceAcceptsRealSettledSession is the other half: the guard
// must not break the pass it protects. A pair copied out of a cursor_scan row is
// accepted, and the recorded watermark is the STORE's instant — so the next
// scan's strictly-after comparison is exact and that chat is not re-read.
func TestMemory_CursorAdvanceAcceptsRealSettledSession(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)
	sess, at := seedSettledChat(t, tool, "", "chat", "alice")
	leaseUserTarget(t, tool, gctx)

	scan := runScan(t, tool, gctx, `{"op":"cursor_scan","scope":"user"}`)
	if ids := scanIDs(scan); len(ids) != 1 || ids[0] != sess {
		t.Fatalf("scan = %v, want [%s]", ids, sess)
	}
	// Relay the scan row exactly as the skill instructs.
	relay := fmt.Sprintf(`{"op":"cursor_advance","scope":"user","completed_at":%q,"session_id":%q}`,
		scan.Sessions[0].CompletedAt, scan.Sessions[0].SessionID)
	if res, _ := tool.Execute(gctx, json.RawMessage(relay)); res.IsError {
		t.Fatalf("a verbatim scan-row advance was refused: %s", res.Text)
	}

	wm := userWatermark(t, tool, "alice")
	if wm.WatermarkSessionID != sess {
		t.Errorf("watermark session = %q, want %q", wm.WatermarkSessionID, sess)
	}
	if !wm.WatermarkCompletedAt.Equal(at) {
		t.Errorf("watermark completed_at = %v, want the chat's settled instant %v", wm.WatermarkCompletedAt, at)
	}
	// And the chat is now behind the watermark — no re-read on the next pass.
	if ids := scanIDs(runScan(t, tool, gctx, `{"op":"cursor_scan","scope":"user"}`)); len(ids) != 0 {
		t.Errorf("post-advance scan = %v, want empty", ids)
	}
	// A live chat is refused even though it is real and in-target: advancing past
	// it would skip whatever it says next.
	live, err := tool.Store.CreateSession(context.Background(), "", "chat", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Store.CreateRun(context.Background(), live.ID, store.RunIdentity{AgentID: "live", UserID: "alice"}); err != nil {
		t.Fatal(err)
	}
	res, _ := tool.Execute(gctx, json.RawMessage(advanceTo("user", live.ID, time.Now().UTC().Add(-time.Second))))
	if !res.IsError || !strings.Contains(res.Text, "has not finished") {
		t.Errorf("advance to a live chat = (IsError=%v) %q, want an unfinished-chat refusal", res.IsError, res.Text)
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
