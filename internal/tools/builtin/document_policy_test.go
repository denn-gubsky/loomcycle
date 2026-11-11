package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// policyOntologyMD is a file plan: Records (10y, compliance) > Risk (risk) >
// Outsourcing (5y, ghosts — a group the tenant does not have), and Notes (1y, legal).
const policyOntologyMD = "# Ontology\n\n" +
	"## Classes\n@section classes\n\n### Document\n- `title` text\n\n" +
	"## Schemes\n@section schemes\n\n### file-plan\n\n" +
	"#### Records\n@retention 10y\n@acl compliance\n\n" +
	"##### Risk\n@acl risk\n\n" +
	"###### Outsourcing\n@retention 5y\n@acl ghosts\n\n" +
	"#### Notes\n@retention 1y\n@acl legal\n"

const (
	tagRecords     = "file-plan/records"
	tagRisk        = "file-plan/records/risk"
	tagOutsourcing = "file-plan/records/risk/outsourcing"
	tagNotes       = "file-plan/notes"
)

type filePlanFixture struct {
	t        *testing.T
	d        *Document
	ctx      context.Context // operator plane, tenant + user scopes
	concepts map[string]describedConcept
	desc     ontologyDescription
}

// newFilePlanFixture installs the file plan with groups compliance {u1, u2}, risk {u1}
// and legal {u2}: nobody is in risk and legal both.
func newFilePlanFixture(t *testing.T, status string) *filePlanFixture {
	t.Helper()
	d, base, _ := documentFixture(t)
	ctx := operatorCtx(installTenantOntology(t, d, base, policyOntologyMD, status))
	for g, members := range map[string][]string{"compliance": {"u1", "u2"}, "risk": {"u1"}, "legal": {"u2"}} {
		if _, err := d.Store.GroupPut(ctx, store.GroupRow{TenantID: "tnt", Name: g}); err != nil {
			t.Fatal(err)
		}
		for _, m := range members {
			if err := d.Store.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "tnt", GroupName: g,
				MemberKind: store.GroupMemberUser, MemberID: m}); err != nil {
				t.Fatal(err)
			}
		}
	}
	f := &filePlanFixture{t: t, d: d, ctx: ctx, concepts: map[string]describedConcept{}}
	f.desc = describeOntologyVia(t, d, ctx)
	for _, s := range f.desc.Schemes {
		for _, c := range s.Concepts {
			f.concepts[c.Tag] = c
		}
	}
	return f
}

func (f *filePlanFixture) do(ctx context.Context, v map[string]any) (map[string]any, tools.Result) {
	f.t.Helper()
	b, _ := json.Marshal(v)
	return docExec(f.t, f.d, ctx, string(b))
}

func (f *filePlanFixture) must(v map[string]any) map[string]any {
	f.t.Helper()
	out, r := f.do(f.ctx, v)
	if r.IsError {
		f.t.Fatalf("%v: %s", v["op"], r.Text)
	}
	return out
}

func (f *filePlanFixture) doc(scope, path string) (docID, rootID string) {
	f.t.Helper()
	out := f.must(map[string]any{"op": "create_document", "scope": scope, "title": "Doc", "path": path})
	return out["document_id"].(string), out["root_chunk_id"].(string)
}

func (f *filePlanFixture) policy(ctx context.Context, scope, docID string) effectivePolicyAnswer {
	f.t.Helper()
	_, r := f.do(ctx, map[string]any{"op": "effective_policy", "scope": scope, "document_id": docID})
	if r.IsError {
		f.t.Fatalf("effective_policy: %s", r.Text)
	}
	var a effectivePolicyAnswer
	if err := json.Unmarshal([]byte(r.Text), &a); err != nil {
		f.t.Fatalf("decode: %v\n%s", err, r.Text)
	}
	return a
}

func conceptTags(a effectivePolicyAnswer) []string {
	var out []string
	for _, c := range a.Concepts {
		out = append(out, c.Tag)
	}
	return out
}

