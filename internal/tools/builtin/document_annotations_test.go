package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The annotation layer: a brief and an abstract per branch and document, beside
// the content, invalidated by a hash of the inputs the runtime computes.

// annOperator is the operator plane in a scope that grants the tenant documents.
func annOperator(ctx context.Context) context.Context {
	return operatorCtx(tenantDocCtx(ctx, nil))
}

// annDriver is a code-js driver's run holding the annotate grant.
func annDriver(ctx context.Context) context.Context {
	return tools.WithRunID(tools.WithRunPolicy(ctx, tools.RunPolicy{
		Tools:            []string{"*"},
		Memory:           tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
		SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
		StatementMethods: []string{annotationGrant},
	}), "run-annotator")
}

// annCorpus is a document whose tree is
//
//	root ── Setup ── Step one
//	     │        └─ Step two
//	     └─ Notes
type annCorpus struct {
	doc, root, setup, step1, step2, notes string
}

func seedAnnCorpus(t *testing.T, d *Document, ctx context.Context) annCorpus {
	t.Helper()
	var c annCorpus
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Widget guide","path":"/kb/widgets/guide","type":"Guide"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	c.doc, c.root = out["document_id"].(string), out["root_chunk_id"].(string)
	mk := func(parent, title, body string) string {
		t.Helper()
		p := ""
		if parent != "" {
			p = fmt.Sprintf(`,"parent_id":%q`, parent)
		}
		o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q%s,"title":%q,"body":%q}`, c.doc, p, title, body))
		if r.IsError {
			t.Fatalf("create_chunk %s: %s", title, r.Text)
		}
		return o["id"].(string)
	}
	c.setup = mk("", "Setup", "Before you start, read the safety notes and check the widget for damage.")
	c.step1 = mk(c.setup, "Step one", "Unpack the widget and remove the transport lock from its base.")
	c.step2 = mk(c.setup, "Step two", "Connect the widget to the controller with the supplied cable.")
	c.notes = mk("", "Notes", "The widget is rated for indoor use only and must be kept dry.")
	return c
}

func annInputs(t *testing.T, d *Document, ctx context.Context, target string) map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"annotation_inputs","scope":"tenant",`+target+`}`)
	if r.IsError {
		t.Fatalf("annotation_inputs %s: %s", target, r.Text)
	}
	return out
}

// annWrite reads the target's inputs and writes an annotation of them.
func annWrite(t *testing.T, d *Document, ctx context.Context, target, brief string) map[string]any {
	t.Helper()
	in := annInputs(t, d, ctx, target)
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant",%s,"brief":%q,"abstract":%q,"inputs_sha256":%q,"model":"test-model"}`,
		target, brief, "Abstract of "+brief+".", in["inputs_sha256"]))
	if r.IsError {
		t.Fatalf("set_annotation %s: %s", target, r.Text)
	}
	return out["annotation"].(map[string]any)
}

