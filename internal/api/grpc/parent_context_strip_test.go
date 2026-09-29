package grpc

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestParentContextFromProto_DropsRuntimeOwnedFields: the gRPC ParentContext
// message is shared with the run-state echo, so it CAN carry walk_id, wave_id,
// wave_index and the board fields inbound. The mapper takes only the caller's
// fields; widening it to "map every field", as the echo mapper does, would let a
// gRPC caller place its run inside a team walk. RunOnce strips them as well —
// this pins the ingress so the two stay in agreement.
func TestParentContextFromProto_DropsRuntimeOwnedFields(t *testing.T) {
	got := parentContextFromProto(&loomcyclepb.ParentContext{
		RootAgentRunId: "r_root", FunctionKey: "fk", TierAtRun: "pro",
		WalkId: "r_victim_walk", WaveId: "wav_victim", WaveIndex: 3,
		BoardScope: "user", BoardChunkId: "c_victim", BoardDocumentId: "d_victim",
	})
	want := store.ParentContext{RootAgentRunID: "r_root", FunctionKey: "fk", TierAtRun: "pro"}
	if got == nil || *got != want {
		t.Errorf("parent_context = %+v, want only the caller's fields %+v", got, want)
	}
	if got := parentContextFromProto(&loomcyclepb.ParentContext{WalkId: "r_victim_walk", WaveIndex: 3}); got != nil {
		t.Errorf("a runtime-only parent_context = %+v, want nil", got)
	}
}
