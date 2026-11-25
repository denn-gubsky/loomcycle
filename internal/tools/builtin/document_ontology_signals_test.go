package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// signalsOntologyMD declares two classes the corpus uses and one it will retire
// (Legacy), a relation kind with a domain and a range (cites) and one it will retire
// (succeeds). memo, clause, gadget and supervises are used and never declared.
const signalsOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Provision\nA rule a regulator sets.\n\n#### Article\n\n### Legacy\nAn older kind of record.\n\n" +
	"## Relations\n@section relations\n\n### cites\n@domain Provision\n@range Provision\n\n### succeeds\nAn older kind.\n"

// Marks no answer may carry: every title and body of the fixture holds one.
const (
	signalsTitleMark = "SIGNALS-TITLE-MARK"
	signalsBodyMark  = "SIGNALS-BODY-MARK"
)

// signalsFixture is a tenant with signalsOntologyMD confirmed and a corpus written
// under it: an existing state, with the retirements made after the rows that use them.
type signalsFixture struct {
	t        *testing.T
	d        *Document
	op       context.Context // the tenant's operator
	user     context.Context // a run of user u1 with an agent, in the same tenant
	tenantID string
	// What the samples may and may not name.
	tenantDocs, tenantChunks, tenantEdges map[string]bool
	otherIDs                              map[string]bool
	edges                                 int64
}

func newSignalsFixture(t *testing.T, d *Document, base context.Context) *signalsFixture {
	t.Helper()
	grant := func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant", "user", "agent"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant", "user", "agent"}}
		rp.Tools = []string{"*"}
	}
	base = tools.UpdateRunPolicy(base, grant)
	op := tools.UpdateRunPolicy(installTenantOntology(t, d, base, signalsOntologyMD, memrank.OntologyConfirmedStatus), grant)
	f := &signalsFixture{t: t, d: d, op: op, user: base, tenantID: sqlScopeTenant(op),
		tenantDocs: map[string]bool{}, tenantChunks: map[string]bool{}, tenantEdges: map[string]bool{}, otherIDs: map[string]bool{}}
	f.populate()
	return f
}

func (f *signalsFixture) key(scope string) sqlmem.ScopeKey {
	id := map[string]string{"tenant": f.tenantID, "user": "u1", "agent": tools.AgentName(f.user)}[scope]
	return sqlmem.ScopeKey{Tenant: f.tenantID, Scope: scope, ScopeID: id}
}

func (f *signalsFixture) exec(ctx context.Context, v map[string]any) map[string]any {
	f.t.Helper()
	b, _ := json.Marshal(v)
	out, r := docExec(f.t, f.d, ctx, string(b))
	if r.IsError {
		f.t.Fatalf("%v: %s", v["op"], r.Text)
	}
	return out
}

// doc creates a document of typ in scope and returns its id and root chunk.
func (f *signalsFixture) doc(ctx context.Context, scope, typ string) (id, root string) {
	f.t.Helper()
	out := f.exec(ctx, map[string]any{"op": "create_document", "scope": scope, "title": signalsTitleMark + " doc", "type": typ})
	id, root = out["document_id"].(string), out["root_chunk_id"].(string)
	if scope == "tenant" {
		f.tenantDocs[id], f.tenantChunks[root] = true, true
	} else {
		f.otherIDs[id], f.otherIDs[root] = true, true
	}
	return id, root
}

// part adds a chunk of typ to a document.
func (f *signalsFixture) part(ctx context.Context, scope, docID, typ string) string {
	f.t.Helper()
	id := f.exec(ctx, map[string]any{"op": "create_chunk", "scope": scope, "document_id": docID,
		"title": signalsTitleMark + " part", "type": typ, "body": signalsBodyMark})["id"].(string)
	if scope == "tenant" {
		f.tenantChunks[id] = true
	} else {
		f.otherIDs[id] = true
	}
	return id
}

