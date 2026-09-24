package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The read-modify-write ops check their size caps on the value they are about
// to commit. A refusal must therefore leave the stored value as it was: the
// same check made after the update returned told the agent "refused" about a
// write that had already landed.
func TestMemoryReducers_RefusedOverCapLeavesTheValueUnchanged(t *testing.T) {
	type cap struct {
		name  string
		setup func(ctx context.Context, tool *Memory) context.Context
		want  string
	}
	caps := []cap{
		{"scope quota", func(ctx context.Context, tool *Memory) context.Context {
			tool.DefaultQuotaBytes = 64
			return ctx
		}, "quota"},
		{"core block limit_bytes", func(ctx context.Context, _ *Memory) context.Context {
			return tools.WithCoreBlocksPolicy(ctx, tools.CoreBlocksPolicyValue{Blocks: []config.CoreBlock{
				{Label: "k", Scope: "agent", LimitBytes: 40},
			}})
		}, "limit_bytes"},
	}
	ops := []struct {
		name, seed, grow string
	}{
		{"merge", `{"a":"small"}`, `{"op":"merge","scope":"agent","key":"core/k","value":{"b":"a value long enough to push the row past either cap"}}`},
		{"append_dedupe", `["small"]`, `{"op":"append_dedupe","scope":"agent","key":"core/k","value":"a value long enough to push the row past either cap"}`},
		{"bounded_list", `["small"]`, `{"op":"bounded_list","scope":"agent","key":"core/k","value":"a value long enough to push the row past either cap","limit":10}`},
	}
	for _, c := range caps {
		for _, op := range ops {
			t.Run(c.name+"/"+op.name, func(t *testing.T) {
				tool, ctx, cleanup := memoryFixture(t)
				defer cleanup()
				if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"set","scope":"agent","key":"core/k","value":`+op.seed+`}`)); res.IsError {
					t.Fatalf("seed: %s", res.Text)
				}
				ctx = c.setup(ctx, tool)

				res, _ := tool.Execute(ctx, json.RawMessage(op.grow))
				if !res.IsError || !strings.Contains(res.Text, c.want) {
					t.Fatalf("over-cap %s: %q, want a refusal naming %s", op.name, res.Text, c.want)
				}
				if !strings.HasPrefix(res.Text, "Memory."+op.name+":") {
					t.Errorf("refusal %q does not name the op that made it", res.Text)
				}
				got, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","scope":"agent","key":"core/k"}`))
				var out struct {
					Value json.RawMessage `json:"value"`
				}
				_ = json.Unmarshal([]byte(got.Text), &out)
				if canonJSON(t, out.Value) != canonJSON(t, json.RawMessage(op.seed)) {
					t.Errorf("value after a refused %s = %s, want it unchanged (%s)", op.name, out.Value, op.seed)
				}
			})
		}
	}
}

// The quota charges the post-write size against what the rest of the scope
// holds: a reducer that fits is still accepted, and one that does not is
// refused, measured the same way set measures.
func TestMemoryReducers_WithinQuotaIsAccepted(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	tool.DefaultQuotaBytes = 256
	for _, req := range []string{
		`{"op":"merge","scope":"agent","key":"m","value":{"a":1}}`,
		`{"op":"append_dedupe","scope":"agent","key":"a","value":"x"}`,
		`{"op":"bounded_list","scope":"agent","key":"b","value":"y","limit":3}`,
	} {
		if res, _ := tool.Execute(ctx, json.RawMessage(req)); res.IsError {
			t.Errorf("%s within quota refused: %s", req, res.Text)
		}
	}
}

func canonJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
