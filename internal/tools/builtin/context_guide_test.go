package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// hintedFakeTool is a fakeTool that also implements tools.HintedTool, so the
// guide test can assert the hand-written hint is surfaced.
type hintedFakeTool struct {
	fakeTool
	hint string
}

func (h *hintedFakeTool) UsageHint() string { return h.hint }

// TestContextTool_GuideParsesOpsRequiredAndHint: op=guide reads the op enum and
// the required list straight from each tool's OWN schema (so the digest cannot
// drift from what the tool accepts) and surfaces the optional UsageHint.
func TestContextTool_GuideParsesOpsRequiredAndHint(t *testing.T) {
	ct := &Context{Tools: []tools.Tool{
		&hintedFakeTool{
			fakeTool: fakeTool{
				NameVal:   "Widget",
				DescVal:   "A widget tool.",
				SchemaVal: `{"type":"object","properties":{"op":{"enum":["poke","prod"]}},"required":["op","target"]}`,
			},
			hint: "poke reads, prod writes.",
		},
		// A tool with no op enum, no required and no hint: the digest omits those
		// fields rather than inventing them.
		&fakeTool{NameVal: "Read", DescVal: "Read a file.", SchemaVal: `{"type":"object"}`},
	}}
	ctx := tools.WithAgentTools(context.Background(), []string{"Widget", "Read"})

	res, err := ct.Execute(ctx, json.RawMessage(`{"op":"guide"}`))
	if err != nil || res.IsError {
		t.Fatalf("op=guide failed: err=%v isErr=%v text=%s", err, res.IsError, res.Text)
	}

	var parsed struct {
		Tools []struct {
			Name            string   `json:"name"`
			SideEffectClass string   `json:"side_effect_class"`
			Ops             []string `json:"ops"`
			Required        []string `json:"required"`
			Hint            string   `json:"hint"`
		} `json:"tools"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(res.Text), &parsed); err != nil {
		t.Fatalf("decode: %v (text=%s)", err, res.Text)
	}
	if parsed.Count != 2 {
		t.Fatalf("count = %d, want 2", parsed.Count)
	}
	byName := map[string]int{}
	for i, e := range parsed.Tools {
		byName[e.Name] = i
	}
	w := parsed.Tools[byName["Widget"]]
	if len(w.Ops) != 2 || w.Ops[0] != "poke" || w.Ops[1] != "prod" {
		t.Errorf("Widget ops = %v, want [poke prod]", w.Ops)
	}
	if len(w.Required) != 2 || w.Required[0] != "op" || w.Required[1] != "target" {
		t.Errorf("Widget required = %v, want [op target]", w.Required)
	}
	if w.Hint != "poke reads, prod writes." {
		t.Errorf("Widget hint = %q, want the UsageHint", w.Hint)
	}
	r := parsed.Tools[byName["Read"]]
	if len(r.Ops) != 0 || len(r.Required) != 0 || r.Hint != "" {
		t.Errorf("Read should have no ops/required/hint, got ops=%v required=%v hint=%q", r.Ops, r.Required, r.Hint)
	}
}

// TestContextTool_GuideFiltersToAgentTools: like op=tools, the guide reflects
// THIS run's resolved tools — a tool not on the ctx allowlist is omitted.
func TestContextTool_GuideFiltersToAgentTools(t *testing.T) {
	ct := &Context{Tools: []tools.Tool{
		&fakeTool{NameVal: "Read", DescVal: "Read", SchemaVal: `{"type":"object"}`},
		&fakeTool{NameVal: "Bash", DescVal: "Run shell", SchemaVal: `{"type":"object"}`},
	}}
	ctx := tools.WithAgentTools(context.Background(), []string{"Read"}) // Bash withheld

	res, err := ct.Execute(ctx, json.RawMessage(`{"op":"guide"}`))
	if err != nil || res.IsError {
		t.Fatalf("op=guide failed: err=%v isErr=%v text=%s", err, res.IsError, res.Text)
	}
	var parsed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(res.Text), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(parsed.Tools) != 1 || parsed.Tools[0].Name != "Read" {
		t.Fatalf("guide = %+v, want only Read (Bash withheld from this run)", parsed.Tools)
	}
}
