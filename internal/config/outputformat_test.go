package config

import (
	"strings"
	"testing"
)

func verdictSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"verdict": map[string]any{"type": "string"}},
		"required": []any{"verdict"}, "additionalProperties": false}
}

// An output_format no provider could apply is refused where it is written.
func TestOutputFormatValidate(t *testing.T) {
	for _, tc := range []struct {
		of      OutputFormat
		wantErr string
	}{
		{OutputFormat{Type: "json_schema", Schema: verdictSchema()}, ""},
		{OutputFormat{Type: "json_schema", Name: "review_verdict", Schema: verdictSchema()}, ""},
		{OutputFormat{Type: "json_object", Schema: verdictSchema()}, "not supported"},
		{OutputFormat{Type: "json_schema"}, "schema is required"},
		{OutputFormat{Schema: verdictSchema()}, ""}, // type defaults to json_schema
		{OutputFormat{Type: "json_schema", Schema: map[string]any{"type": "array"}}, `type "object" at its root`},
		{OutputFormat{Type: "json_schema", Name: "has space", Schema: verdictSchema()}, "must match"},
	} {
		err := tc.of.Validate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%+v: unexpected error %v", tc.of, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%+v: error %v, want one containing %q", tc.of, err, tc.wantErr)
		}
	}
	if (&OutputFormat{}).EffectiveName() != "output" {
		t.Error("an unnamed schema must default to \"output\"")
	}
}

// A per-run schema REPLACES the agent's whole, and the merge aliases nothing —
// the schema is a nested tree, so a shallow copy would share it.
func TestMergeOutputFormat_ReplacesWholeAndDeepCopies(t *testing.T) {
	agent := &OutputFormat{Type: "json_schema", Name: "a", Schema: verdictSchema()}
	run := &OutputFormat{Type: "json_schema", Schema: map[string]any{"type": "object"}}
	if got := MergeOutputFormat(agent, run); got.Name != "" || len(got.Schema) != 1 {
		t.Errorf("merge = %+v, want the run's format exactly", got)
	}
	kept := MergeOutputFormat(agent, nil)
	kept.Schema["properties"].(map[string]any)["verdict"].(map[string]any)["type"] = "number"
	if agent.Schema["properties"].(map[string]any)["verdict"].(map[string]any)["type"] != "string" {
		t.Error("the merge aliased the agent's schema")
	}
}

func TestLoad_ValidatesAgentOutputFormat(t *testing.T) {
	cfg, err := loadYAMLString(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  judge:
    model: claude-sonnet-4-6
    output_format:
      type: json_schema
      schema: { type: object, properties: { verdict: { type: string } }, required: [verdict], additionalProperties: false }
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if of := cfg.Agents["judge"].OutputFormat; of == nil || of.Schema["type"] != "object" {
		t.Errorf("output_format = %+v", of)
	}
	if _, err := loadYAMLString(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  bad:
    model: claude-sonnet-4-6
    output_format: { type: json_schema, schema: { type: string } }
`); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Errorf("a non-object root schema loaded: %v", err)
	}
}
