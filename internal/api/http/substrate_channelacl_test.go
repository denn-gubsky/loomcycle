package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestSubstrateAdminCtx_ChannelPolicyActuallyGrants: see the gRPC twin. This
// plane holds every channel, so the assertion has to be that it ALLOWS one —
// a non-empty allowlist proves nothing when the entry is a name that matches
// no channel.
func TestSubstrateAdminCtx_ChannelPolicyActuallyGrants(t *testing.T) {
	cp := tools.ChannelPolicy(substrateAdminCtx(context.Background()))
	for _, side := range []string{"publish", "subscribe"} {
		all, list := cp.GrantsFor(side)
		if !all && !builtin.ChannelAllowed("any-channel-name", list) {
			t.Errorf("the HTTP substrate-admin plane grants no %s on an arbitrary channel (all=%v list=%v)",
				side, all, list)
		}
	}
}
