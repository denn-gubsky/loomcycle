package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// memoryFixture builds a Memory tool backed by a SQLite store and
// returns a ctx pre-populated with a sensible run identity. Tests
// override per-call ctx values (policy, agent name, user_id) when
// they want to exercise specific gates.
func memoryFixture(t *testing.T) (*Memory, context.Context, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	tool := &Memory{
		Store:             s,
		MaxValueBytes:     65536,
		DefaultQuotaBytes: 1 << 20,
	}
	ctx := tools.WithAgentName(context.Background(), "qa-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", AgentID: "a_test"})
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user"},
		QuotaBytes:    0,
	})
	return tool, ctx, func() { _ = s.Close() }
}

// TestMemory_TenantIsolation drives the Memory tool under two RunIdentity
// tenants writing the SAME user-scope key: each reads back only its own value,
// and a cross-tenant get returns not-found (value:null). RFC BL turn-on — fails
// on the pre-stamp code where every op keyed tenant "" (b's write clobbers a's,
// and a-only is cross-tenant visible).
func TestMemory_TenantIsolation(t *testing.T) {
	tool, _, cleanup := memoryFixture(t)
	defer cleanup()

	mkCtx := func(tenant string) context.Context {
		ctx := tools.WithAgentName(context.Background(), "qa-agent")
		ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", TenantID: tenant})
		ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"agent", "user"}})
		return ctx
	}
	ctxA := mkCtx("tenant-a")
	ctxB := mkCtx("tenant-b")

	// Both tenants write the SAME user-scope key with distinct values.
	if res, _ := tool.Execute(ctxA, json.RawMessage(`{"op":"set","scope":"user","key":"voice","value":{"who":"a"}}`)); res.IsError {
		t.Fatalf("set(a): %s", res.Text)
	}
	if res, _ := tool.Execute(ctxB, json.RawMessage(`{"op":"set","scope":"user","key":"voice","value":{"who":"b"}}`)); res.IsError {
		t.Fatalf("set(b): %s", res.Text)
	}

	// Each reads back ONLY its own value (no clobber across tenants).
	resA, _ := tool.Execute(ctxA, json.RawMessage(`{"op":"get","scope":"user","key":"voice"}`))
	if !strings.Contains(resA.Text, `"who":"a"`) || strings.Contains(resA.Text, `"who":"b"`) {
		t.Errorf("tenant a get = %s, want who:a only", resA.Text)
	}
	resB, _ := tool.Execute(ctxB, json.RawMessage(`{"op":"get","scope":"user","key":"voice"}`))
	if !strings.Contains(resB.Text, `"who":"b"`) || strings.Contains(resB.Text, `"who":"a"`) {
		t.Errorf("tenant b get = %s, want who:b only", resB.Text)
	}

	// A key written ONLY under tenant a is invisible to tenant b (opaque miss).
	if res, _ := tool.Execute(ctxA, json.RawMessage(`{"op":"set","scope":"user","key":"a-only","value":1}`)); res.IsError {
		t.Fatalf("set(a-only): %s", res.Text)
	}
	resMiss, _ := tool.Execute(ctxB, json.RawMessage(`{"op":"get","scope":"user","key":"a-only"}`))
	if !strings.Contains(resMiss.Text, `"value":null`) {
		t.Errorf("tenant b must NOT see tenant a's a-only key; got %s", resMiss.Text)
	}
}

// TestMemory_RejectsGlobalScope pins the isolation invariant the help index
// (RFC BL P1) depends on: the Memory tool never exposes the reserved `global`
// scope, so no wire path can reach the reserved `__help__` namespace where the
// help index lives. resolveScope refuses it both at the ACL gate (default
// policy) AND at the switch (even a misconfigured policy that lists "global").
func TestMemory_RejectsGlobalScope(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	// Default policy is {agent, user}; global is refused at the ACL gate.
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"global","key":"__help__"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !res.IsError {
		t.Fatalf("scope=global was accepted; the reserved __help__ namespace must be unreachable")
	}

	// Defense in depth: even if a policy erroneously lists "global", resolveScope's
	// switch has no global case, so it still refuses — the reserved namespace
	// stays unreachable across any future policy refactor.
	ctx2 := tools.WithAgentName(context.Background(), "qa-agent")
	ctx2 = tools.WithRunIdentity(ctx2, tools.RunIdentityValue{UserID: "alice"})
	ctx2 = tools.WithMemoryPolicy(ctx2, tools.MemoryPolicyValue{AllowedScopes: []string{"agent", "user", "global"}})
	res2, err := tool.Execute(ctx2, json.RawMessage(`{"op":"get","scope":"global","key":"__help__"}`))
	if err != nil {
		t.Fatalf("execute (policy-allows-global): %v", err)
	}
	if !res2.IsError {
		t.Fatalf("scope=global accepted via resolveScope switch; reserved namespace reachable")
	}
}

