package main

import (
	"strings"
	"testing"
)

// Filing (RFC FP Phase D, owner decision D2: only on explicit input): a document
// whose class is in force gets a name in its class's home.

func filingFixture() *kbFixture {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.classified = map[string]any{"result": "classified", "live": true, "class": "Law"}
	return f
}

const filingInput = `{"document_id":"doc-target","layers":["L0"],"write":"propose","file":{"root":"/board/by-class"}}`

// TestKBLinker_FilingAddsANameInTheClassHome: with file, a live class files the
// document at <root>/<Class>/<title> through set_path, and the report names the class
// and the outcome but never the path (it carries the title).
func TestKBLinker_FilingAddsANameInTheClassHome(t *testing.T) {
	f := filingFixture()

	rep, raw := runLinker(t, f, filingInput)

	calls := f.callsOf("Document", "set_path")
	if len(calls) != 1 || calls[0].Input["id"] != "doc-target" || calls[0].Input["path"] != "/board/by-class/Law/TITLE-OF-THE-TARGET" {
		t.Fatalf("set_path calls = %v, want doc-target at /board/by-class/Law/TITLE-OF-THE-TARGET", calls)
	}
	fl, _ := layerReport(t, rep, "L0")["filing"].(map[string]any)
	if fl["status"] != "filed" || fl["class"] != "Law" {
		t.Errorf("filing = %v, want filed under Law", fl)
	}
	if strings.Contains(raw, "TITLE-OF-THE-TARGET") {
		t.Errorf("the report carries the document's title:\n%s", raw)
	}
}

// TestKBLinker_FilingNeverTakesAnotherDocumentsName: a name another document holds is
// left to it; the document's id is appended.
func TestKBLinker_FilingNeverTakesAnotherDocumentsName(t *testing.T) {
	f := filingFixture()
	f.docTypes["doc-other"] = "Law"
	f.docPaths["doc-other"] = "/board/by-class/Law/TITLE-OF-THE-TARGET"

	rep, _ := runLinker(t, f, filingInput)

	calls := f.callsOf("Document", "set_path")
	if len(calls) != 1 || calls[0].Input["path"] != "/board/by-class/Law/TITLE-OF-THE-TARGET-doc-targ" {
		t.Fatalf("set_path calls = %v, want the name suffixed with the document's id", calls)
	}
	if fl, _ := layerReport(t, rep, "L0")["filing"].(map[string]any); fl["suffixed"] != true {
		t.Errorf("filing = %v, want suffixed", fl)
	}
}

// TestKBLinker_FilingOnlyWhenAskedAndOnlyForALiveClass: no file input, a class that is
// only proposed, or a report pass: nothing is filed (the report pass counts it).
func TestKBLinker_FilingOnlyWhenAskedAndOnlyForALiveClass(t *testing.T) {
	f := filingFixture()
	runLinker(t, f, `{"document_id":"doc-target","layers":["L0"],"write":"propose"}`)
	if n := len(f.callsOf("Document", "set_path")); n != 0 {
		t.Errorf("without file: set_path calls = %d", n)
	}

	g := filingFixture()
	g.classified = map[string]any{"result": "proposed", "live": false, "class": "Law"}
	runLinker(t, g, filingInput)
	if n := len(g.callsOf("Document", "set_path")); n != 0 {
		t.Errorf("a proposed class: set_path calls = %d", n)
	}

	h := filingFixture()
	h.classified = map[string]any{"result": "classified", "live": false, "class": "Law"}
	rep, _ := runLinker(t, h, `{"document_id":"doc-target","layers":["L0"],"file":{"root":"/board/by-class"}}`)
	assertNoWrites(t, h, rep)
	if fl, _ := layerReport(t, rep, "L0")["filing"].(map[string]any); fl["status"] != "would_file" {
		t.Errorf("report pass filing = %v, want would_file", fl)
	}
}

// TestKBLinker_FilingInputIsChecked: file needs a root made of path segments and the
// L0 layer.
func TestKBLinker_FilingInputIsChecked(t *testing.T) {
	for _, in := range []string{
		`{"document_id":"doc-target","layers":["L0"],"file":{"root":"board"}}`,
		`{"document_id":"doc-target","layers":["L0"],"file":{"root":"/a b"}}`,
		`{"document_id":"doc-target","layers":["L0"],"file":{"root":"/ok","move":true}}`,
		`{"document_id":"doc-target","layers":["L2"],"file":{"root":"/ok"}}`,
	} {
		f := filingFixture()
		rep, _ := runLinker(t, f, in)
		if rep["status"] != "refused" || len(f.calls) != 0 {
			t.Errorf("%s: status %v after %d calls, want refused before any", in, rep["status"], len(f.calls))
		}
	}
}
