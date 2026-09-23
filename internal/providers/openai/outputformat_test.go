package openai

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func openaiBody(t *testing.T, req providers.Request) map[string]any {
	t.Helper()
	b, err := buildRequestBody(req)
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// The Chat Completions shape is NESTED (response_format.json_schema.{name,
// schema,strict}), and strict is claimed only when the schema is eligible for
// it: every object closed AND every property required. An optional property
// makes it non-strict rather than silently mandatory.
func TestOutputFormat_NestedJSONSchemaStrictOnlyWhenEligible(t *testing.T) {
	for _, tc := range []struct {
		name, schema string
		strict       bool
	}{
		{"all required", `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`, true},
		{"optional field", `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a"]}`, false},
		{"open object", `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":true}`, false},
		{"nested optional", `{"type":"object","properties":{"o":{"type":"object","properties":{"x":{"type":"string"}}}},"required":["o"]}`, false},
	} {
		req := providers.Request{Model: "gpt-5", Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
			OutputFormat: &providers.OutputFormat{Name: "answer", Schema: json.RawMessage(tc.schema)}}
		rf, _ := openaiBody(t, req)["response_format"].(map[string]any)
		if rf["type"] != "json_schema" {
			t.Fatalf("%s: response_format = %v", tc.name, rf)
		}
		js := rf["json_schema"].(map[string]any)
		if js["name"] != "answer" || js["strict"] != tc.strict {
			t.Errorf("%s: json_schema = %v, want name answer strict %v", tc.name, js, tc.strict)
		}
		if tc.strict && js["schema"].(map[string]any)["additionalProperties"] != false {
			t.Errorf("%s: a strict schema must be closed", tc.name)
		}
	}
	if _, ok := openaiBody(t, providers.Request{Model: "gpt-5"})["response_format"]; ok {
		t.Error("response_format sent without a format")
	}
}

func TestEnforcesStructuredOutput_RefusesTheModelsThatPredateIt(t *testing.T) {
	d := &Driver{}
	for model, want := range map[string]bool{
		"gpt-5": true, "gpt-4o": true, "gpt-4o-mini": true, "gpt-4.1": true, "o3": true, "some-local-model": true,
		"gpt-3.5-turbo": false, "gpt-4": false, "gpt-4-turbo": false, "gpt-4o-2024-05-13": false, "o1-mini": false,
	} {
		if got := d.EnforcesStructuredOutput(model, true); got != want {
			t.Errorf("%s: EnforcesStructuredOutput = %v, want %v", model, got, want)
		}
	}
}
