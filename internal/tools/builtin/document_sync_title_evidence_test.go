package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A push carries evidence that quotes a chunk's title (part "title"). A peer
// older than that feature ignores part, looks for the quote in the body and
// refuses the whole statement. The push then sends the statement once more
// without its title rows and counts them as dropped. Every fixture is invented.

// hookedPeer is a real Document tool behind /v1/_document whose link_chunks
// payloads pass through hook first: hook may rewrite the payload, or refuse it
// by returning the refusal text. links counts the link_chunks calls.
func hookedPeer(t *testing.T, hook func(map[string]any) string) (pd *Document, pctx context.Context, url string, links *atomic.Int32) {
	t.Helper()
	pd, pctx, _ = documentFixture(t)
	links = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		if payload["op"] == "link_chunks" {
			links.Add(1)
			if refusal := hook(payload); refusal != "" {
				w.WriteHeader(422)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "tool_refused", "error": refusal})
				return
			}
			raw, _ = json.Marshal(payload)
		}
		res, err := pd.Execute(pctx, raw)
		if err != nil || res.IsError {
			w.WriteHeader(422)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "tool_refused", "error": res.Text})
			return
		}
		_, _ = io.WriteString(w, res.Text)
	}))
	t.Cleanup(srv.Close)
	return pd, pctx, srv.URL, links
}

// olderPeerHook drops part from every evidence row, as a peer that predates it
// does when it decodes the request.
func olderPeerHook(payload map[string]any) string {
	rows, _ := payload["evidence"].([]any)
	for _, r := range rows {
		if m, ok := r.(map[string]any); ok {
			delete(m, "part")
		}
	}
	return ""
}

// titledStatementPush binds a local document to the peer and writes there
// nkA -cites-> nkB with one title row and one body row quoting nkA.
func titledStatementPush(t *testing.T, url string, bodyQuote string) (d *Document, ctx context.Context, localDoc string) {
	t.Helper()
	d, ctx, _ = documentFixture(t)
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{
		"peerA": {Config: config.DocumentSourceConfig{BaseURL: url}},
	}}
	localDoc = newValidityDoc(t, d, ctx, "/docs/local")
	vExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","id":%q,"source":"peerA","remote_ref":"/docs/remote"}`, localDoc))
	a := resultField(toolResult(vExec(t, d, ctx, fmt.Sprintf(
		`{"op":"upsert_chunk","document_id":%q,"natural_key":"nkA","title":"Incident reporting","body":"The licensee shall report every incident."}`, localDoc))), "id")
	b := upsertKeyed(t, d, ctx, localDoc, "nkB", "Article B sets the reporting duty.")
	ev := fmt.Sprintf(`[{"chunk_id":%q,"quote":"Incident reporting","part":"title"},{"chunk_id":%q,"quote":%q}]`, a, a, bodyQuote)
	if bodyQuote == "" {
		ev = fmt.Sprintf(`[{"chunk_id":%q,"quote":"shall report"}]`, a)
	}
	linkJSON(t, d, operatorCtx(ctx), fmt.Sprintf(`{"op":"link_chunks","from_id":%q,"to_id":%q,"kind":"cites","evidence":%s}`, a, b, ev))
	return d, ctx, localDoc
}

func TestDocumentSyncPush_AnOlderPeerGetsTheStatementWithoutItsTitleRows(t *testing.T) {
	pd, pctx, url, links := hookedPeer(t, olderPeerHook)
	remoteDoc := newValidityDoc(t, pd, pctx, "/docs/remote")
	d, ctx, localDoc := titledStatementPush(t, url, "shall report")

	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","id":%q,"direction":"push"}`, localDoc))
	if r.IsError {
		t.Fatalf("push to an older peer refused: %s", r.Text)
	}
	if out["edges_added"] != float64(1) || out["evidence_dropped"] != float64(1) {
		t.Errorf("push report = %v, want edges_added 1, evidence_dropped 1 (the title row)", out)
	}
	if n := links.Load(); n != 2 {
		t.Errorf("link_chunks calls = %d, want 2 (refused, then sent without the title row)", n)
	}
	s := statementsOfKind(t, pd, pctx, remoteDoc, "cites", 1)[0]
	ev, _ := s["evidence"].([]any)
	if len(ev) != 1 || ev[0].(map[string]any)["quote"] != "shall report" {
		t.Errorf("peer evidence = %v, want the body row only", ev)
	}
}

func TestDocumentSyncPush_ACurrentPeerKeepsTheTitleRow(t *testing.T) {
	pd, pctx, url, links := hookedPeer(t, func(map[string]any) string { return "" })
	remoteDoc := newValidityDoc(t, pd, pctx, "/docs/remote")
	d, ctx, localDoc := titledStatementPush(t, url, "shall report")

	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","id":%q,"direction":"push"}`, localDoc))
	if r.IsError {
		t.Fatalf("push: %s", r.Text)
	}
	if out["edges_added"] != float64(1) || out["evidence_dropped"] != nil {
		t.Errorf("push report = %v, want edges_added 1 and nothing dropped", out)
	}
	if n := links.Load(); n != 1 {
		t.Errorf("link_chunks calls = %d, want 1", n)
	}
	s := statementsOfKind(t, pd, pctx, remoteDoc, "cites", 1)[0]
	ev, _ := s["evidence"].([]any)
	parts := map[any]any{}
	for _, row := range ev {
		m := row.(map[string]any)
		parts[m["quote"]] = m["part"]
	}
	if len(ev) != 2 || parts["Incident reporting"] != "title" {
		t.Errorf("peer evidence = %v, want the title row kept as part title beside the body row", ev)
	}
}

// A peer that refuses a quote is still a refusal when no title row explains it:
// the push stops as before, with one call, and a statement whose title-less
// retry is refused again stops too, after exactly one retry.
func TestDocumentSyncPush_ARefusedQuoteWithoutATitleRowStillFails(t *testing.T) {
	refuseAll := func(payload map[string]any) string {
		if _, ok := payload["evidence"]; ok {
			return "link_chunks: evidence_quote_not_found — the quote does not occur in chunk x"
		}
		return ""
	}
	for _, tc := range []struct {
		name, bodyQuote string
		wantLinks       int32
	}{
		{"body rows only", "", 1},
		{"title and body rows", "shall report", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pd, pctx, url, links := hookedPeer(t, refuseAll)
			newValidityDoc(t, pd, pctx, "/docs/remote")
			d, ctx, localDoc := titledStatementPush(t, url, tc.bodyQuote)

			_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","id":%q,"direction":"push"}`, localDoc))
			if !r.IsError || !strings.Contains(r.Text, "evidence_quote_not_found") {
				t.Errorf("push = %s, want the peer's evidence_quote_not_found", r.Text)
			}
			if n := links.Load(); n != tc.wantLinks {
				t.Errorf("link_chunks calls = %d, want %d", n, tc.wantLinks)
			}
		})
	}
}
