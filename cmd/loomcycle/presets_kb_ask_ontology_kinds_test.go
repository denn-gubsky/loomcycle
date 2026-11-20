package main

import (
	"strings"
	"testing"
)

// kb/ask reads the kinds a listing may ask for from the tenant ontology's document
// classes (presets_kb_ask_test.go holds the fake, the period and listing tests the
// route helpers and the notes).

// kindsClass is one class as describe_ontology describes it.
func kindsClass(name, parent, kind string, alt ...string) map[string]any {
	c := map[string]any{"name": name, "kind": kind, "source": "tenant"}
	if parent != "" {
		c["parent"] = parent
	}
	if len(alt) > 0 {
		c["alt"] = alt
	}
	return c
}

// kindsOntology is a confirmed tenant ontology's description: the root Document,
// the kinds set 12 asked for, a subclass, a sibling the head noun reaches, an
// operational type and an entity type. runbook adds Runbook, with or without its
// other name.
func kindsOntology(confirmed bool, runbook ...string) map[string]any {
	classes := []any{
		kindsClass("Document", "", "document"),
		kindsClass("Rfc", "Document", "document"),
		kindsClass("DesignNote", "Document", "document"),
		kindsClass("Plan", "Document", "document"),
		kindsClass("ImplementationPlan", "Plan", "document"),
		kindsClass("Research", "Document", "document", "research paper"),
		kindsClass("TechnicalResearch", "Document", "document"),
		kindsClass("Guide", "Document", "document"),
		kindsClass("SourceStub", "Document", "document"),
		kindsClass("person", "", "chunk"),
	}
	if len(runbook) > 0 {
		classes = append(classes, kindsClass("Runbook", "Document", "document", runbook[1:]...))
	}
	return map[string]any{"exists": true, "confirmed": confirmed, "status": map[bool]string{true: "confirmed", false: "draft"}[confirmed], "classes": classes}
}

// classesNote is a listing note followed by the CLASSES line naming classes.
func classesNote(note string, classes ...string) string {
	return note + "\nCLASSES: " + strings.Join(classes, ", ") + ". Pass each class name to type exactly as written, " +
		"one listing per class; each includes the kinds under it."
}

// wantOntologyWalk runs q against the ontology, under the cascade and
// classifier=agent, and checks it went to the walker as a listing with exactly
// wantNote after the question, after one describe_ontology call.
func wantOntologyWalk(t *testing.T, ontology map[string]any, q, wantNote string) {
	t.Helper()
	for name, metadata := range askMetadatas {
		t.Run(name+"/"+q, func(t *testing.T) {
			f := &askFake{score: 0.9, answers: askAnswers("YES", "partial"), ontology: ontology}
			out := runAsk(t, f, q, metadata)
			wantSpawns(t, f, "kb/walker")
			wantRouteLine(t, out, "ROUTE: walker | SCORE: none | CLASSIFIER: listing | RUNS: r1:completed")
			if pr, _ := f.inputs[0]["prompt"].(string); pr != q+"\n\n"+wantNote {
				t.Errorf("the walker's prompt =\n%s\nwant\n%s", pr, q+"\n\n"+wantNote)
			}
			if len(f.docCalls) != 1 || f.docCalls[0]["op"] != "describe_ontology" {
				t.Errorf("Document calls = %v, want one describe_ontology", f.docCalls)
			}
		})
	}
}

// wantOntologyCascade runs q against the ontology and checks no listing rule took it.
func wantOntologyCascade(t *testing.T, ontology map[string]any, q string) {
	t.Helper()
	t.Run(q, func(t *testing.T) {
		f := &askFake{score: 0.9, answers: askAnswers("YES", "complete"), ontology: ontology}
		out := runAsk(t, f, q, nil)
		wantSpawns(t, f, "kb/survey")
		wantRouteLine(t, out, "ROUTE: survey | SCORE: 0.9 | CLASSIFIER: decision | RUNS: r1:completed")
		if pr, _ := f.inputs[0]["prompt"].(string); pr != q {
			t.Errorf("the survey's prompt = %q, want the question alone", pr)
		}
	})
}

// TestKbAsk_KindListingNamesTheOntologyClasses: with a confirmed ontology the kinds
// are its document classes, said as words or by an @alt name, and the walker's note
// names the classes the kind resolved to: the class itself, a class whose name ends
// with the kind (research is Research and TechnicalResearch, the sibling set 12's
// walk missed), less a class whose ancestor is named (Plan covers
// ImplementationPlan).
func TestKbAsk_KindListingNamesTheOntologyClasses(t *testing.T) {
	o := kindsOntology(true, "Runbook", "playbook")
	for _, tc := range []struct {
		q, note string
		classes []string
	}{
		{"Which playbooks do we have?", listingTypeNote, []string{"Runbook"}},
		{"Which runbooks do we have?", listingTypeNote, []string{"Runbook"}},
		{"Which research do we have?", listingTypeNote, []string{"Research", "TechnicalResearch"}},
		{"Which research papers are there?", listingTypeNote, []string{"Research"}},
		{"Which plans do we have?", listingTypeNote, []string{"Plan"}},
		{"Which implementation plans do we have?", listingTypeNote, []string{"ImplementationPlan"}},
		{"Which design notes do we have?", listingTypeNote, []string{"DesignNote"}},
		{"What RFCs do we hold?", listingTypeNote, []string{"Rfc"}},
		{"Which of our RFCs are still drafts?", statusNote("draft"), []string{"Rfc"}},
		{"Show me every plan that is not done yet.", statusNote("draft, review, confirmed"), []string{"Plan"}},
		{"Which RFCs has the owner confirmed but not yet marked done?", statusNote("confirmed"), []string{"Rfc"}},
	} {
		wantOntologyWalk(t, o, tc.q, classesNote(tc.note, tc.classes...))
	}
}

