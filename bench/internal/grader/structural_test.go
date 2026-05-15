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

// TestStructural_ExtractsJSONAfterProse — Sweep #4 surfaced the
// pattern where models lead with narration like "I have called the
// tool...\n{...}". The bench should extract the JSON object and
// validate THAT against the schema, not reject on the first 'I' char.
//
// Cases that require bare-JSON output can use must_not_match to
// flag pre-JSON prose as a separate structural sub-check.
func TestStructural_ExtractsJSONAfterProse(t *testing.T) {
	text := `I have called the tool successfully. Here is the result:

{"verdicts": [{"index": 0, "safe": true, "score": 0.0, "reason": "ok"}]}`
	exp := cases.Structural{
		Schema: `{"type":"object","required":["verdicts"],"properties":{
			"verdicts":{"type":"array","items":{"type":"object",
			"required":["index","safe","score","reason"]}}}}`,
	}
	r := Structural(text, exp)
	if !r.Pass {
		t.Fatalf("expected pass after extraction; reasons: %v", r.Reasons)
	}
}

// TestStructural_ExtractsNestedJSONWithBracesInStrings — verifies
// the extractor correctly accounts for `{` / `}` inside JSON strings
// (a regex-based extractor would get this wrong).
func TestStructural_ExtractsNestedJSONWithBracesInStrings(t *testing.T) {
	text := `Output below.\n\n{"note": "value with {braces} inside the string", "n": 1}`
	exp := cases.Structural{
		Schema: `{"type":"object","required":["note","n"],"properties":{
			"note":{"type":"string"},"n":{"type":"integer"}}}`,
	}
	r := Structural(text, exp)
	if !r.Pass {
		t.Fatalf("expected pass; reasons: %v", r.Reasons)
	}
}

// TestStructural_BareProseIsStillRejected — when the response has
// no JSON at all, structural should still fail with a clear message.
func TestStructural_BareProseIsStillRejected(t *testing.T) {
	text := "The model decided not to produce a JSON output."
	exp := cases.Structural{
		Schema: `{"type":"object","required":["x"]}`,
	}
	r := Structural(text, exp)
	if r.Pass {
		t.Fatal("expected fail when no JSON block present")
	}
	if !contains(r.Reasons, "no JSON object or array found") {
		t.Errorf("expected diagnostic about missing JSON; got %v", r.Reasons)
	}
}

// TestStructural_MustNotMatchStillCatchesPreJSONNarration — cases
// that need bare-JSON output (e.g., production injection-judge's
// downstream parser) can use must_not_match to fail on prose-before-
// JSON even though the schema now extracts and passes.
func TestStructural_MustNotMatchStillCatchesPreJSONNarration(t *testing.T) {
	text := "I have called the tool successfully:\n{\"x\": 1}"
	exp := cases.Structural{
		MustNotMatch: `(?im)^I have called|^Here is`,
		Schema:       `{"type":"object","required":["x"]}`,
	}
	r := Structural(text, exp)
	if r.Pass {
		t.Fatal("expected must_not_match to flag the 'I have called' preamble")
	}
}

// TestStructural_ExtractsLargestNotFirstJSON — Sweep #6 mid-04
// failure pattern: model emits a tool-response JSON snippet inline
// in prose ("getResearch returned {...}") BEFORE its actual answer
// block in a fenced final output. Old "first balanced block"
// extractor caught the inline snippet, schema rejected on missing
// fields. New extractor picks the largest balanced block (the
// actual answer).
func TestStructural_ExtractsLargestNotFirstJSON(t *testing.T) {
	text := `I'll run both tasks. Here are the findings:

- Task B: getResearch returned {"exists": false, "profiles": {}} — no stored research.

` + "```json\n" + `{
  "go_version": "1.25",
  "research_exists": false,
  "tools_used": ["brave_web_search", "getResearch"]
}` + "\n```"
	exp := cases.Structural{
		Schema: `{"type":"object","required":["go_version","research_exists","tools_used"],"properties":{
			"go_version":{"type":"string"},
			"research_exists":{"type":"boolean"},
			"tools_used":{"type":"array"}}}`,
	}
	r := Structural(text, exp)
	if !r.Pass {
		t.Fatalf("expected the LARGEST JSON block (final answer) to be extracted, not the inline tool-response snippet; reasons: %v", r.Reasons)
	}
}
