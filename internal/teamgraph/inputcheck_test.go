package teamgraph

import (
	"encoding/json"
	"strings"
	"testing"
)

// formTeam is a team whose entry is an input state carrying schema.
func formTeam(schema string) Definition {
	return Definition{
		Entry: "form",
		States: []State{
			{ID: "form", Handler: Handler{Kind: HandlerInput, Schema: json.RawMessage(schema)}},
			{ID: "done", Handler: Handler{Kind: HandlerTerminal}},
		},
		Transitions: []Transition{{From: "form", To: "done", On: OnSuccess}},
	}
}

const pcpartsForm = `{"type":"object","required":["document_id","chunk_id"],
 "properties":{
   "document_id":{"type":"string","title":"Document","x-loomcycle-picker":{"kind":"document","scope":"user"}},
   "chunk_id":{"type":"string","title":"Part","x-loomcycle-picker":{"kind":"chunk","document":"document_id","depth":1}}}}`

func TestCheckInput_AppliesTheThreeRules(t *testing.T) {
	for _, tc := range []struct {
		name, schema, input string
		want                string // "" = accepted; else a substring of the refusal
	}{
		{"valid form", pcpartsForm, `{"document_id":"d","chunk_id":"c"}`, ""},
		{"required field missing", pcpartsForm, `{"document_id":"d"}`, `input field "chunk_id" is required`},
		{"property mistyped", pcpartsForm, `{"document_id":7,"chunk_id":"c"}`, `input field "document_id" must be a string, got a number`},
		{"top-level type", pcpartsForm, `["d","c"]`, `input must be a JSON object, got a JSON array`},
		{"integer accepts a whole number", `{"type":"integer"}`, `3`, ""},
		{"integer accepts 1.5e1", `{"type":"integer"}`, `1.5e1`, ""},
		{"integer refuses a fraction", `{"type":"integer"}`, `3.5`, `input must be an integer, got a number`},
		{"number accepts a fraction", `{"type":"number"}`, `3.5`, ""},
		{"list of types: either", `{"type":["string","null"]}`, `null`, ""},
		{"list of types: neither", `{"type":["string","null"]}`, `true`, `input must be a string or null, got a boolean`},
		{"property integer refuses a fraction", `{"properties":{"n":{"type":"integer"}}}`, `{"n":1.5}`, `input field "n" must be an integer`},
		{"property integer accepts 1.0", `{"properties":{"n":{"type":"integer"}}}`, `{"n":1.0}`, ""},
		{"absent optional property ignored", `{"properties":{"n":{"type":"integer"}}}`, `{}`, ""},
		{"text input is the {text} object", `{"type":"object","required":["text"]}`, `a quiet gaming pc`, ""},
		{"text input against type string refused", `{"type":"string"}`, `a quiet gaming pc`, `input must be a string, got a JSON object — input that is not JSON is read as the object {"text": "<input>"}`},
		{"empty input against required", pcpartsForm, ``, `input field "document_id" is required`},
		{"schema not an object", `["type","object"]`, `[1]`, ""},
		{"schema a bare string", `"object"`, `[1]`, ""},
		{"nested schema not checked", `{"properties":{"part":{"type":"object","required":["sku"],"properties":{"sku":{"type":"string"}}}}}`, `{"part":{"sku":5}}`, ""},
		{"x- keywords, enum and format ignored", `{"properties":{"k":{"enum":["a"],"format":"uuid","x-loomcycle-picker":{"kind":"document"}}}}`, `{"k":"zzz"}`, ""},
		{"unknown type name not checked", `{"type":"objekt"}`, `[1]`, ""},
		{"malformed required ignored", `{"required":"chunk_id"}`, `{}`, ""},
		{"required on a non-object value not applied", `{"required":["a"]}`, `"s"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckInput(formTeam(tc.schema), tc.input)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("CheckInput(%s) = %v, want accepted", tc.input, err)
			case tc.want != "" && err == nil:
				t.Errorf("CheckInput(%s) accepted, want %q", tc.input, tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Errorf("CheckInput(%s) = %q, want it to contain %q", tc.input, err, tc.want)
			}
		})
	}
}

func TestIsWholeNumber_JudgesTheLiteral(t *testing.T) {
	for lit, want := range map[string]bool{
		"0": true, "-0": true, "12": true, "1.0": true, "1.50e1": true, "100e-2": true, "1e400": true,
		"0e-9": true, "1E+2": true, "123456789012345678901234567890": true,
		"1.5": false, "1e-1": false, "-0.001": false, "1e-99999999999": false,
		"12345678901234567890123.5": false,
	} {
		if got := isWholeNumber(lit); got != want {
			t.Errorf("isWholeNumber(%s) = %v, want %v", lit, got, want)
		}
	}
}

// The form is checked only where it is the walk's front door: an input-sourced
// Starter at the entry is checked like an input state, and a schema anywhere
// but the entry describes nothing the caller sends.
func TestCheckInput_ChecksOnlyTheEntryForm(t *testing.T) {
	starter := okInputStarter()
	starter.Schema = json.RawMessage(pcpartsForm)
	d := Definition{
		Entry:       "research",
		States:      []State{{ID: "research", Handler: starter}, {ID: "done", Handler: Handler{Kind: HandlerTerminal}}},
		Transitions: []Transition{{From: "research", To: "done", On: OnSuccess}},
	}
	if err := CheckInput(d, `{"document_id":"d"}`); err == nil || !strings.Contains(err.Error(), `"chunk_id" is required`) {
		t.Errorf("input-sourced entry starter: err = %v, want chunk_id required", err)
	}

	later := Definition{
		Entry: "go",
		States: []State{
			{ID: "go", Handler: Handler{Kind: HandlerAgent, Agent: "a"}},
			{ID: "form", Handler: Handler{Kind: HandlerInput, Schema: json.RawMessage(pcpartsForm)}},
			{ID: "done", Handler: Handler{Kind: HandlerTerminal}},
		},
		Transitions: []Transition{{From: "go", To: "form", On: OnSuccess}, {From: "form", To: "done", On: OnSuccess}},
	}
	if err := CheckInput(later, `anything`); err != nil {
		t.Errorf("a form on a non-entry state checked the walk's input: %v", err)
	}
	if err := CheckInput(formTeam(""), `anything`); err != nil {
		t.Errorf("an entry with no schema checked the input: %v", err)
	}
}
