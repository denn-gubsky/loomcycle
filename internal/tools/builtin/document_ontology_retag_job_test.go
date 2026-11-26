package builtin

// document_ontology_retag_job_test.go — RFC GQ P5.5: the concept re-tag job.
//
// Every test starts from an EXISTING state: documents in the tenant, user and agent
// scopes are filed under the concept Risk (topics/risk) and its narrower Credit risk by
// their tags, and classification records propose them, before Risk is moved or its
// parent renamed. Each positive has a minimal pair beside it: Safety's tag, and
// topics/risk-management, a tag that shares Risk's text but is not under it.

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const retagOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Document\n\n" +
	"## Schemes\n@section schemes\n\n" +
	"### topics\n\n#### Risk\n\n##### Credit risk\n\n#### Reporting\n\n#### Safety\n"

type retagFixture struct {
	*renameFixture
	risk, reporting string
}

// newRetagFixtureOn files, in every scope, a document under Credit risk (a document
// tag), a chunk under Risk (a chunk tag), Safety's and topics/risk-management's
// documents (the pairs), and a classification record proposing Credit risk and Safety.
func newRetagFixtureOn(t *testing.T, d *Document, base context.Context) *retagFixture {
	t.Helper()
	ctx := tools.UpdateRunPolicy(operatorCtx(installTenantOntology(t, d, base, retagOntologyMD, memrank.OntologyConfirmedStatus)),
		func(rp *tools.RunPolicy) {
			rp.Memory = tools.MemoryPolicyValue{AllowedScopes: renameFixtureScopes}
			rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: renameFixtureScopes}
		})
	f := &retagFixture{renameFixture: &renameFixture{retiredFixture: &retiredFixture{t: t, d: d, ctx: ctx}, scopes: map[string]renameScope{}}}
	for _, s := range renameFixtureScopes {
		key, mscope, err := d.resolveScope(ctx, s)
		if err != nil {
			t.Fatalf("scope %s: %v", s, err)
		}
		f.scopes[s] = renameScope{key: key, mscope: mscope, tenant: s == "tenant", name: s}
	}
	f.key = f.scopes["tenant"].key
	f.risk, f.reporting = f.chunk("Risk"), f.chunk("Reporting")
	if got := ontologyConceptTags(f.read())[f.chunk("Credit risk")]; got != "topics/risk/credit-risk" {
		t.Fatalf("Credit risk's tag = %q, want topics/risk/credit-risk", got)
	}
	for _, s := range renameFixtureScopes {
		f.seedTags(s)
	}
	return f
}

func newRetagFixture(t *testing.T) *retagFixture {
	t.Helper()
	d, base, _ := documentFixture(t)
	return newRetagFixtureOn(t, d, base)
}

func (f *retagFixture) seedTags(scope string) {
	f.t.Helper()
	doc := func(title string, tags ...string) map[string]any {
		out := f.must(map[string]any{"op": "create_document", "scope": scope, "title": title})
		if len(tags) > 0 {
			f.must(map[string]any{"op": "add_tags", "scope": scope, "document_id": out["document_id"], "tags": tags})
		}
		return out
	}
	doc("Credit memo", "topics/risk/credit-risk", "plain")
	pack := doc("Board pack")
	section := f.must(map[string]any{"op": "create_chunk", "scope": scope, "document_id": pack["document_id"], "title": "Section one", "body": "Text."})
	f.must(map[string]any{"op": "add_tags", "scope": scope, "id": section["id"], "tags": []string{"topics/risk"}})
	doc("Fire drill", "topics/safety")
	doc("Management note", "topics/risk-management")
	f.proposeTags(scope, asStr(pack["root_chunk_id"]), "topics/risk/credit-risk", "topics/safety")
}

// proposeTags writes a classification record proposing tags on a root, as classify
// leaves one outside the tenant scope.
func (f *retagFixture) proposeTags(scope, root string, tags ...string) {
	f.t.Helper()
	g := f.must(map[string]any{"op": "get_chunk", "scope": scope, "id": root})
	tj, _ := json.Marshal(tags)
	rec := json.RawMessage(`{"by":"classify","at":` + renameJSONInt(classificationAt) + `,"proposed_tags":` + string(tj) + `}`)
	f.must(map[string]any{"op": "update_chunk", "scope": scope, "id": root, "revision": int(g["revision"].(float64)),
		"fields": map[string]any{classifyField: rec}})
}

// tags is a scope's tags on its documents and chunks, the ontology's own left out, as
// "<title>→<tag>".
func (f *retagFixture) tags(scope string) []string {
	f.t.Helper()
	ont := f.read().DocumentID
	k := f.scopes[scope].key
	out := f.stringsIn(k, `SELECT d.title, t.tag FROM document_tags t JOIN documents d ON d.id = t.document_id WHERE d.id <> ?`, ont)
	out = append(out, f.stringsIn(k, `SELECT c.title, t.tag FROM chunk_tags t JOIN chunks c ON c.id = t.chunk_id WHERE c.document_id <> ?`, ont)...)
	return sortedStrings(out)
}

