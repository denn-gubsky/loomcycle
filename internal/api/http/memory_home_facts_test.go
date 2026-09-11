package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Subject homing moves a fact into the document that IS its subject (RFC CV P3).
// These pin the move, the things it must NOT move, and the merge — because the
// migration's whole value is that a fact keeps its identity while changing homes.

// homingFixture is a scope holding the OLD shape: one shared /memory/entities
// document with subject nodes and fact nodes as siblings, joined by `about` edges.
type homingFixture struct {
	t      *testing.T
	srv    *Server
	doc    *builtin.Document
	ctx    context.Context
	shared string
}

func newHomingFixture(t *testing.T) *homingFixture {
	t.Helper()
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	withSqlMem(t, srv)
	f := &homingFixture{
		t: t, srv: srv,
		doc: &builtin.Document{Store: srv.store, SqlMem: srv.sqlMem},
		ctx: tools.WithAgentName(tools.WithRunIdentity(context.Background(),
			tools.RunIdentityValue{UserID: "alice"}), "consolidator"),
	}
	f.shared = f.mkDoc("Entities", "/memory/entities")
	return f
}

func (f *homingFixture) exec(body string) tools.Result {
	f.t.Helper()
	res, err := f.doc.Execute(f.ctx, json.RawMessage(body))
	if err != nil {
		f.t.Fatalf("document: %v", err)
	}
	if res.IsError {
		f.t.Fatalf("document op failed: %s", res.Text)
	}
	return res
}

func (f *homingFixture) mkDoc(title, path string) string {
	f.t.Helper()
	res := f.exec(`{"op":"create_document","scope":"user","title":"` + title + `","path":"` + path + `"}`)
	var out struct {
		DocumentID string `json:"document_id"`
	}
	_ = json.Unmarshal([]byte(res.Text), &out)
	if out.DocumentID == "" {
		f.t.Fatalf("create_document %s returned no id: %s", path, res.Text)
	}
	return out.DocumentID
}

// subject writes an OLD-SHAPE subject node: a sibling chunk in the shared document.
func (f *homingFixture) subject(docID, typ, name, key string) string {
	f.t.Helper()
	res := f.exec(`{"op":"upsert_chunk","scope":"user","document_id":"` + docID + `",
		"natural_key":"` + key + `","title":"` + name + `","type":"` + typ + `","subject":"` + name + `"}`)
	return chunkIDOf(f.t, res)
}

// fact writes a fact node and, when subjID is non-empty, the `about` edge that makes
// it reachable from its subject — the pair the migration reads.
func (f *homingFixture) fact(docID, key, text, subjID string) string {
	f.t.Helper()
	res := f.exec(`{"op":"upsert_chunk","scope":"user","document_id":"` + docID + `",
		"natural_key":"` + key + `","title":"` + text + `","body":"` + text + `","type":"fact"}`)
	id := chunkIDOf(f.t, res)
	if subjID != "" {
		f.exec(`{"op":"link_chunks","scope":"user","from_id":"` + id + `","to_id":"` + subjID + `","kind":"about"}`)
	}
	return id
}

func (f *homingFixture) rootOf(docID string) string {
	f.t.Helper()
	rows := f.sqlRows(`SELECT root_chunk_id FROM documents WHERE id = '` + docID + `'`)
	if len(rows) != 1 {
		f.t.Fatalf("document %s: %v", docID, rows)
	}
	return asString(rows[0][0])
}

func chunkIDOf(t *testing.T, res tools.Result) string {
	t.Helper()
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(res.Text), &out)
	if out.ID == "" {
		t.Fatalf("no chunk id in reply: %s", res.Text)
	}
	return out.ID
}

