package builtin

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/mcp"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// A minimum under the floor, or one set where capture is off, is refused at
// create, at fork and at restore, naming the key, as the yaml load refuses it.
func TestMCPServerDefTool_BinaryResultsMinBytesRefused(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()
	validate := MCPServerDefBodyValidator(tool.Cfg)

	for _, tc := range []struct{ overlay, says string }{
		{`"binary_results":"capture","binary_results_min_bytes":100`, "128"},
		{`"binary_results":"capture","binary_results_min_bytes":-5`, "128"},
		{`"binary_results_min_bytes":256`, "capture"},
		{`"binary_results":"passthrough","binary_results_min_bytes":256`, "capture"},
	} {
		res := execDef(t, tool, ctx, `{"op":"create","name":"files","discover":false,"overlay":{`+brURL+`,`+tc.overlay+`}}`)
		if !res.IsError || !strings.Contains(res.Text, "binary_results_min_bytes") || !strings.Contains(res.Text, tc.says) {
			t.Errorf("create %s: %s", tc.overlay, res.Text)
		}
		if err := validate(json.RawMessage(`{` + brURL + `,` + tc.overlay + `}`)); err == nil || !strings.Contains(err.Error(), "binary_results_min_bytes") {
			t.Errorf("restore validator %s: err = %v", tc.overlay, err)
		}
	}

	// A fork onto a passthrough def that sets only the minimum does nothing.
	createDefFor(t, tool, ctx, "plain", `{`+brURL+`}`)
	res := execDef(t, tool, ctx, `{"op":"fork","name":"plain","promote":true,"discover":false,"overlay":{"binary_results_min_bytes":256}}`)
	if !res.IsError || !strings.Contains(res.Text, "binary_results_min_bytes") {
		t.Errorf("fork of a passthrough def with only a minimum: %s", res.Text)
	}
}

// The minimum set at create reaches the stored body, the registry the pool's
// result policy reads, a fresh boot's rehydrated registry and a fork that does
// not restate it; a fork back to passthrough drops it with capture.
func TestMCPServerDefTool_BinaryResultsMinBytesRoundTrips(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	createDefFor(t, tool, ctx, "files", `{`+brURL+`,"binary_results":"capture","binary_results_min_bytes":256}`)
	if got := storedOverlay(t, tool, "", "files").BinaryResultsMinBytes; got != 256 {
		t.Errorf("stored = %d", got)
	}
	if spec, ok := tool.Registry.Get("", "files"); !ok || spec.BinaryResultsMinBytes != 256 {
		t.Errorf("registry = %+v, %v", spec, ok)
	}
	reg := loommcp.NewDynamicRegistry()
	if _, err := RehydrateMCPRegistry(context.Background(), tool.Store, tool.Cfg.MCPServers, reg, slog.New(slog.NewTextHandler(t.Output(), nil))); err != nil {
		t.Fatal(err)
	}
	if spec, ok := reg.Get("", "files"); !ok || spec.BinaryResultsMinBytes != 256 {
		t.Errorf("rehydrated = %+v, %v", spec, ok)
	}

	if res := execDef(t, tool, ctx, `{"op":"fork","name":"files","promote":true,"discover":false,"overlay":{"description":"moved"}}`); res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if got := storedOverlay(t, tool, "", "files").BinaryResultsMinBytes; got != 256 {
		t.Errorf("a fork that does not restate it dropped it: %d", got)
	}
	if res := execDef(t, tool, ctx, `{"op":"fork","name":"files","promote":true,"discover":false,"overlay":{"binary_results":"passthrough"}}`); res.IsError {
		t.Fatalf("fork to passthrough: %s", res.Text)
	}
	if spec, _ := tool.Registry.Get("", "files"); spec.BinaryResults != "passthrough" || spec.BinaryResultsMinBytes != 0 {
		t.Errorf("a fork back to passthrough left %+v", spec)
	}
}

// The minimum is content: a new value is a new version. The default spelled
// out is the same content as leaving it out, and the sign package's own copy
// of the default is the runtime's (a drift guard across the two packages).
func TestMCPServerDefTool_BinaryResultsMinBytesIsContentIdentifying(t *testing.T) {
	tool, ctx, cleanup := mcpServerDefFixture(t)
	defer cleanup()

	sha := func(res string) string { return decodeResult(t, res)["content_sha256"].(string) }
	first := execDef(t, tool, ctx, `{"op":"create","name":"files","discover":false,"overlay":{`+brURL+`,"binary_results":"capture"}}`)
	same := execDef(t, tool, ctx, `{"op":"create","name":"files","discover":false,"overlay":{`+brURL+`,"binary_results":"capture","binary_results_min_bytes":`+jsonInt(config.DefaultBinaryResultsMinBytes)+`}}`)
	if decodeResult(t, same.Text)["deduplicated"] != true || sha(same.Text) != sha(first.Text) {
		t.Errorf("the default minimum spelled out is not the default's content: %s", same.Text)
	}
	low := execDef(t, tool, ctx, `{"op":"create","name":"files","discover":false,"overlay":{`+brURL+`,"binary_results":"capture","binary_results_min_bytes":256}}`)
	if low.IsError || decodeResult(t, low.Text)["deduplicated"] == true || sha(low.Text) == sha(first.Text) {
		t.Fatalf("lowering the minimum was deduplicated: %s", low.Text)
	}
	base := mcp.MCPServerContent{Name: "files", Transport: "http", URL: "https://a.example/mcp", BinaryResults: "capture"}
	dflt := base
	dflt.BinaryResultsMinBytes = config.DefaultBinaryResultsMinBytes
	if mcp.Sign(dflt) != mcp.Sign(base) {
		t.Error("mcp.Sign's default minimum is not config.DefaultBinaryResultsMinBytes")
	}
	if loommcp.DefaultBase64MinBytes != config.DefaultBinaryResultsMinBytes {
		t.Error("the capture's default minimum is not the config's")
	}
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
