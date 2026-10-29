package builtin

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A pull over a relation this side rejected skips it and says so: it neither
// fails every later pull nor, for an operator, re-asserts the rejection.
func TestDocumentSync_SkipsARelationTheLocalSideRejected(t *testing.T) {
	srv := httptest.NewServer(treeStubPeer())
	t.Cleanup(srv.Close)
	d, ctx, _ := documentFixture(t)
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{
		"peerA": {Config: config.DocumentSourceConfig{BaseURL: srv.URL}},
	}}
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Local"}`)
	docID := out["document_id"].(string)
	docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","scope":"user","id":%q,"source":"peerA","remote_ref":"/docs/tree"}`, docID))
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	// The relation arrives proposed here (say an edition dropped it to review),
	// and the operator rejects it.
	localC, localS := "", ""
	for _, nk := range []struct {
		nk  string
		dst *string
	}{{"nkC", &localC}, {"nkS", &localS}} {
		o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":%q,"title":"x","body":"x"}`, docID, nk.nk))
		if r.IsError {
			t.Fatal(r.Text)
		}
		*nk.dst = o["id"].(string)
	}
	p, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"relates"}`, localC, localS))
	if r.IsError {
		t.Fatal(r.Text)
	}
	if _, r := docExec(t, d, operatorCtx(ctx), fmt.Sprintf(`{"op":"reject_statement","scope":"user","id":%q,"reason":"not related"}`, p["id"])); r.IsError {
		t.Fatal(r.Text)
	}
	for name, c := range map[string]context.Context{"caller": ctx, "operator": operatorCtx(ctx)} {
		s, r := docExec(t, d, c, fmt.Sprintf(`{"op":"sync","scope":"user","id":%q}`, docID))
		if r.IsError {
			t.Fatalf("%s pull over a rejected relation: %s", name, r.Text)
		}
		if s["edges_added"].(float64) != 0 || s["edges_skipped_rejected"] != float64(1) {
			t.Errorf("%s pull report = %+v, want edges_added=0 edges_skipped_rejected=1", name, s)
		}
		st, _ := d.statementByID(ctx, key, p["id"].(string))
		if st == nil || st.Status != edgeStatusRejected {
			t.Errorf("%s pull changed the rejected relation: %+v", name, st)
		}
	}
}
