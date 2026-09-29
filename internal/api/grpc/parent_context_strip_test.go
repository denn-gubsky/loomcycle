package grpc

import (
	"reflect"
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
		State: "s_victim", StateVisit: 5,
	})
	want := store.ParentContext{RootAgentRunID: "r_root", FunctionKey: "fk", TierAtRun: "pro"}
	if got == nil || *got != want {
		t.Errorf("parent_context = %+v, want only the caller's fields %+v", got, want)
	}
	if got := parentContextFromProto(&loomcyclepb.ParentContext{WalkId: "r_victim_walk", WaveIndex: 3}); got != nil {
		t.Errorf("a runtime-only parent_context = %+v, want nil", got)
	}
}

// TestParentContextToProto_CarriesEveryField: the echo mapper must carry every
// field of the store struct onto the wire message. Derived from BOTH types, so a
// field added to store.ParentContext and not to the proto message, or to the
// message and not to the mapper, reds here instead of vanishing from the gRPC
// run-state stream while HTTP still shows it.
func TestParentContextToProto_CarriesEveryField(t *testing.T) {
	full := &store.ParentContext{}
	fv := reflect.ValueOf(full).Elem()
	for i := 0; i < fv.NumField(); i++ {
		switch f := fv.Field(i); f.Kind() {
		case reflect.String:
			f.SetString("set-" + fv.Type().Field(i).Name)
		case reflect.Int:
			f.SetInt(int64(i + 1))
		default:
			t.Fatalf("ParentContext.%s has kind %s — extend this test for it", fv.Type().Field(i).Name, f.Kind())
		}
	}
	msg := parentContextToProto(full).ProtoReflect()
	fields := msg.Descriptor().Fields()
	if fields.Len() != fv.NumField() {
		t.Errorf("proto ParentContext has %d fields, store.ParentContext has %d", fields.Len(), fv.NumField())
	}
	for i := 0; i < fields.Len(); i++ {
		if fd := fields.Get(i); !msg.Has(fd) {
			t.Errorf("parentContextToProto left %s unset", fd.Name())
		}
	}
}