// proposed is a scope's classification records' proposed_tags, per document title.
func (f *retagFixture) proposed(scope string) map[string][]string {
	f.t.Helper()
	sc := f.scopes[scope]
	out := map[string][]string{}
	for _, row := range f.stringsIn(sc.key, `SELECT root_chunk_id, title FROM documents`) {
		parts := splitArrow(row)
		cb, err := f.d.readBody(f.ctx, sc.mscope, sc.key, parts[0])
		if err != nil {
			f.t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(cb.Fields, &fields)
		var rec struct {
			Tags []string        `json:"proposed_tags"`
			At   json.RawMessage `json:"at"`
		}
		if raw, ok := fields[classifyField]; ok && json.Unmarshal(raw, &rec) == nil && rec.Tags != nil {
			if string(rec.At) != renameJSONInt(classificationAt) {
				f.t.Errorf("%s: the record's time became %s", parts[1], rec.At)
			}
			out[parts[1]] = rec.Tags
		}
	}
	return out
}

// moveRisk moves Risk under Reporting with a current plan's fingerprint.
func (f *retagFixture) moveRisk() map[string]any {
	f.t.Helper()
	out, r := f.do(f.ctx, map[string]any{"op": "move_chunk", "scope": "tenant", "id": f.risk, "new_parent_id": f.reporting,
		"plan_fingerprint": f.planFingerprint(f.risk)})
	if r.IsError {
		f.t.Fatalf("move_chunk: %s", r.Text)
	}
	return out
}

func retagJob(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	job, _ := out["retag_job"].(map[string]any)
	if job == nil {
		t.Fatalf("no retag_job in %v", out)
	}
	return job
}

// TestConceptMove_DocumentTagsInAUserScopeFollowThePath: moving Risk under Reporting
// carries every tag at or under topics/risk — a document's tag, a chunk's tag, a
// classification record's proposed tag — to topics/reporting/risk in the tenant, user and
// agent scopes; renaming Reporting then carries them on to topics/filings. Safety's tag
// and topics/risk-management stay as they were.
func TestConceptMove_DocumentTagsInAUserScopeFollowThePath(t *testing.T) {
	assertConceptMoveRetags(t, newRetagFixture(t))
}

func TestConceptMove_DocumentTagsInAUserScopeFollowThePathOnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	assertConceptMoveRetags(t, newRetagFixtureOn(t, d, base))
}

func assertConceptMoveRetags(t *testing.T, f *retagFixture) {
	t.Helper()
	want := func(prefix string) []string {
		return sortedStrings([]string{"Credit memo→plain", "Credit memo→" + prefix + "/credit-risk", "Section one→" + prefix,
			"Fire drill→topics/safety", "Management note→topics/risk-management"})
	}
	for _, s := range renameFixtureScopes {
		if got := f.tags(s); !reflect.DeepEqual(got, want("topics/risk")) {
			t.Fatalf("%s tags before = %v", s, got)
		}
	}
	out := f.moveRisk()
	job := retagJob(t, out)
	if asStr(job["state"]) != jobStateDone || asStr(job["kind"]) != ontologyJobKindConceptRetag || asInt(job["scopes_done"]) != 3 {
		t.Fatalf("retag_job = %v, want a done concept_retag over the three scopes", job)
	}
	if !reflect.DeepEqual(job["from"], []any{"topics/risk"}) || !reflect.DeepEqual(job["to_tags"], []any{"topics/reporting/risk"}) {
		t.Errorf("retag_job moves %v -> %v, want topics/risk -> topics/reporting/risk alone (Credit risk follows as a prefix)", job["from"], job["to_tags"])
	}
	for _, s := range renameFixtureScopes {
		if got := f.tags(s); !reflect.DeepEqual(got, want("topics/reporting/risk")) {
			t.Errorf("%s tags after the move = %v, want %v", s, got, want("topics/reporting/risk"))
		}
		if got := f.proposed(s)["Board pack"]; !reflect.DeepEqual(got, []string{"topics/reporting/risk/credit-risk", "topics/safety"}) {
			t.Errorf("%s proposed_tags after the move = %v", s, got)
		}
	}
	wantTitles(t, "user query_documents tag=topics/reporting/risk/credit-risk",
		rowTitles(f.must(map[string]any{"op": "query_documents", "scope": "user", "tag": "topics/reporting/risk/credit-risk"}), "documents"),
		"Credit memo")

	// A rename moves the tags of everything under the renamed concept.
	g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": f.reporting})
	out, r := f.do(f.ctx, map[string]any{"op": "update_chunk", "scope": "tenant", "id": f.reporting, "revision": int(g["revision"].(float64)),
		"title": "Filings", "plan_fingerprint": f.planFingerprint(f.reporting)})
	if r.IsError {
		t.Fatalf("rename Reporting: %s", r.Text)
	}
	if job := retagJob(t, out); asStr(job["state"]) != jobStateDone || !reflect.DeepEqual(job["from"], []any{"topics/reporting"}) {
		t.Errorf("rename retag_job = %v, want done from topics/reporting", job)
	}
	for _, s := range renameFixtureScopes {
		if got := f.tags(s); !reflect.DeepEqual(got, want("topics/filings/risk")) {
			t.Errorf("%s tags after the rename = %v, want %v", s, got, want("topics/filings/risk"))
		}
	}
}

