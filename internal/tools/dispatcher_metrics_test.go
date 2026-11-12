package tools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Every dispatched call is counted by tool and outcome, tracing on or off. An
// MCP tool is counted by its server, a client tool as "client", and a name the
// run was not given never becomes a label: the model chose that text. A tool
// that names its own label (an MCP tool of the operator's server) keeps it.
func TestDispatcherCall_CountsEachCallByCollapsedToolAndOutcome(t *testing.T) {
	restore, err := lcotel.SetMetricsForTest(false, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	d := tools.NewDispatcherWithFallback([]tools.Tool{
		&fakeTool{name: "Read", res: tools.Result{Text: "ok"}},
		&fakeTool{name: "Refuses", res: tools.Result{Text: "no", IsError: true}},
		&fakeTool{name: "Breaks", err: errors.New("boom")},
		&fakeTool{name: "mcp__github__create_issue", res: tools.Result{Text: "ok"}},
		&labeledTool{fakeTool: fakeTool{name: "mcp__ops__page", res: tools.Result{Text: "ok"}}, label: "mcp:ops"},
		&fakeTool{name: "client__open_file", res: tools.Result{Text: "ok"}},
	}, func(context.Context, string, json.RawMessage) (tools.Result, bool) { return tools.Result{}, false })
	ctx := context.Background()
	for _, name := range []string{"Read", "Read", "Refuses", "Breaks", "mcp__github__create_issue", "mcp__ops__page", "client__open_file",
		"made_up_tool_with_user_text", "mcp__invented__x"} {
		_, _ = d.Call(ctx, name, json.RawMessage(`{}`))
	}
	var b bytes.Buffer
	if err := lcotel.WritePrometheus(ctx, &b, lcotel.PromOptions{}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		`loomcycle_tool_calls_total{outcome="ok",tool="Read"} 2`,
		`loomcycle_tool_calls_total{outcome="is_error",tool="Refuses"} 1`,
		`loomcycle_tool_calls_total{outcome="error",tool="Breaks"} 1`,
		`loomcycle_tool_calls_total{outcome="ok",tool="mcp"} 1`,
		`loomcycle_tool_calls_total{outcome="ok",tool="mcp:ops"} 1`,
		`loomcycle_tool_calls_total{outcome="ok",tool="client"} 1`,
		`loomcycle_tool_calls_total{outcome="is_error",tool="unknown"} 1`,
		`loomcycle_tool_calls_total{outcome="is_error",tool="mcp"} 1`,
		`loomcycle_tool_call_duration_seconds_count{tool="Read"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{"made_up", "invented"} {
		if strings.Contains(out, leak) {
			t.Errorf("a name the model chose became a label (%q):\n%s", leak, out)
		}
	}
}

type labeledTool struct {
	fakeTool
	label string
}

func (t *labeledTool) MetricLabel() string { return t.label }
