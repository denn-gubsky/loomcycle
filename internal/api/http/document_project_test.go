package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// projectFixture is a server with SQL Memory, a vector fake and the projection
// on, plus a document of six chunks whose vectors spread in three directions.
func projectFixture(t *testing.T, supportsVectors bool) (*Server, *builtin.Document, context.Context) {
	t.Helper()
	srv, emb, vs := vectorAdminFixture(t, supportsVectors)
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	srv.sqlMem = mgr
	cfg := &config.Config{}
	cfg.Memory.Projection.Enabled = true
	srv.cfgHolder = config.NewHolder(cfg)

	ctx := tools.WithAgentName(context.Background(), "doc-agent")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: "alice"})
	doc := &builtin.Document{Store: vs, SqlMem: mgr, Embedder: emb, Cfg: cfg}
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
	d := run(map[string]any{"op": "create_document", "scope": "user", "title": "Map"})
	for i, v := range [][]float32{{1, 0, 0, 0}, {0, 2, 0, 0}, {0, 0, 3, 0}, {1, 1, 0, 0}, {0, 1, 1, 1}, {2, 0, 1, 0}} {
		id := run(map[string]any{"op": "create_chunk", "scope": "user", "document_id": d["document_id"],
			"title": "Part", "body": "text"})["id"].(string)
		if !supportsVectors {
			continue
		}
		if err := vs.MemoryEmbedSet(context.Background(), "", store.MemoryScopeUser, "alice", "doc.chunk:"+id,
			store.MemoryEmbedding{Provider: emb.provider, Model: emb.model, Dimension: 4, Vector: v,
				EmbedText: "text", CreatedAt: time.Now().Add(time.Duration(i))}); err != nil {
			t.Fatal(err)
		}
	}
	return srv, doc, ctx
}

func callProject(t *testing.T, srv *Server, qs string) (int, builtin.ProjectionReport) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleDocumentProject(rec, httptest.NewRequest("POST", "/v1/_document/project?scope=user&scope_id=alice"+qs, nil))
	var rep builtin.ProjectionReport
	if rec.Code == http.StatusOK {
		if err := json.NewDecoder(rec.Body).Decode(&rep); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, rep
}

func basisVersionOf(t *testing.T, doc *builtin.Document, ctx context.Context) int {
	t.Helper()
	r, err := doc.Execute(ctx, json.RawMessage(`{"op":"graph_snapshot","scope":"user","include":["documents","chunks","projection"]}`))
	if err != nil || r.IsError {
		t.Fatalf("graph_snapshot: %v %s", err, r.Text)
	}
	var out struct {
		Projection struct {
			BasisVersion int `json:"basis_version"`
		} `json:"projection"`
	}
	_ = json.Unmarshal([]byte(r.Text), &out)
	return out.Projection.BasisVersion
}

// TestDocumentProject_DryRunsByDefaultThenPlaces — the default call reports the fit
// and placements and writes nothing; dry_run=false makes the map.
func TestDocumentProject_DryRunsByDefaultThenPlaces(t *testing.T) {
	srv, doc, ctx := projectFixture(t, true)
	code, rep := callProject(t, srv, "")
	if code != http.StatusOK || !rep.DryRun || !rep.Refitted || rep.Placed != 6 || rep.BasisVersion != 1 {
		t.Fatalf("default call = %d %+v, want a dry run reporting a v1 fit and 6 placements", code, rep)
	}
	if v := basisVersionOf(t, doc, ctx); v != 0 {
		t.Fatalf("the dry run left basis v%d behind", v)
	}
	code, rep = callProject(t, srv, "&dry_run=false")
	if code != http.StatusOK || rep.DryRun || rep.Placed != 6 || rep.Model != "openai/text-embedding-3-large" {
		t.Fatalf("real run = %d %+v", code, rep)
	}
	if v := basisVersionOf(t, doc, ctx); v != 1 {
		t.Fatalf("after the real run the snapshot serves v%d, want v1", v)
	}
	if code, rep = callProject(t, srv, "&dry_run=false&refit=true&limit=2"); rep.BasisVersion != 2 || rep.Placed != 2 || !rep.More {
		t.Fatalf("forced, limited refit = %d %+v, want v2 with 2 placed and more", code, rep)
	}
}

// TestDocumentProject_RefusedWhileOff — with memory.projection off the route
// answers 409 and touches nothing.
func TestDocumentProject_RefusedWhileOff(t *testing.T) {
	srv, _, _ := projectFixture(t, true)
	srv.cfgHolder = config.NewHolder(&config.Config{})
	if code, _ := callProject(t, srv, "&dry_run=false"); code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while the projection is off", code)
	}
}

// TestDocumentProject_ReportsUnsupportedWithoutVectors — a store with no vector
// index has nothing to place, and the pass says so rather than failing.
func TestDocumentProject_ReportsUnsupportedWithoutVectors(t *testing.T) {
	srv, _, _ := projectFixture(t, false)
	code, rep := callProject(t, srv, "&dry_run=false")
	if code != http.StatusOK || !rep.Unsupported || rep.Placed != 0 {
		t.Fatalf("= %d %+v, want 200 with unsupported", code, rep)
	}
}

func TestDocumentProject_RefusesABadScope(t *testing.T) {
	srv, _, _ := projectFixture(t, true)
	rec := httptest.NewRecorder()
	srv.handleDocumentProject(rec, httptest.NewRequest("POST", "/v1/_document/project?scope=run", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
