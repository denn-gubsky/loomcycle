package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// mergeFixture is a tenant scope holding two subjects that name one person, a
// report that cites the duplicate, and the statements a merge has to carry.
type mergeFixture struct {
	d                    *Document
	ctx                  context.Context // the operator: tenant grants, no run
	w                    *recordingWriter
	dupDoc, dupRoot      string
	survDoc, survRoot    string
	fact, grandchild     string // the duplicate's facts
	report, reportChunk  string // a document whose chunk cites the duplicate
	cites, dupToSurv     string // statement ids
	heldOrig, heldOnSurv string // the same statement on both subjects
}

func newMergeFixture(t *testing.T) *mergeFixture {
	t.Helper()
	d, base, _ := documentFixture(t)
	f := &mergeFixture{d: d, w: &recordingWriter{}}
	d.Writer = f.w
	f.ctx = grantedTenantCtx(base)
	mk := func(title, path, key string) (string, string) {
		out, r := docExec(t, d, f.ctx, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"path":%q,"type":"person","subject":%q,"natural_key":%q}`, title, path, title, key))
		if r.IsError {
			t.Fatalf("create %s: %s", title, r.Text)
		}
		return out["document_id"].(string), out["root_chunk_id"].(string)
	}
	f.dupDoc, f.dupRoot = mk("Ada King", "/facts/ada-king", "person:ada-king")
	f.survDoc, f.survRoot = mk("Ada Lovelace", "/facts/ada-lovelace", "person:ada-lovelace")
	chunk := func(docID, parent, title, body string) string {
		in := fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":%q,"body":%q`, docID, title, body)
		if parent != "" {
			in += fmt.Sprintf(`,"parent_id":%q`, parent)
		}
		out, r := docExec(t, d, f.ctx, in+`}`)
		if r.IsError {
			t.Fatalf("create_chunk %s: %s", title, r.Text)
		}
		return out["id"].(string)
	}
	chunk(f.survDoc, "", "Born 1815", "She was born in 1815.")
	f.fact = chunk(f.dupDoc, "", "Wrote the notes", "She wrote the notes on the Engine.")
	f.grandchild = chunk(f.dupDoc, f.fact, "Note G", "Note G holds the first program.")
	out, r := docExec(t, d, f.ctx, `{"op":"create_document","scope":"tenant","title":"Report","path":"/kb/report"}`)
	if r.IsError {
		t.Fatalf("report: %s", r.Text)
	}
	f.report = out["document_id"].(string)
	f.reportChunk = chunk(f.report, "", "Summary", "Ada King wrote the notes.")
	link := func(from, to, kind, quoteChunk, quote string) string {
		in := fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":%q`, from, to, kind)
		if quote != "" {
			in += fmt.Sprintf(`,"evidence":[{"chunk_id":%q,"quote":%q}]`, quoteChunk, quote)
		}
		out, r := docExec(t, d, f.ctx, in+`}`)
		if r.IsError {
			t.Fatalf("link %s: %s", kind, r.Text)
		}
		return out["id"].(string)
	}
	f.cites = link(f.reportChunk, f.dupRoot, "cites", f.reportChunk, "Ada King wrote the notes")
	f.dupToSurv = link(f.dupRoot, f.survRoot, "same_as", "", "")
	f.heldOrig = link(f.reportChunk, f.dupRoot, "mentions_person", "", "")
	f.heldOnSurv = link(f.reportChunk, f.survRoot, "mentions_person", "", "")
	return f
}

func (f *mergeFixture) statement(t *testing.T, id string) *statementRow {
	t.Helper()
	s, err := f.d.statementByID(f.ctx, mustTenantKey(t, f.d, f.ctx), id)
	if err != nil || s == nil {
		t.Fatalf("statement %s: %v", id, err)
	}
	return s
}

func (f *mergeFixture) rootFields(t *testing.T, root string) (status string, fields map[string]any) {
	t.Helper()
	out, r := docExec(t, f.d, f.ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, root))
	if r.IsError {
		t.Fatalf("get_chunk: %s", r.Text)
	}
	fields, _ = out["fields"].(map[string]any)
	status, _ = out["status"].(string)
	return status, fields
}