// sqlRows runs a read straight against the scope's chunk tables. The migration's
// postconditions are document_id/parent_id/edge rows, none of which any read op
// reports — asserting them through the tool surface would assert the surface.
func (f *homingFixture) sqlRows(query string) [][]any {
	f.t.Helper()
	res := f.exec(`{"op":"query_chunks","scope":"user","sql":` + jsonStr(query) + `}`)
	var out struct {
		Rows [][]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		f.t.Fatalf("query_chunks: %v (%s)", err, res.Text)
	}
	return out.Rows
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func homeReq(t *testing.T, srv *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/_memory/home_facts"+query, nil).
		WithContext(auth.WithPrincipal(context.Background(), auth.Principal{
			Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}))
	srv.handleMemoryHomeFacts(rec, req)
	return rec
}

func homeRun(t *testing.T, srv *Server, query string) builtin.FactHomingReport {
	t.Helper()
	rec := homeReq(t, srv, query)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var rep builtin.FactHomingReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("report: %v (%s)", err, rec.Body.String())
	}
	return rep
}

// TestHomeFacts_AFactMovesIntoItsSubjectsDocument is the migration proper.
//
// The assertions are the ones that say a MOVE happened rather than a copy: BOTH the
// fact and its subject keep their chunk ids, so their bodies, embeddings, sidecars,
// spans, counters and edges follow them for free. The subject BECOMES the new
// document's root — which is why the `about` edge still points at the same chunk and
// why nothing ever has to hold the unique natural key twice.
func TestHomeFacts_AFactMovesIntoItsSubjectsDocument(t *testing.T) {
	f := newHomingFixture(t)
	subj := f.subject(f.shared, "person", "Denn", "person:denn")
	fact := f.fact(f.shared, "memory/fact/denn-writes-go", "Denn writes Go.", subj)

	rep := homeRun(t, f.srv, "?scope=user&scope_id=alice&tenant=&dry_run=false")
	if rep.FactsMoved != 1 || rep.DocumentsCreated != 1 || rep.SubjectsHomed != 1 {
		t.Fatalf("report = %+v, want one fact moved into one created document", rep)
	}

	rows := f.sqlRows(`SELECT c.id, c.document_id, c.parent_id FROM chunks c WHERE c.id = '` + fact + `'`)
	if len(rows) != 1 {
		t.Fatalf("the fact chunk is gone — it must MOVE, never be re-created: %v", rows)
	}
	docID, parentID := asString(rows[0][1]), asString(rows[0][2])

	newDoc := f.exec(`{"op":"get_document","scope":"user","path":"/facts/denn"}`)
	var nd struct {
		DocumentID  string `json:"document_id"`
		RootChunkID string `json:"root_chunk_id"`
	}
	_ = json.Unmarshal([]byte(newDoc.Text), &nd)
	if docID != nd.DocumentID {
		t.Errorf("fact is in document %q, want /facts/denn (%q)", docID, nd.DocumentID)
	}
	if parentID != nd.RootChunkID {
		t.Errorf("fact's parent is %q, want the root %q — the subject's facts are its "+
			"CHILDREN, which is what makes the dossier one read", parentID, nd.RootChunkID)
	}

	// THE SUBJECT NODE IS THE ROOT. Not a copy of it beside a fresh one: the same
	// chunk, which is what carries the unique natural key across without ever holding
	// it twice, and what leaves the `about` edge pointing at the right thing.
	if nd.RootChunkID != subj {
		t.Errorf("root of /facts/denn = %q, want the subject node %q itself — re-creating it "+
			"would need the unique key held by two chunks at once", nd.RootChunkID, subj)
	}
	keys := f.sqlRows(`SELECT chunk_id FROM chunk_memory_meta WHERE natural_key = 'person:denn'`)
	if len(keys) != 1 || asString(keys[0][0]) != subj {
		t.Errorf("chunks claiming person:denn = %v, want only the moved subject node %q", keys, subj)
	}
	// The subject is a root now: no parent, and out of the shared document.
	sr := f.sqlRows(`SELECT document_id, parent_id FROM chunks WHERE id = '` + subj + `'`)
	if len(sr) != 1 || asString(sr[0][0]) != nd.DocumentID || sr[0][1] != nil {
		t.Errorf("subject node sits at %v, want document %q with no parent", sr, nd.DocumentID)
	}
	// The edge never moved, because neither endpoint changed id.
	edges := f.sqlRows(`SELECT to_id FROM chunk_edges WHERE from_id = '` + fact + `' AND kind = 'about'`)
	if len(edges) != 1 || asString(edges[0][0]) != subj {
		t.Errorf("about edge = %v, want one pointing at the subject %q — an edge dropped "+
			"here is a retrieval loss even though the fact survives", edges, subj)
	}
}

