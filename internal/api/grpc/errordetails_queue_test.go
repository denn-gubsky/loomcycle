package grpc

import (
	"fmt"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// TestRunRefusal_FullQueueCarriesItsBoundInErrorInfo pins RFC GK D6 on gRPC: a
// run refused for a full admission queue names the bound and how many runs
// held it in the ErrorInfo metadata, as the HTTP 429 body does.
func TestRunRefusal_FullQueueCarriesItsBoundInErrorInfo(t *testing.T) {
	err := fmt.Errorf("%w: %w", runner.ErrBackpressure, &concurrency.BackpressureError{QueueCapacity: 4, QueuedNow: 4})
	st, ok := status.FromError(mapRunnerErr(err))
	if !ok {
		t.Fatalf("not a status error")
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			md := info.GetMetadata()
			if md["queue_capacity"] != "4" || md["queued_now"] != "4" {
				t.Errorf("metadata = %v, want queue_capacity 4, queued_now 4", md)
			}
			return
		}
	}
	t.Fatal("no ErrorInfo detail")
}
