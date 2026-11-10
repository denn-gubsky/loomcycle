package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// The ontology's say in annotations: a class's meaning and @annotate slots in
// what an annotation is written from, the tenant's enforced language, and a
// folder named for a concept.

const annOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n" +
	"### Document\n\n" +
	"#### Guide\nA how-to guide for one product.\n@annotate the steps; the caveats\n\n" +
	"##### QuickGuide\nA one-page guide.\n\n" +
	"## Schemes\n@section schemes\n\n" +
	"### topics\n\n#### Products\nThe things the company sells.\n@alt product-catalogue\n"

func annOntologyFixture(t *testing.T, md string) (*Document, context.Context) {
	t.Helper()
	d, base, _ := documentFixture(t)
	ctx := annOperator(installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus))
	return d, ctx
}

func TestAnnotationInputs_CarryTheClassMeaningAndSlots(t *testing.T) {
	d, ctx := annOntologyFixture(t, annOntologyMD)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Widget guide","path":"/kb/widgets","type":"QuickGuide"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	doc := out["document_id"].(string)
	in := annInputs(t, d, ctx, `"document_id":"`+doc+`"`)
	// QuickGuide declares no slots of its own: it inherits Guide's.
	if in["class"] != "QuickGuide" || in["class_description"] != "A one-page guide." || in["slots"] != "the steps; the caveats" {
		t.Fatalf("inputs class = %v / %v / %v", in["class"], in["class_description"], in["slots"])
	}
	desc, _ := docExec(t, d, ctx, `{"op":"describe_ontology"}`)
	found := false
	for _, c := range desc["classes"].([]any) {
		m := c.(map[string]any)
		if m["name"] == "Guide" {
			dirs, _ := m["directives"].(map[string]any)
			found = dirs["annotate"] == "the steps; the caveats" && m["meaning"] == "A how-to guide for one product."
		}
	}
	if !found {
		t.Fatalf("describe_ontology does not report Guide's annotate slots and meaning: %v", desc["classes"])
	}
}

func TestParseClassBody_AnnotateJoinsLinesAndReportsAnEmptyOne(t *testing.T) {
	d := memrank.ParseClassBody("A thing.\n@annotate the client; the scope\n@annotate the price\n@annotate\n")
	if d.Annotate != "the client; the scope; the price" || d.Meaning != "A thing." {
		t.Fatalf("decl = %+v", d)
	}
	if len(d.Issues) != 1 || !strings.Contains(d.Issues[0], "@annotate") {
		t.Fatalf("issues = %v", d.Issues)
	}
}

// The tenant's ontology root can enforce one language for every annotation: the
// detector is then not asked, and a change of it is a change of every hash.
func TestAnnotationInputs_EnforcedLanguage(t *testing.T) {
	d, ctx := annOntologyFixture(t, annOntologyMD)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Widget guide","path":"/kb/widgets","type":"Guide"}`)
	doc := out["document_id"].(string)
	if _, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"tenant","document_id":"`+doc+`","title":"Body","body":"The widget is installed in three steps and is checked by the installer."}`); r.IsError {
		t.Fatal(r.Text)
	}
	if in := annInputs(t, d, ctx, `"document_id":"`+doc+`"`); in["language"] != "en" {
		t.Fatalf("detected language = %v", in["language"])
	}
	annWrite(t, d, ctx, `"document_id":"`+doc+`"`, "A guide")
	read, _ := d.tenantOntologyRead(ctx)
	g, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+read.RootChunkID+`"}`)
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"fields":{"annotation_language":"uk"},"revision":%v}`,
		read.RootChunkID, g["revision"])); r.IsError {
		t.Fatal(r.Text)
	}
	in := annInputs(t, d, ctx, `"document_id":"`+doc+`"`)
	if in["language"] != "uk" || in["stale"] != true {
		t.Fatalf("enforced: language %v, stale %v; want uk and stale", in["language"], in["stale"])
	}
}

func TestFolderInputs_NameTheConceptAFolderIsNamedFor(t *testing.T) {
	d, ctx := annOntologyFixture(t, annOntologyMD)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Widget guide","path":"/kb/product-catalogue/widgets","type":"Guide"}`)
	_ = out
	in, r := docExec(t, d, ctx, `{"op":"annotation_inputs","scope":"tenant","level":"path","path":"/kb/product-catalogue"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	if in["concept"] != "Products" || in["concept_meaning"] != "The things the company sells." {
		t.Fatalf("folder concept = %v / %v", in["concept"], in["concept_meaning"])
	}
}
