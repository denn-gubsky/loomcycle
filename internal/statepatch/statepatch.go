// Package statepatch implements the two operations RFC CR L2 (structured
// execution state) needs on the state object Σ: a JSON Merge Patch (RFC 7386,
// null-deletes) to apply a model-proposed patch, and a minimal JSON-Schema
// validator for that patch.
//
// It is deliberately small — it supports the flat, typed object schemas the
// state protocol uses (object / properties / required / additionalProperties,
// scalar types, and array-of-items), not the full JSON Schema spec (no $ref,
// no formats, no patternProperties). That subset is what an operator- or
// model-authored state schema needs; extend it here when a real schema does.
// No external dependency (the repo vendors no schema validator).
package statepatch

import (
	"encoding/json"
	"fmt"
	"math"
)

// Merge applies an RFC 7386 JSON Merge Patch to base: a null value in `patch`
// DELETES the key, two objects merge recursively, and any other value (scalar or
// array) replaces. Neither input is mutated; the result is a fresh top-level map
// (unpatched nested subtrees are shared by reference — the caller treats Σ as
// immutable per step, replacing it wholesale, so nothing mutates them in place).
// A nil base is treated as empty.
func Merge(base, patch map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(patch))
	for k, v := range base {
		out[k] = v
	}
	for k, pv := range patch {
		if pv == nil {
			delete(out, k) // RFC 7386: null deletes
			continue
		}
		if pm, ok := pv.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = Merge(bm, pm) // both objects → recurse
				continue
			}
		}
		out[k] = pv // scalar / array / type-mismatch → replace
	}
	return out
}

// ValidatePatch checks a patch against the schema subset: every non-null value
// must match the declared type of its property, and — when the (nested)
// additionalProperties is false — no key outside `properties` may appear. Null
// values (deletions) always pass. An empty/nil schema passes everything
// (permissive: a stateful run may run before a schema is adopted).
//
// `required` is intentionally NOT enforced here. Σ accretes over a run, so a
// per-step patch legitimately omits fields that are not set yet; completeness is
// a whole-Σ property, not a patch property. The invariant this enforces is "no
// patch ever writes a wrong-typed or unknown field into Σ".
func ValidatePatch(schema, patch map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	return validateObject("", schema, patch)
}

// validateObject checks the keys of `obj` against an object schema's properties +
// additionalProperties. name is the dotted path prefix for error messages ("" at
// the root).
func validateObject(name string, schema, obj map[string]any) error {
	props, _ := schema["properties"].(map[string]any)
	additional := true
	if b, ok := schema["additionalProperties"].(bool); ok {
		additional = b
	}
	for k, v := range obj {
		if v == nil {
			continue // deletion / explicit null
		}
		ps, ok := props[k].(map[string]any)
		if !ok {
			if !additional {
				return fmt.Errorf("field %q: not permitted by the schema (additionalProperties=false)", qualify(name, k))
			}
			continue
		}
		if err := checkType(qualify(name, k), ps, v); err != nil {
			return err
		}
	}
	return nil
}

// checkType verifies v against a property schema's `type` (and recurses for
// object/array).
func checkType(name string, schema map[string]any, v any) error {
	t, _ := schema["type"].(string)
	switch t {
	case "", "null":
		return nil // untyped → accept anything
	case "string":
		if _, ok := v.(string); !ok {
			return typeErr(name, "string", v)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return typeErr(name, "boolean", v)
		}
	case "number":
		if _, ok := asFloat(v); !ok {
			return typeErr(name, "number", v)
		}
	case "integer":
		f, ok := asFloat(v)
		if !ok || f != math.Trunc(f) {
			return typeErr(name, "integer", v)
		}
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return typeErr(name, "object", v)
		}
		return validateObject(name, schema, m)
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return typeErr(name, "array", v)
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, e := range arr {
				if err := checkType(fmt.Sprintf("%s[%d]", name, i), items, e); err != nil {
					return err
				}
			}
		}
	default:
		return fmt.Errorf("field %q: unsupported schema type %q", name, t)
	}
	return nil
}

// asFloat coerces a JSON-decoded numeric (float64), a json.Number, or a Go
// int/int64 to float64. Returns false for a non-number.
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func typeErr(name, want string, got any) error {
	return fmt.Errorf("field %q: want %s, got %T", name, want, got)
}

func qualify(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}
