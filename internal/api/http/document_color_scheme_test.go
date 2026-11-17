package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The Web UI explorer and universe read each document's color_scheme from
// POST /v1/_document and send no flag, so the route keeps it in the default
// answer. The connector method the MCP `document` tool calls is the same
// dispatch without the route, and leaves it out: that is what a model reads.
func TestDocumentRoute_KeepsColorSchemeTheMCPConnectorOmits(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1}}
	cfg.Env.AuthToken = "test-token"
	docTool := &builtin.Document{Store: st, SqlMem: mgr, Bus: channels.NewBus()}
	srv := New(cfg, &stubResolver{}, []tools.Tool{&builtin.Path{Store: st}, docTool}, concurrency.New(1, 1, time.Second), st)
	srv.SetSqlMem(mgr)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	post := func(body string) map[string]any {
		t.Helper()
		r := postAdmin(t, ts, "/v1/_document", body)
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		if r.StatusCode != 200 {
			t.Fatalf("%s: %d %s", body, r.StatusCode, b)
		}
		var out map[string]any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
		return out
	}
	doc := post(`{"op":"create_document","scope":"user","title":"Coloured","path":"/docs/coloured"}`)
	id, root := doc["document_id"].(string), doc["root_chunk_id"].(string)
	post(`{"op":"update_chunk","scope":"user","id":"` + root + `","revision":1,"type":"rfc","status":"done","fields":{"color_enabled":true,"color_scheme":{"doc.rfc.done":"#123456"}}}`)

	summary := `{"op":"documents_summary","scope":"user","document_ids":["` + id + `"]}`
	schemeOf := func(out map[string]any) any {
		docs, _ := out["documents"].([]any)
		if len(docs) != 1 {
			t.Fatalf("documents_summary = %v", out)
		}
		return docs[0].(map[string]any)["color_scheme"]
	}
	if cs, _ := schemeOf(post(summary)).(map[string]any); cs["doc.rfc.done"] != "#123456" {
		t.Errorf("route documents_summary color_scheme = %v, want the stored palette", cs)
	}
	if cs, _ := post(`{"op":"get_document","scope":"user","id":"` + id + `"}`)["color_scheme"].(map[string]any); cs["doc.rfc.done"] != "#123456" {
		t.Errorf("route get_document color_scheme = %v, want the stored palette", cs)
	}
	if cs := schemeOf(post(strings.TrimSuffix(summary, "}") + `,"include_color_scheme":false}`)); cs != nil {
		t.Errorf("route include_color_scheme:false kept color_scheme: %v", cs)
	}

	// The MCP handler calls srv.Document on its own principal ctx; build the
	// same subject's ctx the route would, without the route.
	legacy := auth.Principal{TenantID: "default", Subject: "default", Scopes: []string{auth.ScopeAdmin}, Legacy: true}
	req := httptest.NewRequest("POST", "/v1/_document", nil)
	ctx := srv.substrateBrowseCtxFn(req)(auth.WithPrincipal(context.Background(), legacy))
	res, err := srv.Document(ctx, json.RawMessage(summary))
	if err != nil || res.IsError {
		t.Fatalf("connector documents_summary: %v %s", err, res.Text)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	if docs, _ := out["documents"].([]any); len(docs) != 1 {
		t.Fatalf("connector reached a different scope: %s", res.Text)
	}
	if strings.Contains(res.Text, "color_scheme") {
		t.Errorf("connector (MCP) documents_summary carries color_scheme: %s", res.Text)
	}
}
