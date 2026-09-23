package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

const verdictSchemaJSON = `{"type":"object","properties":{"verdict":{"type":"string"}},"required":["verdict"],"additionalProperties":false}`

// RFC DI: an AgentDef's output_format survives create → persist → lookup, is
// content-identifying, and an invalid one is refused on the substrate path.
func TestAgentDefTool_OutputFormatRoundTripsAndHashes(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"judge-of","overlay":{"system_prompt":"judge"}}`))
	parent, _ := decodeResult(t, res.Text)["content_sha256"].(string)

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"judge-of","promote":true,"overlay":{`+
		`"output_format":{"type":"json_schema","name":"verdict","schema":`+verdictSchemaJSON+`}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	fork, _ := decodeResult(t, res.Text)["content_sha256"].(string)
	if parent == "" || parent == fork {
		t.Errorf("an output_format-only fork kept content_sha256 %q", parent)
	}
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", "judge-of")
	if !ok || def.OutputFormat == nil || def.OutputFormat.Name != "verdict" || def.OutputFormat.Schema["type"] != "object" {
		t.Errorf("output_format after fork = %+v", def.OutputFormat)
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad-of","overlay":{"system_prompt":"x",`+
		`"output_format":{"type":"json_schema","schema":{"type":"string"}}}}`))
	if !res.IsError || !strings.Contains(res.Text, "root") {
		t.Errorf("a non-object schema was accepted: %q", res.Text)
	}
}

func TestStaticToMergedDef_PreservesOutputFormat(t *testing.T) {
	md := staticToMergedDef(config.AgentDef{OutputFormat: &config.OutputFormat{Type: "json_schema", Schema: map[string]any{"type": "object"}}})
	if md.OutputFormat == nil || md.OutputFormat.Schema["type"] != "object" {
		t.Errorf("staticToMergedDef dropped output_format: %+v", md.OutputFormat)
	}
}
