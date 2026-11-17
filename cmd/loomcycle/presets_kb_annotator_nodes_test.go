package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// kb/annotator's ontology mode (RFC GM §4, D33; plan P4.2), its shipped body
// against the real Document tool with a fake model: every node of every confirmed
// taxonomy gets an annotation, a node with no member is written without a model
// call, and a node's request carries its members' status counts.

// annNodesOntologyMD: one scheme of four concepts (Widgets › Installation, Safety,
// Pricing) and two declared document classes (Document, Guide).
const annNodesOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n### Document\n\n#### Guide\nA how-to guide for one product.\n\n" +
	"## Schemes\n@section schemes\n\n### topics\n\n" +
	"#### Widgets\nThe widgets the company makes.\n\n##### Installation\nHow a widget is installed.\n\n" +
	"#### Safety\nRules for safe use.\n\n#### Pricing\nWhat things cost.\n"

type annNodesFixture struct {
	d                    *builtin.Document
	author, driver, op   context.Context
	concepts             map[string]string // name → chunk id
	scheme               string
	g1, g2, g3, g4, safe string // documents
}

// newAnnNodesFixture: Installation holds three guides, Widgets those and a fourth,
// Safety one, Pricing none. Two guides are done, one confirmed, one draft.
func newAnnNodesFixture(t *testing.T) *annNodesFixture {
	t.Helper()
	d, author, driver, op := annRealDocumentOp(t)
	withRealOntology(t, d, author, annNodesOntologyMD)
	f := &annNodesFixture{d: d, author: author, driver: driver, op: op, concepts: map[string]string{}}
	desc := realDoc(t, d, op, `{"op":"describe_ontology"}`)
	for _, s := range desc["schemes"].([]any) {
		sm := s.(map[string]any)
		f.scheme = sm["chunk_id"].(string)
		for _, c := range sm["concepts"].([]any) {
			cm := c.(map[string]any)
			f.concepts[cm["name"].(string)] = cm["chunk_id"].(string)
		}
	}
	if len(f.concepts) != 4 || f.scheme == "" {
		t.Fatalf("ontology = %v", desc["schemes"])
	}
	mk := func(title, status string, concepts ...string) string {
		out := realDoc(t, d, op, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"path":"/kb/%s","type":"Guide"}`,
			title, strings.ToLower(strings.ReplaceAll(title, " ", "-"))))
		root := out["root_chunk_id"].(string)
		part := realDoc(t, d, op, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"Body","body":"How to work with the widget %s."}`,
			out["document_id"], title))["id"].(string)
		for _, c := range concepts {
			realDoc(t, d, op, fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"subject"}`, part, f.concepts[c]))
		}
		if status != "" {
			g := realDoc(t, d, op, fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, root))
			realDoc(t, d, op, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"status":%q,"revision":%v}`, root, status, g["revision"]))
		}
		return out["document_id"].(string)
	}
	f.g1 = mk("Install guide one", "done", "Installation")
	f.g2 = mk("Install guide two", "done", "Installation")
	f.g3 = mk("Install guide three", "confirmed", "Installation")
	f.g4 = mk("Widget overview", "draft", "Widgets")
	f.safe = mk("Safety sheet", "", "Safety")
	return f
}

// promptFor is the request whose TITLE is title, "" when none was made.
func promptFor(f *annFake, title string) string {
	for _, p := range f.prompts {
		if strings.Contains(p, "\nTITLE: "+title+"\n") {
			return p
		}
	}
	return ""
}

func nodeRecordsAt(t *testing.T, d *builtin.Document, ctx context.Context, level string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, a := range realDoc(t, d, ctx, `{"op":"list_annotations","scope":"tenant","level":"`+level+`","limit":500}`)["annotations"].([]any) {
		rec := a.(map[string]any)
		out[rec["chunk_id"].(string)] = rec
	}
	return out
}