// describe_ontology reports each concept's effective retention (own, else the nearest
// ancestor's) and groups (accumulated down the scheme; an unknown group is left out and
// reported), and says nothing is enforced.
func TestDescribeOntology_ReportsEffectiveRetentionAndAccumulatedGroups(t *testing.T) {
	f := newFilePlanFixture(t, memrank.OntologyConfirmedStatus)

	risk := f.concepts[tagRisk]
	if risk.EffectiveRetention != "10y" || !risk.InheritedRetention || risk.RetentionFrom != tagRecords {
		t.Errorf("Risk retention = %q inherited=%v from %q, want 10y from %s", risk.EffectiveRetention, risk.InheritedRetention, risk.RetentionFrom, tagRecords)
	}
	if want := []string{"compliance", "risk"}; !reflect.DeepEqual(risk.EffectiveGroups, want) {
		t.Errorf("Risk groups = %v, want %v", risk.EffectiveGroups, want)
	}
	out := f.concepts[tagOutsourcing]
	if out.EffectiveRetention != "5y" || out.InheritedRetention || out.RetentionFrom != "" {
		t.Errorf("Outsourcing retention = %+v, want its own 5y", out)
	}
	if want := []string{"compliance", "risk"}; !reflect.DeepEqual(out.EffectiveGroups, want) {
		t.Errorf("Outsourcing groups = %v, want %v (ghosts is not a group)", out.EffectiveGroups, want)
	}
	p := f.desc.Policy
	if p == nil || p.Enforced || !strings.Contains(p.Note, "Reported only") {
		t.Fatalf("policy = %+v, want an unenforced report", p)
	}
	if !strings.Contains(strings.Join(p.Issues, "\n"), tagOutsourcing+`: @acl "ghosts" is not a group of this tenant`) {
		t.Errorf("the unknown group was not reported: %v", p.Issues)
	}
}

// A draft sets no policy: no concepts are described and the note says why.
func TestDescribeOntology_ADraftSetsNoPolicy(t *testing.T) {
	f := newFilePlanFixture(t, memrank.OntologyDraftStatus)
	if len(f.desc.Schemes) != 0 {
		t.Errorf("a draft described schemes: %+v", f.desc.Schemes)
	}
	if p := f.desc.Policy; p == nil || p.Enforced || !strings.Contains(p.Note, "draft") || len(p.Issues) != 0 {
		t.Errorf("policy = %+v, want an unenforced note that a draft sets none", p)
	}
}

// Several asserted concepts: the longest retention governs and every group is
// required. A proposed statement changes nothing.
func TestEffectivePolicy_SeveralConceptsTakeLongestRetentionAndEveryGroup(t *testing.T) {
	f := newFilePlanFixture(t, memrank.OntologyConfirmedStatus)
	docID, root := f.doc("tenant", "/kb/vendor-contract")
	for _, tag := range []string{tagRisk, tagNotes} {
		f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": root, "to_id": f.concepts[tag].ChunkID, "kind": "subject"})
	}
	a := f.policy(f.ctx, "tenant", docID)
	if want := []string{tagNotes, tagRisk}; !reflect.DeepEqual(conceptTags(a), want) {
		t.Fatalf("concepts = %v, want %v", conceptTags(a), want)
	}
	if a.Retention != "10y" || a.RetentionFrom != tagRisk {
		t.Errorf("retention = %q from %q, want 10y from %s (longer than Notes' 1y)", a.Retention, a.RetentionFrom, tagRisk)
	}
	if want := []string{"compliance", "legal", "risk"}; !reflect.DeepEqual(a.Groups, want) {
		t.Errorf("groups = %v, want %v", a.Groups, want)
	}
	if a.Enforced || a.OntologyStatus != "confirmed" || !strings.Contains(a.Note, "Reported only") {
		t.Errorf("answer = %+v, want an unenforced report over a confirmed ontology", a)
	}

	// A proposed statement alone, on a fresh document, sets nothing.
	other, otherRoot := f.doc("tenant", "/kb/draft-note")
	f.must(map[string]any{"op": "propose_statement", "scope": "tenant", "from_id": otherRoot,
		"to_id": f.concepts[tagNotes].ChunkID, "kind": "subject"})
	b := f.policy(f.ctx, "tenant", other)
	if len(b.Concepts) != 0 || b.Retention != "" || len(b.Groups) != 0 {
		t.Errorf("a proposed statement set a policy: %+v", b)
	}
	if b.Groups == nil {
		t.Errorf("groups must be [] on the wire, not absent")
	}
}

// Outside the tenant scope a document is under the concepts whose mirror tag it
// carries; its Outsourcing tag reaches the unknown-group issue of that concept.
func TestEffectivePolicy_UserScopeReadsMirrorTags(t *testing.T) {
	f := newFilePlanFixture(t, memrank.OntologyConfirmedStatus)
	docID, root := f.doc("user", "/notes/vendor")
	f.must(map[string]any{"op": "add_tags", "scope": "user", "id": root, "tags": []string{tagOutsourcing, "unrelated/tag"}})
	a := f.policy(f.ctx, "user", docID)
	if want := []string{tagOutsourcing}; !reflect.DeepEqual(conceptTags(a), want) || a.Concepts[0].Via != "tag" {
		t.Fatalf("concepts = %+v, want Outsourcing by tag", a.Concepts)
	}
	if a.Retention != "5y" || !reflect.DeepEqual(a.Groups, []string{"compliance", "risk"}) {
		t.Errorf("retention %q groups %v, want 5y and [compliance risk]", a.Retention, a.Groups)
	}
	if !strings.Contains(strings.Join(a.Issues, "\n"), `"ghosts" is not a group`) {
		t.Errorf("the unknown group on the document's concept was not reported: %v", a.Issues)
	}
}

