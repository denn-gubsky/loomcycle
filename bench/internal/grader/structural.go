package grader

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/denn-gubsky/loomcycle/bench/internal/cases"
)

// Structural grades the final assistant text against the case's
// structural expectations: regex match / anti-match, plus a
// lightweight JSON-schema-ish object validator.
//
// Returns Pass=true only when ALL declared checks pass. Cases that
// declare no checks (zero-value Structural) pass trivially.
func Structural(finalText string, exp cases.Structural) AxisResult {
	r := AxisResult{Pass: true, Score: 1.0}

	if exp.MustMatch != "" {
		re, err := regexp.Compile(exp.MustMatch)
		if err != nil {
			return failAxis(fmt.Sprintf("must_match regex compile: %v", err))
		}
		if !re.MatchString(finalText) {
			r.Pass = false
			r.Score = 0
			r.Reasons = append(r.Reasons, "must_match pattern did not match final text")
		}
	}

	if exp.MustNotMatch != "" {
		re, err := regexp.Compile(exp.MustNotMatch)
		if err != nil {
			return failAxis(fmt.Sprintf("must_not_match regex compile: %v", err))
		}
		if re.MatchString(finalText) {
			r.Pass = false
			r.Score = 0
			r.Reasons = append(r.Reasons, "must_not_match pattern matched (forbidden content present)")
		}
	}

	if schema := exp.Schema; schema != "" {
		if ok, why := validateJSONAgainstSchema(finalText, schema); !ok {
			r.Pass = false
			r.Score = 0
			r.Reasons = append(r.Reasons, "schema: "+why)
		}
	}

	if schema := exp.SchemaAfterSeparator; schema != "" {
		// mid-08: prose then "\n---\n" then JSON. Validate the JSON
		// after the separator only.
		const sep = "\n---\n"
		idx := strings.LastIndex(finalText, sep)
		if idx < 0 {
			r.Pass = false
			r.Score = 0
			r.Reasons = append(r.Reasons, "schema_after_separator: '\\n---\\n' separator not found")
		} else {
			jsonPart := strings.TrimSpace(finalText[idx+len(sep):])
			if ok, why := validateJSONAgainstSchema(jsonPart, schema); !ok {
				r.Pass = false
				r.Score = 0
				r.Reasons = append(r.Reasons, "schema_after_separator: "+why)
			}
		}
	}

	return r
}

func failAxis(reason string) AxisResult {
	return AxisResult{Pass: false, Score: 0, Reasons: []string{reason}}
}

// validateJSONAgainstSchema parses both inputs as JSON and recursively
// verifies the value satisfies the schema. Supported keywords (small
// pragmatic subset for the bench's cases — NOT a full JSON Schema
// implementation):
//
//	type            string|number|integer|boolean|array|object
//	required        []string (object only)
//	properties      map[string]subschema (object only)
//	enum            []any (any type)
//	minimum/maximum number (number/integer)
//	minLength       integer (string)
//	maxLength       integer (string)
//	pattern         regex (string)
//	items           subschema (array, applied to every element)
//	minItems        integer (array)
//	maxItems        integer (array)
//
// Anything else in the schema is silently ignored — keep the
// validator small and predictable.
//
// Response-text discipline:
//   - First we strip outer whitespace + leading/trailing ``` fences.
//   - If the result starts with `{` or `[`, validate it directly.
//   - Otherwise extract the largest balanced `{...}` / `[...]` block
//     and validate that. This handles the common "model leads with
//     prose / 'I have called the tool...' before emitting the JSON"
//     case — production agents can strip prose, so capability rank
//     shouldn't fail on it. Cases that REQUIRE bare-JSON output
//     (e.g., production injection-judge's downstream parser) can use
//     `must_not_match` regex to flag pre-JSON narration as a separate
//     structural sub-check.
func validateJSONAgainstSchema(textBody, schemaJSON string) (bool, string) {
	text := strings.TrimSpace(textBody)
	text = stripCodeFences(text)
	jsonPart, ok := extractJSONBlock(text)
	if !ok {
		return false, "no JSON object or array found in response (first char: " + firstChar(text) + ")"
	}
	var value any
	if err := json.Unmarshal([]byte(jsonPart), &value); err != nil {
		return false, "json parse: " + err.Error()
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
		return false, "schema parse (internal): " + err.Error()
	}
	if err := checkSchema(value, schema, "$"); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// extractJSONBlock finds and returns the largest balanced {...} or
// [...] substring in s. Returns ("", false) when no balanced block
// exists.
//
// Implementation: walk s with a depth counter, tracking whether we're
// inside a JSON string (to ignore brace/bracket chars within strings).
// We seek the FIRST opening brace/bracket, then find its matching
// close. This is good enough for the bench's use case — cases produce
// one top-level JSON value preceded/followed by at most narration.
//
// Not a full JSON parser; just a balanced-delimiter scanner. The
// json.Unmarshal call downstream is what enforces well-formedness.
func extractJSONBlock(s string) (string, bool) {
	first := -1
	var open, close byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '{' {
			first, open, close = i, '{', '}'
			break
		}
		if c == '[' {
			first, open, close = i, '[', ']'
			break
		}
	}
	if first < 0 {
		return "", false
	}
	depth := 0
	inString := false
	escape := false
	for i := first; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if inString {
			if c == '\\' {
				escape = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			continue
		}
		if c == open {
			depth++
		} else if c == close {
			depth--
			if depth == 0 {
				return s[first : i+1], true
			}
		}
	}
	return "", false
}