// TestConceptMove_ExistingNewTagIsNotDuplicated: a document, a chunk and a classification
// record that already carry the new tag beside the old one keep it once.
func TestConceptMove_ExistingNewTagIsNotDuplicated(t *testing.T) {
	f := newRetagFixture(t)
	for _, s := range []string{"tenant", "user"} {
		doc := f.must(map[string]any{"op": "create_document", "scope": s, "title": "Twice filed"})
		f.must(map[string]any{"op": "add_tags", "scope": s, "document_id": doc["document_id"],
			"tags": []string{"topics/risk", "topics/reporting/risk"}})
		f.must(map[string]any{"op": "add_tags", "scope": s, "id": doc["root_chunk_id"],
			"tags": []string{"topics/risk/credit-risk", "topics/reporting/risk/credit-risk"}})
		f.proposeTags(s, asStr(doc["root_chunk_id"]), "topics/reporting/risk", "topics/risk", "topics/safety")
	}
	if job := retagJob(t, f.moveRisk()); asStr(job["state"]) != jobStateDone {
		t.Fatalf("retag_job = %v, want done", job)
	}
	for _, s := range []string{"tenant", "user"} {
		var twice []string
		for _, tag := range f.tags(s) {
			if len(tag) > len("Twice filed→") && tag[:len("Twice filed→")] == "Twice filed→" {
				twice = append(twice, tag)
			}
		}
		want := []string{"Twice filed→topics/reporting/risk", "Twice filed→topics/reporting/risk/credit-risk"}
		if !reflect.DeepEqual(twice, want) {
			t.Errorf("%s: Twice filed's tags = %v, want %v", s, twice, want)
		}
		if got := f.proposed(s)["Twice filed"]; !reflect.DeepEqual(got, []string{"topics/reporting/risk", "topics/safety"}) {
			t.Errorf("%s: proposed_tags = %v, want the new tag once and Safety's", s, got)
		}
	}
}

// TestConceptMove_OntologyOwnMirrorTagsAreLeftToConfirm: the job leaves the ontology
// document's own concept chunks alone (a document of the tenant scope filed under Risk
// moves); their mirror tags are the mirror's, which gives Risk and Credit risk their new
// tags when it runs at confirm.
func TestConceptMove_OntologyOwnMirrorTagsAreLeftToConfirm(t *testing.T) {
	f := newRetagFixture(t)
	if _, err := f.d.MirrorSchemeTags(f.ctx, false); err != nil {
		t.Fatal(err)
	}
	ownTags := func() []string {
		ont := f.read().DocumentID
		return f.stringsIn(f.key, `SELECT c.title, t.tag FROM chunk_tags t JOIN chunks c ON c.id = t.chunk_id
			WHERE c.document_id = ? AND c.title IN ('Risk', 'Credit risk')`, ont)
	}
	old := []string{"Credit risk→topics/risk/credit-risk", "Risk→topics/risk"}
	if got := ownTags(); !reflect.DeepEqual(got, old) {
		t.Fatalf("the ontology's own tags before = %v, want %v", got, old)
	}
	if job := retagJob(t, f.moveRisk()); asStr(job["state"]) != jobStateDone {
		t.Fatalf("retag_job = %v, want done", job)
	}
	if got := f.tags("tenant"); !containsString(got, "Section one→topics/reporting/risk") {
		t.Errorf("tenant tags = %v, want the tenant document's chunk moved", got)
	}
	if got := ownTags(); !reflect.DeepEqual(got, old) {
		t.Errorf("the ontology's own tags after the job = %v, want them left as they were, for the mirror", got)
	}
	if _, err := f.d.MirrorSchemeTags(f.ctx, false); err != nil {
		t.Fatal(err)
	}
	if got, want := ownTags(), []string{"Credit risk→topics/reporting/risk/credit-risk", "Risk→topics/reporting/risk"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the ontology's own tags after the mirror = %v, want %v", got, want)
	}
}

func sortedStrings(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func splitArrow(row string) []string { return strings.SplitN(row, "→", 2) }
