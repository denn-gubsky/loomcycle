package builtin

import (
	"context"
	"fmt"
	"testing"
)

// A done RFC's annotation that still calls the RFC a proposal: the framing is
// removed at save, and nothing else is.

// seedStatusRfc makes an RFC titled title with the stored status status and a
// section that cites another RFC's proposal.
func seedStatusRfc(t *testing.T, d *Document, ctx context.Context, title, status string) annRfcCorpus {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"type":"Rfc","status":%q,"body":"The design, as it was written while it was a proposal."}`, title, status))
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	c := annRfcCorpus{doc: out["document_id"].(string), root: out["root_chunk_id"].(string)}
	o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"Background","body":"RFC AB proposes a statement graph, awaiting approval."}`, c.doc))
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	c.section = o["id"].(string)
	return c
}

// setDocAnnotation writes a model's document-level annotation of the root and
// returns set_annotation's result.
func setDocAnnotation(t *testing.T, d *Document, ctx context.Context, root, brief, abstract string) map[string]any {
	t.Helper()
	in := annInputs(t, d, ctx, idTarget(root))
	if in["level"] != annotationLevelDocument {
		t.Fatalf("level of the root = %v, want document", in["level"])
	}
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","id":%q,"brief":%q,"abstract":%q,"inputs_sha256":%q}`,
		root, brief, abstract, in["inputs_sha256"]))
	if r.IsError {
		t.Fatalf("set_annotation: %s", r.Text)
	}
	return out
}

func repairReasons(out map[string]any) []string {
	var reasons []string
	reps, _ := out["repaired"].([]any)
	for _, r := range reps {
		reasons = append(reasons, fmt.Sprint(r.(map[string]any)["reason"]))
	}
	return reasons
}

// The four briefs a model wrote on dev for done RFCs lose the proposal framing
// and keep the rest as written.
func TestSetAnnotation_DoneDocumentBriefLosesTheProposalFraming(t *testing.T) {
	cases := []struct{ title, brief, want string }{
		{"RFC AM — Path console",
			"Design proposal (RFC AM) for a Web UI that adds a Path tree console, with a browser and a search.",
			"RFC AM: a Web UI that adds a Path tree console, with a browser and a search."},
		{"RFC AR — Credentials",
			"Design proposal RFC AR (credentials) for loomcycle, defining a tenant-scoped CredentialDef store; status done.",
			"RFC AR (credentials) for loomcycle, defining a tenant-scoped CredentialDef store; status done."},
		{"RFC CY — Team definitions",
			"Design proposal RFC CY, the loomcycle half of extending TeamDef into a full team runtime.",
			"RFC CY, the loomcycle half of extending TeamDef into a full team runtime."},
		{"RFC GL — Question routing",
			"Design proposal RFC GL, done and shipped in v1.153.0, routing knowledge-base questions by type: lookups first.",
			"RFC GL, done and shipped in v1.153.0, routing knowledge-base questions by type: lookups first."},
	}
	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			d, ctx := annOntologyFixture(t, annRfcOntologyMD)
			c := seedStatusRfc(t, d, ctx, tc.title, "done")
			out := setDocAnnotation(t, d, ctx, c.root, tc.brief, "The routing and its walker, as built.")
			if got := out["annotation"].(map[string]any)["brief"]; got != tc.want {
				t.Fatalf("brief = %q, want %q", got, tc.want)
			}
			if r := repairReasons(out); len(r) != 1 || r[0] != annotationRepairStatusWording {
				t.Fatalf("repaired = %v, want one %s repair", out["repaired"], annotationRepairStatusWording)
			}
		})
	}
}

// An abstract that says the done RFC proposes, would add, or awaits approval
// is reworded; its stale status sentence is dropped; a sentence about another
// RFC's proposal stays.
func TestSetAnnotation_DoneDocumentAbstractSelfReferenceIsReworded(t *testing.T) {
	d, ctx := annOntologyFixture(t, annRfcOntologyMD)
	c := seedStatusRfc(t, d, ctx, "RFC GL — Question routing", "done")
	abstract := "This RFC proposes routing knowledge-base questions by type. RFC GL would add a walker for lookups. " +
		"It builds on RFC AB, which proposes a statement graph. Status is Design LOCKED 2026-09-10. This RFC is awaiting approval."
	out := setDocAnnotation(t, d, ctx, c.root, "Routing knowledge-base questions by type.", abstract)
	want := "This RFC specifies routing knowledge-base questions by type. RFC GL adds a walker for lookups. " +
		"It builds on RFC AB, which proposes a statement graph."
	if got := out["annotation"].(map[string]any)["abstract"]; got != want {
		t.Fatalf("abstract = %q, want %q", got, want)
	}
	if r := repairReasons(out); len(r) != 4 {
		t.Fatalf("repaired = %v, want four %s repairs", out["repaired"], annotationRepairStatusWording)
	}
}

