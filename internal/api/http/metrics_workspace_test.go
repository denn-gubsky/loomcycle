package http

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// The workspace workers' gauge and counters are written with the replica's
// labels, ops by op and outcome.
func TestWriteWorkspaceMetrics_ReportsWorkersOpsAndDeaths(t *testing.T) {
	var b strings.Builder
	writeWorkspaceMetrics(&b, `{replica="r1"}`, workspace.Stats{Workers: 2, Deaths: 1,
		Ops: map[string]uint64{"open/ok": 3, "status/worker_lost": 1}})
	out := b.String()
	for _, want := range []string{
		`loomcycle_workspace_workers{replica="r1"} 2`,
		`loomcycle_workspace_worker_deaths_total{replica="r1"} 1`,
		`loomcycle_workspace_ops_total{op="open",outcome="ok",replica="r1"} 3`,
		`loomcycle_workspace_ops_total{op="status",outcome="worker_lost",replica="r1"} 1`,
		"# TYPE loomcycle_workspace_ops_total counter",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
