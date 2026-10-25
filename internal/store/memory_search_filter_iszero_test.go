package store

import (
	"reflect"
	"testing"
	"time"
)

// TestMemorySearchFilter_IDsIsZero — IsZero licenses a backend to drop every
// predicate it does not see, so a filter carrying only an id list must not report
// "constrains nothing". Derived from the STRUCT, so a field added later without an
// IsZero clause reds here too.
func TestMemorySearchFilter_IDsIsZero(t *testing.T) {
	if !(MemorySearchFilter{}).IsZero() {
		t.Fatal("the zero filter must report IsZero")
	}
	rt := reflect.TypeOf(MemorySearchFilter{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		var mf MemorySearchFilter
		v := reflect.ValueOf(&mf).Elem().Field(i)
		switch {
		case f.Type.Kind() == reflect.String:
			v.SetString("x")
		case f.Type.Kind() == reflect.Bool:
			v.SetBool(true)
		case f.Type.Kind() == reflect.Uint8:
			v.SetUint(1)
		case f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.String:
			v.Set(reflect.ValueOf([]string{"x"}))
		case f.Type == reflect.TypeOf(time.Time{}):
			v.Set(reflect.ValueOf(time.Unix(1, 0)))
		default:
			t.Fatalf("field %s has type %s — extend this test for it", f.Name, f.Type)
		}
		if mf.IsZero() {
			t.Errorf("MemorySearchFilter{%s: set} reports IsZero — a backend trusting it would drop the predicate", f.Name)
		}
	}
}