func TestMemoryTool_SetGetDeleteRoundTrip(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"user","key":"voice","value":{"style":"concise"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("set is_error: %s", res.Text)
	}

	res, err = tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"user","key":"voice"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("get is_error: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"style":"concise"`) {
		t.Errorf("get result missing stored value: %s", res.Text)
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"delete","scope":"user","key":"voice"}`))
	if !strings.Contains(res.Text, `"deleted":true`) {
		t.Errorf("delete result: %s", res.Text)
	}
}

func TestMemoryTool_GetMissingReturnsNullValue(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"agent","key":"absent"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("get on missing key should not be an error; got %s", res.Text)
	}
	if !strings.Contains(res.Text, `"value":null`) {
		t.Errorf("expected value:null for missing key, got %s", res.Text)
	}
}

func TestMemoryTool_IncrementCounter(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	for i, want := range []string{`"value":1`, `"value":2`, `"value":7`} {
		input := `{"op":"incr","scope":"agent","key":"warnings"}`
		if i == 2 {
			input = `{"op":"incr","scope":"agent","key":"warnings","delta":5}`
		}
		res, _ := tool.Execute(ctx, json.RawMessage(input))
		if res.IsError {
			t.Fatalf("incr[%d] is_error: %s", i, res.Text)
		}
		if !strings.Contains(res.Text, want) {
			t.Errorf("incr[%d] result %s, want %s", i, res.Text, want)
		}
	}
}

func TestMemoryTool_IncrementWrongType(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"agent","key":"obj","value":{"hello":"world"}}`))
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"incr","scope":"agent","key":"obj"}`))
	if !res.IsError {
		t.Errorf("incr on object should be is_error; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "JSON number") {
		t.Errorf("expected wrong-type error, got %s", res.Text)
	}
}

func TestMemoryTool_ListPrefix(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	for _, k := range []string{"events/1", "events/2", "prefs/x"} {
		_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"agent","key":"`+k+`","value":1}`))
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list","scope":"agent","prefix":"events/"}`))
	if res.IsError {
		t.Fatalf("list is_error: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"events/1"`) || !strings.Contains(res.Text, `"events/2"`) {
		t.Errorf("list missing expected keys: %s", res.Text)
	}
	if strings.Contains(res.Text, `"prefs/x"`) {
		t.Errorf("list returned non-prefix-matching key: %s", res.Text)
	}
}

func TestMemoryTool_ScopeNotInPolicyRefused(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	// Override policy: only allow agent scope.
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"agent"}})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"user","key":"x","value":1}`))
	if !res.IsError {
		t.Fatalf("write to disallowed scope should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "memory_scopes") {
		t.Errorf("expected diagnostic mentioning memory_scopes, got %s", res.Text)
	}
}

func TestMemoryTool_NoPolicyMeansNoAccess(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	// Strip policy entirely — agent has Memory in tools but
	// no memory_scopes — this simulates the "tool granted but no
	// scopes configured" misconfiguration.
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"agent","key":"x"}`))
	if !res.IsError {
		t.Errorf("expected refusal on empty memory_scopes; got %s", res.Text)
	}
}

