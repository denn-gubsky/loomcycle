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
