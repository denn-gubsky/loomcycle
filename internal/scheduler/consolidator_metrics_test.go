package scheduler

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The consolidation gauges move with tracing off, as most deployments run: the
// lag is now minus the watermark the pass left its target at, and pending is
// the targets the fan-out found with work.
func TestConsolidationMetrics_LagAndPendingWithTracingOff(t *testing.T) {
	lcotel.ResetObservablesForTest()
	t.Cleanup(lcotel.ResetObservablesForTest)
	restore, err := lcotel.SetMetricsForTest(false, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	const userID = "u-metrics"
	sched, _, st := otelFanoutFixture(t, userID)
	ctx := context.Background()
	staleAt := time.Now().UTC().Add(-2 * time.Hour)
	if _, acquired, err := st.MemoryCursorLease(ctx, "", store.MemoryScopeUser, userID, "seed", time.Now().UTC(), time.Minute); err != nil || !acquired {
		t.Fatalf("lease: acquired=%v err=%v", acquired, err)
	}
	if err := st.MemoryCursorAdvance(ctx, "", store.MemoryScopeUser, userID, "seed", staleAt, "s_long_gone"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := st.MemoryCursorRelease(ctx, "", store.MemoryScopeUser, userID, "seed"); err != nil {
		t.Fatalf("release: %v", err)
	}

	sched.fireConsolidationFanout(ctx, store.ScheduleDueRow{DefID: "sd-test", Name: "consol"}, fanoutDef(nil), time.Now())

	var b bytes.Buffer
	if err := lcotel.WritePrometheus(ctx, &b, lcotel.PromOptions{}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, `loomcycle_memory_consolidation_pending{backend="inprocess"} 1`) {
		t.Errorf("pending gauge missing or wrong:\n%s", out)
	}
	var lag float64
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, `loomcycle_memory_consolidation_lag_seconds{backend="inprocess"} `); ok {
			lag, _ = strconv.ParseFloat(v, 64)
		}
	}
	if lag < (2*time.Hour-time.Minute).Seconds() || lag > (2*time.Hour+time.Minute).Seconds() {
		t.Errorf("lag = %vs, want about two hours:\n%s", lag, out)
	}
}
