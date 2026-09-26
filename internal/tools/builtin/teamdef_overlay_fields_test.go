package builtin

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// applyTeamOverlay names each Definition field it merges, so a new field with
// no case is silently dropped by every fork (Layout, then Hooks). This walks the
// struct itself: each field set alone in an overlay must reach the result.
func TestApplyTeamOverlay_CarriesEveryDefinitionField(t *testing.T) {
	typ := reflect.TypeOf(teamgraph.Definition{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		var ov teamgraph.Definition
		v := reflect.ValueOf(&ov).Elem().Field(i)
		switch v.Kind() {
		case reflect.String:
			v.SetString("x")
		case reflect.Int:
			v.SetInt(1)
		case reflect.Slice:
			v.Set(reflect.MakeSlice(f.Type, 1, 1))
		case reflect.Map:
			v.Set(reflect.MakeMap(f.Type))
		case reflect.Pointer:
			v.Set(reflect.New(f.Type.Elem()))
		default:
			t.Fatalf("field %s: kind %s has no test value; extend this test", f.Name, v.Kind())
		}
		var base teamgraph.Definition
		applyTeamOverlay(&base, ov)
		if !reflect.DeepEqual(reflect.ValueOf(base).Field(i).Interface(), v.Interface()) {
			t.Errorf("a fork drops Definition.%s: applyTeamOverlay has no case for it", f.Name)
		}
	}
}
