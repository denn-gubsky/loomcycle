package http

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// RFC DC V3, first half. Every field of config.AgentDef must be classified.
//
// This is the test the whole override feature rests on. AgentDef has fifty
// fields and grows; without somewhere a new one MUST appear, it arrives
// unclassified — and unclassified here means nobody decided whether a caller
// may set it. That is how a field that grants reach ends up caller-settable
// because it read like tuning.
//
// If you are here because this test failed: you added a field to AgentDef. Put
// it in agentDefOverridability, and choose by asking what a hostile caller
// gains by setting it, not by what it is called.
func TestOverridability_EveryAgentDefFieldIsClassified(t *testing.T) {
	typ := reflect.TypeOf(config.AgentDef{})

	var unclassified []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue // unexported: not part of the definition's surface
		}
		if _, ok := agentDefOverridability[f.Name]; !ok {
			unclassified = append(unclassified, f.Name)
		}
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("these config.AgentDef fields are not classified in agentDefOverridability:\n  %s\n\n"+
			"Decide whether a RUN may set each one. Ask what a hostile caller gains by setting "+
			"it — not what it is called. Reach, authoring authority, identity and the operator's "+
			"own declarations are never overridable.", strings.Join(unclassified, "\n  "))
	}

	// The other direction: a classification for a field that no longer exists
	// is stale, and a stale entry silently stops guarding anything.
	live := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		live[typ.Field(i).Name] = true
	}
	var stale []string
	for name := range agentDefOverridability {
		if !live[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("agentDefOverridability classifies fields that no longer exist on "+
			"config.AgentDef: %s", strings.Join(stale, ", "))
	}

	// Non-vacuity: reflection over the wrong type would classify nothing and
	// pass silently.
	if typ.NumField() < 40 {
		t.Fatalf("config.AgentDef has %d fields; this test expects the real definition "+
			"and is not looking at it", typ.NumField())
	}
}

// RFC DC V3, second half. The override structs may only express fields a run is
// allowed to choose.
//
// Relying on handlers to ignore a field they were handed is the weaker
// guarantee — it holds until someone wires the field through "for completeness".
// This asserts the STRUCTS cannot carry it, so the boundary is in the type
// rather than in everyone's memory.
func TestOverridability_OverrideStructsExpressOnlyAllowedFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  reflect.Type
	}{
		{"routingOverride", reflect.TypeOf(routingOverride{})},
		{"resourceOverride", reflect.TypeOf(resourceOverride{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.typ.NumField() == 0 {
				t.Fatal("no fields; this check is looking at the wrong type")
			}
			for i := 0; i < tc.typ.NumField(); i++ {
				name := tc.typ.Field(i).Name
				kind, ok := agentDefOverridability[name]
				if !ok {
					t.Errorf("%s.%s has no counterpart in config.AgentDef, so nothing says "+
						"whether a run may set it", tc.name, name)
					continue
				}
				if kind == notOverridable {
					t.Errorf("%s can express %s, which is NOT overridable. The struct must not "+
						"be able to carry it — a handler that ignores it today is one edit from "+
						"honouring it.", tc.name, name)
				}
			}
		})
	}
}

// The narrowing-only fields are called out separately because "may be set" and
// "may only be shrunk" are different permissions, and a table that blurred them
// would let a lower-only bound be raised by anyone reading only the first column.
func TestOverridability_NarrowingOnlyFieldsAreDistinctFromFreeOnes(t *testing.T) {
	want := map[string]bool{"MaxConcurrentChildren": true, "Tools": true}
	got := map[string]bool{}
	for name, kind := range agentDefOverridability {
		if kind == runMayNarrow {
			got[name] = true
		}
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("narrowing-only set = %v, want %v.\n\nAdding one is a deliberate act: it means "+
			"the value is a BOUND that something else depends on, not a preference.", got, want)
	}
}