func annStale(t *testing.T, d *Document, ctx context.Context, target string) map[string]bool {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"get_annotation","scope":"tenant",`+target+`}`)
	if r.IsError {
		t.Fatalf("get_annotation %s: %s", target, r.Text)
	}
	got := map[string]bool{}
	for _, a := range out["annotations"].([]any) {
		m := a.(map[string]any)
		got[m["level"].(string)] = m["stale"].(bool)
	}
	return got
}

func idTarget(id string) string { return fmt.Sprintf(`"id":%q`, id) }

func TestSetAnnotation_WritesWithTheCurrentHashAndRefusesAMovedOne(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		ctx := annOperator(base)
		c := seedAnnCorpus(t, d, ctx)
		in := annInputs(t, d, ctx, idTarget(c.setup))
		if in["level"] != "branch" || in["stale"] != true || in["record"] != nil {
			t.Fatalf("inputs of an unannotated branch = %v", in)
		}
		if kids := in["children"].([]any); len(kids) != 2 || kids[0].(map[string]any)["body"] == "" {
			t.Fatalf("children = %v, want both steps with their text", kids)
		}
		old := in["inputs_sha256"].(string)
		// The text moves under the driver before it writes.
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"body":"Unpack it.","revision":1}`, c.step1)); r.IsError {
			t.Fatalf("update_chunk: %s", r.Text)
		}
		_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","id":%q,"brief":"b","abstract":"a","inputs_sha256":%q}`, c.setup, old))
		if !r.IsError || !strings.Contains(r.Text, "annotation_inputs_changed") {
			t.Fatalf("write over moved inputs = %s, want annotation_inputs_changed", r.Text)
		}
		rec := annWrite(t, d, ctx, idTarget(c.setup), "Setting up the widget")
		if rec["level"] != "branch" || rec["stale"] != false || rec["model"] != "test-model" || rec["written_by"] != "operator" || rec["language"] != "en" {
			t.Fatalf("record = %v", rec)
		}
		if got := annStale(t, d, ctx, idTarget(c.setup)); got["branch"] {
			t.Fatalf("fresh record reads stale: %v", got)
		}
	})
}

// Writing an annotation changes nothing the document says: the body, the
// revision, history and the export are what they were.
func TestSetAnnotation_LeavesTheContentUntouched(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	c := seedAnnCorpus(t, d, ctx)
	read := func() string {
		var b strings.Builder
		for _, q := range []string{
			`{"op":"get_chunk","scope":"tenant","id":"` + c.setup + `"}`,
			`{"op":"get_document","scope":"tenant","id":"` + c.doc + `"}`,
			`{"op":"export_md","scope":"tenant","id":"` + c.doc + `"}`,
			`{"op":"history","scope":"tenant","id":"` + c.setup + `"}`,
			`{"op":"query_documents","scope":"tenant"}`,
		} {
			_, r := docExec(t, d, ctx, q)
			b.WriteString(r.Text)
		}
		return b.String()
	}
	before := read()
	annWrite(t, d, ctx, idTarget(c.setup), "Setting up the widget")
	annWrite(t, d, ctx, `"document_id":"`+c.doc+`"`, "A guide to the widget")
	if after := read(); after != before {
		t.Fatalf("reads changed after annotating:\nbefore %s\nafter  %s", before, after)
	}
}