// edge stores a statement row as it is: the rows a bulk import, a consolidator or a
// write from before a rule existed left behind, which no authored call can make now
// (an authored statement of an undeclared kind is refused). Each gets its own valid_at:
// a scope holds one statement per (from, to, kind, valid_at).
func (f *signalsFixture) edge(scope, from, to, kind, status, attributes string, expired bool) string {
	f.t.Helper()
	id := newDocID()
	f.edges++
	var attrs, exp any
	if attributes != "" {
		attrs = attributes
	}
	if expired {
		exp = time.Now().UnixNano()
	}
	if _, err := f.d.docSQL().Exec(context.Background(), f.key(scope), f.d.docSQL().Rebind(
		`INSERT INTO chunk_edges (id, from_id, to_id, kind, created_at, status, method, attributes, expired_at, valid_at) VALUES (?, ?, ?, ?, ?, ?, 'manual', ?, ?, ?)`),
		[]any{id, from, to, kind, time.Now().UnixNano(), status, attrs, exp, f.edges}, 0); err != nil {
		f.t.Fatalf("insert %s statement: %v", kind, err)
	}
	if scope == "tenant" {
		f.tenantEdges[id] = true
	} else {
		f.otherIDs[id] = true
	}
	return id
}

func (f *signalsFixture) ontologyChunk(title string) string {
	f.t.Helper()
	read, err := f.d.OntologyTermsFromTree(f.op, "tenant", memrank.OntologyPath)
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := f.d.query(f.op, f.key("tenant"), `SELECT id FROM chunks WHERE document_id = ? AND title = ?`, read.DocumentID, title)
	if err != nil || len(res.Rows) != 1 {
		f.t.Fatalf("ontology chunk %q: %v %v", title, err, res)
	}
	return asStr(res.Rows[0][0])
}

func (f *signalsFixture) setStatus(id, status string, fingerprint bool) {
	f.t.Helper()
	g := f.exec(f.op, map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})
	v := map[string]any{"op": "update_chunk", "scope": "tenant", "id": id, "revision": int(g["revision"].(float64)), "status": status}
	if fingerprint {
		plan, err := f.d.PlanOntology(f.op, []connector.OntologyChange{{Op: connector.OntologyPlanRetire, Entry: id}})
		if err != nil {
			f.t.Fatalf("plan: %v", err)
		}
		v["plan_fingerprint"] = plan.Fingerprint
	}
	f.exec(f.op, v)
}

