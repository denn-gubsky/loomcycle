package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// textCaller is an MCP server with one tool whose result is one text block.
type textCaller struct{ text string }

func (c textCaller) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	switch method {
	case "initialize":
		return json.Marshal(map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "files"}})
	case "tools/list":
		return json.Marshal(map[string]any{"tools": []map[string]any{{"name": "download", "inputSchema": map[string]any{"type": "object"}}}})
	}
	return json.Marshal(map[string]any{"content": []map[string]any{{"type": "text", "text": c.text}}})
}
func (textCaller) Notify(context.Context, string, any) error { return nil }
func (textCaller) Healthy() bool                             { return true }

// modelTextFrom returns what the model is handed for text from server, with
// the pool rendering through mcpResultPolicy as main wires it.
func modelTextFrom(t *testing.T, ctx context.Context, cfg *config.Config, reg *mcp.DynamicRegistry, server, text string) string {
	t.Helper()
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) { return textCaller{text}, nil }, nil, nil)
	pool.SetResultPolicy(mcpResultPolicy(cfg, mcpLookupView{reg}))
	t.Cleanup(pool.Close)
	_, descs, err := pool.Get(ctx, server)
	if err != nil || len(descs) == 0 {
		t.Fatalf("Get(%q): %v (%d tools)", server, err, len(descs))
	}
	res, err := mcp.NewTool(pool, server, descs[0]).Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res.Text
}

// smallTextFile is a ~1 KB Markdown file as a server that returns small files
// as base64 inside JSON sends it.
func smallTextFile() (block, b64 string) {
	md := strings.Repeat("- Received from the intake mailbox; filed for the next meeting.\n", 16)
	b64 = base64.StdEncoding.EncodeToString([]byte("# Intake record\n\n" + md))
	return `{"name":"intake.md","contentBytes":"` + b64 + `"}`, b64
}

// opaqueID is an id-like base64 string of n characters that decodes to
// neither a recognisable file nor readable text.
func opaqueID(n int) string {
	var raw []byte
	seed := sha256.Sum256([]byte("id"))
	for len(raw) < n {
		seed = sha256.Sum256(seed[:])
		raw = append(raw, seed[:]...)
	}
	raw[0] = 0xff
	return base64.RawURLEncoding.EncodeToString(raw)[:n]
}

// binary_results_min_bytes is read for the server that answered, from the
// yaml (stdio and http) and from an MCPServerDef alike: a small text file is
// kept where the server lowered the minimum and left as text where it did not.
func TestMCPResultPolicy_MinBytesKeepsASmallFileOnlyWhereTheServerSetsIt(t *testing.T) {
	cfg := &config.Config{MCPServers: map[string]config.MCPServer{
		"yaml-stdio": {Transport: "stdio", Command: "x", BinaryResults: "capture", BinaryResultsMinBytes: 256},
		"yaml-http":  {Transport: "http", URL: "https://a.example/mcp", BinaryResults: "capture", BinaryResultsMinBytes: 256},
		"yaml-dflt":  {Transport: "stdio", Command: "x", BinaryResults: "capture"},
	}}
	reg := mcp.NewDynamicRegistry()
	reg.Set(mcp.DynamicMCPServerSpec{Name: "def", Transport: "http", URL: "https://b.example/mcp", BinaryResults: "capture", BinaryResultsMinBytes: 256})
	reg.Set(mcp.DynamicMCPServerSpec{Name: "def-dflt", Transport: "http", URL: "https://c.example/mcp", BinaryResults: "capture"})

	block, b64 := smallTextFile()
	for _, server := range []string{"yaml-stdio", "yaml-http", "def"} {
		r := inboxtest.New(t, "root-"+server, inbox.Service{})
		got := modelTextFrom(t, r.Ctx, cfg, reg, server, block)
		if strings.Contains(got, b64) || !strings.Contains(got, "[file: volume:inbox:documents/intake.md") {
			t.Errorf("%s (min 256): the small file was not kept as an inbox reference: %.200q", server, got)
		}
	}
	for _, server := range []string{"yaml-dflt", "def-dflt"} {
		r := inboxtest.New(t, "root-"+server, inbox.Service{})
		if got := modelTextFrom(t, r.Ctx, cfg, reg, server, block); got != block {
			t.Errorf("%s (default min): the small file was changed: %.200q", server, got)
		}
		if evs := r.Events(); len(evs) != 0 {
			t.Errorf("%s (default min): a file was kept: %+v", server, evs)
		}
	}
	if got := mcpResultPolicy(cfg, mcpLookupView{reg})("", "def").Base64MinBytes; got != 256 {
		t.Errorf("def policy min = %d, want 256 (the yaml server's)", got)
	}
}

// An id-like string under a lowered minimum is never a file: one shorter than
// the minimum is not a candidate, and one longer decodes to nothing readable.
func TestMCPResultPolicy_MinBytesLeavesOpaqueIDs(t *testing.T) {
	cfg := &config.Config{MCPServers: map[string]config.MCPServer{
		"files": {Transport: "stdio", Command: "x", BinaryResults: "capture", BinaryResultsMinBytes: 256},
	}}
	for _, n := range []int{200, 400} {
		r := inboxtest.New(t, "root-1", inbox.Service{})
		block := `{"value":[{"id":"` + opaqueID(n) + `","name":"Inbox"}]}`
		if got := modelTextFrom(t, r.Ctx, cfg, nil, "files", block); got != block {
			t.Errorf("a %d-character id was changed: %.200q", n, got)
		}
		if evs := r.Events(); len(evs) != 0 {
			t.Errorf("a %d-character id was kept as a file: %+v", n, evs)
		}
	}
}
