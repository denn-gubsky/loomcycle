// Package tools defines the Tool interface and the dispatcher that routes
// tool_use calls from the model to a built-in or MCP-backed implementation.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Tool is one tool the agent can invoke. The Name is what the model sees and
// what allowlists are matched against. The InputSchema is JSON Schema; the
// dispatcher passes the raw model-generated input straight through.
type Tool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	Execute(ctx context.Context, input json.RawMessage) (Result, error)
}

// Result is the output of one tool invocation. Text is the human-readable
// payload the model will see in the next tool_result block. IsError flags a
// failed execution (the model should self-correct, not surface to the user).
type Result struct {
	Text    string
	IsError bool
}

// Spec converts a Tool to the providers.ToolSpec the model receives.
func Spec(t Tool) providers.ToolSpec {
	return providers.ToolSpec{
		Name:        t.Name(),
		Description: t.Description(),
		InputSchema: t.InputSchema(),
	}
}

// Dispatcher resolves tool names to Tool implementations and invokes them.
// A new Dispatcher is built per run with the run's allowed-tools list so
// off-policy calls fail fast.
type Dispatcher struct {
	tools map[string]Tool
}

// NewDispatcher builds a dispatcher from the given tools.
func NewDispatcher(tools []Tool) *Dispatcher {
	m := make(map[string]Tool, len(tools))
	for _, t := range tools {
		m[t.Name()] = t
	}
	return &Dispatcher{tools: m}
}

// Specs returns the providers.ToolSpec slice for all registered tools, in the
// order they were passed to NewDispatcher (map iteration would be non-deterministic).
func (d *Dispatcher) Specs(order []Tool) []providers.ToolSpec {
	out := make([]providers.ToolSpec, 0, len(order))
	for _, t := range order {
		if _, ok := d.tools[t.Name()]; ok {
			out = append(out, Spec(t))
		}
	}
	return out
}

// Execute looks up the named tool and runs it. Unknown tool names return an
// error result (the model can self-correct) rather than a hard error.
func (d *Dispatcher) Execute(ctx context.Context, name string, input json.RawMessage) Result {
	t, ok := d.tools[name]
	if !ok {
		return Result{Text: fmt.Sprintf("tool not found: %s", name), IsError: true}
	}
	res, err := t.Execute(ctx, input)
	if err != nil {
		return Result{Text: err.Error(), IsError: true}
	}
	return res
}
