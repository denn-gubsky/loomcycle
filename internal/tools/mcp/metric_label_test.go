package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// A runtime-defined MCP server is named by whoever authored its definition, a
// tenant among them: its tools share one "mcp" series, so no tenant can spend
// the operator-wide cardinality limit with invented server names, and two
// tenants' servers of one name never share a series of their own. Only the
// operator's configured servers are labelled by name.
func TestDispatcherCall_LabelsOnlyOperatorStaticMCPServersByName(t *testing.T) {
	restore, err := lcotel.SetMetricsForTest(false, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) { return nil, errors.New("down") }, nil, nil)
	t.Cleanup(pool.Close)
	d := tools.NewDispatcher([]tools.Tool{
		mcp.NewStaticTool(pool, "ops", mcp.ToolDescriptor{Name: "page"}),
		mcp.NewTool(pool, "tenant_crm", mcp.ToolDescriptor{Name: "lookup"}),
	})
	ctx := context.Background()
	for _, name := range []string{"mcp__ops__page", "mcp__tenant_crm__lookup"} {
		_, _ = d.Call(ctx, name, json.RawMessage(`{}`))
	}
	var b bytes.Buffer
	if err := lcotel.WritePrometheus(ctx, &b, lcotel.PromOptions{}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		`loomcycle_tool_call_duration_seconds_count{tool="mcp:ops"} 1`,
		`loomcycle_tool_call_duration_seconds_count{tool="mcp"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tenant_crm") {
		t.Errorf("a runtime-defined server's name became a label:\n%s", out)
	}
}

// A static server recovered at run start is still the operator's own.
func TestStaticToolsForRun_ToolsKeepTheServerLabel(t *testing.T) {
	pool := newPoolWithFake(t, "ok")
	out := mcp.StaticToolsForRun(context.Background(), pool,
		staticServers("search"), map[string]bool{"search": true},
		"", map[string]bool{"search": true}, 5*time.Second, nil)
	if len(out) == 0 {
		t.Fatal("no tool recovered")
	}
	l, ok := out[0].(tools.MetricLabeler)
	if !ok || l.MetricLabel() != "mcp:search" {
		t.Fatalf("recovered static tool label = %v (labeler %v), want mcp:search", l, ok)
	}
}
