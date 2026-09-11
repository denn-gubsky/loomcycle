package store

import (
	"reflect"
	"testing"
)

// TestParentContext_IsZeroCoversEveryField is the guard on a silent-drop bug.
//
// EncodeParentContext writes NULL when IsZero says so, so a field IsZero does
// not mention makes a context carrying only that field vanish on write — every
// run, no error, nothing to point at. The wave correlation was added to the
// struct and not to IsZero, and every run a Starter spawned stored a NULL
// parent_context as a result.
//
// This derives the field list from the STRUCT, so adding one and forgetting
// IsZero reds here instead of silently dropping data.
func TestParentContext_IsZeroCoversEveryField(t *testing.T) {
	rt := reflect.TypeOf(ParentContext{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		p := &ParentContext{}
		v := reflect.ValueOf(p).Elem().Field(i)
		// Set this field alone to a non-zero value.
		switch f.Type.Kind() {
		case reflect.String:
			v.SetString("x")
		case reflect.Int, reflect.Int64:
			v.SetInt(7)
		case reflect.Bool:
			v.SetBool(true)
		default:
			t.Logf("field %s has kind %s — extend this test for it", f.Name, f.Type.Kind())
			continue
		}
		if p.IsZero() {
			t.Errorf("ParentContext{%s: set} reports IsZero — EncodeParentContext would write NULL "+
				"and the value would be dropped silently on every run. Add %s to IsZero.", f.Name, f.Name)
		}
		// And it must actually survive a round trip.
		enc, ok, err := EncodeParentContext(p)
		if err != nil {
			t.Fatalf("%s: encode: %v", f.Name, err)
		}
		if !ok || enc == "" {
			t.Errorf("%s: encode reported nothing to store", f.Name)
			continue
		}
		back, err := DecodeParentContext(enc)
		if err != nil || back == nil {
			t.Fatalf("%s: decode: %v", f.Name, err)
		}
		if !reflect.DeepEqual(*back, *p) {
			t.Errorf("%s: round trip changed the value: %+v -> %+v", f.Name, *p, *back)
		}
	}

	// The genuinely empty case still encodes to nothing, so an ordinary run
	// keeps its NULL column rather than storing an empty object.
	if _, ok, _ := EncodeParentContext(&ParentContext{}); ok {
		t.Error("an empty ParentContext encoded something — ordinary runs must keep a NULL column")
	}
	if !(*ParentContext)(nil).IsZero() {
		t.Error("a nil ParentContext must be zero")
	}
}

// TestParentContext_WaveIndexZeroSurvives: index 0 is a real position in a
// wave. With omitempty it vanished, making the FIRST run of every wave
// indistinguishable from a run that carries no index — which is precisely the
// run a canvas draws first.
func TestParentContext_WaveIndexZeroSurvives(t *testing.T) {
	p := &ParentContext{WalkID: "r_a", WaveID: "wav_1", WaveIndex: 0}
	enc, ok, err := EncodeParentContext(p)
	if err != nil || !ok {
		t.Fatalf("encode: ok=%v err=%v", ok, err)
	}
	if !contains(enc, `"wave_index":0`) {
		t.Errorf("encoded %s — index 0 was dropped", enc)
	}
	back, err := DecodeParentContext(enc)
	if err != nil || back == nil || back.WaveIndex != 0 || back.WalkID != "r_a" {
		t.Errorf("round trip lost the position: %+v (err=%v)", back, err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