// A change to a leaf makes its ancestors' and its document's records stale and
// no other: the sibling subtree keeps a current record.
func TestAnnotation_StalenessClimbsOnlyAsFarAsTheTextChanged(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		ctx := annOperator(base)
		c := seedAnnCorpus(t, d, ctx)
		// Notes is a short leaf: covered by the document's annotation, but an
		// operator may still annotate it as a branch.
		annWrite(t, d, ctx, idTarget(c.notes), "Notes on use")
		annWrite(t, d, ctx, idTarget(c.setup), "Setting up the widget")
		annWrite(t, d, ctx, `"document_id":"`+c.doc+`"`, "A guide to the widget")
		for _, id := range []string{c.notes, c.setup, c.root} {
			for lvl, stale := range annStale(t, d, ctx, idTarget(id)) {
				if stale {
					t.Fatalf("%s %s stale before any change", id, lvl)
				}
			}
		}
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"body":"Unpack the widget carefully.","revision":1}`, c.step2)); r.IsError {
			t.Fatalf("update_chunk: %s", r.Text)
		}
		if !annStale(t, d, ctx, idTarget(c.setup))["branch"] {
			t.Error("Setup: a child changed, record not stale")
		}
		if annStale(t, d, ctx, idTarget(c.notes))["branch"] {
			t.Error("Notes: unchanged sibling subtree read stale")
		}
		// The document reads its top-level children's RECORDS: Setup's record has
		// not changed yet, so the document is current until Setup is rewritten.
		if annStale(t, d, ctx, idTarget(c.root))["document"] {
			t.Error("document stale before its child's record changed")
		}
		annWrite(t, d, ctx, idTarget(c.setup), "Setting up the widget, carefully")
		if !annStale(t, d, ctx, idTarget(c.root))["document"] {
			t.Error("document: a top-level record changed, document not stale")
		}
		// A new prompt version is a different hash.
		in := annInputs(t, d, ctx, idTarget(c.notes)+`,"prompt_version":2`)
		if in["stale"] != true {
			t.Errorf("a new prompt version reads current: %v", in["stale"])
		}
	})
}

func TestSetAnnotation_ManualRecordIsKeptFromTheWalk(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := annOperator(base)
	c := seedAnnCorpus(t, d, op)
	if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","id":%q,"brief":"Hand-written","abstract":"An operator's text.","manual":true}`, c.setup)); r.IsError {
		t.Fatalf("manual write: %s", r.Text)
	}
	drv := annDriver(base)
	in := annInputs(t, d, drv, idTarget(c.setup))
	_, r := docExec(t, d, drv, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","id":%q,"brief":"b","abstract":"a","inputs_sha256":%q}`, c.setup, in["inputs_sha256"]))
	if !r.IsError || !strings.Contains(r.Text, "manual_record") {
		t.Fatalf("driver over a manual record = %s, want manual_record", r.Text)
	}
	if _, r := docExec(t, d, drv, fmt.Sprintf(`{"op":"delete_annotation","scope":"tenant","id":%q,"level":"branch"}`, c.setup)); !r.IsError {
		t.Fatal("driver deleted a manual record")
	}
	// The manual record goes stale like any other when its inputs change.
	if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"body":"Changed.","revision":1}`, c.step1)); r.IsError {
		t.Fatal(r.Text)
	}
	if !annStale(t, d, op, idTarget(c.setup))["branch"] {
		t.Error("manual record not reported stale after its inputs changed")
	}
	// A driver may not write manual records.
	if _, r := docExec(t, d, drv, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","id":%q,"brief":"b","abstract":"a","manual":true}`, c.notes)); !r.IsError {
		t.Fatal("driver wrote a manual record")
	}
}

func TestAnnotationOps_WhoMayWrite(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := annOperator(base)
	c := seedAnnCorpus(t, d, op)
	// An ordinary run with the tenant scope but no grant.
	plain := tools.WithRunID(tenantDocCtx(base, nil), "run-plain")
	for _, q := range []string{
		`{"op":"annotation_inputs","scope":"tenant","id":"` + c.setup + `"}`,
		`{"op":"set_annotation","scope":"tenant","id":"` + c.setup + `","brief":"b","abstract":"a","inputs_sha256":"x"}`,
		`{"op":"delete_annotation","scope":"tenant","id":"` + c.setup + `","level":"branch"}`,
	} {
		if _, r := docExec(t, d, plain, q); !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryPermission {
			t.Errorf("%s without the grant = %+v, want a permission refusal", q, r)
		}
	}
	// The driver writes; the record says which run.
	rec := annWrite(t, d, annDriver(base), idTarget(c.setup), "Setting up the widget")
	if rec["written_by"] != "run:run-annotator" {
		t.Errorf("written_by = %v", rec["written_by"])
	}
	// Reads need no grant.
	if _, r := docExec(t, d, plain, `{"op":"get_annotation","scope":"tenant","id":"`+c.setup+`"}`); r.IsError {
		t.Errorf("get_annotation: %s", r.Text)
	}
	// The grant takes the annotator lease and no other.
	drv := annDriver(base)
	if _, r := docExec(t, d, drv, `{"op":"lease","scope":"tenant","name":"annotator"}`); r.IsError {
		t.Errorf("annotator lease: %s", r.Text)
	}
	if _, r := docExec(t, d, drv, `{"op":"lease","scope":"tenant","name":"linker"}`); !r.IsError {
		t.Error("the annotate grant took the linker lease")
	}
}

