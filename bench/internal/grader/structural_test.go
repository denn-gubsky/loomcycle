package grader

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/bench/internal/cases"
)

// TestStructural_PureJSONPasses verifies the happy path: well-formed
// JSON object that satisfies the schema returns Pass=true with no
// reasons.
func TestStructural_PureJSONPasses(t *testing.T) {
	text := `{"verdicts":[
		{"index":0,"safe":true,"score":0.0,"reason":"benign"},
		{"index":1,"safe":false,"score":0.7,"reason":"injection"}
	]}`
	exp := cases.Structural{
		MustMatch: `^\s*\{[\s\S]*\}\s*$`,
		Schema: `{"type":"object","required":["verdicts"],"properties":{
			"verdicts":{"type":"array","minItems":2,"items":{"type":"object",
			"required":["index","safe","score","reason"]}}}}`,
	}
	r := Structural(text, exp)
	if !r.Pass {
		t.Fatalf("expected pass; reasons: %v", r.Reasons)
	}
}

// TestStructural_NarrationBeforeJSONFailsMustNotMatch — covers the
// model that politely prefaces its JSON with "Here is the result:".
func TestStructural_NarrationBeforeJSONFailsMustNotMatch(t *testing.T) {
	text := "Here is the JSON:\n{\"verdicts\":[]}"
	exp := cases.Structural{
		MustNotMatch: `(?im)^here is|^let me`,
	}
	r := Structural(text, exp)
	if r.Pass {
		t.Fatal("expected fail on must_not_match (narration prefix)")
	}
	if len(r.Reasons) == 0 {
		t.Fatal("expected diagnostic reason")
	}
}

// TestStructural_SchemaCatchesMissingRequiredField.
func TestStructural_SchemaCatchesMissingRequiredField(t *testing.T) {
	text := `{"verdicts":[{"index":0,"safe":true,"score":0.0}]}` // missing "reason"
	exp := cases.Structural{
		Schema: `{"type":"object","required":["verdicts"],"properties":{
			"verdicts":{"type":"array","items":{"type":"object",
			"required":["index","safe","score","reason"]}}}}`,
	}
	r := Structural(text, exp)
	if r.Pass {
		t.Fatal("expected fail on missing 'reason' field")
	}
	if !contains(r.Reasons, "reason: required field missing") {
		t.Errorf("expected reason about missing field; got %v", r.Reasons)
	}
}

// TestStructural_SchemaEnumRejectsUnknownValue.
func TestStructural_SchemaEnumRejectsUnknownValue(t *testing.T) {
	text := `{"verdict": "uncertain"}` // not in enum
	exp := cases.Structural{
		Schema: `{"type":"object","properties":{
			"verdict":{"type":"string","enum":["safe","unsafe"]}}}`,
	}
	r := Structural(text, exp)
	if r.Pass {
		t.Fatal("expected fail on enum")
	}
}

// TestStructural_SchemaAfterSeparator — mid-08 format-switching case.
func TestStructural_SchemaAfterSeparator(t *testing.T) {
	text := "Some prose paragraph here.\n---\n{\"summary_json\":{\"years_experience\":5}}"
	exp := cases.Structural{
		MustMatch: `(?s)^[\s\S]+\n---\n\s*\{[\s\S]+\}\s*$`,
		SchemaAfterSeparator: `{"type":"object","required":["summary_json"],"properties":{
			"summary_json":{"type":"object","required":["years_experience"],"properties":{
			"years_experience":{"type":"integer","minimum":1,"maximum":10}}}}}`,
	}
	r := Structural(text, exp)
	if !r.Pass {
		t.Fatalf("expected pass; reasons: %v", r.Reasons)
	}
}

// TestStructural_StripsCodeFencesSoSchemaCanStillValidate — many
// candidate models wrap JSON in ```json fences even when told not
// to. The validator strips them; must_not_match flags the violation.
func TestStructural_StripsCodeFencesSoSchemaCanStillValidate(t *testing.T) {
	text := "```json\n{\"x\":1}\n```"
	// Schema-only check: passes because fences are stripped.
	exp := cases.Structural{
		Schema: `{"type":"object","required":["x"],"properties":{"x":{"type":"integer"}}}`,
	}
	r := Structural(text, exp)
	if !r.Pass {
		t.Fatalf("expected schema to pass after fence strip; reasons: %v", r.Reasons)
	}
	// Add must_not_match: fences caught here.
	exp.MustNotMatch = "(?m)^```"
	r2 := Structural(text, exp)
	if r2.Pass {
		t.Fatal("expected must_not_match to flag fences")
	}
}

func contains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
