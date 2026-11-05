package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// A pull writes the bound document and nothing else in the scope. A natural key
// is unique across the scope, so a peer chunk whose key a chunk of ANOTHER local
// document holds would, through upsert_chunk, rewrite that chunk and the
// hierarchy pass would then move it into the synced document — reaching a
// document the caller was only asked to sync into. The key is a conflict: the
// other document is left as it was, the conflict is counted and named, and the
// rest of the pull still lands.
func TestSyncPull_NeverWritesOutsideTheBoundDocument(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		localDoc, pd, pctx, remoteDoc := carryPair(t, d, ctx)
		victimDoc := newValidityDoc(t, d, ctx, "/docs/victim")
		victim := resultField(toolResult(vExec(t, d, ctx, fmt.Sprintf(
			`{"op":"upsert_chunk","document_id":%q,"natural_key":"nkV","title":"Alpha","body":"body-1"}`, victimDoc))), "id")

		parent := upsertKeyed(t, pd, pctx, remoteDoc, "nkP", "the peer's parent")
		vExec(t, pd, pctx, fmt.Sprintf(
			`{"op":"upsert_chunk","document_id":%q,"natural_key":"nkV","title":"Overwritten","body":"the peer's text"}`, remoteDoc))
		pkey, _, _ := pd.resolveScope(pctx, "user")
		peerV, _ := pd.chunkIDByNaturalKey(pctx, pkey, "nkV")
		vExec(t, pd, pctx, fmt.Sprintf(`{"op":"move_chunk","id":%q,"new_parent_id":%q,"position":0}`, peerV, parent))
		// A peer link to the conflicting key cannot be carried, and says so.
		linkJSON(t, pd, operatorCtx(pctx), fmt.Sprintf(`{"op":"link_chunks","from_id":%q,"to_id":%q,"kind":"cites"}`, parent, peerV))

		// The dry run names it before anything is written.
		diff, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"diff_remote","id":%q}`, localDoc))
		if res.IsError {
			t.Fatalf("diff_remote: %s", res.Text)
		}
		if c, _ := diff["conflicts"].([]any); len(c) != 1 || c[0].(map[string]any)["natural_key"] != "nkV" {
			t.Errorf("diff_remote conflicts = %v, want nkV", diff["conflicts"])
		}

		out, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"sync","id":%q,"direction":"pull"}`, localDoc))
		if res.IsError {
			t.Fatalf("a key held by another document failed the whole pull: %s", res.Text)
		}
		if out["conflicts"] != float64(1) {
			t.Errorf("pull report = %v, want conflicts 1", out)
		}
		if keys, _ := out["conflicting_keys"].([]any); len(keys) != 1 || keys[0] != "nkV" {
			t.Errorf("conflicting_keys = %v, want [nkV]", out["conflicting_keys"])
		}
		if out["created"] != float64(1) {
			t.Errorf("pull report = %v, want the rest of the document (nkP) created", out)
		}
		if out["edges_skipped_conflict"] != float64(1) {
			t.Errorf("pull report = %v, want edges_skipped_conflict 1", out)
		}

		got := map[string]any{}
		_ = json.Unmarshal([]byte(vExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","id":%q}`, victim))), &got)
		if got["title"] != "Alpha" || got["body"] != "body-1" || got["document_id"] != victimDoc {
			t.Errorf("the other document's chunk changed: title %v, body %v, document %v", got["title"], got["body"], got["document_id"])
		}
	})
}

// untrustedPair is carryPair with the source left at its default trust.
func untrustedPair(t *testing.T, d *Document, ctx context.Context) (string, *Document, context.Context, string) {
	t.Helper()
	localDoc, pd, pctx, remoteDoc := carryPair(t, d, ctx)
	src := d.Cfg.DocumentSources["peerA"]
	src.Trust = ""
	d.Cfg.DocumentSources["peerA"] = src
	return localDoc, pd, pctx, remoteDoc
}

// A peer says a statement is asserted and that a person made it; neither claim
// is this store's. A pull files it for review here, naming the peer as where it
// came from and keeping what the peer said (its method and status) beside it.
// A peer's manual never becomes this store's person: a later pass may maintain it.
func TestSyncPull_APeersAssertedStatementArrivesProposed(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		localDoc, pd, pctx, remoteDoc := untrustedPair(t, d, ctx)
		a := upsertKeyed(t, pd, pctx, remoteDoc, "nkA", "The licensee shall report every incident.")
		b := upsertKeyed(t, pd, pctx, remoteDoc, "nkB", "Article B sets the reporting duty.")
		linkJSON(t, pd, operatorCtx(pctx), fmt.Sprintf(`{"op":"link_chunks","from_id":%q,"to_id":%q,"kind":"cites",
			"confidence":0.8,"evidence":[{"chunk_id":%q,"quote":"shall report"}]}`, a, b, a))

		rep := syncReport(t, d, ctx, localDoc, "pull")
		if rep["edges_added"] != float64(1) {
			t.Errorf("pull = %v, want edges_added 1", rep)
		}
		got := proposedStatements(t, d, ctx, localDoc, "cites")
		if len(got) != 1 {
			t.Fatalf("%d proposed cites statements here, want 1", len(got))
		}
		s := got[0]
		attrs, _ := s["attributes"].(map[string]any)
		if s["method"] != edgeMethodSync || attrs[edgeOriginMethodAttr] != "peer:peerA" ||
			attrs[edgePeerMethodAttr] != edgeMethodManual || attrs[edgePeerStatusAttr] != edgeStatusAsserted {
			t.Errorf("pulled statement = method %v, attributes %v; want sync with origin_method peer:peerA, peer_method manual, peer_status asserted", s["method"], attrs)
		}
		if s["evidence_count"] != float64(1) {
			t.Errorf("pulled evidence = %v, want the one checked quote", s["evidence_count"])
		}
		key, _, _ := d.resolveScope(ctx, "user")
		if st, _ := d.statementByID(ctx, key, asStr(s["id"])); st == nil || madeByAPerson(st) {
			t.Errorf("a peer's manual statement reads as a person's here: %+v", st)
		}

		// Again: the proposal is the one already filed, not a second.
		if rep := syncReport(t, d, ctx, localDoc, "pull"); rep["edges_added"] != float64(0) {
			t.Errorf("re-pull = %v, want edges_added 0", rep)
		}
		if again := proposedStatements(t, d, ctx, localDoc, "cites"); len(again) != 1 || again[0]["evidence_count"] != float64(1) {
			t.Errorf("after a re-pull: %v", again)
		}
	})
}

