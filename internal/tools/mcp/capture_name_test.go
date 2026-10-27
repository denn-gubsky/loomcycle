package mcp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// runWithArgs is what the model sees when the tool is called with args and
// answers with content, rendered under pol.
func runWithArgs(t *testing.T, ctx context.Context, pol mcp.ResultPolicy, args string, content ...map[string]any) string {
	t.Helper()
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) { return contentCaller{content: content}, nil }, nil, nil)
	pool.SetResultPolicy(func(_, _ string) mcp.ResultPolicy { return pol })
	t.Cleanup(pool.Close)
	res, err := firstTool(t, ctx, pool, "ms365").Execute(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res.Text
}

// A download tool's reply is often the bytes alone, base64 in a text block,
// with no name beside them: the file used to be named after the tool
// ("download-bytes.pdf"). The call itself named the file, so that name is
// taken before the tool's.
func TestMCPCapture_NameFromCallArguments(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pdfOf(6000))
	for _, tc := range []struct {
		name, args, want string
	}{
		{"fileName", `{"driveId":"d1","fileName":"Протокол 7.pdf"}`, "documents/Protokol-7.pdf"},
		{"filename", `{"filename":"Board minutes.pdf"}`, "documents/Board-minutes.pdf"},
		{"file_name", `{"file_name":"minutes"}`, "documents/minutes.pdf"},
		{"name", `{"name":"report.pdf"}`, "documents/report.pdf"},
		// A path's last segment, as any name is taken.
		{"path", `{"path":"/sites/board/Shared Documents/2026/Minutes 3.pdf"}`, "documents/Minutes-3.pdf"},
		// fileName ranks above a path that names a folder.
		{"ranked", `{"path":"/sites/board/2026","fileName":"agenda.pdf"}`, "documents/agenda.pdf"},
		// Not a string, or no name key at all: the tool's name, as before.
		{"not a string", `{"fileName":{"x":1}}`, "documents/get.pdf"},
		{"no name", `{"itemId":"0123"}`, "documents/get.pdf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := inboxtest.New(t, "root-1", inbox.Service{})
			got := runWithArgs(t, r.Ctx, capturePolicy, tc.args, map[string]any{"type": "text", "text": b64})
			if want := "[file: volume:inbox:" + tc.want + " · application/pdf"; !strings.HasPrefix(got, want) {
				t.Errorf("model text = %q, want prefix %q", short(got), want)
			}
		})
	}
}

// An image block carries no name either; it takes the call's.
func TestMCPCapture_ImageBlockNamedFromCallArguments(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	got := runWithArgs(t, r.Ctx, mcp.ResultPolicy{}, `{"fileName":"scan 1.png"}`,
		map[string]any{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(pngBytes)})
	if !strings.HasPrefix(got, "[file: volume:inbox:images/scan-1.png · image/png") {
		t.Errorf("model text = %q", got)
	}
}

// A name the result gives wins over the call's: the result describes the
// bytes it carries.
func TestMCPCapture_ResultNameBeatsCallArgument(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	pre, suf := graphAttachment()
	got := runWithArgs(t, r.Ctx, capturePolicy, `{"fileName":"other.pdf"}`,
		map[string]any{"type": "text", "text": pre + base64.StdEncoding.EncodeToString(pdfOf(6000)) + suf})
	if !strings.Contains(got, "volume:inbox:documents/Board-papers.pdf") {
		t.Errorf("model text = %q", short(got))
	}
}
