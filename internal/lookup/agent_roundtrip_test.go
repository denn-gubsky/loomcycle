package lookup

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// The Library hands the UI one shape for static and runtime-authored agents
// alike, converted by SubstrateAgentDefFromConfig / ToConfigDef. These two
// assertions are what keep that pair honest, and they catch different faults:
//
//   - COVERAGE: populate every config.AgentDef field, convert, and require no
//     field of the result to be left at its zero value. This is the one that
//     catches a forgotten assignment — the fault that let the old hand-written
//     wire shape drift to 19 of 46 fields, hiding `sql_scopes` (and with it the
//     grant that gates every tenant-scoped write) from the Library entirely.
//   - ROUND TRIP: convert back and require equality. This catches a field wired
//     to the WRONG source, which coverage alone cannot see: `SqlScopes:
//     def.MemoryScopes` leaves nothing zero and is still wrong.
//
// A round trip alone would miss a SYMMETRIC omission — a field dropped by both
// directions survives it untouched — which is why coverage is not redundant.
func TestSubstrateAgentDef_ConfigRoundTripCoversEveryField(t *testing.T) {
	// Absent from SubstrateAgentDef by design, so the conversion cannot carry
	// them. Each is load-time-only or dead; adding to this map is the conscious
	// decision the test forces.
	exempt := map[string]string{
		"SystemPromptFile": "a load-time path read from disk; a runtime-authored def has no filesystem to read",
		"DisableContext":   "load-time only — it suppresses the default-add of the Context tool and bakes into Tools before anything resolves",
		"SkillDefScopes":   "removed-field tombstone (RFC BA); skill authoring is governed by the skills: allowlist, and carrying it would resurrect a dead gate",
	}

	var full config.AgentDef
	populate(t, reflect.ValueOf(&full).Elem(), "AgentDef")

	sub := SubstrateAgentDefFromConfig(full)

	// COVERAGE — nothing the substrate shape declares may be left behind.
	sv := reflect.ValueOf(sub)
	for i := 0; i < sv.NumField(); i++ {
		name := sv.Type().Field(i).Name
		if sv.Field(i).IsZero() {
			t.Errorf("SubstrateAgentDefFromConfig leaves %s at its zero value — the field is declared on the wire and never filled, so the Library renders an agent that looks like it lacks the capability",
				name)
		}
	}

	// ROUND TRIP — and every field must come back as itself.
	back := sub.ToConfigDef()
	fv := reflect.ValueOf(full)
	bv := reflect.ValueOf(back)
	for i := 0; i < fv.NumField(); i++ {
		name := fv.Type().Field(i).Name
		if why, ok := exempt[name]; ok {
			if !bv.Field(i).IsZero() {
				t.Errorf("AgentDef.%s is exempt (%s) but came back non-zero — the exemption is stale", name, why)
			}
			continue
		}
		if !reflect.DeepEqual(fv.Field(i).Interface(), bv.Field(i).Interface()) {
			t.Errorf("AgentDef.%s does not survive config -> substrate -> config:\n  sent: %#v\n  got:  %#v\nEither wire it through both converters OR add it to exempt with a justification.",
				name, fv.Field(i).Interface(), bv.Field(i).Interface())
		}
	}
}

// populate fills every field of v with a distinctive non-zero value, so a
// forgotten or mis-wired assignment shows up as a zero or a mismatch rather
// than as two empty values that happen to compare equal.
func populate(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("v-" + path)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(len(path) + 1))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(len(path) + 1))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(len(path)) + 0.5)
	case reflect.Ptr:
		p := reflect.New(v.Type().Elem())
		populate(t, p.Elem(), path)
		v.Set(p)
	case reflect.Slice:
		e := reflect.New(v.Type().Elem()).Elem()
		populate(t, e, path)
		v.Set(reflect.Append(v, e))
	case reflect.Map:
		k := reflect.New(v.Type().Key()).Elem()
		populate(t, k, path+"-key")
		e := reflect.New(v.Type().Elem()).Elem()
		populate(t, e, path)
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if !v.Field(i).CanSet() {
				continue // unexported: nothing a converter could carry
			}
			populate(t, v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	case reflect.Interface:
		v.Set(reflect.ValueOf("v-" + path))
	}
}
