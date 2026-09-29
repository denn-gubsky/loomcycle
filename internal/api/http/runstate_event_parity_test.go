package http

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// busOnlyRunStateFields are runstate.RunStateEvent fields no transport sends.
// tenant_id is the stream's isolation key: every transport filters on it
// server-side (StreamUserRunStates), and the connector event deliberately does
// not carry it, so a subscriber is never told which tenant a run is in.
var busOnlyRunStateFields = map[string]bool{"tenant_id": true}

// runStateWireFields is what every transport's run-state event carries: the bus
// event's fields, less the bus-only ones. Derived, not listed, so a field added
// to the bus event is a field every mirror below must declare.
func runStateWireFields(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, f := range jsonFields(t, runstate.RunStateEvent{}) {
		if !busOnlyRunStateFields[f] {
			out = append(out, f)
		}
	}
	if len(out) < 10 {
		t.Fatalf("only %d fields read from runstate.RunStateEvent — the reflection has stopped matching", len(out))
	}
	return out
}

// The run-state event has five hand-written mirrors: the connector event every
// transport reads, the proto message, the TS type and the Python dict. Nothing
// checked them against each other, so a field could reach one transport and
// not another — the gRPC stream shipped without parent_context that way.
func TestRunStateEvent_EveryMirrorCarriesEveryField(t *testing.T) {
	want := runStateWireFields(t)

	conn := map[string]bool{}
	for _, f := range jsonFields(t, connector.RunStateEvent{}) {
		conn[f] = true
	}
	for _, f := range want {
		if !conn[f] {
			t.Errorf("runstate.RunStateEvent has %q but connector.RunStateEvent does not", f)
		}
	}
	for f := range conn {
		if !slices.Contains(want, f) {
			t.Errorf("connector.RunStateEvent has %q the bus event does not produce", f)
		}
	}

	proto := declaredNames(protoFieldRe, sourceBlock(t, "../../../proto/loomcycle.proto", "message RunStateEvent {", "\n}"))
	ts := declaredNames(tsFieldRe, sourceBlock(t, "../../../adapters/ts/src/types.ts", "export interface RunStateEvent {", "\n}"))
	py := sourceBlock(t, "../../../adapters/python/loomcycle/client.py", "def _run_state_event_to_dict(", "\ndef ")
	for _, f := range want {
		if !proto[f] {
			t.Errorf("proto RunStateEvent does not declare %q", f)
		}
		if !ts[f] {
			t.Errorf("TS RunStateEvent does not declare %q", f)
		}
		if !strings.Contains(py, `"`+f+`":`) {
			t.Errorf("the Python _run_state_event_to_dict does not map %q", f)
		}
	}
	for f := range proto {
		if !slices.Contains(want, f) {
			t.Errorf("proto RunStateEvent declares %q the bus event does not produce", f)
		}
	}
}

// runStateEventToConnector copies every wire field. Filled by reflection so a
// new field is exercised without anyone remembering to add it here; a field of
// a kind this cannot fill fails the test instead of being skipped.
func TestRunStateEventToConnector_CopiesEveryField(t *testing.T) {
	var in runstate.RunStateEvent
	rv := reflect.ValueOf(&in).Elem()
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		switch v := f.Addr().Interface().(type) {
		case *string:
			*v = "v_" + rv.Type().Field(i).Name
		case *time.Time:
			*v = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
		case **store.ParentContext:
			*v = &store.ParentContext{WalkID: "r_walk", WaveIndex: 2}
		default:
			t.Fatalf("field %s is a %s this test does not know how to fill", rv.Type().Field(i).Name, f.Type())
		}
	}
	src, dst := jsonMap(t, in), jsonMap(t, runStateEventToConnector(in))
	for _, f := range runStateWireFields(t) {
		if f == "ts" {
			if dst[f] != "2026-09-29T12:00:00Z" {
				t.Errorf("ts = %v, want the RFC3339 form of the bus time", dst[f])
			}
			continue
		}
		a, _ := json.Marshal(src[f])
		b, _ := json.Marshal(dst[f])
		if string(a) != string(b) {
			t.Errorf("%s: bus %s, connector %s", f, a, b)
		}
	}
	if _, leaked := dst["tenant_id"]; leaked {
		t.Error("the connector event carries tenant_id")
	}
}

func jsonMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