// TestMergeSubjects_MovesFactsAndRepointsStatements (RFC FR §3, P2).
//
// One merge: the duplicate's facts move under the survivor with their keys; a
// statement that cites the duplicate is copied onto the survivor (revision_of
// naming it, its evidence carried) and the original is expired as merged; a
// statement between the two is only expired; one the survivor already holds is
// only expired; and the duplicate stays, retired, naming the survivor.
func TestMergeSubjects_MovesFactsAndRepointsStatements(t *testing.T) {
	f := newMergeFixture(t)
	key := mustTenantKey(t, f.d, f.ctx)

	res, err := f.d.MergeSubjects(f.ctx, "tenant", "person:ada-king", f.survDoc)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.FactsMoved != 2 || res.StatementsRepointed != 1 || res.StatementsRetired != 2 || res.StatementsLeft != 0 {
		t.Errorf("result = %+v, want 2 facts moved, 1 statement re-pointed, 2 retired, 0 left", res)
	}
	if res.Duplicate.DocumentID != f.dupDoc || res.Survivor.NaturalKey != "person:ada-lovelace" || res.NotAnnounced != "" {
		t.Errorf("result names = %+v", res)
	}

	// Facts moved, keyed and nested as they were.
	for id, parent := range map[string]string{f.fact: f.survRoot, f.grandchild: f.fact} {
		row, ok, err := f.d.getChunkRow(f.ctx, key, id)
		if err != nil || !ok || row.DocumentID != f.survDoc || row.ParentID != parent {
			t.Errorf("chunk %s = %+v (%v), want in the survivor's document under %s", id, row, err, parent)
		}
	}

	// The citing statement: copied onto the survivor, the original expired.
	orig := f.statement(t, f.cites)
	if orig.ExpiredAt == nil || orig.JudgeReason != "merged" {
		t.Errorf("original = %+v, want expired with judge_reason merged", orig)
	}
	cp, err := f.d.statementByKey(f.ctx, key, f.reportChunk, f.survRoot, "cites", nil, "")
	if err != nil || cp == nil {
		t.Fatalf("no copy on the survivor: %v", err)
	}
	if cp.ExpiredAt != nil || cp.Attributes["revision_of"] != f.cites || cp.Method != orig.Method || cp.Status != orig.Status {
		t.Errorf("copy = %+v, want live, revision_of %s, method and status kept", cp, f.cites)
	}
	ev, _, err := f.d.evidenceFor(f.ctx, key, []string{cp.ID}, 0)
	if err != nil || len(ev[cp.ID]) != 1 || ev[cp.ID][0].Quote != "Ada King wrote the notes" {
		t.Errorf("copy evidence = %+v (%v), want the original's quote", ev[cp.ID], err)
	}
	// Between the two: expired, never a survivor→survivor loop.
	if s := f.statement(t, f.dupToSurv); s.ExpiredAt == nil {
		t.Errorf("the statement between the two subjects is still live: %+v", s)
	}
	if loop, _ := f.d.statementByKey(f.ctx, key, f.survRoot, f.survRoot, "same_as", nil, ""); loop != nil {
		t.Errorf("a self-loop was written: %+v", loop)
	}
	// Already held by the survivor: the original expired, the survivor's untouched.
	if s := f.statement(t, f.heldOrig); s.ExpiredAt == nil {
		t.Errorf("the duplicate's copy of a held statement is still live")
	}
	if s := f.statement(t, f.heldOnSurv); s.ExpiredAt != nil {
		t.Errorf("the survivor's own statement was expired")
	}

	// The duplicate retires but stays, keyed, pathed and pointing on.
	status, fields := f.rootFields(t, f.dupRoot)
	if status != subjectMergedStatus || fields[subjectMergedIntoField] != f.survDoc {
		t.Errorf("duplicate root status %q fields %v, want merged into %s", status, fields, f.survDoc)
	}
	if id, _ := f.d.chunkIDByNaturalKey(f.ctx, key, "person:ada-king"); id != f.dupRoot {
		t.Errorf("the duplicate's natural key resolves to %q, want its root %s", id, f.dupRoot)
	}
	if out, r := docExec(t, f.d, f.ctx, `{"op":"get_document","scope":"tenant","path":"/facts/ada-king"}`); r.IsError || out["document_id"] != f.dupDoc {
		t.Errorf("the duplicate's path no longer resolves to it: %v %s", out, r.Text)
	}

	evs := f.w.subjectEvents(t)
	if len(evs) != 1 {
		t.Fatalf("events = %v, want the one merge", evs)
	}
	ev0 := evs[0]
	if ev0["op"] != "merged" || ev0["subject_natural_key"] != "person:ada-king" || ev0["document_id"] != f.dupDoc ||
		ev0["root_chunk_id"] != f.dupRoot || ev0["surviving_key"] != "person:ada-lovelace" || ev0["surviving_document_id"] != f.survDoc {
		t.Errorf("event = %v", ev0)
	}
}

