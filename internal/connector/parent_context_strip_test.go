package connector

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Who owns each ParentContext field. This is the classification, written down
// once: the strip is checked against it, not the other way round.
var (
	parentContextCallerFields = map[string]bool{
		"RootAgentRunID": true, "FunctionKey": true, "TierAtRun": true,
	}
	parentContextRuntimeFields = map[string]bool{
		"BoardScope": true, "BoardChunkID": true, "BoardDocumentID": true,
		"WalkID": true, "WaveID": true, "WaveIndex": true,
	}
)

// TestStripRuntimeParentContext_ClassifiesEveryField derives the field list from
// the STRUCT, so a field added to ParentContext without deciding who owns it reds
// here — a runtime field nobody classified would otherwise be accepted from every
// caller, which is exactly how walk_id/wave_id/wave_index were.
//
// It then sets every field and strips: each caller field must survive and each
// runtime field must be cleared, so classifying a field without teaching the
// strip about it reds too.
func TestStripRuntimeParentContext_ClassifiesEveryField(t *testing.T) {
	rt := reflect.TypeOf(store.ParentContext{})
	full := &store.ParentContext{}
	fv := reflect.ValueOf(full).Elem()
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		caller, runtime := parentContextCallerFields[name], parentContextRuntimeFields[name]
		switch {
		case !caller && !runtime:
			t.Errorf("ParentContext.%s is not classified: add it to the caller or the runtime list "+
				"here, and if it is runtime-owned clear it in StripRuntimeParentContext", name)
		case caller && runtime:
			t.Errorf("ParentContext.%s is in both lists", name)
		}
		switch f := fv.Field(i); f.Kind() {
		case reflect.String:
			f.SetString("set-" + name)
		case reflect.Int, reflect.Int64:
			f.SetInt(7)
		default:
			t.Fatalf("ParentContext.%s has kind %s — extend this test for it", name, f.Kind())
		}
	}

	got := StripRuntimeParentContext(full)
	if got == nil {
		t.Fatal("strip of a context carrying caller fields returned nil")
	}
	gv := reflect.ValueOf(got).Elem()
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		isZero := gv.Field(i).IsZero()
		if parentContextRuntimeFields[name] && !isZero {
			t.Errorf("runtime-owned %s survived the strip: %v", name, gv.Field(i).Interface())
		}
		if parentContextCallerFields[name] && !reflect.DeepEqual(gv.Field(i).Interface(), fv.Field(i).Interface()) {
			t.Errorf("caller field %s changed: %v -> %v", name, fv.Field(i).Interface(), gv.Field(i).Interface())
		}
	}
	// A copy, never the caller's struct: RunOnce strips a pointer the caller
	// still holds (an echo, a stored draft).
	if full.WalkID == "" || got == full {
		t.Error("strip mutated or returned its input instead of a copy")
	}
}

// TestStripRuntimeParentContext_RuntimeOnlyContextBecomesNil: a context carrying
// only runtime fields has nothing left once they go, and must read as absent —
// the same normalisation every ingress applied to an all-empty struct.
func TestStripRuntimeParentContext_RuntimeOnlyContextBecomesNil(t *testing.T) {
	for _, pc := range []*store.ParentContext{
		nil,
		{},
		{WalkID: "r_walk", WaveID: "wav_1", WaveIndex: 2},
		{BoardScope: "user", BoardChunkID: "c1", BoardDocumentID: "d1"},
	} {
		if got := StripRuntimeParentContext(pc); got != nil {
			t.Errorf("StripRuntimeParentContext(%+v) = %+v, want nil", pc, got)
		}
	}
}