// Only a done document is repaired: for a confirmed one the proposal framing
// may be true, and the text is stored as written.
func TestSetAnnotation_ConfirmedDocumentKeepsTheProposalFraming(t *testing.T) {
	for _, status := range []string{"confirmed", "draft", "review", ""} {
		t.Run("status "+status, func(t *testing.T) {
			d, ctx := annOntologyFixture(t, annRfcOntologyMD)
			c := seedStatusRfc(t, d, ctx, "RFC GL — Question routing", status)
			brief := "Design proposal RFC GL, routing knowledge-base questions by type."
			abstract := "This RFC proposes routing knowledge-base questions by type. This RFC is awaiting approval."
			out := setDocAnnotation(t, d, ctx, c.root, brief, abstract)
			rec := out["annotation"].(map[string]any)
			if _, has := out["repaired"]; has || rec["brief"] != brief || rec["abstract"] != abstract {
				t.Fatalf("result = %v, want the text as written and no repair", out)
			}
		})
	}
}

// A done document's annotation that cites another RFC's proposal is not about
// itself and stays as written.
func TestSetAnnotation_DoneDocumentKeepsACitedProposal(t *testing.T) {
	d, ctx := annOntologyFixture(t, annRfcOntologyMD)
	c := seedStatusRfc(t, d, ctx, "RFC GL — Question routing", "done")
	brief := "Question routing by type, building on the proposal for statements."
	abstract := "Routing questions by type. RFC AB proposes a statement graph, awaiting approval. The proposal of RFC AB would add edges."
	out := setDocAnnotation(t, d, ctx, c.root, brief, abstract)
	rec := out["annotation"].(map[string]any)
	if _, has := out["repaired"]; has || rec["brief"] != brief || rec["abstract"] != abstract {
		t.Fatalf("result = %v, want the text as written and no repair", out)
	}
}

// An operator's own text on a done document is stored as written.
func TestSetAnnotation_DoneDocumentManualTextIsNotRepaired(t *testing.T) {
	d, ctx := annOntologyFixture(t, annRfcOntologyMD)
	c := seedStatusRfc(t, d, ctx, "RFC GL — Question routing", "done")
	brief, abstract := "Design proposal RFC GL, routing questions.", "This RFC proposes routing questions by type."
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","id":%q,"brief":%q,"abstract":%q,"manual":true}`, c.root, brief, abstract))
	if r.IsError {
		t.Fatalf("set_annotation: %s", r.Text)
	}
	rec := out["annotation"].(map[string]any)
	if _, has := out["repaired"]; has || rec["brief"] != brief || rec["abstract"] != abstract {
		t.Fatalf("result = %v, want the manual text as written", out)
	}
}

func TestRepairStatusWording(t *testing.T) {
	cases := []struct{ name, brief, want string }{
		{"an article and a for", "A design proposal for a Path console.", "A design for a Path console."},
		{"a side qualifier", "Loomcycle-side design proposal for team definitions.", "Loomcycle-side design for team definitions."},
		{"a bare proposal with for", "Proposal for routing questions by type.", "Design for routing questions by type."},
		{"a design RFC", "Design RFC GL, routing questions.", "RFC GL, routing questions."},
		{"a colon", "Design proposal: routing questions.", "Routing questions."},
		{"a bare proposal naming a workflow stays", "Proposal workflow for RFCs, with review.", "Proposal workflow for RFCs, with review."},
		{"a lower-case parenthesis stays", "Design proposal (draft) for routing.", "Design proposal (draft) for routing."},
		{"a proposal mid-sentence stays", "Routing questions, a design proposal no longer.", "Routing questions, a design proposal no longer."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := repairStatusWording(annotationLevelDocument, "done", "RFC GL", tc.brief, "x")
			if got != tc.want {
				t.Fatalf("brief = %q, want %q", got, tc.want)
			}
		})
	}
	// A section's record is the section's: it is not repaired.
	if got, _, r := repairStatusWording(annotationLevelBranch, "done", "RFC GL", "Design proposal RFC GL.", "x"); got != "Design proposal RFC GL." || r != nil {
		t.Fatalf("branch brief = %q (%v), want it as written", got, r)
	}
	// A version's full stop does not end the status sentence.
	_, abs, _ := repairStatusWording(annotationLevelDocument, "done", "", "x", "Routing. Status: locked in v1.2.3 by the owner. More.")
	if abs != "Routing. More." {
		t.Fatalf("abstract = %q", abs)
	}
}