// TestMergeSubjects_RetiredRootStillAnswersAReferenceFromAnotherScope (RFC FR §3,
// P2): a scope that refers to the duplicate by its natural key still finds it after
// the merge — the retired root, which says where the subject went.
func TestMergeSubjects_RetiredRootStillAnswersAReferenceFromAnotherScope(t *testing.T) {
	f := newMergeFixture(t)
	// The user's own dossier on the person, under the duplicate's key.
	user, r := docExec(t, f.d, f.ctx, `{"op":"create_document","scope":"user","title":"Ada King","path":"/facts/ada-king",
		"type":"person","subject":"Ada King","natural_key":"person:ada-king"}`)
	if r.IsError {
		t.Fatalf("user dossier: %s", r.Text)
	}
	if _, err := f.d.MergeSubjects(f.ctx, "tenant", f.dupDoc, f.survDoc); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, r := docExec(t, f.d, f.ctx, fmt.Sprintf(`{"op":"get_document","scope":"user","id":%q,"across_scopes":true}`, user["document_id"]))
	if r.IsError {
		t.Fatalf("get_document: %s", r.Text)
	}
	elsewhere, _ := got["across_scopes"].([]any)
	var tenantRoot string
	for _, e := range elsewhere {
		if m, _ := e.(map[string]any); m["scope"] == "tenant" {
			tenantRoot, _ = m["entity_chunk_id"].(string)
		}
	}
	if tenantRoot != f.dupRoot {
		t.Fatalf("across_scopes = %v, want the tenant's retired root %s", elsewhere, f.dupRoot)
	}
	if status, fields := f.rootFields(t, tenantRoot); status != subjectMergedStatus || fields[subjectMergedIntoField] != f.survDoc {
		t.Errorf("the root it resolves to: status %q fields %v, want merged into %s", status, fields, f.survDoc)
	}
}

// TestMergeSubjects_RefusesWhatIsNotAMerge (RFC FR §3): the same subject twice, a
// document that is not a subject, a subject already merged (either side), and a
// reference from another scope are each refused with their own code, and nothing
// changes.
func TestMergeSubjects_RefusesWhatIsNotAMerge(t *testing.T) {
	f := newMergeFixture(t)
	user, r := docExec(t, f.d, f.ctx, `{"op":"create_document","scope":"user","title":"Bob","path":"/facts/bob",
		"type":"person","subject":"Bob","natural_key":"person:bob"}`)
	if r.IsError {
		t.Fatalf("user subject: %s", r.Text)
	}
	for _, tc := range []struct {
		name, dup, surv string
		want            error
	}{
		{"same subject", f.dupDoc, "person:ada-king", connector.ErrSubjectMergeSame},
		{"not a subject", f.report, f.survDoc, connector.ErrSubjectMergeNotSubject},
		{"a fact's key", f.dupDoc, f.fact, connector.ErrSubjectMergeNotInScope},
		{"another scope's subject", user["document_id"].(string), f.survDoc, connector.ErrSubjectMergeNotInScope},
		{"another scope's key", "person:bob", f.survDoc, connector.ErrSubjectMergeNotInScope},
		{"missing", "", f.survDoc, connector.ErrSubjectMergeInvalid},
	} {
		if _, err := f.d.MergeSubjects(f.ctx, "tenant", tc.dup, tc.surv); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if status, fields := f.rootFields(t, f.dupRoot); status == subjectMergedStatus || fields[subjectMergedIntoField] != nil {
		t.Fatalf("a refused merge changed the duplicate: %q %v", status, fields)
	}
	if evs := f.w.subjectEvents(t); len(evs) != 0 {
		t.Fatalf("a refused merge announced: %v", evs)
	}

	// Merged once; neither side can be merged again.
	if _, err := f.d.MergeSubjects(f.ctx, "tenant", f.dupDoc, f.survDoc); err != nil {
		t.Fatalf("merge: %v", err)
	}
	third, r := docExec(t, f.d, f.ctx, `{"op":"create_document","scope":"tenant","title":"A. A. Lovelace","path":"/facts/aal",
		"type":"person","subject":"A. A. Lovelace","natural_key":"person:aal"}`)
	if r.IsError {
		t.Fatalf("third: %s", r.Text)
	}
	if _, err := f.d.MergeSubjects(f.ctx, "tenant", f.dupDoc, third["document_id"].(string)); !errors.Is(err, connector.ErrSubjectMergeAlreadyMerged) {
		t.Errorf("re-merging the duplicate: err = %v, want already merged", err)
	}
	if _, err := f.d.MergeSubjects(f.ctx, "tenant", third["document_id"].(string), f.dupDoc); !errors.Is(err, connector.ErrSubjectMergeAlreadyMerged) {
		t.Errorf("merging into a merged subject: err = %v, want already merged", err)
	}
	if evs := f.w.subjectEvents(t); len(evs) != 1 {
		t.Errorf("events = %d, want only the one merge", len(evs))
	}
}

// TestMergeSubjects_NeedsWriteOnBothDocuments (RFC FR §9): an operator whose rules
// keep it from writing the survivor is refused before anything — not even the
// duplicate's merged_into — is written. A statement whose other end it may not link
// is left on the duplicate for the links agent rather than re-pointed.
func TestMergeSubjects_NeedsWriteOnBothDocuments(t *testing.T) {
	f := newMergeFixture(t)
	all := authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}}

	noWrite := tools.UpdateRunPolicy(f.ctx, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, all, authz.Rule{Effect: authz.Deny, Actions: []string{"write"}, Resources: []string{"document:tenant:/facts/ada-lovelace"}})
	})
	if _, err := f.d.MergeSubjects(noWrite, "tenant", f.dupDoc, f.survDoc); !errors.Is(err, connector.ErrSubjectMergeForbidden) {
		t.Fatalf("err = %v, want forbidden", err)
	}
	if status, fields := f.rootFields(t, f.dupRoot); status == subjectMergedStatus || fields[subjectMergedIntoField] != nil {
		t.Fatalf("a forbidden merge changed the duplicate: %q %v", status, fields)
	}

	noLink := tools.UpdateRunPolicy(f.ctx, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t, all, authz.Rule{Effect: authz.Deny, Actions: []string{"link"}, Resources: []string{"document:tenant:/kb/**"}})
	})
	res, err := f.d.MergeSubjects(noLink, "tenant", f.dupDoc, f.survDoc)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.StatementsLeft != 2 || res.StatementsRepointed != 0 {
		t.Errorf("result = %+v, want the two report statements left", res)
	}
	if s := f.statement(t, f.cites); s.ExpiredAt != nil || s.ToID != f.dupRoot {
		t.Errorf("a statement the operator may not link was moved: %+v", s)
	}
}