func TestSetAnnotation_TenantScopeOnly(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := operatorCtx(base)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Mine"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	_, r = docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_annotation","scope":"user","document_id":%q,"brief":"b","abstract":"a","manual":true}`, out["document_id"]))
	if !r.IsError || !strings.Contains(r.Text, "tenant") {
		t.Fatalf("user-scope annotation = %s, want refused", r.Text)
	}
}

func TestSetAnnotation_ValidatesLevelAndLength(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	c := seedAnnCorpus(t, d, ctx)
	for _, q := range []string{
		`{"op":"set_annotation","scope":"tenant","id":"` + c.setup + `","level":"document","brief":"b","abstract":"a","manual":true}`,
		`{"op":"set_annotation","scope":"tenant","id":"` + c.root + `","level":"branch","brief":"b","abstract":"a","manual":true}`,
		`{"op":"set_annotation","scope":"tenant","id":"` + c.setup + `","brief":"` + strings.Repeat("x", 241) + `","abstract":"a","manual":true}`,
		`{"op":"set_annotation","scope":"tenant","id":"` + c.setup + `","brief":"b","abstract":"` + strings.Repeat("x", 2001) + `","manual":true}`,
		`{"op":"set_annotation","scope":"tenant","id":"` + c.setup + `","brief":"","abstract":"a","manual":true}`,
		`{"op":"set_annotation","scope":"tenant","id":"` + c.setup + `","brief":"b","abstract":"a","language":"English","manual":true}`,
		`{"op":"set_annotation","scope":"tenant","id":"` + c.setup + `","brief":"b","abstract":"a"}`,
	} {
		if _, r := docExec(t, d, ctx, q); !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryValidation {
			t.Errorf("%.120s = %+v, want a validation refusal", q, r)
		}
	}
}

// with_summary attaches the nearest annotation; without it every read is the
// same bytes as before.
func TestWithSummary_AttachesTheNearestAnnotation(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	c := seedAnnCorpus(t, d, ctx)
	annWrite(t, d, ctx, idTarget(c.setup), "Setting up the widget")
	annWrite(t, d, ctx, `"document_id":"`+c.doc+`"`, "A guide to the widget")

	out, r := docExec(t, d, ctx, `{"op":"get_document","scope":"tenant","id":"`+c.doc+`","with_summary":true}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	sm, _ := out["summary"].(map[string]any)
	if sm["level"] != "document" || sm["brief"] != "A guide to the widget" || sm["abstract"] == nil {
		t.Fatalf("get_document summary = %v", out["summary"])
	}
	// A leaf below Setup: Setup's record, with the document's beside it.
	out, r = docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+c.step1+`","with_summary":true}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	sm, _ = out["summary"].(map[string]any)
	if sm["level"] != "branch" || sm["chunk_id"] != c.setup || sm["brief"] != "Setting up the widget" {
		t.Fatalf("get_chunk summary = %v", out["summary"])
	}
	if ds, _ := out["document_summary"].(map[string]any); ds["brief"] != "A guide to the widget" {
		t.Fatalf("document_summary = %v", out["document_summary"])
	}
	// Notes has no record of its own: the document's.
	out, _ = docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+c.notes+`","with_summary":true}`)
	if sm, _ := out["summary"].(map[string]any); sm["level"] != "document" {
		t.Fatalf("Notes summary = %v, want the document's", out["summary"])
	}
	// query_documents rows carry the brief, not the abstract.
	out, _ = docExec(t, d, ctx, `{"op":"query_documents","scope":"tenant","with_summary":true}`)
	row := out["documents"].([]any)[0].(map[string]any)
	if sm, _ := row["summary"].(map[string]any); sm["brief"] != "A guide to the widget" || sm["abstract"] != nil {
		t.Fatalf("query_documents summary = %v", row["summary"])
	}
	// A stale record is shown, marked.
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"body":"Changed.","revision":1}`, c.step1)); r.IsError {
		t.Fatal(r.Text)
	}
	out, _ = docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+c.step2+`","with_summary":true}`)
	if sm, _ := out["summary"].(map[string]any); sm["stale"] != true {
		t.Fatalf("stale summary = %v, want stale: true", out["summary"])
	}
	// summary_chars caps the abstracts.
	out, _ = docExec(t, d, ctx, `{"op":"get_document","scope":"tenant","id":"`+c.doc+`","with_summary":true,"summary_chars":5}`)
	if sm, _ := out["summary"].(map[string]any); sm["abstract"] != nil || sm["brief"] == nil {
		t.Fatalf("capped summary = %v, want the brief only", out["summary"])
	}
	// An op that does not take it refuses it.
	if _, r := docExec(t, d, ctx, `{"op":"list_tags","scope":"tenant","with_summary":true}`); !r.IsError {
		t.Fatal("list_tags took with_summary")
	}
}