// TestHomeFacts_AFactAboutNobodyStaysInTheSharedDocument. mirrorEntity refuses to
// invent a subject for a fact that names none — inventing one is how two different
// things end up merged onto a single node — so the migration must refuse it too.
// /memory/entities stays the home for facts about nobody.
func TestHomeFacts_AFactAboutNobodyStaysInTheSharedDocument(t *testing.T) {
	f := newHomingFixture(t)
	subj := f.subject(f.shared, "person", "Denn", "person:denn")
	homed := f.fact(f.shared, "memory/fact/denn-writes-go", "Denn writes Go.", subj)
	orphan := f.fact(f.shared, "memory/fact/releases-are-friday", "Releases ship on Friday.", "")

	rep := homeRun(t, f.srv, "?scope=user&scope_id=alice&tenant=&dry_run=false")
	if rep.FactsMoved != 1 {
		t.Errorf("moved %d facts, want 1 — only the one with a subject", rep.FactsMoved)
	}
	if rep.FactsWithoutSubject != 1 {
		t.Errorf("facts_without_subject = %d, want 1 — the gap must READ as work still to "+
			"do rather than vanish into a silent default", rep.FactsWithoutSubject)
	}
	rows := f.sqlRows(`SELECT id, document_id FROM chunks WHERE id IN ('` + homed + `','` + orphan + `')`)
	for _, r := range rows {
		id, doc := asString(r[0]), asString(r[1])
		if id == orphan && doc != f.shared {
			t.Errorf("the subject-less fact moved to %q — it has no subject to be homed under", doc)
		}
		if id == homed && doc == f.shared {
			t.Error("the subjected fact did not move")
		}
	}
}

// TestHomeFacts_AdoptsTheEmptyDocumentAnUnmigratedPassLeftBehind.
//
// MEASURED: on a store whose subject node predates subject-homing, create_document
// inserted the document and its root and only THEN failed on the unique natural key,
// so every consolidation pass left one more empty, path-less /facts/<slug> behind. The
// document is now rolled back, but stores that ran the broken version still carry
// them — and one of them may be the very document at the path this subject needs. It
// is adopted rather than sidestepped, or the migration would create a second
// /facts/denn the Path tree cannot name.
func TestHomeFacts_AdoptsTheEmptyDocumentAnUnmigratedPassLeftBehind(t *testing.T) {
	f := newHomingFixture(t)
	stray := f.mkDoc("Denn", "/facts/denn") // no entity fields: exactly what leaked
	strayRoot := f.rootOf(stray)
	subj := f.subject(f.shared, "person", "Denn", "person:denn")
	fact := f.fact(f.shared, "memory/fact/denn-writes-go", "Denn writes Go.", subj)

	rep := homeRun(t, f.srv, "?scope=user&scope_id=alice&tenant=&dry_run=false")
	if rep.DocumentsAdopted != 1 || rep.DocumentsCreated != 0 {
		t.Errorf("report = %+v, want the stray /facts/denn ADOPTED, not a second one made", rep)
	}
	rows := f.sqlRows(`SELECT document_id, parent_id FROM chunks WHERE id = '` + fact + `'`)
	if len(rows) != 1 || asString(rows[0][0]) != stray || asString(rows[0][1]) != subj {
		t.Errorf("fact landed at %v, want the adopted document %q under the subject %q", rows, stray, subj)
	}
	if root := f.rootOf(stray); root != subj {
		t.Errorf("adopted document's root = %q, want the subject node %q", root, subj)
	}
	if len(f.sqlRows(`SELECT id FROM chunks WHERE id = '`+strayRoot+`'`)) != 0 {
		t.Error("the placeholder root survives, so the document has two roots")
	}
	// One document at the path, not two — the Path tree names only one of them.
	if docs := f.sqlRows(`SELECT id FROM documents`); len(docs) != 2 {
		t.Errorf("%d documents, want 2 (the shared one and /facts/denn)", len(docs))
	}
}

