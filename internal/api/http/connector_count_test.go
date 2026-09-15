package http

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// countingTool reports a fixed count, so the seam under test is the CARRY and
// nothing else.
type countingTool struct {
	name  string
	count *int
}

func (c *countingTool) Name() string                 { return c.name }
func (c *countingTool) Description() string          { return "counts" }
func (c *countingTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (c *countingTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Text: "rendered", Count: c.count}, nil
}

// TestDispatchBuiltin_CarriesCount covers the connector seam.
//
// It exists because a probe showed that deleting `Count: res.Count` from all
// ten builtin mappings broke nothing: the MCP tests construct a
// connector.ToolResult directly and the builtin tests stop at tools.Result, so
// both sit on either SIDE of the carry without crossing it. That is the third
// time in this feature line that a seam was covered by nothing while the code
// on both ends was tested.
func TestDispatchBuiltin_CarriesCount(t *testing.T) {
	t.Run("zero survives as zero, not as absent", func(t *testing.T) {
		zero := 0
		s := &Server{tools: []tools.Tool{&countingTool{name: "counter", count: &zero}}}

		res, err := s.dispatchBuiltin(context.Background(), "counter", json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("dispatchBuiltin: %v", err)
		}
		if res.Count == nil {
			t.Fatal("Count was dropped crossing the connector — an empty result now " +
				"looks identical to a tool that never counted")
		}
		if *res.Count != 0 {
			t.Errorf("Count = %d, want 0", *res.Count)
		}
	})

	t.Run("a non-zero count survives", func(t *testing.T) {
		seven := 7
		s := &Server{tools: []tools.Tool{&countingTool{name: "counter", count: &seven}}}
		res, err := s.dispatchBuiltin(context.Background(), "counter", json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("dispatchBuiltin: %v", err)
		}
		if res.Count == nil || *res.Count != 7 {
			t.Errorf("Count = %v, want 7", res.Count)
		}
	})

	t.Run("a tool that does not count stays uncounted", func(t *testing.T) {
		s := &Server{tools: []tools.Tool{&countingTool{name: "counter"}}}
		res, err := s.dispatchBuiltin(context.Background(), "counter", json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("dispatchBuiltin: %v", err)
		}
		if res.Count != nil {
			t.Errorf("invented a count of %d for a tool that does not count", *res.Count)
		}
	})
}
