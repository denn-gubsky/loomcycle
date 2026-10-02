package teamgraph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// CheckInput checks a walk's input against the entry state's `schema` — the
// team's input form — before the walk starts. It applies only when the entry
// is an input state or a Starter that reads the walk's input, and only when
// that state carries a schema; every other definition passes.
//
// The check is deliberately minimal, three rules and nothing more:
//
//  1. a top-level `type` (a name or a list of names) — the value must be of
//     that JSON type; `integer` is a number with no fractional part;
//  2. `required` — when the value is an object, each named field is present;
//  3. `properties.<field>.type` — when the value is an object, each PRESENT
//     field with a declared type is of that type. One level only.
//
// Everything else ($ref, enum, format, nested schemas, x-* keywords) is
// ignored, and so is a schema that is not a JSON object or a rule whose own
// shape is not one of the above: the form is the client's to render, and a
// check that refused on a keyword it does not implement would make a team
// unrunnable for a reason nobody wrote down. The point is narrower — a
// missing or mistyped field fails here, naming the field, rather than after
// a model has been paid to discover it.
//
// The input is read by the same rule every other reader of it uses
// (InputValue): valid JSON is that value, anything else is {"text": input}.
func CheckInput(d Definition, input string) error {
	entry, ok := StateByID(d, d.Entry)
	if !ok || len(entry.Handler.Schema) == 0 {
		return nil
	}
	if entry.Handler.Kind != HandlerInput && !IsInputStarter(entry) {
		return nil
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(entry.Handler.Schema, &schema); err != nil {
		return nil
	}
	value, err := decodeNumbers(InputValue(input))
	if err != nil {
		return nil
	}

	if types, ok := schemaTypes(schema["type"]); ok && !matchesAny(value, types) {
		msg := fmt.Sprintf("input must be %s, got %s", describeTypes(types), describeValue(value))
		if !json.Valid([]byte(input)) {
			msg += ` — input that is not JSON is read as the object {"text": "<input>"}`
		}
		return errors.New(msg)
	}

	obj, isObject := value.(map[string]any)
	if !isObject {
		return nil
	}
	var required []string
	if err := json.Unmarshal(schema["required"], &required); err == nil {
		for _, field := range required {
			if _, present := obj[field]; !present {
				return fmt.Errorf("input field %q is required", field)
			}
		}
	}
	var props map[string]json.RawMessage
	if err := json.Unmarshal(schema["properties"], &props); err != nil {
		return nil
	}
	// Sorted so a value with two bad fields always names the same one.
	fields := make([]string, 0, len(props))
	for f := range props {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	for _, field := range fields {
		v, present := obj[field]
		if !present {
			continue
		}
		var prop map[string]json.RawMessage
		if err := json.Unmarshal(props[field], &prop); err != nil {
			continue
		}
		if types, ok := schemaTypes(prop["type"]); ok && !matchesAny(v, types) {
			return fmt.Errorf("input field %q must be %s, got %s", field, describeTypes(types), describeValue(v))
		}
	}
	return nil
}

// decodeNumbers decodes a JSON value keeping numbers as json.Number, so an
// integer can be told from a number with a fraction.
func decodeNumbers(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

// typeLabel is how a refusal names a JSON type; ok is false for a name the
// check does not know, and a declaration naming one is not checked rather
// than refusing every input.
func typeLabel(t string) (label string, ok bool) {
	switch t {
	case "object":
		return "a JSON object", true
	case "array":
		return "a JSON array", true
	case "string":
		return "a string", true
	case "number":
		return "a number", true
	case "integer":
		return "an integer", true
	case "boolean":
		return "a boolean", true
	case "null":
		return "null", true
	}
	return "", false
}

// schemaTypes reads a `type` keyword: one name or a list of names. ok is
// false when there is none, or when it is not a shape the check knows.
func schemaTypes(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return knownTypes([]string{one})
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil && len(many) > 0 {
		return knownTypes(many)
	}
	return nil, false
}

func knownTypes(names []string) ([]string, bool) {
	for _, n := range names {
		if _, ok := typeLabel(n); !ok {
			return nil, false
		}
	}
	return names, true
}

func matchesAny(v any, types []string) bool {
	for _, t := range types {
		if matchesType(v, t) {
			return true
		}
	}
	return false
}

func matchesType(v any, t string) bool {
	switch x := v.(type) {
	case map[string]any:
		return t == "object"
	case []any:
		return t == "array"
	case string:
		return t == "string"
	case bool:
		return t == "boolean"
	case nil:
		return t == "null"
	case json.Number:
		if t == "number" {
			return true
		}
		if t != "integer" {
			return false
		}
		return isWholeNumber(x.String())
	}
	return false
}

// isWholeNumber reports whether a JSON number literal has no fractional
// part: 1, -0, 1.0 and 1.5e1 are whole, 1.5 and 1e-1 are not. Read from the
// literal's digits rather than through a float, so a value past float64's
// exact range is judged by what was written, and rather than through an
// arbitrary-precision type, which would materialise 1e999999999 to decide.
func isWholeNumber(lit string) bool {
	lit = strings.TrimPrefix(lit, "-")
	mant, exp := lit, int64(0)
	if i := strings.IndexAny(lit, "eE"); i >= 0 {
		mant = lit[:i]
		// A JSON number's exponent is always well-formed; one out of int32's
		// range comes back clamped to it, keeping its sign, and either way the
		// point then moves past every digit.
		e, _ := strconv.ParseInt(lit[i+1:], 10, 32)
		exp = e
	}
	intPart, frac, _ := strings.Cut(mant, ".")
	digits := intPart + frac
	// The decimal point sits after len(intPart) digits; the exponent moves it.
	// Every digit at or after its new position is fractional.
	point := int64(len(intPart)) + exp
	if point < 0 {
		point = 0
	}
	if point > int64(len(digits)) {
		return true
	}
	return strings.Trim(digits[point:], "0") == ""
}

func describeTypes(types []string) string {
	out := make([]string, len(types))
	for i, t := range types {
		out[i], _ = typeLabel(t)
	}
	return strings.Join(out, " or ")
}

// describeValue names a value's JSON type for a refusal; a number is
// "a number" whether or not it is whole.
func describeValue(v any) string {
	t := "null"
	switch v.(type) {
	case map[string]any:
		t = "object"
	case []any:
		t = "array"
	case string:
		t = "string"
	case bool:
		t = "boolean"
	case json.Number:
		t = "number"
	}
	label, _ := typeLabel(t)
	return label
}