// TestHomeFacts_IsIdempotent. A migration that has to be run exactly once is a
// migration that will be run twice.
func TestHomeFacts_IsIdempotent(t *testing.T) {
	f := newHomingFixture(t)
	subj := f.subject(f.shared, "person", "Denn", "person:denn")
	f.fact(f.shared, "memory/fact/denn-writes-go", "Denn writes Go.", subj)

	if rep := homeRun(t, f.srv, "?scope=user&scope_id=alice&tenant=&dry_run=false"); rep.FactsMoved != 1 {
		t.Fatalf("first run moved %d, want 1", rep.FactsMoved)
	}
	rep := homeRun(t, f.srv, "?scope=user&scope_id=alice&tenant=&dry_run=false")
	if rep.FactsMoved != 0 || rep.Subjects != 0 || rep.SubjectsHomed != 0 {
		t.Errorf("second run = %+v, want nothing left to do", rep)
	}
}

// TestHomeFacts_DryRunIsTheDefaultAndMovesNothing. A bare POST must not rewrite a
// store, and must still report what it WOULD do — that report is the reason this is
// operator-triggered rather than run at boot.
func TestHomeFacts_DryRunIsTheDefaultAndMovesNothing(t *testing.T) {
	f := newHomingFixture(t)
	subj := f.subject(f.shared, "person", "Denn", "person:denn")
	fact := f.fact(f.shared, "memory/fact/denn-writes-go", "Denn writes Go.", subj)

	rep := homeRun(t, f.srv, "?scope=user&scope_id=alice&tenant=") // no dry_run param
	if !rep.DryRun {
		t.Fatal("dry_run defaulted to FALSE — a bare POST would rewrite the store")
	}
	if rep.Subjects != 1 || rep.FactsMoved != 1 {
		t.Errorf("report = %+v, want it to say one fact WOULD move", rep)
	}
	rows := f.sqlRows(`SELECT document_id FROM chunks WHERE id = '` + fact + `'`)
	if len(rows) != 1 || asString(rows[0][0]) != f.shared {
		t.Errorf("the fact moved on a DRY RUN: %v", rows)
	}
	if len(f.sqlRows(`SELECT id FROM chunks WHERE id = '`+subj+`'`)) != 1 {
		t.Error("the old subject node was removed on a DRY RUN")
	}
}

// TestHomeFacts_AnAdminMustNameTheTenant. Documents are named in a tenant's Path
// tree, so an admin naming no tenant would silently create /facts/<slug> in the
// default one — the same refusal the collapse makes.
func TestHomeFacts_AnAdminMustNameTheTenant(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	withSqlMem(t, srv)
	rec := homeReq(t, srv, "?scope=user&scope_id=alice&dry_run=false") // no ?tenant=
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	var e map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e["code"] != "tenant_required" {
		t.Errorf("error = %v, want tenant_required", e["code"])
	}
}

// TestHomeFacts_AScopeWithNoSharedDocumentIsNotAFault. A scope written entirely in
// the new shape has no /memory/entities at all; "nothing to do" must read as success
// so an operator sweeping every scope is not told a clean one is broken.
func TestHomeFacts_AScopeWithNoSharedDocumentIsNotAFault(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	withSqlMem(t, srv)
	rep := homeRun(t, srv, "?scope=user&scope_id=nobody&tenant=&dry_run=false")
	if rep.Subjects != 0 || rep.FactsMoved != 0 {
		t.Errorf("report = %+v, want an empty run", rep)
	}
	if !strings.Contains(rep.Note, "nothing to home") {
		t.Errorf("note = %q, want it to say why there was nothing to do", rep.Note)
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
