package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A run's copy into the tenant carries a relation as a proposal whatever its
// kind — the method says who made it — with no judge's verdict on it, leaves a
// rejection rejected, and keeps the runtime's own edges (a [[name]] link) as
// they are.
func TestCopyDocuments_ARunsCopyKeepsOnlyTheRuntimesEdgesAsserted(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		run := tenantRunCtx(base)
		a, b := twoChunks(t, d, run, "user", "/mine/kb")
		out, r := docExec(t, d, run, `{"op":"get_chunk","scope":"user","id":"`+a+`"}`)
		if r.IsError {
			t.Fatal(r.Text)
		}
		doc := asStr(out["document_id"])
		for _, c := range []string{`"title":"Target","body":"the target"`, `"title":"Pointer","body":"see [[Target]]"`} {
			if _, r := docExec(t, d, run, `{"op":"create_chunk","scope":"user","document_id":"`+doc+`",`+c+`}`); r.IsError {
				t.Fatal(r.Text)
			}
		}
		judged := linkFrom(t, d, run, "user", a, b, "cites")["id"]
		rejected := linkFrom(t, d, run, "user", b, a, "refers")["id"]
		// A run's row of a kind the runtime also writes (as written before kinds
		// were reserved): its method, not its kind, says who made it.
		reservedKind := linkFrom(t, d, run, "user", a, b, "notes")["id"]
		scopeExec(t, d, run, `UPDATE chunk_edges SET kind = 'supported_by' WHERE id = ?`, reservedKind)
		scopeExec(t, d, run, `UPDATE chunk_edges SET judged_at = 1, judged_by = 'judge', judge_reason = 'r', verdict_confidence = 0.9 WHERE id = ?`, judged)
		scopeExec(t, d, run, `UPDATE chunk_edges SET status = 'rejected', judged_at = 1, judged_by = 'operator', judge_reason = 'wrong', operator_ruled_at = 1, ruled_by = 'operator:u1' WHERE id = ?`, rejected)

		if cp, r := docExec(t, d, run, `{"op":"copy_documents","scope":"user","to_scope":"tenant","under_path":"/mine","dry_run":false}`); r.IsError {
			t.Fatalf("copy_documents: %s", r.Text)
		} else if counts, _ := cp["counts"].(map[string]any); counts["documents"] != float64(1) {
			t.Fatalf("copy = %v, want the document copied", cp)
		}
		key, _, err := d.resolveScope(reviewOperatorCtx(base, nil), "tenant")
		if err != nil {
			t.Fatal(err)
		}
		res, err := d.query(base, key, `SELECT kind, method, status, judged_at, judged_by, verdict_confidence FROM chunk_edges`)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string][]any{}
		for _, row := range res.Rows {
			got[asStr(row[0])+"/"+asStr(row[1])] = row
		}
		for k, want := range map[string]string{
			"references/" + edgeMethodWikilink: edgeStatusAsserted,
			"cites/" + edgeMethodAgent:         edgeStatusProposed,
			"supported_by/" + edgeMethodAgent:  edgeStatusProposed,
			"refers/" + edgeMethodAgent:        edgeStatusRejected,
		} {
			if got[k] == nil || asStr(got[k][2]) != want {
				t.Errorf("%s in the tenant = %v, want %s", k, got[k], want)
			}
		}
		if c := got["cites/"+edgeMethodAgent]; c != nil && (c[3] != nil || c[4] != nil || c[5] != nil) {
			t.Errorf("a demoted statement kept a verdict: %v", c)
		}
	})
}

// A push never writes a chunk of another document on the peer: a key the peer
// holds elsewhere is looked up first and reported as a conflict.
func TestSyncPush_NeverWritesOutsideThePeerDocument(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		localDoc, pd, pctx, _ := carryPair(t, d, ctx)
		victimDoc := newValidityDoc(t, pd, pctx, "/docs/victim")
		victim := resultField(toolResult(vExec(t, pd, pctx, fmt.Sprintf(
			`{"op":"upsert_chunk","document_id":%q,"natural_key":"nkV","title":"Alpha","body":"body-1"}`, victimDoc))), "id")
		upsertKeyed(t, d, ctx, localDoc, "nkV", "the local text")
		upsertKeyed(t, d, ctx, localDoc, "nkA", "a")

		rep := syncReport(t, d, ctx, localDoc, "push")
		if rep["conflicts"] != float64(1) || rep["created"] != float64(1) {
			t.Errorf("push report = %v, want conflicts 1 and nkA created", rep)
		}
		got := map[string]any{}
		_ = json.Unmarshal([]byte(vExec(t, pd, pctx, fmt.Sprintf(`{"op":"get_chunk","id":%q}`, victim))), &got)
		if got["title"] != "Alpha" || got["body"] != "body-1" || got["document_id"] != victimDoc {
			t.Errorf("the push changed another peer document's chunk: %v", got)
		}
	})
}

// A peer that cannot say which of its documents holds a key (it refuses the
// read the push asks first) gets no write for that key: the push reports it as
// a conflict instead of writing and checking afterwards, when another peer
// document's chunk would already be overwritten.
func TestSyncPush_APeerThatCannotAnswerThePreCheckGetsNoWrite(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	pd, pctx, _ := documentFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in struct {
			Op  string `json:"op"`
			SQL string `json:"sql"`
		}
		_ = json.Unmarshal(raw, &in)
		w.Header().Set("Content-Type", "application/json")
		if in.Op == "query_chunks" && in.SQL != "" {
			// The credential is refused the scope-wide read.
			w.WriteHeader(403)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "forbidden", "error": "read refused"})
			return
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
	newValidityDoc(t, pd, pctx, "/docs/remote")
	victimDoc := newValidityDoc(t, pd, pctx, "/docs/victim")
	victim := resultField(toolResult(vExec(t, pd, pctx, fmt.Sprintf(
		`{"op":"upsert_chunk","document_id":%q,"natural_key":"nkV","title":"Alpha","body":"body-1"}`, victimDoc))), "id")
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{
		"peerA": {Config: config.DocumentSourceConfig{BaseURL: srv.URL}},
	}}
	localDoc := newValidityDoc(t, d, ctx, "/docs/local")
	vExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","id":%q,"source":"peerA","remote_ref":"/docs/remote"}`, localDoc))
	upsertKeyed(t, d, ctx, localDoc, "nkV", "the local text")

	rep := syncReport(t, d, ctx, localDoc, "push")
	if rep["conflicts"] != float64(1) || rep["created"] != float64(0) {
		t.Errorf("push report = %v, want the key reported as a conflict and nothing created", rep)
	}
	got := map[string]any{}
	_ = json.Unmarshal([]byte(vExec(t, pd, pctx, fmt.Sprintf(`{"op":"get_chunk","id":%q}`, victim))), &got)
	if got["title"] != "Alpha" || got["body"] != "body-1" || got["document_id"] != victimDoc {
		t.Errorf("the push overwrote another peer document's chunk: %v", got)
	}
}