// TestNodeAnnotation_EveryNodeOfASchemeHasOne — after one ontology pass the scheme
// has concepts + 1 annotations, every declared document class has one, and a second
// pass finds nothing to do.
func TestNodeAnnotation_EveryNodeOfASchemeHasOne(t *testing.T) {
	f := newAnnNodesFixture(t)
	fake := &annFake{reply: annStableReply}
	rep := runAnnotator(t, f.d, f.driver, fake, `{"mode":"ontology"}`)
	onto, _ := rep["ontology"].(map[string]any)
	if onto["missing_count"] != float64(7) || len(onto["missing"].([]any)) != 7 {
		t.Fatalf("report lists %v missing (%v), want the 5 scheme nodes and 2 classes", onto["missing_count"], onto["missing"])
	}
	if rep["would_call"] != float64(6) || rep["would_write"] != float64(7) {
		t.Fatalf("report = %v, want 6 model calls (Pricing is declared) and 7 writes", rep)
	}

	rep = runAnnotator(t, f.d, f.driver, fake, `{"mode":"ontology","write":"annotate"}`)
	if rep["status"] != "complete" || rep["writes"] != float64(7) {
		t.Fatalf("annotate = %v, want 7 writes", rep)
	}
	concepts, schemes := nodeRecordsAt(t, f.d, f.author, "concept"), nodeRecordsAt(t, f.d, f.author, "scheme")
	if got := len(concepts) + len(schemes); got != len(f.concepts)+1 {
		t.Fatalf("scheme topics has %d node annotations, want concepts + 1 = %d", got, len(f.concepts)+1)
	}
	if len(nodeRecordsAt(t, f.d, f.author, "class")) != 2 {
		t.Fatal("the two declared document classes are not both annotated")
	}
	if onto, _ := rep["ontology"].(map[string]any); onto["statistics"] != "not_permitted" {
		t.Errorf("statistics = %v, want not_permitted (the annotate grant runs no node_stats)", onto["statistics"])
	}

	before := len(fake.prompts)
	rep = runAnnotator(t, f.d, f.driver, fake, `{"mode":"ontology","write":"annotate"}`)
	if n := len(fake.prompts) - before; n != 0 || rep["writes"] != float64(0) {
		t.Fatalf("a second pass made %d calls, report %v; want nothing to do", n, rep)
	}
	// Deepest first: Installation's record is in Widgets' request.
	if p := promptFor(fake, "Widgets"); !strings.Contains(p, "Installation (3)") || !strings.Contains(p, "brief: Brief of Installation") {
		t.Fatalf("Widgets' request does not carry its narrower topic's record:\n%s", p)
	}
}

// TestNodeAnnotation_EmptyNodeIsDeclaredWithoutAModelCall — Pricing holds nothing:
// its record is the runtime's composed text, written with no model call.
func TestNodeAnnotation_EmptyNodeIsDeclaredWithoutAModelCall(t *testing.T) {
	f := newAnnNodesFixture(t)
	fake := &annFake{reply: annStableReply}
	runAnnotator(t, f.d, f.driver, fake, `{"mode":"ontology","scheme":"topics","write":"annotate"}`)
	if p := promptFor(fake, "Pricing"); p != "" {
		t.Fatalf("a model call was made for the empty Pricing node:\n%s", p)
	}
	rec := nodeRecordsAt(t, f.d, f.author, "concept")[f.concepts["Pricing"]]
	if rec == nil || rec["basis"] != "declared" || !strings.Contains(rec["abstract"].(string), "Nothing is held under it yet") {
		t.Fatalf("Pricing's record = %v, want the declared text", rec)
	}
	if got := nodeRecordsAt(t, f.d, f.author, "concept")[f.concepts["Safety"]]; got == nil || got["basis"] != "thin" {
		t.Fatalf("Safety's record = %v, want basis thin", got)
	}
	if len(fake.prompts) != 4 {
		t.Fatalf("%d model calls, want 4 (Widgets, Installation, Safety, topics)", len(fake.prompts))
	}
}

// TestNodeAnnotation_RequestCarriesMemberStatusCounts — a node's request says how
// many of its members are done, confirmed and draft, above its material.
func TestNodeAnnotation_RequestCarriesMemberStatusCounts(t *testing.T) {
	f := newAnnNodesFixture(t)
	fake := &annFake{reply: annStableReply}
	runAnnotator(t, f.d, f.driver, fake, `{"mode":"ontology","scheme":"topics","write":"annotate"}`)
	p := promptFor(fake, "Widgets")
	t.Logf("Widgets' request:\n%s", p)
	if !strings.Contains(p, "\nSTATUS: 2 done, 1 confirmed, 1 draft\n") {
		t.Fatalf("Widgets' request has no member status counts:\n%s", p)
	}
	if strings.Index(p, "STATUS:") > strings.Index(p, "MATERIAL") {
		t.Fatal("the status counts come after the material")
	}
	if p := promptFor(fake, "Safety"); !strings.Contains(p, "STATUS: 1 none") || !strings.Contains(p, "a thin node") {
		t.Fatalf("Safety's request:\n%s", p)
	}
}

