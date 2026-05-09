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
	// Strip policy entirely — agent has Memory in allowed_tools but
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