// populate writes the corpus, then retires Legacy and succeeds, which it uses.
func (f *signalsFixture) populate() {
	f.t.Helper()
	// Tenant scope. Declared: two Provision documents and a cites statement between them.
	_, p1 := f.doc(f.op, "tenant", "Provision")
	_, p2 := f.doc(f.op, "tenant", "Provision")
	f.exec(f.op, map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": p1, "to_id": p2, "kind": "cites"})
	// Undeclared document type, in two spellings; undeclared chunk type in one document.
	memo, memoRoot := f.doc(f.op, "tenant", "memo")
	f.doc(f.op, "tenant", "memo")
	f.doc(f.op, "tenant", "Memo")
	c1 := f.part(f.op, "tenant", memo, "clause")
	c2 := f.part(f.op, "tenant", memo, "clause")
	// A type and a kind in use that are retired below.
	legacy, legacyRoot := f.doc(f.op, "tenant", "Legacy")
	f.part(f.op, "tenant", legacy, "Legacy")
	f.edge("tenant", legacyRoot, p1, "succeeds", edgeStatusAsserted, "", false)
	// Undeclared kind: three statements, two statuses, two pairs of ends.
	f.edge("tenant", c1, c2, "supervises", edgeStatusProposed, "", false)
	f.edge("tenant", c2, c1, "supervises", edgeStatusProposed, `{"ontology_valid":false}`, false)
	f.edge("tenant", memoRoot, c1, "supervises", edgeStatusAsserted, "", false)
	// A declared kind marked invalid between clause and memo: two live ones counted;
	// the same row valid, unmarked, rejected and retired are not.
	f.edge("tenant", c1, memoRoot, "cites", edgeStatusProposed, `{"ontology_valid":false}`, false)
	f.edge("tenant", c2, memoRoot, "cites", edgeStatusProposed, `{"ontology_valid":false,"aspect":"x"}`, false)
	f.edge("tenant", c1, memoRoot, "cites", edgeStatusProposed, `{"ontology_valid":true}`, false)
	f.edge("tenant", c2, memoRoot, "cites", edgeStatusProposed, "", false)
	f.edge("tenant", c1, memoRoot, "cites", edgeStatusRejected, `{"ontology_valid":false}`, false)
	f.edge("tenant", c2, memoRoot, "cites", edgeStatusProposed, `{"ontology_valid":false}`, true)

	// User scope: a memo document, two facts of an undeclared type, statements of the
	// undeclared kind. Agent scope: a memo document.
	f.doc(f.user, "user", "memo")
	holder, _ := f.doc(f.user, "user", "Provision")
	for _, name := range []string{"one", "two"} {
		id := f.exec(f.user, map[string]any{"op": "upsert_chunk", "scope": "user", "document_id": holder, "natural_key": "object:" + name,
			"type": "object", "subject": name, "title": signalsTitleMark + " " + name, "body": signalsBodyMark})["id"].(string)
		// A fact typed before the ontology was confirmed: a write of an undeclared
		// entity type is refused now, so the stored type is set as such a row has it.
		if _, err := f.d.docSQL().Exec(context.Background(), f.key("user"), f.d.docSQL().Rebind(`UPDATE chunks SET type = 'gadget' WHERE id = ?`), []any{id}, 0); err != nil {
			f.t.Fatalf("retype fact: %v", err)
		}
		f.otherIDs[id] = true
	}
	for i := 0; i < 4; i++ {
		f.edge("user", "nowhere-a", "nowhere-b", "supervises", edgeStatusProposed, "", false)
	}
	f.doc(f.user, "agent", "memo")

	f.setStatus(f.ontologyChunk("Legacy"), memrank.OntologyStatusRetired, true)
	f.setStatus(f.ontologyChunk("succeeds"), memrank.OntologyStatusRetired, true)
}

// signals runs the op as ctx and decodes its answer.
func (f *signalsFixture) signals(ctx context.Context, d *Document, args map[string]any) (OntologySignals, string) {
	f.t.Helper()
	v := map[string]any{"op": ontologySignalsOp}
	for k, a := range args {
		v[k] = a
	}
	b, _ := json.Marshal(v)
	r, err := d.Execute(ctx, b)
	if err != nil || r.IsError {
		f.t.Fatalf("ontology_signals: %v %s", err, r.Text)
	}
	var out OntologySignals
	if err := json.Unmarshal([]byte(r.Text), &out); err != nil {
		f.t.Fatalf("decode: %v in %s", err, r.Text)
	}
	return out, r.Text
}

func (f *signalsFixture) read() OntologySignals {
	f.t.Helper()
	out, _ := f.signals(f.op, f.d, nil)
	return out
}

// direct sums a count(*) over the tenant, user and agent scopes.
func (f *signalsFixture) direct(stmt string, args ...any) (tenant, other int) {
	f.t.Helper()
	for _, scope := range []string{"tenant", "user", "agent"} {
		res, err := f.d.docSQL().Query(context.Background(), f.key(scope), f.d.docSQL().Rebind(stmt), args)
		if err != nil {
			f.t.Fatalf("%s in %s: %v", stmt, scope, err)
		}
		if scope == "tenant" {
			tenant += asInt(res.Rows[0][0])
		} else {
			other += asInt(res.Rows[0][0])
		}
	}
	return tenant, other
}

func signalType(s OntologySignals, typ, population string) *UndeclaredTypeSignal {
	for i := range s.UndeclaredTypes {
		if strings.EqualFold(s.UndeclaredTypes[i].Type, typ) && s.UndeclaredTypes[i].Population == population {
			return &s.UndeclaredTypes[i]
		}
	}
	return nil
}

func signalKind(s OntologySignals, kind string) *UndeclaredKindSignal {
	for i := range s.UndeclaredKinds {
		if s.UndeclaredKinds[i].Kind == kind {
			return &s.UndeclaredKinds[i]
		}
	}
	return nil
}

// signalsOnBothTiers runs a check on the sqlite tier and, where the aux database is
// set, on postgres.
func signalsOnBothTiers(t *testing.T, check func(t *testing.T, f *signalsFixture)) {
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		check(t, newSignalsFixture(t, d, ctx))
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		check(t, newSignalsFixture(t, d, tools.WithAgentName(tools.WithRunIdentity(ctx,
			tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"}), "doc-agent")))
	})
}

// TestOntologySignals_CountsEqualDirectQueries — every count the op reports equals what
// a direct query finds over the tenant, user and agent scopes, on both tiers.
func TestOntologySignals_CountsEqualDirectQueries(t *testing.T) {
	signalsOnBothTiers(t, func(t *testing.T, f *signalsFixture) {
		s := f.read()
		if !s.Complete || s.Ontology.DocumentID == "" || !s.Ontology.Confirmed || len(s.Ontology.Fingerprint) != 64 {
			t.Fatalf("header %+v complete %v, want the confirmed ontology and a whole count", s.Ontology, s.Complete)
		}
		notRoot := ` AND c.id NOT IN (SELECT root_chunk_id FROM documents)`
		for _, c := range []struct {
			typ, population, stmt string
			tenant, other, docs   int
		}{
			{"memo", signalPopDocument, `SELECT count(*) FROM documents WHERE type IN ('memo', 'Memo')`, 3, 2, 5},
			{"clause", signalPopChunk, `SELECT count(*) FROM chunks c WHERE c.type = 'clause'` + notRoot +
				` AND c.id NOT IN (SELECT chunk_id FROM chunk_memory_meta)`, 2, 0, 1},
			{"gadget", signalPopFact, `SELECT count(*) FROM chunks c WHERE c.type = 'gadget'` + notRoot +
				` AND c.id IN (SELECT chunk_id FROM chunk_memory_meta)`, 0, 2, 1},
		} {
			got := signalType(s, c.typ, c.population)
			tenant, other := f.direct(c.stmt)
			if got == nil || got.TenantScope != tenant || got.OtherScopes != other || got.N != tenant+other ||
				tenant != c.tenant || other != c.other || got.Documents != c.docs {
				t.Errorf("%s %s = %+v; direct tenant %d other %d, want %d + %d over %d documents", c.population, c.typ, got, tenant, other, c.tenant, c.other, c.docs)
			}
		}
		if n := len(s.UndeclaredTypes); n != 3 {
			t.Errorf("%d undeclared types %+v, want memo, clause and gadget only", n, s.UndeclaredTypes)
		}

		tenant, other := f.direct(`SELECT count(*) FROM chunk_edges WHERE kind = 'supervises'`)
		k := signalKind(s, "supervises")
		if k == nil || k.N != tenant+other || k.TenantScope != tenant || k.OtherScopes != other || tenant != 3 || other != 4 ||
			!reflect.DeepEqual(k.ByStatus, map[string]int{"proposed": 6, "asserted": 1}) {
			t.Errorf("supervises = %+v; direct tenant %d other %d, want 3 + 4, six proposed and one asserted", k, tenant, other)
		}
		if k != nil && !reflect.DeepEqual(k.Ends, []SignalEnds{{"", "", 4}, {"clause", "clause", 2}, {"memo", "clause", 1}}) {
			t.Errorf("supervises ends = %+v, want the untyped user rows, clause to clause twice, memo to clause once", k.Ends)
		}

		// Counted by the text of the attribute, not by the tier's JSON reader.
		marked, _ := f.direct(`SELECT count(*) FROM chunk_edges WHERE kind = 'cites' AND attributes LIKE '%"ontology_valid":false%'
			AND status <> 'rejected' AND expired_at IS NULL`)
		want := []InvalidEndsSignal{{Kind: "cites", From: "clause", To: "memo", N: marked, Domain: []string{"Provision"}, Range: []string{"Provision"}}}
		got := append([]InvalidEndsSignal{}, s.InvalidEnds...)
		for i := range got {
			if len(got[i].Sample) != 2 {
				t.Errorf("invalid ends sample %v, want the two marked statements", got[i].Sample)
			}
			got[i].Sample = nil
		}
		if marked != 2 || !reflect.DeepEqual(got, want) {
			t.Errorf("invalid_ends = %+v, want %+v (direct %d)", got, want, marked)
		}
		if len(s.Truncated) != 0 {
			t.Errorf("truncated = %v, want nothing cut", s.Truncated)
		}
	})
}

// TestOntologySignals_UndeclaredKindIsListedAndADeclaredOneIsNot — statements that
// differ only in their kind: the undeclared one is listed with its sample, the declared
// kind, the seed kind and the reserved kind are not.
func TestOntologySignals_UndeclaredKindIsListedAndADeclaredOneIsNot(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newSignalsFixture(t, d, ctx)
	for _, kind := range []string{"supervises", "cites", "related", "about"} {
		f.edge("tenant", "pair-a", "pair-b", kind, edgeStatusAsserted, "", false)
	}
	s := f.read()
	var kinds []string
	for _, k := range s.UndeclaredKinds {
		kinds = append(kinds, k.Kind)
	}
	if !reflect.DeepEqual(kinds, []string{"supervises"}) {
		t.Fatalf("undeclared kinds = %v, want supervises alone (cites is declared, related is a seed kind, about is reserved)", kinds)
	}
	k := s.UndeclaredKinds[0]
	if k.N != 8 || k.TenantScope != 4 || len(k.Sample) != 4 {
		t.Errorf("supervises = %+v, want 8 with the tenant scope's four as the sample", k)
	}

	// A tenant with documents and no ontology document is read against the seed: the
	// seed kind and the reserved kind are declared there too.
	bare := tools.WithRunIdentity(f.op, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "bare"})
	f.exec(bare, map[string]any{"op": "create_document", "scope": "tenant", "title": "x"})
	for i, kind := range []string{"supervises", "related", "about"} {
		if _, err := d.docSQL().Exec(context.Background(), sqlmem.ScopeKey{Tenant: "bare", Scope: "tenant", ScopeID: "bare"}, d.docSQL().Rebind(
			`INSERT INTO chunk_edges (id, from_id, to_id, kind, created_at, status, method) VALUES (?, 'a', 'b', ?, 1, 'asserted', 'manual')`),
			[]any{fmt.Sprint("e", i), kind}, 0); err != nil {
			t.Fatal(err)
		}
	}
	s, _ = f.signals(bare, d, nil)
	if s.Ontology.DocumentID != "" || s.Ontology.Fingerprint != "" || len(s.UndeclaredKinds) != 1 || s.UndeclaredKinds[0].Kind != "supervises" {
		t.Errorf("no ontology document: %+v kinds %+v; want no document and supervises alone", s.Ontology, s.UndeclaredKinds)
	}
}

// TestOntologySignals_RetiredNameIsNotUndeclared — a type and a kind whose entries were
// retired after rows used them stay declared; the never-declared ones beside them are
// listed.
func TestOntologySignals_RetiredNameIsNotUndeclared(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newSignalsFixture(t, d, ctx)
	read, err := d.OntologyTermsFromTree(f.op, "tenant", memrank.OntologyPath)
	if err != nil {
		t.Fatal(err)
	}
	retired := 0
	for _, term := range read.Terms {
		if term.Name == "Legacy" && term.EntryStatus == memrank.OntologyStatusRetired {
			retired++
		}
	}
	for _, r := range read.Relations {
		if r.Name == "succeeds" && r.EntryStatus == memrank.OntologyStatusRetired {
			retired++
		}
	}
	if tenant, _ := f.direct(`SELECT count(*) FROM documents WHERE type = 'Legacy'`); retired != 2 || tenant != 1 {
		t.Fatalf("fixture: %d retired entries, %d Legacy documents; want Legacy and succeeds retired and in use", retired, tenant)
	}
	s := f.read()
	for _, pop := range []string{signalPopDocument, signalPopChunk} {
		if got := signalType(s, "Legacy", pop); got != nil {
			t.Errorf("retired class Legacy is listed as an undeclared %s type: %+v", pop, got)
		}
	}
	if got := signalKind(s, "succeeds"); got != nil {
		t.Errorf("retired kind succeeds is listed as undeclared: %+v", got)
	}
	if signalType(s, "memo", signalPopDocument) == nil || signalKind(s, "supervises") == nil {
		t.Errorf("the never-declared memo and supervises are not listed: %+v %+v", s.UndeclaredTypes, s.UndeclaredKinds)
	}
}

// TestOntologySignals_CountsCoverUserScopesAndSampleOnlyTheTenant — rows in user and
// agent scopes are counted and never sampled; another tenant's are not counted.
func TestOntologySignals_CountsCoverUserScopesAndSampleOnlyTheTenant(t *testing.T) {
	signalsOnBothTiers(t, func(t *testing.T, f *signalsFixture) {
		s := f.read()
		if s.ScopesCounted != 2 || s.ScopesSkipped != 0 {
			t.Errorf("scopes counted %d skipped %d, want the user and the agent scope", s.ScopesCounted, s.ScopesSkipped)
		}
		memo, gadget, kind := signalType(s, "memo", signalPopDocument), signalType(s, "gadget", signalPopFact), signalKind(s, "supervises")
		if memo == nil || gadget == nil || kind == nil {
			t.Fatalf("signals missing: %+v", s)
		}
		if memo.OtherScopes != 2 || gadget.OtherScopes != 2 || kind.OtherScopes != 4 {
			t.Errorf("other_scopes: memo %d gadget %d supervises %d, want 2, 2 and 4", memo.OtherScopes, gadget.OtherScopes, kind.OtherScopes)
		}
		if len(memo.Sample) != 3 || len(gadget.Sample) != 0 || len(kind.Sample) != 3 {
			t.Errorf("samples: memo %v gadget %v supervises %v; want the tenant's three, none, the tenant's three", memo.Sample, gadget.Sample, kind.Sample)
		}
		for _, ty := range s.UndeclaredTypes {
			for _, row := range ty.Sample {
				if !f.tenantDocs[row.DocumentID] || (row.ChunkID != "" && !f.tenantChunks[row.ChunkID]) {
					t.Errorf("%s sample %+v is not a row of the tenant scope", ty.Type, row)
				}
			}
		}
		for _, id := range append(append([]string{}, kind.Sample...), s.InvalidEnds[0].Sample...) {
			if !f.tenantEdges[id] {
				t.Errorf("statement sample %s is not a statement of the tenant scope", id)
			}
		}

		other := tools.UpdateRunPolicy(tools.WithRunIdentity(f.user, tools.RunIdentityValue{AgentID: "a", UserID: "u9", TenantID: "other"}),
			func(rp *tools.RunPolicy) {
				rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user"}}
				rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"user"}}
			})
		for i := 0; i < 3; i++ {
			f.exec(other, map[string]any{"op": "create_document", "scope": "user", "title": "x", "type": "memo"})
		}
		after := f.read()
		if got := signalType(after, "memo", signalPopDocument); got.N != memo.N || after.ScopesCounted != s.ScopesCounted {
			t.Errorf("memo %d → %d, scopes %d → %d after another tenant wrote three memo documents", memo.N, got.N, s.ScopesCounted, after.ScopesCounted)
		}
	})
}

