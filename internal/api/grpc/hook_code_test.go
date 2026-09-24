package grpc

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
)

// A code body crosses gRPC both ways: registered in place of a callback URL,
// and listed back.
func TestGrpc_ACodeHookBodyCrossesBothWays(t *testing.T) {
	const body = `function hook(ev) { return {}; }`
	hc := &hookConnector{
		registerResp: connector.RegisterHookResponse{ID: "hook_c"},
		listResp:     connector.ListHooksResponse{Hooks: []*hooks.Hook{{ID: "hook_c", Owner: "ops", Name: "g", Phase: hooks.PhasePre, Code: body}}},
	}
	adapter := New(Config{Connector: hc, CancelReg: cancel.NewRegistry()})
	if _, err := adapter.RegisterHook(context.Background(), &loomcyclepb.RegisterHookRequest{Owner: "ops", Name: "g", Phase: "pre", Code: body}); err != nil {
		t.Fatal(err)
	}
	if got := hc.gotRegister.Load().(connector.RegisterHookRequest); got.Code != body || got.CallbackURL != "" {
		t.Errorf("connector saw %+v", got)
	}
	list, err := adapter.ListHooks(context.Background(), &loomcyclepb.ListHooksRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetHooks()) != 1 || list.GetHooks()[0].GetCode() != body {
		t.Errorf("listed %+v", list.GetHooks())
	}
}