// An operator who vouches for a peer (trust: asserted) keeps its asserted
// statements asserted; where they came from is recorded all the same.
func TestSyncPull_ATrustedSourceKeepsTheAssertedStatus(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		localDoc, pd, pctx, remoteDoc := carryPair(t, d, ctx)
		a := upsertKeyed(t, pd, pctx, remoteDoc, "nkA", "a")
		b := upsertKeyed(t, pd, pctx, remoteDoc, "nkB", "b")
		linkJSON(t, pd, operatorCtx(pctx), fmt.Sprintf(`{"op":"link_chunks","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b))

		syncReport(t, d, ctx, localDoc, "pull")
		s := statementsOfKind(t, d, ctx, localDoc, "cites", 1)[0]
		attrs, _ := s["attributes"].(map[string]any)
		if s["status"] != edgeStatusAsserted || attrs[edgeOriginMethodAttr] != "peer:peerA" || attrs[edgePeerMethodAttr] != edgeMethodManual {
			t.Errorf("a trusted pull = status %v, attributes %v; want asserted, from peer:peerA, peer_method manual", s["status"], attrs)
		}
	})
}

// proposedStatements returns a document's proposed statements of one kind, each
// as get_edge reads it.
func proposedStatements(t *testing.T, d *Document, ctx context.Context, docID, kind string) []map[string]any {
	t.Helper()
	var ls struct {
		Statements []map[string]any `json:"statements"`
	}
	_ = json.Unmarshal([]byte(vExec(t, d, operatorCtx(ctx), fmt.Sprintf(`{"op":"list_statements","document_id":%q,"kind":%q,"status":"proposed"}`, docID, kind))), &ls)
	out := make([]map[string]any, 0, len(ls.Statements))
	for _, e := range ls.Statements {
		var one struct {
			Statement map[string]any `json:"statement"`
		}
		_ = json.Unmarshal([]byte(vExec(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","id":%q}`, e["id"]))), &one)
		out = append(out, one.Statement)
	}
	return out
}

// A peer that is not trusted changes nothing that already holds here. A statement
// an earlier pull asserted (from a source trusted then, or before pulls filed
// proposals) keeps its envelope when the peer changes it, and gains none of the
// peer's evidence: the difference is reported as kept.
func TestSyncPull_AnUntrustedPeerCannotChangeAnAssertedStatement(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		localDoc, pd, pctx, remoteDoc := carryPair(t, d, ctx)
		id := richPeerStatement(t, d, ctx, pd, pctx, remoteDoc)
		syncReport(t, d, ctx, localDoc, "pull")
		before := statementsOfKind(t, d, ctx, localDoc, "cites", 1)[0]
		if before["status"] != edgeStatusAsserted {
			t.Fatalf("the trusted pull = %v, want asserted", before["status"])
		}

		src := d.Cfg.DocumentSources["peerA"]
		src.Trust = ""
		d.Cfg.DocumentSources["peerA"] = src
		scopeExec(t, pd, pctx, `UPDATE chunk_edges SET confidence = ?, invalid_at = ?, attributes = ? WHERE id = ?`, 0.1, ns2025, `{"note":"peer rewrote"}`, id)
		pa, _, _ := pd.resolveScope(pctx, "user")
		peerB, _ := pd.chunkIDByNaturalKey(pctx, pa, "nkB")
		scopeExec(t, pd, pctx, `INSERT INTO chunk_edge_evidence (id, edge_id, chunk_id, quote, created_at) VALUES (?, ?, ?, ?, ?)`,
			"ev-new", id, peerB, "reporting duty", 2)

		rep := syncReport(t, d, ctx, localDoc, "pull")
		if rep["edges_updated"] != nil || rep["edges_envelope_kept"] != float64(1) || rep["edges_added"] != float64(0) {
			t.Errorf("an untrusted pull over an asserted statement = %v, want edges_envelope_kept 1 and nothing updated or added", rep)
		}
		after := statementsOfKind(t, d, ctx, localDoc, "cites", 1)[0]
		attrs, _ := after["attributes"].(map[string]any)
		if after["confidence"] != before["confidence"] || after["invalid_at"] != before["invalid_at"] || attrs["note"] != "para 3" {
			t.Errorf("an untrusted peer changed an asserted statement: %v", after)
		}
		if after["evidence_count"] != before["evidence_count"] {
			t.Errorf("an untrusted peer added evidence to an asserted statement: %v -> %v", before["evidence_count"], after["evidence_count"])
		}
	})
}