func TestMemoryTool_AgentScopeRequiresAgentName(t *testing.T) {
	tool, _, cleanup := memoryFixture(t)
	defer cleanup()
	// Build a fresh ctx WITHOUT an agent name.
	ctx := tools.WithMemoryPolicy(context.Background(), tools.MemoryPolicyValue{AllowedScopes: []string{"agent"}})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"agent","key":"x"}`))
	if !res.IsError {
		t.Errorf("missing agent name should be a refusal; got %s", res.Text)
	}
}

func TestMemoryTool_UserScopeRequiresUserID(t *testing.T) {
	tool, _, cleanup := memoryFixture(t)
	defer cleanup()
	ctx := tools.WithAgentName(context.Background(), "qa-agent")
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: []string{"user"}})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"user","key":"x"}`))
	if !res.IsError {
		t.Errorf("missing user_id should be a refusal; got %s", res.Text)
	}
}

func TestMemoryTool_ScopeIsolation(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	// Set under user=alice's user-scope.
	_, _ = tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"user","key":"secret","value":"alice-only"}`))
	// Switch to user=bob.
	ctxBob := tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "bob", AgentID: "a_b"})
	res, _ := tool.Execute(ctxBob, json.RawMessage(`{"op":"get","scope":"user","key":"secret"}`))
	if res.IsError {
		t.Fatalf("get is_error: %s", res.Text)
	}
	if strings.Contains(res.Text, "alice-only") {
		t.Errorf("user-scope leaked across user_ids: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"value":null`) {
		t.Errorf("bob should see no value for alice's key, got %s", res.Text)
	}
}

func TestMemoryTool_ValueTooLarge(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	tool.MaxValueBytes = 16

	big := strings.Repeat("a", 100)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"agent","key":"k","value":"`+big+`"}`))
	if !res.IsError {
		t.Errorf("oversized write should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "max") {
		t.Errorf("expected size error, got %s", res.Text)
	}
}

func TestMemoryTool_QuotaEnforcement(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	// Tighten the per-scope quota via policy override. 64 bytes is
	// enough for one small value but not two.
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent"},
		QuotaBytes:    64,
	})

	// First write fits.
	value := json.RawMessage(`"` + strings.Repeat("a", 30) + `"`)
	in1 := mustMarshal(t, map[string]any{"op": "set", "scope": "agent", "key": "k1", "value": value})
	if res, _ := tool.Execute(ctx, in1); res.IsError {
		t.Fatalf("first write should fit: %s", res.Text)
	}
	// Second write blows the cap.
	in2 := mustMarshal(t, map[string]any{"op": "set", "scope": "agent", "key": "k2", "value": value})
	res, _ := tool.Execute(ctx, in2)
	if !res.IsError {
		t.Errorf("second write should be quota-refused; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "quota") {
		t.Errorf("expected quota error, got %s", res.Text)
	}
}

func TestMemoryTool_UnknownOp(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"clear","scope":"agent"}`))
	if !res.IsError {
		t.Errorf("unknown op should be is_error; got %s", res.Text)
	}
}

func TestMemoryTool_NoStore(t *testing.T) {
	tool := &Memory{}
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"op":"get","scope":"agent","key":"x"}`))
	if !res.IsError {
		t.Errorf("nil-store call should be is_error")
	}
	if !strings.Contains(res.Text, "not configured") {
		t.Errorf("expected 'not configured' diagnostic, got %s", res.Text)
	}
}

// Compile-time guard: every store the test fixture uses satisfies
// store.Store. (Just a smoke check; the SQLite adapter is well-tested
// already.)
var _ store.Store = (*sqlite.Store)(nil)

// mustMarshal is a tiny helper for building input JSON in tests.
func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- v0.12.x reducer ops: merge / append_dedupe / bounded_list ----

func TestMemoryTool_Merge_OnEmpty(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	res, err := tool.Execute(ctx, json.RawMessage(
		`{"op":"merge","scope":"user","key":"profile","value":{"name":"Alice"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("merge is_error: %s", res.Text)
	}
	// Verify via Get.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"user","key":"profile"}`))
	if !strings.Contains(res.Text, `"name":"Alice"`) {
		t.Errorf("merged value not stored: %s", res.Text)
	}
}