// A draft ontology sets no policy even on a document with asserted concepts.
func TestEffectivePolicy_DraftOntologySetsNoPolicy(t *testing.T) {
	f := newFilePlanFixture(t, memrank.OntologyDraftStatus)
	docID, root := f.doc("user", "/notes/n")
	f.must(map[string]any{"op": "add_tags", "scope": "user", "id": root, "tags": []string{tagRecords}})
	a := f.policy(f.ctx, "user", docID)
	if a.OntologyStatus != memrank.OntologyDraftStatus || len(a.Concepts) != 0 || a.Retention != "" || len(a.Groups) != 0 {
		t.Errorf("draft answer = %+v, want no policy", a)
	}
	if !strings.Contains(a.Note, "not confirmed") {
		t.Errorf("note = %q, want it to say the ontology is not confirmed", a.Note)
	}
}

// Nobody is in risk and legal both: a caller who may see the members gets the
// warning; one who may not gets a note that it was not checked, and no membership
// answer either way.
func TestEffectivePolicy_WarnsWhenNoMemberIsInEveryGroup(t *testing.T) {
	f := newFilePlanFixture(t, memrank.OntologyConfirmedStatus)
	docID, root := f.doc("tenant", "/kb/both")
	for _, tag := range []string{tagRisk, tagNotes} {
		f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": root, "to_id": f.concepts[tag].ChunkID, "kind": "subject"})
	}
	op := f.policy(tools.WithUnauthenticatedOperator(f.ctx), "tenant", docID)
	if !strings.Contains(strings.Join(op.Issues, "\n"), "no member is in all of compliance, legal, risk") {
		t.Errorf("operator: the empty-readership warning is missing: %v", op.Issues)
	}
	member := f.policy(f.ctx, "tenant", docID)
	all := strings.Join(member.Issues, "\n")
	if strings.Contains(all, "no member is in all") || !strings.Contains(all, "readership not checked") {
		t.Errorf("a caller who may not see the members: issues = %v", member.Issues)
	}
	// Risk alone needs compliance and risk, which u1 is in.
	solo, soloRoot := f.doc("tenant", "/kb/solo")
	f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": soloRoot, "to_id": f.concepts[tagRisk].ChunkID, "kind": "subject"})
	if a := f.policy(tools.WithUnauthenticatedOperator(f.ctx), "tenant", solo); strings.Contains(strings.Join(a.Issues, "\n"), "no member") {
		t.Errorf("u1 is in compliance and risk, yet warned: %v", a.Issues)
	}
}

// effective_policy asks read on the document, as get_document does.
func TestEffectivePolicy_RefusedWhereTheDocumentIsNotReadable(t *testing.T) {
	f := newFilePlanFixture(t, memrank.OntologyConfirmedStatus)
	secret, _ := f.doc("tenant", "/kb/secret/plan")
	public, _ := f.doc("tenant", "/kb/public/notes")
	ctx := tenantDocCtx(f.ctx, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
	_, r := f.do(ctx, map[string]any{"op": "effective_policy", "scope": "tenant", "document_id": secret})
	refusedByACL(t, "effective_policy /kb/secret/plan", r)
	_, r = f.do(ctx, map[string]any{"op": "get_document", "scope": "tenant", "id": secret})
	refusedByACL(t, "get_document /kb/secret/plan", r)
	_, r = f.do(ctx, map[string]any{"op": "effective_policy", "scope": "tenant", "document_id": public})
	allowed(t, "effective_policy /kb/public/notes", r)
	var a effectivePolicyAnswer
	_ = json.Unmarshal([]byte(r.Text), &a)
	if a.OpenMode {
		t.Errorf("a caller with rules is not in open mode: %+v", a)
	}
	if b := f.policy(f.ctx, "tenant", public); !b.OpenMode || !strings.Contains(b.Note, "open mode") {
		t.Errorf("a caller with no rules: open_mode=%v note=%q, want open mode said", b.OpenMode, b.Note)
	}
}
