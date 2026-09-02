package statepatch

import (
	"encoding/json"
	"reflect"
	"testing"
)

// jsonMap decodes a JSON literal into a map the way a real patch/state arrives
// (numbers become float64, etc.) so the tests exercise the actual value shapes.
func jsonMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad json %q: %v", s, err)
	}
	return m
}

func TestMerge_RFC7386(t *testing.T) {
	cases := []struct {
		name              string
		base, patch, want string
	}{
		{"set new key", `{"a":1}`, `{"b":2}`, `{"a":1,"b":2}`},
		{"replace scalar", `{"a":1}`, `{"a":2}`, `{"a":2}`},
		{"null deletes", `{"a":1,"b":2}`, `{"a":null}`, `{"b":2}`},
		{"delete-missing is a no-op", `{"a":1}`, `{"zzz":null}`, `{"a":1}`},
		{"nested objects merge recursively", `{"o":{"x":1,"y":2}}`, `{"o":{"y":3,"z":4}}`, `{"o":{"x":1,"y":3,"z":4}}`},
		{"array replaces (not merged)", `{"a":[1,2,3]}`, `{"a":[9]}`, `{"a":[9]}`},
		{"object replaces a scalar", `{"a":1}`, `{"a":{"x":1}}`, `{"a":{"x":1}}`},
		{"scalar replaces an object", `{"a":{"x":1}}`, `{"a":5}`, `{"a":5}`},
		{"nested null deletes a subkey", `{"o":{"x":1,"y":2}}`, `{"o":{"x":null}}`, `{"o":{"y":2}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base, patch, want := jsonMap(t, c.base), jsonMap(t, c.patch), jsonMap(t, c.want)
			got := Merge(base, patch)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Merge(%s, %s) = %v, want %v", c.base, c.patch, got, want)
			}
			// Inputs must not be mutated.
			if !reflect.DeepEqual(base, jsonMap(t, c.base)) {
				t.Errorf("Merge mutated base: %v", base)
			}
			if !reflect.DeepEqual(patch, jsonMap(t, c.patch)) {
				t.Errorf("Merge mutated patch: %v", patch)
			}
		})
	}
}

func TestMerge_NilBase(t *testing.T) {
	got := Merge(nil, jsonMap(t, `{"a":1}`))
	if !reflect.DeepEqual(got, jsonMap(t, `{"a":1}`)) {
		t.Errorf("Merge(nil, x) = %v", got)
	}
}

func schema(t *testing.T, s string) map[string]any { return jsonMap(t, s) }

func TestValidatePatch_TypesAndUnknownKeys(t *testing.T) {
	sc := schema(t, `{
		"type":"object",
		"additionalProperties":false,
		"properties":{
			"count":{"type":"integer"},
			"name":{"type":"string"},
			"ratio":{"type":"number"},
			"done":{"type":"boolean"},
			"tags":{"type":"array","items":{"type":"string"}},
			"meta":{"type":"object","additionalProperties":false,"properties":{"k":{"type":"string"}}}
		}
	}`)

	good := []string{
		`{"count":3}`,
		`{"name":"x","done":true}`,
		`{"ratio":1.5}`,
		`{"count":5.0}`, // integral float is a valid integer (JSON has no int/float split)
		`{"tags":["a","b"]}`,
		`{"meta":{"k":"v"}}`,
		`{"count":null}`,      // deletion always ok
		`{"name":null}`,       // deletion of a typed field ok
		`{"meta":{"k":null}}`, // nested deletion ok
	}
	for _, p := range good {
		if err := ValidatePatch(sc, jsonMap(t, p)); err != nil {
			t.Errorf("patch %s should be valid: %v", p, err)
		}
	}

	bad := []struct{ patch, why string }{
		{`{"count":"three"}`, "string for integer"},
		{`{"count":1.5}`, "non-integral for integer"},
		{`{"name":7}`, "number for string"},
		{`{"done":"yes"}`, "string for boolean"},
		{`{"tags":[1,2]}`, "int elements for array-of-string"},
		{`{"tags":"a"}`, "string for array"},
		{`{"meta":{"k":9}}`, "number for nested string"},
		{`{"meta":{"other":"x"}}`, "unknown nested key with additionalProperties=false"},
		{`{"bogus":1}`, "unknown top-level key with additionalProperties=false"},
	}
	for _, c := range bad {
		if err := ValidatePatch(sc, jsonMap(t, c.patch)); err == nil {
			t.Errorf("patch %s should be REJECTED (%s)", c.patch, c.why)
		}
	}
}

func TestValidatePatch_AdditionalAllowedByDefault(t *testing.T) {
	// No additionalProperties → unknown keys pass (permissive default).
	sc := schema(t, `{"type":"object","properties":{"a":{"type":"integer"}}}`)
	if err := ValidatePatch(sc, jsonMap(t, `{"a":1,"anything":"goes"}`)); err != nil {
		t.Errorf("unknown key should pass when additionalProperties is unset: %v", err)
	}
	// But a KNOWN key is still type-checked.
	if err := ValidatePatch(sc, jsonMap(t, `{"a":"nope"}`)); err == nil {
		t.Error("known key must still be type-checked")
	}
}

func TestValidatePatch_EmptySchemaPermissive(t *testing.T) {
	if err := ValidatePatch(nil, jsonMap(t, `{"a":1,"b":"x"}`)); err != nil {
		t.Errorf("nil schema must accept everything: %v", err)
	}
	if err := ValidatePatch(map[string]any{}, jsonMap(t, `{"a":1}`)); err != nil {
		t.Errorf("empty schema must accept everything: %v", err)
	}
}
