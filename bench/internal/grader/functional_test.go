package grader

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/bench/internal/cases"
	"github.com/denn-gubsky/loomcycle/bench/internal/runner"
)

func toolCallEvent(name string, args map[string]any) runner.ProviderEvent {
	raw, _ := json.Marshal(args)
	return runner.ProviderEvent{
		Type: "tool_call",
		ToolUse: &runner.ToolUseEvent{
			ID:    "t_" + name,
			Name:  name,
			Input: raw,
		},
	}
}

// TestFunctional_NoExpectedNoActualPasses verifies the trivial case:
// case expects no tool calls and the model made none.
func TestFunctional_NoExpectedNoActualPasses(t *testing.T) {
	r := Functional(nil, cases.Functional{})
	if !r.Pass {
		t.Fatalf("expected trivial pass; reasons: %v", r.Reasons)
	}
}

// TestFunctional_RequiredCallPresent.
func TestFunctional_RequiredCallPresent(t *testing.T) {
	events := []runner.ProviderEvent{
		toolCallEvent("mcp__jobs__getAgentContext", map[string]any{"user_id": "bench-user-fixture-001"}),
	}
	exp := cases.Functional{
		ToolCalls: []cases.ToolCall{
			{
				Name: "mcp__jobs__getAgentContext",
				ArgsMustInclude: map[string]any{
					"user_id": "bench-user-fixture-001",
				},
			},
		},
	}
	r := Functional(events, exp)
	if !r.Pass {
		t.Fatalf("expected pass; reasons: %v", r.Reasons)
	}
}

// TestFunctional_WrongArgValueFails.
func TestFunctional_WrongArgValueFails(t *testing.T) {
	events := []runner.ProviderEvent{
		toolCallEvent("mcp__jobs__getAgentContext", map[string]any{"user_id": "wrong-user-id"}),
	}
	exp := cases.Functional{
		ToolCalls: []cases.ToolCall{
			{
				Name: "mcp__jobs__getAgentContext",
				ArgsMustInclude: map[string]any{
					"user_id": "bench-user-fixture-001",
				},
			},
		},
	}
	r := Functional(events, exp)
	if r.Pass {
		t.Fatal("expected fail on arg mismatch")
	}
}

// TestFunctional_ForbidRepeatCallsCatchesDoomLoop.
func TestFunctional_ForbidRepeatCallsCatchesDoomLoop(t *testing.T) {
	args := map[string]any{"userId": "bench-user-fixture-001"} // wrong field name
	events := []runner.ProviderEvent{
		toolCallEvent("mcp__jobs__getAgentContext", args),
		toolCallEvent("mcp__jobs__getAgentContext", args), // identical retry = doom-loop
	}
	exp := cases.Functional{
		ToolCalls: []cases.ToolCall{
			{Name: "mcp__jobs__getAgentContext"},
		},
		ForbidRepeatCalls: true,
	}
	r := Functional(events, exp)
	if r.Pass {
		t.Fatal("expected fail on doom-loop")
	}
}

// TestFunctional_OrderStrictRequiresSequence.
func TestFunctional_OrderStrictRequiresSequence(t *testing.T) {
	// PATCH-then-GET is wrong order for the read-reason-write case.
	events := []runner.ProviderEvent{
		toolCallEvent("mcp__jobs__patchApplication", map[string]any{"app_id": "x"}),
		toolCallEvent("mcp__jobs__getApplication", map[string]any{"app_id": "x"}),
	}
	exp := cases.Functional{
		ToolCalls: []cases.ToolCall{
			{Name: "mcp__jobs__getApplication"},
			{Name: "mcp__jobs__patchApplication"},
		},
		OrderStrict: true,
	}
	r := Functional(events, exp)
	if r.Pass {
		t.Fatal("expected fail on wrong order")
	}
}

// TestFunctional_NestedJSONArgConstraint.
func TestFunctional_NestedJSONArgConstraint(t *testing.T) {
	events := []runner.ProviderEvent{
		toolCallEvent("mcp__jobs__getAgentContext", map[string]any{
			"user_id": "bench-user-fixture-001",
			"filter":  map[string]any{"include": []any{"a"}, "exclude": []any{}, "limit": 50},
		}),
	}
	exp := cases.Functional{
		ToolCalls: []cases.ToolCall{
			{
				Name: "mcp__jobs__getAgentContext",
				ArgsMustInclude: map[string]any{
					"filter_is_object": true,
				},
			},
		},
	}
	r := Functional(events, exp)
	if !r.Pass {
		t.Fatalf("expected pass on nested filter; reasons: %v", r.Reasons)
	}
}

// TestFunctional_MinCallsMaxCallsBatching — codifies the batched
// ingest expectation: 2-5 calls allowed, fewer or more is a fail.
func TestFunctional_MinCallsMaxCallsBatching(t *testing.T) {
	events := []runner.ProviderEvent{
		toolCallEvent("mcp__jobs__postSearchIngest", map[string]any{"batch": 1}),
		toolCallEvent("mcp__jobs__postSearchIngest", map[string]any{"batch": 2}),
		toolCallEvent("mcp__jobs__postSearchIngest", map[string]any{"batch": 3}),
	}
	exp := cases.Functional{
		ToolCalls: []cases.ToolCall{
			{Name: "mcp__jobs__postSearchIngest", MinCalls: 2, MaxCalls: 5},
		},
	}
	r := Functional(events, exp)
	if !r.Pass {
		t.Fatalf("expected pass; reasons: %v", r.Reasons)
	}

	// One giant batch = fail (min=2 not met).
	singleEvent := []runner.ProviderEvent{
		toolCallEvent("mcp__jobs__postSearchIngest", map[string]any{"all": 25}),
	}
	r2 := Functional(singleEvent, exp)
	if r2.Pass {
		t.Fatal("expected fail on one giant batch (min=2 not met)")
	}
}
