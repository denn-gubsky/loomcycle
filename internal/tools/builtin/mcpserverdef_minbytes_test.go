package builtin

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/mcp"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

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