// firstChar returns a short description of the leading character for
// diagnostic messages. Exported (lowercase-helper) for the no-JSON
// branch of validateJSONAgainstSchema.
func firstChar(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "<empty>"
	}
	return string(s[0])
}

func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		// Drop the leading fence line.
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = s[nl+1:]
		}
	}
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
}

// checkSchema is the recursive validator. path is a dot/index trail
// used in error messages so the operator can find the offending field.
func checkSchema(value any, schema map[string]any, path string) error {
	// type
	if t, ok := schema["type"].(string); ok {
		if err := checkType(value, t, path); err != nil {
			return err
		}
	}
	// enum
	if enum, ok := schema["enum"].([]any); ok {
		if !inEnum(value, enum) {
			return fmt.Errorf("%s: value not in enum", path)
		}
	}

	switch v := value.(type) {
	case map[string]any:
		// required
		if req, ok := schema["required"].([]any); ok {
			for _, rk := range req {
				key, _ := rk.(string)
				if _, present := v[key]; !present {
					return fmt.Errorf("%s.%s: required field missing", path, key)
				}
			}
		}
		// properties
		if props, ok := schema["properties"].(map[string]any); ok {
			for key, sub := range props {
				if val, present := v[key]; present {
					subSchema, _ := sub.(map[string]any)
					if subSchema != nil {
						if err := checkSchema(val, subSchema, path+"."+key); err != nil {
							return err
						}
					}
				}
			}
		}
	case []any:
		// items
		if items, ok := schema["items"].(map[string]any); ok {
			for i, elem := range v {
				if err := checkSchema(elem, items, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
		// minItems / maxItems
		if mi, ok := numericFromAny(schema["minItems"]); ok {
			if len(v) < int(mi) {
				return fmt.Errorf("%s: array length %d < minItems %d", path, len(v), int(mi))
			}
		}
		if ma, ok := numericFromAny(schema["maxItems"]); ok {
			if len(v) > int(ma) {
				return fmt.Errorf("%s: array length %d > maxItems %d", path, len(v), int(ma))
			}
		}
	case string:
		if mi, ok := numericFromAny(schema["minLength"]); ok {
			if len(v) < int(mi) {
				return fmt.Errorf("%s: string length %d < minLength %d", path, len(v), int(mi))
			}
		}
		if ma, ok := numericFromAny(schema["maxLength"]); ok {
			if len(v) > int(ma) {
				return fmt.Errorf("%s: string length %d > maxLength %d", path, len(v), int(ma))
			}
		}
		if pat, ok := schema["pattern"].(string); ok && pat != "" {
			re, err := regexp.Compile(pat)
			if err != nil {
				return fmt.Errorf("%s: schema pattern compile: %v", path, err)
			}
			if !re.MatchString(v) {
				return fmt.Errorf("%s: string does not match pattern %s", path, pat)
			}
		}
	case float64:
		if mi, ok := numericFromAny(schema["minimum"]); ok {
			if v < mi {
				return fmt.Errorf("%s: value %v < minimum %v", path, v, mi)
			}
		}
		if ma, ok := numericFromAny(schema["maximum"]); ok {
			if v > ma {
				return fmt.Errorf("%s: value %v > maximum %v", path, v, ma)
			}
		}
	}
	return nil
}

func checkType(value any, want string, path string) error {
	switch want {
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("%s: expected object", path)
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("%s: expected array", path)
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s: expected string", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: expected boolean", path)
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("%s: expected number", path)
		}
	case "integer":
		f, ok := value.(float64)
		if !ok || f != float64(int64(f)) {
			return fmt.Errorf("%s: expected integer", path)
		}
	}
	return nil
}

func inEnum(value any, enum []any) bool {
	for _, e := range enum {
		if e == value {
			return true
		}
	}
	return false
}

func numericFromAny(x any) (float64, bool) {
	switch v := x.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}
