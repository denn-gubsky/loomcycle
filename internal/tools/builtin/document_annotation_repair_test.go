package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A section summarised under another document's code, and a folder brief that
// says nothing: the document's identity in every annotation's inputs, and the
// repairs set_annotation makes.

// annRfcOntologyMD declares an RFC class the way a tenant does, with an example
// code in its description: the description reaches the model on every request
// for an RFC, so the example is the code a model copies into a section that
// carries none of its own.
const annRfcOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n" +
	"### Document\n\n" +
	"#### Rfc\nA design proposal named by a code, e.g. \"RFC ZZ — An example\".\n" +
	"@identifier_pattern ^RFC (?P<letter>[A-Z]{1,2})\\b\n" +
	"- `letter` text @identity\n"

type annRfcCorpus struct {
	doc, root, section string
}

// seedRfc makes "RFC GE — Annotations" whose one section names no code, and
// whose other section cites another RFC by its code.
func seedRfc(t *testing.T, d *Document, ctx context.Context) annRfcCorpus {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"RFC GE — Annotations","path":"/kb/rfcs/annotations","type":"Rfc"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	c := annRfcCorpus{doc: out["document_id"].(string), root: out["root_chunk_id"].(string)}
	o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"Decisions","body":"The owner approved the decisions on the brief, the abstract and the staleness hash. It builds on RFC AB for the statements."}`, c.doc))
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	c.section = o["id"].(string)
	return c
}

func TestAnnotationInputs_SectionCarriesItsDocumentTitleAndCode(t *testing.T) {
	d, ctx := annOntologyFixture(t, annRfcOntologyMD)
	c := seedRfc(t, d, ctx)
	in := annInputs(t, d, ctx, idTarget(c.section))
	if in["document_title"] != "RFC GE — Annotations" {
		t.Fatalf("document_title = %v, want the document's title", in["document_title"])
	}
	id, _ := in["identity"].(map[string]any)
	if id["letter"] != "GE" {
		t.Fatalf("identity = %v, want letter GE from the title", in["identity"])
	}
}

func TestAnnotationInputs_SectionHashChangesWithTheDocumentTitle(t *testing.T) {
	d, ctx := annOntologyFixture(t, annRfcOntologyMD)
	c := seedRfc(t, d, ctx)
	before := annInputs(t, d, ctx, idTarget(c.section))["inputs_sha256"]
	root, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, c.root))
	if r.IsError {
		t.Fatal(r.Text)
	}
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"title":"RFC GF — Annotations","revision":%v}`, c.root, root["revision"])); r.IsError {
		t.Fatalf("rename: %s", r.Text)
	}
	after := annInputs(t, d, ctx, idTarget(c.section))
	if after["inputs_sha256"] == before {
		t.Fatal("a section's inputs hash did not move when its document was renamed")
	}
	if id, _ := after["identity"].(map[string]any); id["letter"] != "GF" {
		t.Fatalf("identity after the rename = %v, want letter GF", after["identity"])
	}
}

func TestSetAnnotation_RewritesAnotherDocumentsCodeToTheDocumentsOwn(t *testing.T) {
	d, ctx := annOntologyFixture(t, annRfcOntologyMD)
	c := seedRfc(t, d, ctx)
	in := annInputs(t, d, ctx, idTarget(c.section))
	// The model's output, forced: the class example's code where the document's
	// belongs, and a citation the section really makes.
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","id":%q,"brief":%q,"abstract":%q,"inputs_sha256":%q}`,
		c.section, "RFC ZZ section on the approved decisions.",
		"RFC ZZ records the decisions on the brief, the abstract and the hash, building on RFC AB.", in["inputs_sha256"]))
	if r.IsError {
		t.Fatalf("set_annotation: %s", r.Text)
	}
	rec := out["annotation"].(map[string]any)
	if rec["brief"] != "RFC GE section on the approved decisions." {
		t.Errorf("brief = %q, want the document's own code", rec["brief"])
	}
	if rec["abstract"] != "RFC GE records the decisions on the brief, the abstract and the hash, building on RFC AB." {
		t.Errorf("abstract = %q, want the document's own code and the citation kept", rec["abstract"])
	}
	reps, _ := out["repaired"].([]any)
	if len(reps) != 2 {
		t.Fatalf("repaired = %v, want one per field", out["repaired"])
	}
	for i, field := range []string{"brief", "abstract"} {
		m := reps[i].(map[string]any)
		if m["field"] != field || m["from"] != "RFC ZZ" || m["to"] != "RFC GE" || m["reason"] != annotationRepairIdentity {
			t.Errorf("repaired[%d] = %v", i, m)
		}
	}
}

