package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestReembed_ChunkKeepsItsIndexTextAndImageDescription is the regression for the
// re-embed path's own text rule. It re-embedded every row as its RAW value, which for a
// document chunk is the JSON envelope: field names were indexed, the image's generated
// description (which lives beside the chunk, not in the body) was dropped, and — now that
// chunks are indexed under a header — the header was lost on every model migration.
//
// A chunk must come out of a re-embed indexed exactly as the write path indexed it, and a
// chunk that derives to nothing (a bodyless root, left over from before headers) must be
// skipped rather than embedded as its envelope.
func TestReembed_ChunkKeepsItsIndexTextAndImageDescription(t *testing.T) {
	srv, newEmb, vs := vectorAdminFixture(t, true)
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	srv.sqlMem = mgr

	// Author through the tool, under an OLD embedder, so the rows are migration candidates.
	oldEmb := &adminFakeEmbedder{provider: "openai", model: "text-embedding-3-small", dim: 4}
	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "alice"})
	doc := &builtin.Document{Store: vs, SqlMem: mgr, Embedder: oldEmb}
	exec := func(v map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(v)
		r, err := doc.Execute(ctx, b)
		if err != nil || r.IsError {
			t.Fatalf("%v: %v %s", v["op"], err, r.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(r.Text), &out)
		return out
	}
	d := exec(map[string]any{"op": "create_document", "scope": "user", "title": "Guide"})
	docID, rootID := d["document_id"].(string), d["root_chunk_id"].(string)
	setup := exec(map[string]any{"op": "create_chunk", "scope": "user", "document_id": docID,
		"title": "Setup", "body": ""})["id"].(string)
	install := exec(map[string]any{"op": "create_chunk", "scope": "user", "document_id": docID,
		"parent_id": setup, "title": "Install", "body": "Run the installer twice."})["id"].(string)
	screen := exec(map[string]any{"op": "create_chunk", "scope": "user", "document_id": docID,
		"title": "Screen", "type": "image", "body": "the login screen"})["id"].(string)
	png, _ := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==")
	exec(map[string]any{"op": "set_asset", "scope": "user", "id": screen,
		"media_type": "image/png", "data": base64.StdEncoding.EncodeToString(png)})
	if err := doc.SetAssetDescription(ctx, "user", screen, "two text fields and a button"); err != nil {
		t.Fatalf("SetAssetDescription: %v", err)
	}
	// A pre-header deployment indexed the bodyless root under its title. Seed that
	// leftover vector, on the old model, so the re-embed has to decide about it.
	if err := vs.MemoryEmbedSet(context.Background(), "", store.MemoryScopeUser, "alice",
		"doc.chunk:"+rootID, store.MemoryEmbedding{Provider: "openai", Model: "text-embedding-3-small",
			Dimension: 4, Vector: []float32{1, 0, 0, 0}, EmbedText: "Guide", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/_memory/reembed?scope=user&scope_id=alice&dry_run=false", nil)
	rec := httptest.NewRecorder()
	srv.handleMemoryReembed(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp memoryReembedRealResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.RowsSkippedEmpty != 1 {
		t.Errorf("rows_skipped_empty = %d, want 1 (the bodyless root)", resp.RowsSkippedEmpty)
	}

	get := func(id string) store.MemoryEmbedding {
		t.Helper()
		e, err := vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, "alice", "doc.chunk:"+id)
		if err != nil {
			t.Fatalf("embedding for %s: %v", id, err)
		}
		return e
	}
	if e := get(install); e.EmbedText != "Guide — Setup > Install\nRun the installer twice." || e.Model != newEmb.model {
		t.Errorf("prose chunk re-embedded as %q (model %s), want its header + body on the new model", e.EmbedText, e.Model)
	}
	img := get(screen)
	if strings.Contains(img.EmbedText, `"body"`) {
		t.Errorf("the image was re-embedded as its raw JSON envelope: %q", img.EmbedText)
	}
	if !strings.Contains(img.EmbedText, "two text fields and a button") || !strings.HasPrefix(img.EmbedText, "Guide — Screen\n") {
		t.Errorf("the image lost its description or header in the re-embed: %q", img.EmbedText)
	}
	// The bodyless root derives to nothing: it is skipped AND its old vector removed, or
	// it would stay a candidate on every later call and starve a large migration.
	if e, err := vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, "alice", "doc.chunk:"+rootID); err == nil {
		t.Errorf("the bodyless root still has an embedding (model %s, text %q); it must be un-indexed", e.Model, e.EmbedText)
	}
}

// TestDocumentReindex_EndpointDryRunsThenFixesAnOldChunk wires POST
// /v1/_document/reindex end to end: the dry run (the default) reports and writes nothing;
// a real run re-indexes a chunk still carrying its pre-header text.
func TestDocumentReindex_EndpointDryRunsThenFixesAnOldChunk(t *testing.T) {
	srv, emb, vs := vectorAdminFixture(t, true)
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	srv.sqlMem = mgr

	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "alice"})
	doc := &builtin.Document{Store: vs, SqlMem: mgr, Embedder: emb}
	run := func(v map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(v)
		r, err := doc.Execute(ctx, b)
		if err != nil || r.IsError {
			t.Fatalf("%v: %v %s", v["op"], err, r.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(r.Text), &out)
		return out
	}
	d := run(map[string]any{"op": "create_document", "scope": "user", "title": "Guide"})
	install := run(map[string]any{"op": "create_chunk", "scope": "user", "document_id": d["document_id"],
		"title": "Install", "body": "Run the installer twice."})["id"].(string)
	// What a pre-header deployment stored for this chunk.
	if err := vs.MemoryEmbedSet(context.Background(), "", store.MemoryScopeUser, "alice", "doc.chunk:"+install,
		store.MemoryEmbedding{Provider: emb.provider, Model: emb.model, Dimension: 4, Vector: []float32{1, 0, 0, 0},
			EmbedText: "Run the installer twice.", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	call := func(qs string) builtin.ReindexReport {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleDocumentReindex(rec, httptest.NewRequest("POST", "/v1/_document/reindex?scope=user&scope_id=alice"+qs, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var rep builtin.ReindexReport
		if err := json.NewDecoder(rec.Body).Decode(&rep); err != nil {
			t.Fatal(err)
		}
		return rep
	}
	text := func() string {
		e, _ := vs.MemoryEmbedGet(context.Background(), "", store.MemoryScopeUser, "alice", "doc.chunk:"+install)
		return e.EmbedText
	}

	if rep := call(""); !rep.DryRun || rep.Reindexed != 1 || text() != "Run the installer twice." {
		t.Errorf("default call must be a dry run that reports and writes nothing: %+v, text now %q", rep, text())
	}
	if rep := call("&dry_run=false"); rep.Reindexed != 1 || text() != "Guide — Install\nRun the installer twice." {
		t.Errorf("real run: %+v, text now %q", rep, text())
	}
}

// TestDocumentReindex_NeedsSQLMemory — without SQL Memory there is no tree to derive a
// header from, and the endpoint says so instead of reporting an empty success.
func TestDocumentReindex_NeedsSQLMemory(t *testing.T) {
	srv, _, _ := vectorAdminFixture(t, true)
	rec := httptest.NewRecorder()
	srv.handleDocumentReindex(rec, httptest.NewRequest("POST", "/v1/_document/reindex?scope=user&scope_id=alice", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503: %s", rec.Code, rec.Body.String())
	}
}
