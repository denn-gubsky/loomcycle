package mcp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// contentCaller is an in-process MCP server whose one tool returns content.
type contentCaller struct{ content []map[string]any }

func (c contentCaller) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	switch method {
	case "initialize":
		return json.Marshal(map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "files"}})
	case "tools/list":
		return json.Marshal(map[string]any{"tools": []map[string]any{{"name": "get", "inputSchema": map[string]any{"type": "object"}}}})
	}
	return json.Marshal(map[string]any{"content": c.content})
}
func (contentCaller) Notify(context.Context, string, any) error { return nil }
func (contentCaller) Healthy() bool                             { return true }

func runContent(t *testing.T, ctx context.Context, content ...map[string]any) string {
	t.Helper()
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) { return contentCaller{content: content}, nil }, nil, nil)
	t.Cleanup(pool.Close)
	res, err := firstTool(t, ctx, pool, "ms365").Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res.Text
}

var pngBytes = []byte("\x89PNG\r\n\x1a\n-not-really-an-image-")

// Before RFC FF an image-only result reached the model as "": every block
// that was not text was dropped without a word.
func TestMCPResult_ImageOnlyResultIsNotEmpty(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	got := runContent(t, r.Ctx, map[string]any{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(pngBytes)})
	if !strings.HasPrefix(got, "[file: volume:inbox:/images/get.png · image/png") || !strings.Contains(got, "from mcp:ms365/get") {
		t.Fatalf("model text = %q", got)
	}
	ref, _ := r.Set.Get(inbox.Name)
	kept, err := os.ReadFile(filepath.Join(ref.Root, "images", "get.png"))
	if err != nil || string(kept) != string(pngBytes) {
		t.Errorf("kept = %q, %v", kept, err)
	}
	if evs := r.Events(); len(evs) != 1 || evs[0].Type != providers.EventFileCaptured {
		t.Errorf("events = %+v", evs)
	}
}

func TestMCPResult_BlobBlockLandsInInboxAsStub(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	blob := base64.StdEncoding.EncodeToString([]byte("%PDF-1.7 board papers"))
	got := runContent(t, r.Ctx,
		map[string]any{"type": "text", "text": "Downloaded:"},
		map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///drive/Board%20papers.pdf", "mimeType": "application/pdf", "blob": blob}},
	)
	want := "Downloaded:\n[file: volume:inbox:/documents/Board-papers.pdf · application/pdf"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("model text = %q, want prefix %q", got, want)
	}
	if strings.Contains(got, blob) {
		t.Error("the base64 reached the model")
	}
}

func TestMCPResult_ResourceTextIsKept(t *testing.T) {
	got := runContent(t, context.Background(),
		map[string]any{"type": "resource", "resource": map[string]any{"uri": "note://1", "mimeType": "text/plain", "text": "the minutes"}},
	)
	if got != "[resource: note://1]\nthe minutes" {
		t.Errorf("model text = %q", got)
	}
}

func TestMCPResult_ResourceLinkIsShown(t *testing.T) {
	got := runContent(t, context.Background(),
		map[string]any{"type": "resource_link", "uri": "https://graph.example/items/1", "name": "Minutes.docx", "mimeType": "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	)
	if !strings.HasPrefix(got, "[link: Minutes.docx · https://graph.example/items/1 · application/vnd") {
		t.Errorf("model text = %q", got)
	}
}

// Without an inbox the file is still not handed to the model as bytes.
func TestMCPResult_NoInboxGivesRefusalStub(t *testing.T) {
	got := runContent(t, context.Background(), map[string]any{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(pngBytes)})
	if !strings.HasPrefix(got, "[file not kept:") {
		t.Errorf("model text = %q", got)
	}
}

func TestMCPResult_TextOnlyIsUnchanged(t *testing.T) {
	got := runContent(t, context.Background(),
		map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "text", "text": ""}, map[string]any{"type": "text", "text": "b"},
	)
	if want := "a\n\nb"; got != want || got != mcp.JoinTextContent(&mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "a"}, {Type: "text"}, {Type: "text", Text: "b"}}}) {
		t.Errorf("model text = %q", got)
	}
}
