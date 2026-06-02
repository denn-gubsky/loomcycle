package anthropic

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func parseSchema(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("result not an object: %v (%s)", err, raw)
	}
	return m
}

func hasTopLevelCombinator(m map[string]any) bool {
	for _, k := range []string{"oneOf", "anyOf", "allOf"} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// The reproducer: a Zod discriminatedUnion at the ROOT compiles to a
// top-level anyOf of $ref variants + a $defs block — exactly the shape
// Anthropic 400s on. After sanitizing, the top-level combinator is gone
// and every variant's properties are merged into one object schema.
func TestSanitizeAnthropicToolSchema_FlattensTopLevelRefUnion(t *testing.T) {
	raw := json.RawMessage(`{
	  "anyOf": [ {"$ref": "#/$defs/Create"}, {"$ref": "#/$defs/Delete"} ],
	  "$defs": {
	    "Create": {"type":"object","properties":{"kind":{"const":"create"},"name":{"type":"string"}},"required":["kind","name"]},
	    "Delete": {"type":"object","properties":{"kind":{"const":"delete"},"id":{"type":"string"}},"required":["kind","id"]}
	  }
	}`)
	got := parseSchema(t, sanitizeAnthropicToolSchema(raw))
	if hasTopLevelCombinator(got) {
		t.Fatalf("top-level combinator survived: %v", got)
	}
	if got["type"] != "object" {
		t.Errorf("type = %v, want object", got["type"])
	}
	props, _ := got["properties"].(map[string]any)
	for _, want := range []string{"kind", "name", "id"} {
		if _, ok := props[want]; !ok {
			t.Errorf("merged properties missing %q: %v", want, props)
		}
	}
	// $defs may remain (Anthropic accepts nested $ref/$defs); the point
	// is only that the TOP LEVEL has no combinator.
}

func TestSanitizeAnthropicToolSchema_FlattensInlineUnion(t *testing.T) {
	raw := json.RawMessage(`{"oneOf":[
	  {"type":"object","properties":{"a":{"type":"string"}}},
	  {"type":"object","properties":{"b":{"type":"number"}}}
	]}`)
	got := parseSchema(t, sanitizeAnthropicToolSchema(raw))
	if hasTopLevelCombinator(got) {
		t.Fatalf("combinator survived: %v", got)
	}
	props, _ := got["properties"].(map[string]any)
	if _, ok := props["a"]; !ok {
		t.Errorf("missing a: %v", props)
	}
	if _, ok := props["b"]; !ok {
		t.Errorf("missing b: %v", props)
	}
}

// Regression guard: a plain object schema (the overwhelming common case)
// must pass through BYTE-FOR-BYTE unchanged — no over-sanitizing.
func TestSanitizeAnthropicToolSchema_PlainObjectUntouched(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}`)
	got := sanitizeAnthropicToolSchema(raw)
	if string(got) != string(raw) {
		t.Errorf("plain schema was modified:\n got: %s\nwant: %s", got, raw)
	}
}

// A NESTED union must be preserved — Anthropic accepts nested
// oneOf/anyOf/allOf; only the top level is forbidden. Over-sanitizing
// here would regress currently-working tools.
func TestSanitizeAnthropicToolSchema_NestedUnionPreserved(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"payload":{"anyOf":[{"type":"string"},{"type":"number"}]}}}`)
	got := sanitizeAnthropicToolSchema(raw)
	if string(got) != string(raw) {
		t.Errorf("nested union was altered (Anthropic accepts nested):\n got: %s\nwant: %s", got, raw)
	}
}

func TestSanitizeAnthropicToolSchema_TypeConflictDefense(t *testing.T) {
	// One variant object, one array → the array variant's structural
	// fields must not fold into the object (would be malformed).
	raw := json.RawMessage(`{"anyOf":[
	  {"type":"object","properties":{"a":{"type":"string"}}},
	  {"type":"array","items":{"type":"string"}}
	]}`)
	got := parseSchema(t, sanitizeAnthropicToolSchema(raw))
	if got["type"] != "object" {
		t.Errorf("type = %v, want object (first variant wins)", got["type"])
	}
	if _, hasItems := got["items"]; hasItems {
		t.Error("array variant's `items` must not fold into the object schema")
	}
}

func TestSanitizeAnthropicTools_MapsAndDoesNotMutate(t *testing.T) {
	orig := json.RawMessage(`{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}`)
	in := []providers.ToolSpec{
		{Name: "plain", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
		{Name: "union", InputSchema: orig},
	}
	out := sanitizeAnthropicTools(in)
	if hasTopLevelCombinator(parseSchema(t, out[1].InputSchema)) {
		t.Error("union tool not sanitized")
	}
	// Input ToolSpec's raw schema must be untouched (may be shared with a
	// fallback provider).
	if !reflect.DeepEqual([]byte(in[1].InputSchema), []byte(orig)) {
		t.Error("input InputSchema was mutated")
	}
}