// Two sequential merges must combine: the second merge's fields overlay
// the first's. Validates the deep-merge semantics at the tool layer.
func TestMemoryTool_Merge_OverlaysFields(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	// First merge sets name + likes:[rock]
	_, _ = tool.Execute(ctx, json.RawMessage(
		`{"op":"merge","scope":"user","key":"profile","value":{"name":"Alice","likes":["rock"]}}`))
	// Second merge replaces likes (arrays don't concat, they replace).
	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"merge","scope":"user","key":"profile","value":{"likes":["jazz"],"age":30}}`))
	if res.IsError {
		t.Fatalf("second merge is_error: %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"user","key":"profile"}`))
	// Expect name still Alice, likes is the new array, age 30.
	if !strings.Contains(res.Text, `"name":"Alice"`) {
		t.Errorf("merge lost name field: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"likes":["jazz"]`) {
		t.Errorf("merge should replace arrays, not concat: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"age":30`) {
		t.Errorf("merge dropped age: %s", res.Text)
	}
}

// Nested-object merge: a merge into {a:{x:1}} with {a:{y:2}} should
// produce {a:{x:1,y:2}}, not replace `a` entirely.
func TestMemoryTool_Merge_NestedObjects(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	_, _ = tool.Execute(ctx, json.RawMessage(
		`{"op":"merge","scope":"user","key":"cfg","value":{"a":{"x":1}}}`))
	_, _ = tool.Execute(ctx, json.RawMessage(
		`{"op":"merge","scope":"user","key":"cfg","value":{"a":{"y":2}}}`))
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"user","key":"cfg"}`))
	if !strings.Contains(res.Text, `"x":1`) || !strings.Contains(res.Text, `"y":2`) {
		t.Errorf("nested merge lost a key: %s", res.Text)
	}
}

// Merge into a non-object existing value must refuse (silent
// replacement would surprise the agent).
func TestMemoryTool_Merge_RefusesNonObjectExisting(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	_, _ = tool.Execute(ctx, json.RawMessage(
		`{"op":"set","scope":"user","key":"k","value":42}`))
	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"merge","scope":"user","key":"k","value":{"x":1}}`))
	if !res.IsError {
		t.Fatalf("expected is_error on merge into number, got: %s", res.Text)
	}
	if !strings.Contains(res.Text, "not a JSON object") {
		t.Errorf("expected 'not a JSON object' message: %s", res.Text)
	}
}

func TestMemoryTool_Merge_RefusesNonObjectIncoming(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"merge","scope":"user","key":"k","value":[1,2,3]}`))
	if !res.IsError {
		t.Fatalf("expected is_error on merge with array value, got: %s", res.Text)
	}
}

// ---- append_dedupe ----

func TestMemoryTool_AppendDedupe_FirstAppend(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"append_dedupe","scope":"user","key":"seen","value":"article-42"}`))
	if res.IsError {
		t.Fatalf("append is_error: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"appended":true`) {
		t.Errorf("first append should report appended:true: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"article-42"`) {
		t.Errorf("appended item missing from value: %s", res.Text)
	}
}

// Idempotency — the same item twice produces appended:false on the
// second call and the array contains exactly one entry.
func TestMemoryTool_AppendDedupe_Idempotent(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	_, _ = tool.Execute(ctx, json.RawMessage(
		`{"op":"append_dedupe","scope":"user","key":"seen","value":"article-42"}`))
	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"append_dedupe","scope":"user","key":"seen","value":"article-42"}`))
	if res.IsError {
		t.Fatalf("second append is_error: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"appended":false`) {
		t.Errorf("second append should report appended:false: %s", res.Text)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"user","key":"seen"}`))
	// Should still contain article-42 exactly once.
	if got := strings.Count(res.Text, `"article-42"`); got != 1 {
		t.Errorf("article-42 appears %d times, want 1: %s", got, res.Text)
	}
}

// JSON-equality dedupe: two objects with the same fields in different
// orders count as equal.
func TestMemoryTool_AppendDedupe_ObjectFieldOrderEquality(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	_, _ = tool.Execute(ctx, json.RawMessage(
		`{"op":"append_dedupe","scope":"user","key":"items","value":{"a":1,"b":2}}`))
	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"append_dedupe","scope":"user","key":"items","value":{"b":2,"a":1}}`))
	if !strings.Contains(res.Text, `"appended":false`) {
		t.Errorf("field-order-swapped object should dedupe: %s", res.Text)
	}
}

