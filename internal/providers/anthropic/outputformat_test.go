package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// The schema rides in output_config beside effort, rewritten into what the
// grammar accepts: numeric / length bounds and maxItems stripped (a 400
// otherwise), minItems kept only at 0/1, and every open object closed — while
// an object that explicitly allows extras is left for the API to answer.
func TestOutputFormat_RidesInOutputConfigBesideEffortWithTheSchemaMadeAcceptable(t *testing.T) {
	req := baseReq()
	req.Effort = "high"
	req.OutputFormat = &providers.OutputFormat{Name: "answer", Schema: json.RawMessage(`{
		"type":"object","required":["n","tags"],
		"properties":{
			"n":{"type":"integer","minimum":1,"maximum":9},
			"s":{"type":"string","maxLength":5},
			"tags":{"type":"array","minItems":2,"maxItems":4,"items":{"type":"object","properties":{"k":{"type":"string"}}}},
			"one":{"type":"array","minItems":1},
			"free":{"type":"object","additionalProperties":true}
		}}`)}
	oc, _ := bodyOf(t, req)["output_config"].(map[string]any)
	if oc["effort"] != "high" {
		t.Errorf("effort lost beside the format: %v", oc)
	}
	f, _ := oc["format"].(map[string]any)
	if f["type"] != "json_schema" {
		t.Fatalf("output_config.format = %v", oc["format"])
	}
	root := f["schema"].(map[string]any)
	props := root["properties"].(map[string]any)
	if root["additionalProperties"] != false {
		t.Error("the root object was not closed")
	}
	n := props["n"].(map[string]any)
	if _, ok := n["minimum"]; ok {
		t.Error("minimum was sent")
	}
	if _, ok := props["s"].(map[string]any)["maxLength"]; ok {
		t.Error("maxLength was sent")
	}
	tags := props["tags"].(map[string]any)
	if _, ok := tags["minItems"]; ok {
		t.Error("minItems:2 was sent")
	}
	if _, ok := tags["maxItems"]; ok {
		t.Error("maxItems was sent")
	}
	if tags["items"].(map[string]any)["additionalProperties"] != false {
		t.Error("a nested object was not closed")
	}
	if props["one"].(map[string]any)["minItems"] != float64(1) {
		t.Error("minItems:1 is accepted and must be kept")
	}
	if props["free"].(map[string]any)["additionalProperties"] != true {
		t.Error("an explicit additionalProperties:true was overwritten")
	}
	if len(root["required"].([]any)) != 2 {
		t.Error("required changed")
	}
}

// EnforcesStructuredOutput and the wire agree per model: the families that
// predate structured outputs get no format; everything from Opus 4.1 on does.
func TestEnforcesStructuredOutput_MatchesWhatTheRequestBuilderSends(t *testing.T) {
	d := &Driver{}
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"claude-opus-5-5", true},
		{"claude-fable-5-1", true},
		{"claude-sonnet-4-5-20250929", true},
		{"claude-opus-4-1-20250805", true},
		{"claude-haiku-4-5", true},
		{"claude-sonnet-4-20250514", false},
		{"claude-opus-4-0", false},
		{"claude-3-7-sonnet-latest", false},
		{"claude-3-5-haiku-20241022", false},
	} {
		if got := d.EnforcesStructuredOutput(tc.model, true); got != tc.want {
			t.Errorf("%s: EnforcesStructuredOutput = %v, want %v", tc.model, got, tc.want)
		}
		req := baseReq()
		req.Model = tc.model
		req.OutputFormat = &providers.OutputFormat{Name: "a", Schema: json.RawMessage(`{"type":"object"}`)}
		oc, _ := bodyOf(t, req)["output_config"].(map[string]any)
		if _, onWire := oc["format"]; onWire != tc.want {
			t.Errorf("%s: format on the wire = %v, but EnforcesStructuredOutput = %v", tc.model, onWire, tc.want)
		}
	}
}

// No format asked, no format sent — an unopted body stays byte-identical.
func TestOutputFormat_AbsentLeavesTheBodyUnchanged(t *testing.T) {
	if _, ok := bodyOf(t, baseReq())["output_config"]; ok {
		t.Error("output_config sent with neither effort nor a format")
	}
}