// TestKBAnnotator_ExistingLevelsKeepTheirVersions — adding the node levels changes
// no existing level's prompt version: records written by the body before them
// read current to the shipped body.
func TestKBAnnotator_ExistingLevelsKeepTheirVersions(t *testing.T) {
	shipped := knowledgeBaseConfig(t).Agents["kb/annotator"].Code
	if !strings.Contains(shipped, "prompt_versions: { branch: 2, document: 2, path: 2, ") {
		t.Fatal("the shipped body changed an existing level's prompt version")
	}
	d, author, driver := annRealDocument(t)
	a := seedAnnDoc(t, d, author, "Widget guide", "/kb/widgets/guide")
	before := strings.Replace(shipped, annShippedVersions, "prompt_versions: { branch: 2, document: 2, path: 2 },", 1)
	f := &annFake{reply: annStableReply}
	in := fmt.Sprintf(`{"mode":"document","document_id":%q,"write":"annotate"}`, a.doc)
	if rep := runAnnotatorWith(t, d, before, driver, f, in); annCount(rep, "writes") != 6 {
		t.Fatalf("the earlier body's pass = %v, want 6 writes", rep)
	}
	rep := runAnnotator(t, d, driver, f, fmt.Sprintf(`{"mode":"document","document_id":%q}`, a.doc))
	if annCount(rep, "would_call") != 0 {
		t.Fatalf("the shipped body finds the earlier body's records stale: %v", rep)
	}
	rep = runAnnotator(t, d, driver, f, `{"mode":"path","path":"/","recursive":true}`)
	if annCount(rep, "would_call") != 0 {
		t.Fatalf("the shipped body finds the earlier body's folder records stale: %v", rep)
	}
}

// TestKBAnnotator_OntologyModeComputesStatisticsWhenPermitted — a run that may run
// node_stats recomputes the statistics before it reads the nodes, so a written
// node's exemplars come from them.
func TestKBAnnotator_OntologyModeComputesStatisticsWhenPermitted(t *testing.T) {
	f := newAnnNodesFixture(t)
	driver := tools.UpdateRunPolicy(f.driver, func(rp *tools.RunPolicy) {
		rp.StatementMethods = append([]string{"rule"}, rp.StatementMethods...)
	})
	fake := &annFake{reply: annStableReply}
	rep := runAnnotator(t, f.d, driver, fake, `{"mode":"ontology","write":"annotate"}`)
	if onto, _ := rep["ontology"].(map[string]any); onto["statistics"] != "computed" {
		t.Fatalf("statistics = %v, want computed", rep["ontology"])
	}
	in := realDoc(t, f.d, f.op, fmt.Sprintf(`{"op":"annotation_inputs","scope":"tenant","level":"concept","id":%q,"prompt_version":1}`, f.concepts["Installation"]))
	if in["statistics"] == "missing" || in["stale"] != false {
		t.Fatalf("Installation after the pass: statistics %v stale %v", in["statistics"], in["stale"])
	}
}

// TestKBAnnotator_SweepEndsWithTheTaxonomy — a whole-scope sweep annotates the
// nodes after the documents and folders; one under a path does not.
func TestKBAnnotator_SweepEndsWithTheTaxonomy(t *testing.T) {
	f := newAnnNodesFixture(t)
	fake := &annFake{reply: annStableReply}
	rep := runAnnotator(t, f.d, f.driver, fake, `{"mode":"sweep","under_path":"/kb"}`)
	if _, ok := rep["ontology"]; ok {
		t.Fatalf("a sweep under a path walked the taxonomy: %v", rep)
	}
	rep = runAnnotator(t, f.d, f.driver, fake, `{"mode":"sweep"}`)
	onto, _ := rep["ontology"].(map[string]any)
	if onto["missing_count"] != float64(7) {
		t.Fatalf("a whole sweep's report = %v, want the 7 nodes listed", rep["ontology"])
	}
}
