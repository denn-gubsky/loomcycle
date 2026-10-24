package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

type refusingCancel struct {
	*mockConnector
	err error
}

func (r refusingCancel) CancelRun(context.Context, string, string) (connector.CancelRunResult, error) {
	return connector.CancelRunResult{}, r.err
}

// cancel_run of a walk the caller may not cancel (cancel team:<name>) is a
// tool error whose text is the refusal body, as a definition op's is.
func TestCancelRun_AnACLRefusalOfAWalkIsTheRefusalBody(t *testing.T) {
	body := `{"code":"acl_denied","error":"no rule allows cancel team:rev","action":"cancel","resource":"team:rev"}`
	env := &handlerEnv{connector: refusingCancel{&mockConnector{}, &connector.ACLRefusal{Code: "acl_denied", Body: body, Msg: "no rule allows cancel team:rev"}}}
	res, err := handleCancelRun(context.Background(), env, json.RawMessage(`{"agent_id":"team:rev"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || len(res.Content) != 1 || res.Content[0].Text != body {
		t.Errorf("cancel_run refusal = %+v, want a tool error carrying %s", res, body)
	}
}
