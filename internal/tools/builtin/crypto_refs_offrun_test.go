package builtin

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Off a run a volume reference is lent only when the transport authorized the
// volume itself (tools.WithOffRunVolumes); a memory or document reference is
// never lent, marker or not.
func TestCheckRefGrant_OffRunLendsOnlyATransportAuthorizedVolume(t *testing.T) {
	base := tools.WithCallerTier(context.Background(), tools.TierOperatorPlane)
	base = tools.UpdateRunPolicy(base, func(rp *tools.RunPolicy) { rp.Tools = []string{"Write", "Memory", "Document"} })
	vol := dataRef{raw: "volume:reports:a.pdf", kind: refVolume, volume: "reports", path: "a.pdf"}
	mem := dataRef{raw: "memory:tenant:k", kind: refMemory, scope: "tenant", key: "k"}

	if _, ok := checkRefGrant(base, vol, true, FileToolName, "x"); ok {
		t.Error("off-run volume reference without the marker was allowed")
	}
	marked := tools.WithOffRunVolumes(base)
	if res, ok := checkRefGrant(marked, vol, true, FileToolName, "x"); !ok {
		t.Errorf("off-run volume reference with the marker refused: %s", res.Text)
	}
	if _, ok := checkRefGrant(marked, mem, true, FileToolName, "x"); ok {
		t.Error("off-run memory reference was allowed by the volume marker")
	}
	// The marker lends the volume, not the tool: the policy must still hold it.
	bare := tools.UpdateRunPolicy(marked, func(rp *tools.RunPolicy) { rp.Tools = nil })
	if _, ok := checkRefGrant(bare, vol, true, FileToolName, "x"); ok {
		t.Error("off-run volume reference allowed without Write in the policy")
	}
}
