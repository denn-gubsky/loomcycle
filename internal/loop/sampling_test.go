package loop

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestRun_SamplingReachesRequest asserts RunOptions.Sampling is mapped onto the
// flat providers.Request the loop hands the driver — the seam every provider
// reads. scriptedProvider records each Request it receives.
func TestRun_SamplingReachesRequest(t *testing.T) {
	temp, topP := 0.42, 0.8
	seed := 99
	prov := &scriptedProvider{toolCalls: nil}
	disp := tools.NewDispatcher(nil)

	_, _ = Run(t.Context(), RunOptions{
		Provider:        prov,
		Model:           "x",
		Dispatcher:      disp,
		Segments:        userSeg("go"),
		MaxIterations:   1, // one provider call is enough to capture the request
		ToolParallelism: 8,
		Sampling: &config.Sampling{
			Temperature: &temp,
			TopP:        &topP,
			Seed:        &seed,
		},
	})

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.requests) == 0 {
		t.Fatal("provider received no request")
	}
	req := prov.requests[0]
	if req.Temperature == nil || *req.Temperature != 0.42 {
		t.Errorf("req.Temperature = %v, want 0.42", req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0.8 {
		t.Errorf("req.TopP = %v, want 0.8", req.TopP)
	}
	if req.Seed == nil || *req.Seed != 99 {
		t.Errorf("req.Seed = %v, want 99", req.Seed)
	}
}
