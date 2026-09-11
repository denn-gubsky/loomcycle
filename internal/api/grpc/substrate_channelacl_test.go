package grpc

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestSubstrateGRPCCtx_ChannelPolicyActuallyGrants: this plane holds every
// channel, so assert that it ALLOWS one rather than that its allowlist is
// non-empty. The weaker shape is what let the defect live — the plane declared
// Publish: []string{"*"} intending "everything", and the matcher (exact name or
// a trailing "/*" prefix, nothing else) never matched a real channel.
func TestSubstrateGRPCCtx_ChannelPolicyActuallyGrants(t *testing.T) {
	cp := tools.ChannelPolicy(substrateGRPCCtx(context.Background()))
	for _, side := range []string{"publish", "subscribe"} {
		all, list := cp.GrantsFor(side)
		if !all && !builtin.ChannelAllowed("any-channel-name", list) {
			t.Errorf("the gRPC substrate plane grants no %s on an arbitrary channel (all=%v list=%v)",
				side, all, list)
		}
	}
}