// TestKbAsk_KindTheOntologyDoesNotDeclareStaysWithTheCascade: a kind is a class the
// ontology declares, so removing the class (or its other name) stops the routing.
// The root Document class, an operational type and an entity type never make a
// listing, and the set 12 twins stay with the cascade.
func TestKbAsk_KindTheOntologyDoesNotDeclareStaysWithTheCascade(t *testing.T) {
	for _, q := range []string{"Which playbooks do we have?", "Which runbooks do we have?"} {
		wantOntologyCascade(t, kindsOntology(true), q)
	}
	wantOntologyCascade(t, kindsOntology(true, "Runbook"), "Which playbooks do we have?")
	for _, q := range []string{
		"Which documents do we have?", "Which source stubs do we have?", "Which persons do we have?",
		"Which security RFCs do we have?", "Which design notes cover the scheduler?", "What is the status of the scheduler RFC?",
		"What do we hold on the external-document catalogue?", "What is the classification graph, and how much of it is built?",
	} {
		wantOntologyCascade(t, kindsOntology(true, "Runbook", "playbook"), q)
	}
}

// TestKbAsk_KindsFallBackWhenTheOntologyNamesNone: no Document tool, a failed call,
// a draft ontology, or one with no document class but the root: the fixed vocabulary
// routes as before, with no CLASSES line, and a kind only an ontology could name
// stays with the cascade. Routing never fails because of the ontology.
func TestKbAsk_KindsFallBackWhenTheOntologyNamesNone(t *testing.T) {
	rootOnly := map[string]any{"exists": true, "confirmed": true, "classes": []any{kindsClass("Document", "", "document"), kindsClass("person", "", "chunk")}}
	for name, f := range map[string]func() *askFake{
		"no Document tool": func() *askFake { return &askFake{} },
		"failed call":      func() *askFake { return &askFake{ontologyErr: "Document: unavailable: the store is down"} },
		"draft":            func() *askFake { return &askFake{ontology: kindsOntology(false, "Runbook", "playbook")} },
		"root only":        func() *askFake { return &askFake{ontology: rootOnly} },
	} {
		t.Run(name, func(t *testing.T) {
			fk := f()
			fk.score, fk.answers = 0.9, askAnswers("YES", "partial")
			const q = "Which design notes do we have?"
			out := runAsk(t, fk, q, nil)
			wantRouteLine(t, out, "ROUTE: walker | SCORE: none | CLASSIFIER: listing | RUNS: r1:completed")
			if pr, _ := fk.inputs[0]["prompt"].(string); pr != q+"\n\n"+listingTypeNote {
				t.Errorf("the walker's prompt =\n%s\nwant the fallback note and no CLASSES line", pr)
			}
			fk2 := f()
			fk2.score, fk2.answers = 0.9, askAnswers("YES", "complete")
			wantRouteLine(t, runAsk(t, fk2, "Which playbooks do we have?", nil), "ROUTE: survey | SCORE: 0.9 | CLASSIFIER: decision | RUNS: r1:completed")
		})
	}
}

// TestKbAsk_SetTwelvePairsHoldUnderTheOntology: every status and kind pair routes the
// same with the ontology's classes as with the fixed vocabulary: each positive is a
// listing naming its class, each twin stays with the cascade.
func TestKbAsk_SetTwelvePairsHoldUnderTheOntology(t *testing.T) {
	o := kindsOntology(true)
	pairs := append([]struct{ pos, twin string }{}, typePairs...)
	for _, set := range [][]struct{ pos, status, twin string }{statusPairs, statusVerbPairs} {
		for _, p := range set {
			pairs = append(pairs, struct{ pos, twin string }{p.pos, p.twin})
		}
	}
	for _, p := range pairs {
		t.Run(p.pos, func(t *testing.T) {
			f := &askFake{score: 0.9, answers: askAnswers("YES", "partial"), ontology: o}
			out := runAsk(t, f, p.pos, nil)
			wantRouteLine(t, out, "ROUTE: walker | SCORE: none | CLASSIFIER: listing | RUNS: r1:completed")
			if pr, _ := f.inputs[0]["prompt"].(string); !strings.Contains(pr, "\nCLASSES: ") {
				t.Errorf("the walker's prompt names no class:\n%s", pr)
			}
		})
		wantOntologyCascade(t, o, p.twin)
	}
}

// TestKbAsk_OntologyIsReadOnlyForAListingShapedQuestion: a question no listing rule
// could take (no question word or list verb at a clause start) costs no
// describe_ontology call.
func TestKbAsk_OntologyIsReadOnlyForAListingShapedQuestion(t *testing.T) {
	f := &askFake{score: 0.9, answers: askAnswers("YES", "complete"), ontology: kindsOntology(true)}
	runAsk(t, f, "Tell me about the scheduler and its retries.", nil)
	if len(f.docCalls) != 0 {
		t.Errorf("Document calls = %v, want none", f.docCalls)
	}
}