func TestWithSummary_AbsentIsByteIdentical(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	c := seedAnnCorpus(t, d, ctx)
	reads := []string{
		`{"op":"get_document","scope":"tenant","id":"` + c.doc + `"}`,
		`{"op":"get_chunk","scope":"tenant","id":"` + c.step1 + `"}`,
		`{"op":"query_documents","scope":"tenant"}`,
		`{"op":"graph_recall","scope":"tenant","seed_ids":["` + c.setup + `"]}`,
	}
	before := map[string]string{}
	for _, q := range reads {
		_, r := docExec(t, d, ctx, q)
		before[q] = r.Text
	}
	annWrite(t, d, ctx, idTarget(c.setup), "Setting up the widget")
	annWrite(t, d, ctx, `"document_id":"`+c.doc+`"`, "A guide to the widget")
	for _, q := range reads {
		if _, r := docExec(t, d, ctx, q); r.Text != before[q] {
			t.Errorf("%s changed without with_summary:\n%s\n%s", q, before[q], r.Text)
		}
	}
}

// documents_summary and graph_snapshot carry the brief as plain metadata.
func TestDocumentBrief_IsPlainMetadata(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	c := seedAnnCorpus(t, d, ctx)
	q := `{"op":"documents_summary","scope":"tenant","document_ids":["` + c.doc + `"]}`
	_, r := docExec(t, d, ctx, q)
	if strings.Contains(r.Text, "brief") {
		t.Fatalf("brief before any annotation: %s", r.Text)
	}
	annWrite(t, d, ctx, `"document_id":"`+c.doc+`"`, "A guide to the widget")
	out, _ := docExec(t, d, ctx, q)
	if row := out["documents"].([]any)[0].(map[string]any); row["brief"] != "A guide to the widget" {
		t.Fatalf("documents_summary row = %v", row)
	}
	out, r = docExec(t, d, ctx, `{"op":"graph_snapshot","scope":"tenant"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	docs := out["documents"].(map[string]any)
	ids, briefs := docs["ids"].([]any), docs["briefs"].([]any)
	if len(ids) != len(briefs) {
		t.Fatalf("briefs %d not aligned with ids %d", len(briefs), len(ids))
	}
	for i, id := range ids {
		if id == c.doc && briefs[i] != "A guide to the widget" {
			t.Fatalf("snapshot brief = %v", briefs[i])
		}
	}
}

func TestAnnotations_FollowTheirChunks(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	c := seedAnnCorpus(t, d, ctx)
	annWrite(t, d, ctx, idTarget(c.setup), "Setting up the widget")
	annWrite(t, d, ctx, `"document_id":"`+c.doc+`"`, "A guide to the widget")
	count := func() int {
		out, r := docExec(t, d, ctx, `{"op":"list_annotations","scope":"tenant"}`)
		if r.IsError {
			t.Fatal(r.Text)
		}
		return len(out["annotations"].([]any))
	}
	if n := count(); n != 2 {
		t.Fatalf("records = %d, want 2", n)
	}
	// A copy carries them, under the new ids, still current.
	out, r := docExec(t, d, ctx, `{"op":"copy_documents","scope":"tenant","document_ids":["`+c.doc+`"],"to_scope":"tenant","path_map":{"/kb/widgets/guide":"/kb/widgets/guide-copy"},"dry_run":false}`)
	if r.IsError {
		t.Fatalf("copy_documents: %s", r.Text)
	}
	if n := count(); n != 4 {
		t.Fatalf("records after copy = %d, want 4 (%v)", n, out)
	}
	stale, _ := docExec(t, d, ctx, `{"op":"list_annotations","scope":"tenant","stale":true}`)
	if n := len(stale["annotations"].([]any)); n != 0 {
		t.Fatalf("copied records stale: %v", stale)
	}
	if _, r := docExec(t, d, ctx, `{"op":"delete_chunk","scope":"tenant","id":"`+c.setup+`"}`); r.IsError {
		t.Fatal(r.Text)
	}
	if n := count(); n != 3 {
		t.Fatalf("records after delete_chunk = %d, want 3", n)
	}
	if _, r := docExec(t, d, ctx, `{"op":"delete_document","scope":"tenant","id":"`+c.doc+`"}`); r.IsError {
		t.Fatal(r.Text)
	}
	if n := count(); n != 2 {
		t.Fatalf("records after delete_document = %d, want 2 (the copy's)", n)
	}
	var left int
	res, _ := d.query(ctx, mustKey(t, d, ctx), `SELECT COUNT(*) FROM chunk_annotations`)
	left = asInt(res.Rows[0][0])
	if left != 2 {
		t.Fatalf("rows in the table = %d, want 2: an orphan stayed behind", left)
	}
}

func mustKey(t *testing.T, d *Document, ctx context.Context) sqlmem.ScopeKey {
	t.Helper()
	key, _, err := d.resolveScope(ctx, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestListAnnotations_FiltersAndCounts(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	c := seedAnnCorpus(t, d, ctx)
	annWrite(t, d, ctx, idTarget(c.setup), "Setting up the widget")
	annWrite(t, d, ctx, idTarget(c.notes), "Notes on use")
	annWrite(t, d, ctx, `"document_id":"`+c.doc+`"`, "A guide to the widget")
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"body":"Changed.","revision":1}`, c.step1)); r.IsError {
		t.Fatal(r.Text)
	}
	out, r := docExec(t, d, ctx, `{"op":"list_annotations","scope":"tenant","summarize":true}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	raw, _ := json.Marshal(out["counts"])
	if string(raw) != `{"branch":{"manual":0,"stale":1,"total":2},"document":{"manual":0,"stale":0,"total":1}}` {
		t.Fatalf("counts = %s", raw)
	}
	out, _ = docExec(t, d, ctx, `{"op":"list_annotations","scope":"tenant","level":"branch","limit":1}`)
	if len(out["annotations"].([]any)) != 1 || out["next_cursor"] == nil {
		t.Fatalf("page 1 = %v", out)
	}
	out2, _ := docExec(t, d, ctx, `{"op":"list_annotations","scope":"tenant","level":"branch","limit":1,"after":"`+out["next_cursor"].(string)+`"}`)
	a, b := out["annotations"].([]any)[0].(map[string]any), out2["annotations"].([]any)[0].(map[string]any)
	if a["chunk_id"] == b["chunk_id"] || out2["next_cursor"] != nil {
		t.Fatalf("page 2 = %v", out2)
	}
	out, _ = docExec(t, d, ctx, `{"op":"list_annotations","scope":"tenant","under_path":"/kb/other"}`)
	if len(out["annotations"].([]any)) != 0 {
		t.Fatalf("under another path = %v", out)
	}
}
