package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestCoreBlock_OverLimitWriteRefused pins RFC BL P1: an agent write to a
// core/<label> key whose block declares limit_bytes is refused when the value
// exceeds the cap, and permitted when it fits. Fails on pre-change code, where
// the Memory tool never consults the core-block policy and the over-cap write
// lands (it is under MaxValueBytes and the scope quota).
func TestCoreBlock_OverLimitWriteRefused(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	ctx = tools.WithCoreBlocksPolicy(ctx, tools.CoreBlocksPolicyValue{Blocks: []config.CoreBlock{
		{Label: "notes", Scope: "agent", LimitBytes: 16},
	}})

	// 30-byte value > 16-byte limit → refused.
	over := `{"op":"set","scope":"agent","key":"core/notes","value":"xxxxxxxxxxxxxxxxxxxxxxxxxxxx"}`
	res, _ := tool.Execute(ctx, json.RawMessage(over))
	if !res.IsError {
		t.Fatalf("over-limit write should be refused, got success: %s", res.Text)
	}
	if !strings.Contains(res.Text, "limit_bytes") {
		t.Errorf("refusal should name limit_bytes, got: %s", res.Text)
	}

	// A value that fits the limit is accepted.
	under := `{"op":"set","scope":"agent","key":"core/notes","value":"hi"}`
	if res, _ := tool.Execute(ctx, json.RawMessage(under)); res.IsError {
		t.Errorf("within-limit write should succeed, got: %s", res.Text)
	}
}

// TestCoreBlock_ReadOnlyRefusesAgentWrite pins RFC BL P1: a read_only core
// block refuses agent writes entirely (operator-authored). Fails on pre-change
// code, where the write lands.
func TestCoreBlock_ReadOnlyRefusesAgentWrite(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	ctx = tools.WithCoreBlocksPolicy(ctx, tools.CoreBlocksPolicyValue{Blocks: []config.CoreBlock{
		{Label: "human", Scope: "user", ReadOnly: true},
	}})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"user","key":"core/human","value":"tampered"}`))
	if !res.IsError {
		t.Fatalf("write to a read_only core block should be refused, got success: %s", res.Text)
	}
	if !strings.Contains(res.Text, "read_only") {
		t.Errorf("refusal should name read_only, got: %s", res.Text)
	}
}

// TestCoreBlock_ReadOnlyRefusesAllMutatingOps pins RFC BL P1: a read_only core
// block refuses EVERY mutating op, not just set — delete/merge/incr/
// append_dedupe/bounded_list all target the same core/<label> keyspace and must
// be refused so an agent cannot erase or overwrite an operator-seeded block.
// Fails on pre-change code, where only execSet consulted the policy and the
// other ops mutated the key unchecked.
func TestCoreBlock_ReadOnlyRefusesAllMutatingOps(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	ctx = tools.WithCoreBlocksPolicy(ctx, tools.CoreBlocksPolicyValue{Blocks: []config.CoreBlock{
		{Label: "ro", Scope: "agent", ReadOnly: true},
	}})

	cases := []struct {
		name string
		req  string
	}{
		{"delete", `{"op":"delete","scope":"agent","key":"core/ro"}`},
		{"merge", `{"op":"merge","scope":"agent","key":"core/ro","value":{"a":1}}`},
		{"incr", `{"op":"incr","scope":"agent","key":"core/ro"}`},
		{"append_dedupe", `{"op":"append_dedupe","scope":"agent","key":"core/ro","value":1}`},
		{"bounded_list", `{"op":"bounded_list","scope":"agent","key":"core/ro","value":1,"limit":5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := tool.Execute(ctx, json.RawMessage(tc.req))
			if !res.IsError {
				t.Fatalf("%s on a read_only core block should be refused, got success: %s", tc.name, res.Text)
			}
			if !strings.Contains(res.Text, "read_only") {
				t.Errorf("%s refusal should name read_only, got: %s", tc.name, res.Text)
			}
		})
	}
}

// TestCoreBlock_LimitEnforcedOnGrowingOps pins RFC BL P1: the per-block
// limit_bytes cap is enforced on the value-GROWING ops (merge/append_dedupe/
// bounded_list), checked against the post-reduction result — not only on set.
// A result over the cap is refused; a small result is accepted. Fails on
// pre-change code, where these ops never consulted the core-block policy.
func TestCoreBlock_LimitEnforcedOnGrowingOps(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	ctx = tools.WithCoreBlocksPolicy(ctx, tools.CoreBlocksPolicyValue{Blocks: []config.CoreBlock{
		{Label: "cap_m", Scope: "agent", LimitBytes: 24},
		{Label: "cap_a", Scope: "agent", LimitBytes: 24},
		{Label: "cap_b", Scope: "agent", LimitBytes: 24},
		{Label: "cap_ok", Scope: "agent", LimitBytes: 24},
	}})

	// Each op's resulting value exceeds the 24-byte cap → refused.
	over := []struct {
		name string
		req  string
	}{
		{"merge", `{"op":"merge","scope":"agent","key":"core/cap_m","value":{"data":"xxxxxxxxxxxxxxxxxxxxxxxx"}}`},
		{"append_dedupe", `{"op":"append_dedupe","scope":"agent","key":"core/cap_a","value":"xxxxxxxxxxxxxxxxxxxxxxxx"}`},
		{"bounded_list", `{"op":"bounded_list","scope":"agent","key":"core/cap_b","value":"xxxxxxxxxxxxxxxxxxxxxxxx","limit":10}`},
	}
	for _, tc := range over {
		t.Run(tc.name+"_over", func(t *testing.T) {
			res, _ := tool.Execute(ctx, json.RawMessage(tc.req))
			if !res.IsError {
				t.Fatalf("%s exceeding limit_bytes should be refused, got success: %s", tc.name, res.Text)
			}
			if !strings.Contains(res.Text, "limit_bytes") {
				t.Errorf("%s refusal should name limit_bytes, got: %s", tc.name, res.Text)
			}
		})
	}

	// A merge whose result fits the cap is accepted (proves the gate is a cap,
	// not a blanket refusal on the keyspace).
	t.Run("merge_under", func(t *testing.T) {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"merge","scope":"agent","key":"core/cap_ok","value":{"a":1}}`))
		if res.IsError {
			t.Errorf("within-limit merge should succeed, got: %s", res.Text)
		}
	})
}

// TestCoreBlock_ScopeMismatchDoesNotGate pins that a block's scope must match
// the write's resolved scope: a user-scope read_only block does NOT gate an
// agent-scope write of the same label (they are different keys).
func TestCoreBlock_ScopeMismatchDoesNotGate(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	ctx = tools.WithCoreBlocksPolicy(ctx, tools.CoreBlocksPolicyValue{Blocks: []config.CoreBlock{
		{Label: "human", Scope: "user", ReadOnly: true},
	}})
	// agent-scope core/human is a distinct key — the user-scope block must not gate it.
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"agent","key":"core/human","value":"ok"}`)); res.IsError {
		t.Errorf("agent-scope write should not be gated by a user-scope block: %s", res.Text)
	}
}
