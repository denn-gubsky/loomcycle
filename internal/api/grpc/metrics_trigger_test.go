package grpc

import (
	"bytes"
	"context"
	"strings"
	"testing"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

// A run started over gRPC is counted under the grpc trigger, in open mode as
// with a bearer.
func TestRunRPC_CountsTheRunUnderTheGRPCTrigger(t *testing.T) {
	for _, token := range []string{"", "tok"} {
		restore, err := lcotel.SetMetricsForTest(false, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runOverGRPC(t, token, token); err != nil {
			restore()
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := lcotel.WritePrometheus(context.Background(), &b, lcotel.PromOptions{}); err != nil {
			t.Fatal(err)
		}
		restore()
		if !strings.Contains(b.String(), `trigger="grpc"} 1`) {
			t.Errorf("auth token %q: no run counted under the grpc trigger:\n%s", token, b.String())
		}
	}
}