// TestMergeSubjects_WithheldWhenReadingIsPartitioned (RFC FR §2, §10 P2): the merge
// happens, but a caller whose rules hide part of the scope from reading does not
// announce it, and says why.
func TestMergeSubjects_WithheldWhenReadingIsPartitioned(t *testing.T) {
	f := newMergeFixture(t)
	limited := tools.UpdateRunPolicy(f.ctx, func(rp *tools.RunPolicy) {
		rp.ACL = aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}})
	})
	res, err := f.d.MergeSubjects(limited, "tenant", f.dupDoc, f.survDoc)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.NotAnnounced == "" || res.FactsMoved != 2 {
		t.Errorf("result = %+v, want the merge done and the reason it was not announced", res)
	}
	if evs := f.w.subjectEvents(t); len(evs) != 0 {
		t.Errorf("announced under a partitioned scope: %v", evs)
	}
}

// TestMergeSubjects_KeepsTheDuplicatesOtherFieldsExactly: recording merged_into
// rewrites the duplicate root's fields, and must give back every other field as it
// was — a nanosecond timestamp included, which a round trip through float64 would
// round.
func TestMergeSubjects_KeepsTheDuplicatesOtherFieldsExactly(t *testing.T) {
	f := newMergeFixture(t)
	got, r := docExec(t, f.d, f.ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, f.dupRoot))
	if r.IsError {
		t.Fatalf("get_chunk: %s", r.Text)
	}
	if _, r := docExec(t, f.d, f.ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"revision":%v,
		"fields":{"seen_at":1761900000000000123,"alias":"A. King"}}`, f.dupRoot, got["revision"])); r.IsError {
		t.Fatalf("update_chunk: %s", r.Text)
	}
	if _, err := f.d.MergeSubjects(f.ctx, "tenant", f.dupDoc, f.survDoc); err != nil {
		t.Fatalf("merge: %v", err)
	}
	body, err := f.d.readBody(f.ctx, "tenant", mustTenantKey(t, f.d, f.ctx), f.dupRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"seen_at":1761900000000000123`, `"alias":"A. King"`, `"merged_into":"` + f.survDoc + `"`} {
		if !strings.Contains(string(body.Fields), want) {
			t.Errorf("fields = %s, want %s", body.Fields, want)
		}
	}
}