// An annotation that names only its own code, or a code its material cites, is
// stored as written and reports no repair.
func TestSetAnnotation_KeepsTheOwnCodeAndACitedOne(t *testing.T) {
	d, ctx := annOntologyFixture(t, annRfcOntologyMD)
	c := seedRfc(t, d, ctx)
	in := annInputs(t, d, ctx, idTarget(c.section))
	abstract := "RFC GE records its decisions, building on RFC AB for the statements."
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","id":%q,"brief":"The approved decisions of RFC GE.","abstract":%q,"inputs_sha256":%q}`,
		c.section, abstract, in["inputs_sha256"]))
	if r.IsError {
		t.Fatalf("set_annotation: %s", r.Text)
	}
	if _, has := out["repaired"]; has || out["annotation"].(map[string]any)["abstract"] != abstract {
		t.Fatalf("result = %v, want the text as written and no repair", out)
	}
}

// An operator's own text is never rewritten.
func TestSetAnnotation_ManualTextIsNotRepaired(t *testing.T) {
	d, ctx := annOntologyFixture(t, annRfcOntologyMD)
	c := seedRfc(t, d, ctx)
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","id":%q,"brief":"RFC ZZ decisions.","abstract":"RFC ZZ decisions in full.","manual":true}`, c.section))
	if r.IsError {
		t.Fatalf("set_annotation: %s", r.Text)
	}
	if out["annotation"].(map[string]any)["brief"] != "RFC ZZ decisions." {
		t.Fatalf("manual brief rewritten: %v", out)
	}
}

func TestFolderAnnotation_BriefRepeatingTheFolderNameTakesTheAbstractsFirstSentence(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	seedFolderCorpus(t, d, ctx)
	in := annInputs(t, d, ctx, `"level":"path","path":"/kb/products"`)
	abstract := "Guides and price lists for every product the company sells, kept by the product team. Each guide covers setup and care."
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","level":"path","path":"/kb/products","brief":"Products.","abstract":%q,"inputs_sha256":%q}`,
		abstract, in["inputs_sha256"]))
	if r.IsError {
		t.Fatalf("set_annotation: %s", r.Text)
	}
	want := "Guides and price lists for every product the company sells, kept by the product team."
	if got := out["annotation"].(map[string]any)["brief"]; got != want {
		t.Fatalf("brief = %q, want %q", got, want)
	}
	reps, _ := out["repaired"].([]any)
	if len(reps) != 1 {
		t.Fatalf("repaired = %v", out["repaired"])
	}
	if m := reps[0].(map[string]any); m["from"] != "Products." || m["to"] != want || m["reason"] != annotationRepairFolderName {
		t.Fatalf("repaired = %v", m)
	}
}

func TestRepairFolderBrief(t *testing.T) {
	long := strings.Repeat("word ", 80) + "end."
	cases := []struct {
		name, folder, brief, abstract, want, reason string
	}{
		{"the folder name is replaced", "/loomcycle/docs", "docs", "Product guides and design notes. More.", "Product guides and design notes.", annotationRepairFolderName},
		{"the name after normalising", "/kb/rfc-archive", "RFC Archive.", "Every retired proposal, by code.", "Every retired proposal, by code.", annotationRepairFolderName},
		{"a short brief is replaced", "/kb/x", "Some notes.", "Notes from the weekly review meetings! And more.", "Notes from the weekly review meetings!", annotationRepairFolderShort},
		{"a short brief with no abstract is kept", "/kb/x", "Some notes.", "", "Some notes.", ""},
		{"a good brief is kept", "/kb/x", "Notes from the weekly review meetings.", "Other.", "Notes from the weekly review meetings.", ""},
		{"the replacement is cut on a word", "/kb/notes", "x", long, strings.TrimSpace(strings.Repeat("word ", 48)), annotationRepairFolderShort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, r := repairFolderBrief(tc.folder, tc.brief, tc.abstract)
			if got != tc.want {
				t.Fatalf("brief = %q, want %q", got, tc.want)
			}
			reason := ""
			if r != nil {
				reason = r.Reason
			}
			if reason != tc.reason {
				t.Fatalf("reason = %q, want %q", reason, tc.reason)
			}
		})
	}
}