// TestOntologySignals_ReturnsNoTitleOrBody — the answer carries ids and counts only:
// no title or body of the corpus, and no field that could hold one.
func TestOntologySignals_ReturnsNoTitleOrBody(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newSignalsFixture(t, d, ctx)
	s, text := f.signals(f.op, f.d, nil)
	if len(s.UndeclaredTypes) == 0 || len(s.UndeclaredTypes[0].Sample) == 0 {
		t.Fatalf("no sample to check: %s", text)
	}
	for _, mark := range []string{signalsTitleMark, signalsBodyMark, `"title"`, `"body"`, `"quote"`} {
		if strings.Contains(text, mark) {
			t.Errorf("the answer carries %s: %s", mark, text)
		}
	}
	var raw struct {
		UndeclaredTypes []map[string]any `json:"undeclared_types"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		t.Fatal(err)
	}
	for _, ty := range raw.UndeclaredTypes {
		for _, row := range ty["sample"].([]any) {
			var keys []string
			for k := range row.(map[string]any) {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if got := strings.Join(keys, ","); got != "chunk_id,document_id" && got != "document_id" {
				t.Errorf("a sample row has fields %s, want ids only", got)
			}
		}
	}
}

// TestOntologySignals_WritesNothingAndProvisionsNothing — the op, through Execute, from
// a process that has provisioned nothing and for a caller whose own user scope does not
// exist, leaves the ontology's fingerprint, every document table's row count, the set
// of scopes and the scopes' own state (a last-use stamp, provisioning DDL) as they were.
func TestOntologySignals_WritesNothingAndProvisionsNothing(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		root := t.TempDir()
		d, ctx, _ := documentFixture(t)
		mgr, err := sqlmem.New(sqlmem.Config{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = mgr.Close() })
		d.SqlMem = mgr
		fresh, err := sqlmem.New(sqlmem.Config{Root: root, ScopeTTLMS: 3600_000})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = fresh.Close() })
		prepare, traces := sqliteScopeTraces(t, root)
		assertSignalsWriteNothing(t, d, ctx, fresh, prepare, traces)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		dsn := os.Getenv("LOOMCYCLE_TEST_SQLMEM_PG_DSN")
		fresh, err := sqlmem.NewPostgres(context.Background(),
			sqlmem.Config{PgDSN: dsn, StatementTimeoutMS: 30000, MaxRows: 1000, ScopeTTLMS: 3600_000})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = fresh.Close() })
		raw, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		prepare, traces := pgScopeTraces(t, raw)
		assertSignalsWriteNothing(t, d, tools.WithAgentName(tools.WithRunIdentity(ctx,
			tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt"}), "doc-agent"), fresh, prepare, traces)
	})
}

func assertSignalsWriteNothing(t *testing.T, d *Document, base context.Context, fresh *sqlmem.Manager, prepare func(), traces func() string) {
	f := newSignalsFixture(t, d, base)
	docID, err := d.resolveOntologyID(f.op, f.key("tenant"))
	if err != nil || docID == "" {
		t.Fatalf("ontology id: %q %v", docID, err)
	}
	snapshot := func() string {
		fp, err := d.ontologyFingerprint(f.op, f.key("tenant"), docID)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		b.WriteString(fp)
		scopes, err := d.SqlMem.ListScopes(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "|scopes=%d", len(scopes))
		for _, scope := range []string{"tenant", "user"} {
			for _, table := range documentTableNames {
				res, err := d.docSQL().Query(context.Background(), f.key(scope), `SELECT count(*) FROM `+table, nil)
				if err != nil {
					continue // an index name, or a table this scope never made
				}
				fmt.Fprintf(&b, "|%s.%s=%d", scope, table, asInt(res.Rows[0][0]))
			}
		}
		return b.String()
	}
	was := snapshot()
	if !strings.Contains(was, "tenant.chunk_edges=") || !strings.Contains(was, "user.documents=") {
		t.Fatalf("the snapshot reads no tables: %s", was)
	}
	prepare()
	wasTraces := traces()
	// The operator of the same tenant, as a subject with no scope of its own: the
	// call's default scope is that subject's user scope, which must not be made.
	caller := tools.WithRunIdentity(f.op, tools.RunIdentityValue{AgentID: "a", UserID: "nobody", TenantID: f.tenantID})
	s, _ := f.signals(caller, &Document{Store: d.Store, SqlMem: fresh, Bus: d.Bus}, nil)
	if now := traces(); now != wasTraces {
		t.Errorf("the read left a trace on the scopes (a last-use stamp or provisioning DDL):\nbefore %s\nafter  %s", wasTraces, now)
	}
	if now := snapshot(); now != was {
		t.Errorf("the read wrote:\nbefore %s\nafter  %s", was, now)
	}
	if !s.Complete || s.ScopesCounted != 2 || signalKind(s, "supervises") == nil {
		t.Errorf("the read counted %d scopes, complete %v, kinds %+v; want both scopes read", s.ScopesCounted, s.Complete, s.UndeclaredKinds)
	}
}

// TestOntologySignals_MemberIsRefused — the same call from the same tenant: the
// operator is answered; a member, a run and a run started by the operator are refused.
func TestOntologySignals_MemberIsRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newSignalsFixture(t, d, ctx)
	if s := f.read(); len(s.UndeclaredKinds) == 0 {
		t.Fatalf("the operator's read is empty: %+v", s)
	}
	for name, caller := range map[string]context.Context{
		"member":              tools.WithCallerTier(f.op, tools.TierMember),
		"run":                 f.user,
		"run of the operator": inRunCtx(f.op),
	} {
		out, r := docExec(t, d, caller, `{"op":"ontology_signals","scope":"tenant"}`)
		if !r.IsError || !strings.Contains(r.Text, "permission") || out != nil || strings.Contains(r.Text, "supervises") {
			t.Errorf("%s: %q, want a permission refusal that names nothing", name, r.Text)
		}
	}
}

// TestOntologySignals_BudgetCutIsReportedIncomplete — with the scope budget spent, the
// user and agent scopes are reported skipped and the answer incomplete, with nothing of
// them counted; with the budget it has, the same read is complete.
func TestOntologySignals_BudgetCutIsReportedIncomplete(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newSignalsFixture(t, d, ctx)
	whole := f.read()
	d.planScopeBudget = time.Nanosecond
	cut := f.read()
	d.planScopeBudget = 0
	if !whole.Complete || whole.ScopesCounted != 2 || whole.ScopesSkipped != 0 {
		t.Errorf("whole read: complete %v counted %d skipped %d", whole.Complete, whole.ScopesCounted, whole.ScopesSkipped)
	}
	if cut.Complete || cut.ScopesCounted != 0 || cut.ScopesSkipped != 2 {
		t.Errorf("cut read: complete %v counted %d skipped %d; want incomplete with both scopes skipped", cut.Complete, cut.ScopesCounted, cut.ScopesSkipped)
	}
	memo, kind := signalType(cut, "memo", signalPopDocument), signalKind(cut, "supervises")
	if memo == nil || kind == nil || memo.OtherScopes != 0 || memo.TenantScope != 3 || kind.OtherScopes != 0 || kind.TenantScope != 3 {
		t.Errorf("cut read: memo %+v supervises %+v; want the tenant's rows alone", memo, kind)
	}
	if signalType(cut, "gadget", signalPopFact) != nil {
		t.Error("cut read lists gadget, which only a skipped scope holds")
	}
}

// TestOntologySignals_ProposalStatusIsReported — a candidate says whether the ontology
// document already holds a proposal of its name: none, an open one, a rejected one.
func TestOntologySignals_ProposalStatusIsReported(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newSignalsFixture(t, d, ctx)
	s := f.read()
	if memo, kind := signalType(s, "memo", signalPopDocument), signalKind(s, "supervises"); memo.Proposal != "" || kind.Proposal != "" {
		t.Fatalf("before any proposal: memo %q supervises %q, want none", memo.Proposal, kind.Proposal)
	}
	f.exec(f.op, map[string]any{"op": "propose_entity", "name": "Memo"})
	rejected := f.exec(f.op, map[string]any{"op": "propose_entity", "section": "relations", "name": "supervises"})["chunk_id"].(string)
	f.setStatus(rejected, memrank.OntologyStatusRejected, false)
	s = f.read()
	if got := signalType(s, "memo", signalPopDocument); got == nil || got.Proposal != memrank.OntologyStatusProposed {
		t.Errorf("memo = %+v, want proposal proposed (the proposal is spelled Memo)", got)
	}
	if got := signalKind(s, "supervises"); got == nil || got.Proposal != memrank.OntologyStatusRejected {
		t.Errorf("supervises = %+v, want proposal rejected", got)
	}
	if got := signalType(s, "clause", signalPopChunk); got == nil || got.Proposal != "" {
		t.Errorf("clause = %+v, want no proposal", got)
	}
}

// TestOntologySignals_LimitCutIsNamedInTruncated — a list cut at the group limit keeps
// its largest groups and is named; a scope other than the tenant's, and a limit past
// the maximum, are refused.
func TestOntologySignals_LimitCutIsNamedInTruncated(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newSignalsFixture(t, d, ctx)
	s, _ := f.signals(f.op, f.d, map[string]any{"limit": 1})
	if len(s.UndeclaredTypes) != 1 || s.UndeclaredTypes[0].Type != "memo" || !reflect.DeepEqual(s.Truncated, []string{signalUndeclaredTypes}) {
		t.Errorf("limit 1: types %+v truncated %v; want memo alone and undeclared_types named", s.UndeclaredTypes, s.Truncated)
	}
	for _, body := range []string{`{"op":"ontology_signals","scope":"user"}`, `{"op":"ontology_signals","limit":101}`} {
		if _, r := docExec(t, d, f.op, body); !r.IsError {
			t.Errorf("%s was answered: %s", body, r.Text)
		}
	}
}
