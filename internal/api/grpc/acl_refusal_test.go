package grpc

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeRefusal stands in for the HTTP package's access-control refusal, which
// a shared operation returns when the question it asks itself is refused.
type fakeRefusal struct{ unavailable bool }

func (f fakeRefusal) Error() string    { return "acl_denied: manage runtime:retention" }
func (f fakeRefusal) ACLRefusal() bool { return f.unavailable }

// A shared operation's own access-control refusal maps to the code the gRPC
// gate gives one — PermissionDenied for a deny, Unavailable for unreadable
// rules — on the RPCs whose operation asks it (retention, erasure), never to
// Internal.
func TestSharedOperationACLRefusal_MapsToTheGatesCode(t *testing.T) {
	for _, c := range []struct {
		err  error
		want codes.Code
	}{
		{fakeRefusal{}, codes.PermissionDenied},
		{fakeRefusal{unavailable: true}, codes.Unavailable},
		{fmt.Errorf("wrapped: %w", fakeRefusal{}), codes.PermissionDenied},
	} {
		for name, mapErr := range map[string]func(error) error{
			"RetentionOverride": retentionOverrideErrToStatus,
			"ErasureExecute":    erasureErrToStatus,
		} {
			if got := status.Code(mapErr(c.err)); got != c.want {
				t.Errorf("%s(%v) = %v, want %v", name, c.err, got, c.want)
			}
		}
	}
	if got := status.Code(retentionOverrideErrToStatus(errors.New("boom"))); got != codes.Internal {
		t.Errorf("an unrelated error = %v, want Internal", got)
	}
}
