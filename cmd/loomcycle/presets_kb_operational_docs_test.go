package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// Operational documents — a sync's SourceStub (an item it could not import as text)
// and its SyncReport — are not knowledge. The links agent and the judge pass over
// them; these tests hold both to that, and the bundle's list to the runtime's.

// callsNaming is every Document call, other than get_document, whose input names
// docID or one of its chunks.
func callsNaming(f *kbFixture, docID string, chunkIDs ...string) []string {
	names := append([]string{docID}, chunkIDs...)
	var out []string
	for _, c := range f.calls {
		if c.Tool != "Document" || c.Op == "get_document" {
			continue
		}
		b, _ := json.Marshal(c.Input)
		for _, n := range names {
			if strings.Contains(string(b), `"`+n+`"`) {
				out = append(out, c.Op+" "+string(b))
				break
			}
		}
	}
	return out
}

// TestKBLinker_DrainSkipsOperationalDocumentsAndAcksThem: a drain reaching a
// SourceStub or a SyncReport reads the document's type and goes no further — no
// classify, no chunks, no citations, no statements — acks the message as skipped,
// and still links the normal document after them.
func TestKBLinker_DrainSkipsOperationalDocumentsAndAcksThem(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.citations = []map[string]any{kbCiteHit("to the regulator", map[string]any{"resolved": resolvedTo("x2")})}
	f.docTypes["doc-stub"], f.roots["doc-stub"] = "SourceStub", "c-stub-root"
	f.docTypes["doc-report"], f.roots["doc-report"] = "SyncReport", "c-report-root"
	f.queue = []kbMessage{
		{ID: "msg-1", Value: map[string]any{"document_id": "doc-stub", "reason": "created"}},
		{ID: "msg-2", Value: map[string]any{"document_id": "doc-report", "reason": "imported"}},
		{ID: "msg-3", Value: map[string]any{"document_id": "doc-target", "reason": "created"}},
	}

	rep, _ := runLinker(t, f, `{"mode":"drain","write":"propose"}`)

	if got := callsNaming(f, "doc-stub", "c-stub-root"); len(got) != 0 {
		t.Errorf("the pass worked on the stub: %v", got)
	}
	if got := callsNaming(f, "doc-report", "c-report-root"); len(got) != 0 {
		t.Errorf("the pass worked on the sync report: %v", got)
	}
	msgs, _ := rep["messages"].([]any)
	want := []string{"skipped", "skipped", "complete"}
	if len(msgs) != len(want) {
		t.Fatalf("messages = %v", msgs)
	}
	for i, w := range want {
		m := msgs[i].(map[string]any)
		if m["outcome"] != w || m["acked"] != true {
			t.Errorf("messages[%d] = %v, want outcome %s, acked", i, m, w)
		}
	}
	if f.committed != 3 {
		t.Errorf("committed = %d, want all three messages consumed", f.committed)
	}
	if len(f.callsOf("Document", "propose_statement")) == 0 || len(f.callsOf("Document", "classify")) != 1 {
		t.Errorf("the normal document was not linked: propose %d, classify %d",
			len(f.callsOf("Document", "propose_statement")), len(f.callsOf("Document", "classify")))
	}
	skipped, _ := rep["skipped_documents"].([]any)
	if len(skipped) != 2 {
		t.Fatalf("skipped_documents = %v", rep["skipped_documents"])
	}
	for i, w := range []string{"SourceStub", "SyncReport"} {
		s := skipped[i].(map[string]any)
		if s["type"] != w || s["reason"] != "operational_document" {
			t.Errorf("skipped_documents[%d] = %v", i, s)
		}
	}
}

// TestKBLinker_DocumentPassOverAStubDoesNothing: asked for a stub directly, every
// layer (maintenance included) is passed over, the type matched whatever its case.
func TestKBLinker_DocumentPassOverAStubDoesNothing(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.docTypes["doc-stub"], f.roots["doc-stub"] = "sourcestub", "c-stub-root"

	rep, _ := runLinker(t, f, `{"document_id":"doc-stub","write":"propose","layers":["L0","L1","L2","L3","L6"]}`)

	if got := callsNaming(f, "doc-stub", "c-stub-root"); len(got) != 0 {
		t.Errorf("the pass worked on the stub: %v", got)
	}
	if rep["status"] != "complete" || rep["document_id"] != "doc-stub" {
		t.Errorf("report = %v", rep)
	}
	if s, _ := rep["skipped_documents"].([]any); len(s) != 1 || s[0].(map[string]any)["type"] != "SourceStub" {
		t.Errorf("skipped_documents = %v", rep["skipped_documents"])
	}
}

