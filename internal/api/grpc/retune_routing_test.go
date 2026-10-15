package grpc

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
)

// The run's routing block reaches the connector from BOTH steer-side RPCs, a
// request carrying only it is not the empty set, and an empty message is
// passed on as a block (it turns queue-aware routing off), not as unset.
func TestGrpcRetuneAndInput_CarryTheRoutingBlock(t *testing.T) {
	mc := &interactiveMock{steerDelivered: true}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()
	ctx := context.Background()

	if _, err := client.RetuneRun(ctx, &loomcyclepb.RetuneRunRequest{
		RunId: "r_1", Routing: &loomcyclepb.RunRouting{QueueAware: true, SwitchAfterMs: 15000},
	}); err != nil {
		t.Fatalf("RetuneRun with only a routing block: %v", err)
	}
	if r := mc.gotRetuneOv.Routing; r == nil || !r.QueueAware || r.SwitchAfterMS != 15000 {
		t.Errorf("connector routing = %+v, want the retuned block", r)
	}

	if _, err := client.RunInput(ctx, &loomcyclepb.RunInputRequest{
		RunId: "r_1", Text: "go on", Routing: &loomcyclepb.RunRouting{},
	}); err != nil {
		t.Fatalf("RunInput: %v", err)
	}
	if r := mc.gotRetuneOv.Routing; r == nil || r.QueueAware {
		t.Errorf("an empty routing message reached the connector as %+v, want an empty block", r)
	}
}
