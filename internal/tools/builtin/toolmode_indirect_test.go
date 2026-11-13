package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// tool_modes reached indirectly: a read mode on Document or Path is held by
// the tool itself, so another tool calling it in-process (File import, Crypto
// writing a chunk) or a direct call that bypasses the run's dispatcher cannot
// write through it.

func readMode(ctx context.Context, tool string) context.Context {
	return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.ToolModes = map[string]string{tool: "read"}
	})
}

func documentCount(t *testing.T, d *Document, ctx context.Context) int {
	t.Helper()
	out, res := docExec(t, d, ctx, `{"op":"query_documents","scope":"user"}`)
	if res.IsError {
		t.Fatalf("query_documents: %s", res.Text)
	}
	docs, _ := out["documents"].([]any)
	return len(docs)
}

func TestToolModes_DocumentRefusesAWriteCalledDirectlyInReadMode(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	rctx := readMode(ctx, "Document")
	if _, res := docExec(t, d, rctx, `{"op":"create_document","scope":"user","title":"x"}`); !res.IsError || !strings.Contains(res.Text, "read mode") {
		t.Fatalf("create_document in read mode = %q, want refused", res.Text)
	}
	if n := documentCount(t, d, rctx); n != 0 {
		t.Fatalf("the store holds %d documents, want 0", n)
	}
	// The same call with the tool whole writes, so the count can see a write.
	if _, res := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"x"}`); res.IsError {
		t.Fatalf("create_document whole: %s", res.Text)
	}
	if n := documentCount(t, d, ctx); n != 1 {
		t.Fatalf("the store holds %d documents, want 1", n)
	}
}

func TestToolModes_PathRefusesAWriteCalledDirectlyInReadMode(t *testing.T) {
	p, ctx, _ := pathFixture(t)
	res, err := p.Execute(readMode(ctx, "Path"), json.RawMessage(`{"op":"mkdir","scope":"agent","path":"/notes"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Text, "read mode") {
		t.Fatalf("mkdir in read mode = %q (%v), want refused", res.Text, err)
	}
	if res, _ := p.Execute(ctx, json.RawMessage(`{"op":"stat","scope":"agent","path":"/notes"}`)); !res.IsError {
		t.Fatal("/notes exists after a refused mkdir")
	}
}

// File op=import writes a document through the Document tool in-process: in
// read mode it is refused and nothing is written.
func TestToolModes_FileImportIsRefusedForAReadModeDocument(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	rctx := readMode(ctx, "Document")
	kept := inbox.Capture(rctx, inbox.Item{Name: "notes.md", MediaType: "text/markdown", Origin: "mcp:x/get"}, []byte("# Notes\n\nhello\n"))
	_, res := fileOp(t, f, rctx, map[string]any{"op": "import", "path": kept.Record.Ref})
	if !res.IsError || !strings.Contains(res.Text, "read mode") {
		t.Fatalf("File import in read mode = %q, want refused naming the read mode", res.Text)
	}
	if n := documentCount(t, d, ctx); n != 0 {
		t.Fatalf("the store holds %d documents after a refused import, want 0", n)
	}
}

// Crypto writing its output to a chunk goes through the Document tool: in
// read mode the chunk is left as it was.
func TestToolModes_CryptoDecryptToADocumentIsRefusedInReadMode(t *testing.T) {
	d, dctx, _ := documentFixture(t)
	docID, rootID := canvasCreateDoc(t, d, dctx, "vault")
	chunkID := canvasCreateChunk(t, d, dctx, docID, rootID, "card", "4111 1111 1111 1111")
	c := &Crypto{Docs: d}
	ctx := tools.WithRunIdentity(dctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt",
		UserCredentials: map[string]string{"doc-key": "0123456789abcdef0123456789abcdef-a-secret"}})
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Crypto = tools.CryptoPolicyValue{Scopes: []string{"run"}} })
	ctx = refCtx(ctx, t.TempDir(), "Document")
	ref := "document:user/" + chunkID
	_, res := cryptoExec(t, c, ctx, `{"op":"encrypt","run":"doc-key","key_type":"aes-256-gcm","derive":true,"from":"`+ref+`","to":"`+ref+`"}`)
	mustOK(t, "encrypt chunk in place (whole)", res)
	before, _ := docExec(t, d, dctx, `{"op":"get_chunk","scope":"user","id":"`+chunkID+`"}`)

	_, res = cryptoExec(t, c, readMode(ctx, "Document"), `{"op":"decrypt","run":"doc-key","key_type":"aes-256-gcm","derive":true,"from":"`+ref+`","to":"`+ref+`"}`)
	if !res.IsError || !strings.Contains(res.Text, "read mode") {
		t.Fatalf("decrypt to a chunk in read mode = %q, want refused naming the read mode", res.Text)
	}
	after, _ := docExec(t, d, dctx, `{"op":"get_chunk","scope":"user","id":"`+chunkID+`"}`)
	if after["body"] != before["body"] || after["revision"] != before["revision"] {
		t.Fatalf("chunk changed: %v -> %v", before["body"], after["body"])
	}
}

// A board-bound team walk writes the chunk's status: a run holding Document
// in read mode is refused before the walk starts.
func TestToolModes_TeamBoardWalkIsRefusedForAReadModeDocument(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	createTeam(t, tool, ctx, "board-walk", linearBoardTeam)
	tool.Spawn = textSpawn(func(_ context.Context, agent string, _ teamrun.Prompt, _ string) (string, error) {
		return agent + "!", nil
	})
	rec := &walkRunRecorder{}
	tool.WalkRun = rec.open
	board := &fakeBoard{exists: true, status: "a"}
	tool.Board = board
	res, _ := tool.Execute(readMode(ctx, "Document"), json.RawMessage(`{"op":"run","name":"board-walk","input":"x","board_chunk_id":"chunk-1"}`))
	if !res.IsError || !strings.Contains(res.Text, "read mode") {
		t.Fatalf("board walk in read mode = %q, want refused", res.Text)
	}
	if len(board.setCalls) != 0 {
		t.Fatalf("the board was written: %v", board.setCalls)
	}
}