func TestMemoryTool_AppendDedupe_RefusesNonArrayExisting(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	_, _ = tool.Execute(ctx, json.RawMessage(
		`{"op":"set","scope":"user","key":"k","value":{"not":"array"}}`))
	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"append_dedupe","scope":"user","key":"k","value":"x"}`))
	if !res.IsError {
		t.Fatalf("expected is_error on append to object, got: %s", res.Text)
	}
}

// ---- bounded_list ----

func TestMemoryTool_BoundedList_AppendUnderLimit(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	for i, item := range []string{`"a"`, `"b"`, `"c"`} {
		body := `{"op":"bounded_list","scope":"agent","key":"events","value":` + item + `,"limit":10}`
		res, _ := tool.Execute(ctx, json.RawMessage(body))
		if res.IsError {
			t.Fatalf("bounded_list #%d is_error: %s", i, res.Text)
		}
		if !strings.Contains(res.Text, `"dropped":0`) {
			t.Errorf("call %d should drop 0: %s", i, res.Text)
		}
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"agent","key":"events"}`))
	if !strings.Contains(res.Text, `"a","b","c"`) {
		t.Errorf("insertion order broken: %s", res.Text)
	}
}

// At-limit + over-limit behavior: append #4 with limit=3 drops the
// oldest entry. Subsequent appends keep the trailing 3.
func TestMemoryTool_BoundedList_DropsOldest(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	for _, item := range []string{`"a"`, `"b"`, `"c"`} {
		_, _ = tool.Execute(ctx, json.RawMessage(
			`{"op":"bounded_list","scope":"agent","key":"events","value":`+item+`,"limit":3}`))
	}
	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"bounded_list","scope":"agent","key":"events","value":"d","limit":3}`))
	if !strings.Contains(res.Text, `"dropped":1`) {
		t.Errorf("over-cap append should report dropped:1: %s", res.Text)
	}
	if !strings.Contains(res.Text, `"b","c","d"`) {
		t.Errorf("expected b,c,d after dropping oldest a: %s", res.Text)
	}
}

func TestMemoryTool_BoundedList_RejectsZeroLimit(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"bounded_list","scope":"agent","key":"events","value":"x","limit":0}`))
	if !res.IsError {
		t.Fatalf("expected is_error on limit=0, got: %s", res.Text)
	}
}

func TestMemoryTool_BoundedList_RejectsExcessiveLimit(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(
		`{"op":"bounded_list","scope":"agent","key":"events","value":"x","limit":20000}`))
	if !res.IsError {
		t.Fatalf("expected is_error on limit > 10000, got: %s", res.Text)
	}
}

// ---- shared concerns ----

func TestMemoryTool_Reducers_MissingValueFieldRefused(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()

	for _, op := range []string{"merge", "append_dedupe", "bounded_list"} {
		body := `{"op":"` + op + `","scope":"user","key":"k","limit":5}`
		res, _ := tool.Execute(ctx, json.RawMessage(body))
		if !res.IsError {
			t.Errorf("%s with missing value should refuse, got: %s", op, res.Text)
		}
	}
}

func TestMemoryTool_Reducers_RefuseScopeNotInPolicy(t *testing.T) {
	tool, _, cleanup := memoryFixture(t)
	defer cleanup()
	ctx := tools.WithAgentName(context.Background(), "qa-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", AgentID: "a_test"})
	// Policy only allows agent scope; user-scope reducer calls must refuse.
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent"},
	})

	for _, op := range []string{"merge", "append_dedupe", "bounded_list"} {
		body := `{"op":"` + op + `","scope":"user","key":"k","value":{"x":1},"limit":5}`
		res, _ := tool.Execute(ctx, json.RawMessage(body))
		if !res.IsError {
			t.Errorf("%s with disallowed scope should refuse, got: %s", op, res.Text)
		}
		if !strings.Contains(res.Text, "memory_scopes") {
			t.Errorf("%s refusal should mention memory_scopes: %s", op, res.Text)
		}
	}
}
