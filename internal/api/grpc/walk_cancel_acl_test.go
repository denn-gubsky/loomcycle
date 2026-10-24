package grpc

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// A walk's cancel refused by the caller's ACL (cancel team:<name>) answers as
// the HTTP route does: PermissionDenied for acl_denied, Unavailable for
// acl_unavailable, the message starting with the code.
func TestCancelTurn_AnACLRefusalOfAWalkKeepsItsCode(t *testing.T) {
	for _, c := range []struct {
		code string
		want codes.Code
	}{{"acl_denied", codes.PermissionDenied}, {"acl_unavailable", codes.Unavailable}} {
		mc := &parityMock{cancelErr: &connector.ACLRefusal{Code: c.code, Msg: "no rule allows cancel team:rev"}}
		client, cleanup := startTestServerWithConnector(t, mc)
		_, err := client.CancelTurn(context.Background(), &loomcyclepb.CancelTurnRequest{RunId: "run-1"})
		cleanup()
		if status.Code(err) != c.want || !strings.HasPrefix(status.Convert(err).Message(), c.code) {
			t.Errorf("%s: err = %v, want %s with a message starting %q", c.code, err, c.want, c.code)
		}
	}
}