// TestKBJudge_ProposalsTouchingAnOperationalDocumentAreNotJudged: a proposal from a
// stub's root, or to a sync report, costs no verdict call and is reported skipped;
// the normal proposal beside them is judged.
func TestKBJudge_ProposalsTouchingAnOperationalDocumentAreNotJudged(t *testing.T) {
	f := newKBJudgeFixture()
	f.docs["dstub"] = map[string]any{"document_id": "dstub", "title": "A canvas", "type": "SourceStub"}
	f.docs["dreport"] = map[string]any{"document_id": "dreport", "title": "Sync of 2026-10", "type": "SyncReport"}
	f.chunks["cstub"] = map[string]any{"id": "cstub", "document_id": "dstub", "title": "A canvas", "type": "SourceStub", "body": ""}
	f.chunks["creport"] = map[string]any{"id": "creport", "document_id": "dreport", "title": "Run", "type": "section", "body": judgeBodyTo}
	f.addProposal("s2", "cstub", "cb", "dstub", "db", []map[string]any{kbJudgeEvidence("cb", judgeBodyTo, "shall report a material incident")})
	f.addProposal("s3", "ca", "creport", "da", "dreport", []map[string]any{kbJudgeEvidence("creport", judgeBodyTo, "shall report a material incident")})

	rep := runKBJudge(t, f, `{"scope":"tenant"}`)

	if len(f.prompts) != 1 {
		t.Errorf("verdict calls = %d, want 1 (the normal proposal only)", len(f.prompts))
	}
	sent := kbJudgementsSent(t, f)
	if len(sent) != 1 || sent[0]["id"] != "s1" {
		t.Errorf("judgements = %v, want s1 only", sent)
	}
	if sk, _ := rep["skipped"].(map[string]any); sk["operational_document"] != float64(2) {
		t.Errorf("skipped = %v, want operational_document 2", rep["skipped"])
	}
}

// TestKnowledgeBaseBundle_SkipsTheRuntimesOperationalTypes: the bundle's list of
// types to pass over is the runtime's, so a type added to one is added to both.
func TestKnowledgeBaseBundle_SkipsTheRuntimesOperationalTypes(t *testing.T) {
	cfg := knowledgeBaseConfig(t)
	re := regexp.MustCompile(`var OPERATIONAL_DOCUMENT_TYPES = (\[[^\]]*\]);`)
	want, _ := json.Marshal(memrank.OperationalDocumentTypes())
	for _, name := range []string{"kb/linker", "kb/judge"} {
		m := re.FindStringSubmatch(cfg.Agents[name].Code)
		if m == nil {
			t.Errorf("%s declares no OPERATIONAL_DOCUMENT_TYPES", name)
			continue
		}
		var got []string
		if err := json.Unmarshal([]byte(m[1]), &got); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if g, _ := json.Marshal(got); string(g) != string(want) {
			t.Errorf("%s skips %s, the runtime %s", name, g, want)
		}
	}
}

// TestKBLinker_StubPassAgainstTheRealDocumentTool: against the real Document tool, a
// SourceStub that names a scheme's terms gets no statement and keeps its type, while
// a Law with the same text beside it gets its terms linked.
func TestKBLinker_StubPassAgainstTheRealDocumentTool(t *testing.T) {
	d, ctx := realDocument(t)
	ctx = withRealOntology(t, d, ctx, kbRealOntologyMD)
	// Past the linker's min_body_chars (80), so the Law's part is read.
	const body = "Цей текст згадує договір, який оператор укладає з клієнтом на надання послуг у межах цього Закону."
	mk := func(title, typ string) string {
		doc := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"type":%q}`, title, typ))
		realDoc(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"Стаття 2","body":%q}`, doc["document_id"], body))
		return doc["document_id"].(string)
	}
	stub, law := mk("Закон про стаб", "SourceStub"), mk("Закон про послуги", "Law")
	statements := func(docID string) int {
		n := 0
		for _, status := range []string{"proposed", "asserted"} {
			list := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"list_statements","scope":"tenant","status":%q,"document_id":%q}`, status, docID))
			n += len(list["statements"].([]any))
		}
		return n
	}

	in := `{"document_id":%q,"scope":"tenant","write":"propose","layers":["L0","L3"],"terms_scheme":"topics"}`
	rep := runRealLinker(t, d, ctx, fmt.Sprintf(in, stub))
	if s, _ := rep["skipped_documents"].([]any); len(s) != 1 {
		t.Errorf("skipped_documents = %v", rep["skipped_documents"])
	}
	if n := statements(stub); n != 0 {
		t.Errorf("the stub has %d statements, want none", n)
	}
	if got := realDoc(t, d, ctx, fmt.Sprintf(`{"op":"get_document","scope":"tenant","id":%q}`, stub))["type"]; got != "SourceStub" {
		t.Errorf("the stub's type = %v", got)
	}
	runRealLinker(t, d, ctx, fmt.Sprintf(in, law))
	if statements(law) == 0 {
		t.Error("the Law beside it got no statement, so the stub's none proves nothing")
	}
}
