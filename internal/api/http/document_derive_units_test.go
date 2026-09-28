package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

type stubUnitGen struct{ calls int }

func (g *stubUnitGen) ModelID() string { return "stub-writer" }
func (g *stubUnitGen) Generate(_ context.Context, r memrank.UnitRequest) ([]memrank.GeneratedUnit, error) {
	g.calls++
	return []memrank.GeneratedUnit{{Kind: memrank.UnitQuestion, Text: "what does " + r.Text + " say"}}, nil
}

// TestDeriveUnits_EndpointDryRunsThenWrites — the default call is a dry run (no
// model call, nothing written); a real run needs the generator and writes.
func TestDeriveUnits_EndpointDryRunsThenWrites(t *testing.T) {
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
	d := run(map[string]any{"op": "create_document", "scope": "user", "title": "Policy"})
	run(map[string]any{"op": "create_chunk", "scope": "user", "document_id": d["document_id"], "title": "Leave", "body": "carry over five days"})
	root := d["root_chunk_id"].(string)
	rev := run(map[string]any{"op": "get_chunk", "scope": "user", "id": root})["revision"]
	run(map[string]any{"op": "update_chunk", "scope": "user", "id": root, "revision": rev, "fields": map[string]any{"index_units": true}})

	call := func(qs string) (int, builtin.DeriveUnitsReport) {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleDeriveUnits(rec, httptest.NewRequest("POST", "/v1/_document/derive_units?scope=user&scope_id=alice"+qs, nil))
		var rep builtin.DeriveUnitsReport
		_ = json.NewDecoder(rec.Body).Decode(&rep)
		return rec.Code, rep
	}
	if code, _ := call("&dry_run=false"); code != http.StatusServiceUnavailable {
		t.Errorf("a real run with no generator: status %d, want 503", code)
	}
	gen := &stubUnitGen{}
	srv.SetUnitGenerator(gen)
	if code, rep := call(""); code != http.StatusOK || !rep.DryRun || rep.Generated != 1 || gen.calls != 0 {
		t.Errorf("default call must be a dry run: %d %+v, %d model calls", code, rep, gen.calls)
	}
	if code, rep := call("&dry_run=false"); code != http.StatusOK || rep.Generated != 1 || rep.UnitsWritten != 1 || rep.Model != "stub-writer" {
		t.Errorf("real run: %d %+v", code, rep)
	}
}

func TestDeriveUnits_EndpointNeedsSQLMemory(t *testing.T) {
	srv, _, _ := vectorAdminFixture(t, true)
	rec := httptest.NewRecorder()
	srv.handleDeriveUnits(rec, httptest.NewRequest("POST", "/v1/_document/derive_units", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", rec.Code)
	}
}
