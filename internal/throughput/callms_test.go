package throughput

import (
	"fmt"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// The median call time is a call's duration less the time it queued: queue-
// aware routing multiplies it by the calls ahead, and a call's own wait would
// count them twice. An alias finds the model it names; an unknown model and a
// nil estimator report 0.
func TestEstimator_MedianCallMsIsServiceTimeNotQueueTime(t *testing.T) {
	e := newEst(t, config.TimeoutScaling{})
	for i, d := range []int64{8000, 9000, 10000, 11000, 12000} {
		e.Observe(Sample{RunID: fmt.Sprintf("r%d", i), At: t0.Add(time.Duration(i) * time.Second),
			Key: Key{"ollama-local", "qwen3.6"}, Output: 100,
			CallTiming: providers.CallTiming{DurationMs: d + 3000, QueueMs: 3000}})
	}
	if got := e.MedianCallMs("ollama-local", "qwen3.6"); got != 10000 {
		t.Fatalf("median call = %d ms, want 10000 (durations less their 3 s queue)", got)
	}
	if got := e.MedianCallMs("ollama-local", "local-medium"); got != 10000 {
		t.Errorf("by alias = %d ms, want the named model's 10000", got)
	}
	if got := e.Stat("ollama-local", "qwen3.6").CallMsP50; got != 10000 {
		t.Errorf("Stat.CallMsP50 = %d, want 10000", got)
	}
	if got := e.MedianCallMs("ollama-local", "never-seen"); got != 0 {
		t.Errorf("an unseen model = %d ms, want 0", got)
	}
	var off *Estimator
	if got := off.MedianCallMs("p", "m"); got != 0 {
		t.Errorf("a nil estimator = %d ms, want 0", got)
	}
}
