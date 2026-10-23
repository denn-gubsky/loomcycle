package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

const brURL = `"transport":"http","url":"https://n8n.example.com/mcp"`

// A binary_results value the runtime does not know is refused at create and
// fork, and by the restore validator, so no stored def carries one.
func TestMCPServerDefTool_BinaryResultsUnknownValueRefused(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	res := execDef(t, tool, ctx, `{"op":"create","name":"ms365","discover":false,"overlay":{`+brURL+`,"binary_results":"decode"}}`)
	if !res.IsError || !strings.Contains(res.Text, "binary_results") {
		t.Fatalf("create with an unknown binary_results: %s", res.Text)
	}
	if _, ok := tool.Registry.Get("", "ms365"); ok {
		t.Error("a refused create reached the registry")
	}

	createDefFor(t, tool, ctx, "ms365", `{`+brURL+`}`)
	res = execDef(t, tool, ctx, `{"op":"fork","name":"ms365","promote":true,"discover":false,"overlay":{"binary_results":"Capture"}}`)
	if !res.IsError || !strings.Contains(res.Text, "binary_results") {
		t.Errorf("fork with an unknown binary_results: %s", res.Text)
	}

	validate := MCPServerDefBodyValidator(tool.Cfg)
	if err := validate(json.RawMessage(`{` + brURL + `,"binary_results":"decode"}`)); err == nil || !strings.Contains(err.Error(), "binary_results") {
		t.Errorf("restore validator: err = %v, want a binary_results refusal", err)
	}
	if err := validate(json.RawMessage(`{` + brURL + `,"binary_results":"capture"}`)); err != nil {
		t.Errorf("restore validator refused capture: %v", err)
	}
}

// binary_results set at create reaches every place the def is read from: the
// stored body (and its read shape), the registry the pool resolves through,
// a fresh boot's rehydrated registry, and a fork that does not restate it.
func TestMCPServerDefTool_BinaryResultsRoundTrips(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	createDefFor(t, tool, ctx, "ms365", `{`+brURL+`,"binary_results":"capture"}`)
	if got := storedOverlay(t, tool, "", "ms365").BinaryResults; got != "capture" {
		t.Errorf("stored = %q", got)
	}
	row, err := tool.Store.MCPServerDefGetActive(context.Background(), "", "ms365")
	if err != nil {
		t.Fatal(err)
	}
	var read lookup.SubstrateMCPServer
	if err := json.Unmarshal(row.Definition, &read); err != nil || read.BinaryResults != "capture" {
		t.Errorf("read shape = %q, %v", read.BinaryResults, err)
	}
	if spec, ok := tool.Registry.Get("", "ms365"); !ok || spec.BinaryResults != "capture" {
		t.Errorf("registry = %+v, %v", spec, ok)
	}
	reg := loommcp.NewDynamicRegistry()
	if _, err := RehydrateMCPRegistry(context.Background(), tool.Store, tool.Cfg.MCPServers, reg, t.Logf); err != nil {
		t.Fatal(err)
	}
	if spec, ok := reg.Get("", "ms365"); !ok || spec.BinaryResults != "capture" {
		t.Errorf("rehydrated = %+v, %v", spec, ok)
	}

	res := execDef(t, tool, ctx, `{"op":"fork","name":"ms365","promote":true,"discover":false,"overlay":{"description":"moved"}}`)
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if got := storedOverlay(t, tool, "", "ms365").BinaryResults; got != "capture" {
		t.Errorf("a fork that does not restate it dropped it: %q", got)
	}
	res = execDef(t, tool, ctx, `{"op":"fork","name":"ms365","promote":true,"discover":false,"overlay":{"binary_results":"passthrough"}}`)
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if spec, _ := tool.Registry.Get("", "ms365"); spec.BinaryResults != "passthrough" {
		t.Errorf("a fork back to passthrough left the registry at %q", spec.BinaryResults)
	}
}

// binary_results changes what the model is handed, so it is part of the
// def's content: re-creating a def with it set is a new version, not a
// deduplicated no-op that would silently keep the old behaviour. Spelling
// out the default is the same content as leaving it out.
func TestMCPServerDefTool_BinaryResultsIsContentIdentifying(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	sha := func(res string) string { return decodeResult(t, res)["content_sha256"].(string) }
	first := execDef(t, tool, ctx, `{"op":"create","name":"ms365","discover":false,"overlay":{`+brURL+`}}`)
	same := execDef(t, tool, ctx, `{"op":"create","name":"ms365","discover":false,"overlay":{`+brURL+`,"binary_results":"passthrough"}}`)
	if decodeResult(t, same.Text)["deduplicated"] != true || sha(same.Text) != sha(first.Text) {
		t.Errorf("an explicit passthrough is not the default's content: %s", same.Text)
	}
	capture := execDef(t, tool, ctx, `{"op":"create","name":"ms365","discover":false,"overlay":{`+brURL+`,"binary_results":"capture"}}`)
	if capture.IsError || decodeResult(t, capture.Text)["deduplicated"] == true || sha(capture.Text) == sha(first.Text) {
		t.Fatalf("turning capture on was deduplicated against the passthrough def: %s", capture.Text)
	}
	if spec, _ := tool.Registry.Get("", "ms365"); spec.BinaryResults != "capture" {
		t.Errorf("registry = %q after the capture create", spec.BinaryResults)
	}
}
