package main

import (
	"fmt"
	"strings"
	"testing"
)

// skyOneDoc is a fake holding one doc whose outline the structurer is asked for.
func skyOneDoc(t *testing.T) *skyFake {
	f := newSkyFake(t)
	f.items = []*skyItem{skyDocItem("d1", "Plan", "2026-01-01T00:00:00Z")}
	return f
}

// TestSkybitsImport_AnInvalidOutlineIsDiscarded: the structurer's reply is
// data. Each malformed reply is refused, the extract's own outline kept, the
// document imported anyway and the reason reported.
func TestSkybitsImport_AnInvalidOutlineIsDiscarded(t *testing.T) {
	cases := map[string]struct {
		reply any
		want  string
	}{
		"a line that is not in the text": {map[string]any{"headings": []any{map[string]any{"line": 999, "level": 2, "text": "Budget"}}, "notes": ""}, "not a heading or a candidate"},
		"a rewritten line":               {map[string]any{"headings": []any{map[string]any{"line": 7, "level": 2, "text": "The Budget"}}, "notes": ""}, "is not the line's text"},
		"a level jump":                   {map[string]any{"headings": []any{map[string]any{"line": 3, "level": 5, "text": "Overview of the plan"}}, "notes": ""}, "skips more than one level"},
		"lines out of order": {map[string]any{"headings": []any{
			map[string]any{"line": 7, "level": 2, "text": "Budget"},
			map[string]any{"line": 3, "level": 2, "text": "Overview of the plan"}}, "notes": ""}, "increasing order"},
		"not JSON": {"Here are the headings: Budget", "not a heading list"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := skyOneDoc(t)
			f.structure = func(string) any { return tc.reply }
			rep := f.sync(`{"mode":"apply","full":true}`)
			rej, _ := rep["structurer_rejected"].([]any)
			if len(rej) != 1 || !strings.Contains(fmt.Sprint(rej[0]), tc.want) {
				t.Errorf("structurer_rejected = %v, want one naming %q", rep["structurer_rejected"], tc.want)
			}
			if f.manifestRow("d1")["status"] != "imported" {
				t.Errorf("row = %v, want the document imported with the extract's outline", f.manifestRow("d1"))
			}
			if imp := f.callsTo("File", "import"); len(imp) != 1 || imp[0].Args["headings"] != nil {
				t.Errorf("imports = %v, want one without headings", imp)
			}
		})
	}
}

func TestSkybitsImport_TheStructurerSeesOnlyLinesAsData(t *testing.T) {
	f := skyOneDoc(t)
	f.sync(`{"mode":"apply","full":true}`)
	if len(f.structurePayloads) != 1 {
		t.Fatalf("structurer calls = %d, want 1", len(f.structurePayloads))
	}
	p := f.structurePayloads[0]
	for _, want := range []string{"CANDIDATE LINES", "L3: Overview of the plan", "L7: Budget", "TEXT SAMPLE:"} {
		if !strings.Contains(p, want) {
			t.Errorf("payload lacks %q:\n%s", want, p)
		}
	}
	for _, c := range f.callsTo("Agent", "") {
		if c.Args["name"] == "skybits/structure" && strings.Contains(fmt.Sprint(c.Args["prompt"]), "Budget") {
			t.Error("document text reached the trusted prompt")
		}
	}
}

func TestSkybitsImport_ARefusedOutlineFallsBackToTheExtract(t *testing.T) {
	f := skyOneDoc(t)
	f.importRefusesHeadings = true
	f.structure = func(string) any {
		return map[string]any{"headings": []any{map[string]any{"line": 7, "level": 2, "text": "Budget"}}, "notes": ""}
	}
	rep := f.sync(`{"mode":"apply","full":true}`)
	if f.manifestRow("d1")["status"] != "imported" || !strings.Contains(fmt.Sprint(rep["structurer_rejected"]), "refused the heading list") {
		t.Errorf("row %v report %v, want imported after the refusal", f.manifestRow("d1"), rep["structurer_rejected"])
	}
	if n := len(f.callsTo("File", "import")); n != 2 {
		t.Errorf("imports = %d, want the refused one and the fallback", n)
	}
}

func TestSkybitsImport_StructureNeverSkipsTheModel(t *testing.T) {
	f := skyOneDoc(t)
	f.sync(`{"mode":"apply","full":true,"structure":"never"}`)
	if len(f.structurePayloads) != 0 {
		t.Errorf("structure never still asked the model %d times", len(f.structurePayloads))
	}
}

// TestSkybitsImport_ImagesStubWithoutOCRThenImportOnAFullRun: no OCR model
// makes a SourceStub; once OCR is there, a full run imports the same document
// and drops the stub type and tag.
func TestSkybitsImport_ImagesStubWithoutOCRThenImportOnAFullRun(t *testing.T) {
	f := newSkyFake(t)
	png := skyFileItem("f-png", "Whiteboard", "board.png", "2026-01-05T00:00:00Z")
	png.Body = "Whiteboard notes: ship the beta on Friday"
	f.items = []*skyItem{png}
	f.ocr = "off"
	f.sync(`{"mode":"apply","full":true}`)
	row := f.manifestRow("f-png")
	if row["status"] != "stub:ocr_unavailable" || row["document_id"] == "" {
		t.Fatalf("row = %v, want an ocr_unavailable stub document", row)
	}
	id := fmt.Sprint(row["document_id"])
	f.ocr = "ok"
	f.sync(`{"mode":"apply"}`)
	if r := f.manifestRow("f-png"); r["status"] != "stub:ocr_unavailable" {
		t.Errorf("an incremental run retried the capability stub: %v", r)
	}
	f.sync(`{"mode":"apply","full":true}`)
	r := f.manifestRow("f-png")
	if r["status"] != "imported" || r["document_id"] != id {
		t.Fatalf("row = %v, want imported into the stub's document %s", r, id)
	}
	d := f.mustDoc(fmt.Sprintf(`{"op":"get_document","scope":"tenant","id":%q}`, id))
	root := f.mustDoc(fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, d["root_chunk_id"]))
	if root["type"] == "SourceStub" || strings.Contains(fmt.Sprint(root["fields"]), "skybits_stub_reason") || strings.Contains(fmt.Sprint(d["tags"]), "skybits/stub/") {
		t.Errorf("the former stub keeps its stub marks: type %v fields %v tags %v", root["type"], root["fields"], d["tags"])
	}
}

func TestSkybitsImport_RecordingsStubByReason(t *testing.T) {
	for mode, want := range map[string]string{"convert": "stub:stt_needs_conversion", "off": "stub:stt_unavailable"} {
		f := newSkyFake(t)
		f.items = []*skyItem{skyFileItem("f-mp4", "Call", "call.mp4", "2026-01-01T00:00:00Z")}
		f.stt = mode
		f.sync(`{"mode":"apply","full":true}`)
		if r := f.manifestRow("f-mp4"); r["status"] != want || r["document_id"] == "" {
			t.Errorf("stt %s: row = %v, want %s with a stub document", mode, r, want)
		}
	}
	f := newSkyFake(t)
	f.items = []*skyItem{skyFileItem("f-mp4", "Call", "call.mp4", "2026-01-01T00:00:00Z")}
	f.sync(`{"mode":"apply","full":true,"stt_minutes":1}`)
	if r := f.manifestRow("f-mp4"); r["status"] != "stub:stt_over_cap" {
		t.Errorf("row = %v, want stt_over_cap with 1 minute left and 3.5 estimated", r)
	}
}

// TestSkybitsImport_AStubNeverReplacesAnImportedDocument: a file that grew past
// the size cap keeps its earlier text.
func TestSkybitsImport_AStubNeverReplacesAnImportedDocument(t *testing.T) {
	f := newSkyFake(t)
	pdf := skyFileItem("f-pdf", "Contract", "contract.pdf", "2026-01-01T00:00:00Z")
	f.items = []*skyItem{pdf}
	f.sync(`{"mode":"apply","full":true}`)
	id := f.manifestRow("f-pdf")["document_id"]
	pdf.TooLarge, pdf.File.Version, pdf.UpdatedAt = true, 2, "2026-03-01T00:00:00Z"
	rep := f.sync(`{"mode":"apply"}`)
	r := f.manifestRow("f-pdf")
	if r["status"] != "stub:too_large" || r["document_id"] != id || rep["kept_previous_document"] == nil {
		t.Errorf("row %v report %v, want too_large keeping document %v", r, rep["kept_previous_document"], id)
	}
	md := f.mustDoc(fmt.Sprintf(`{"op":"export_md","scope":"tenant","id":%q}`, id))
	if !strings.Contains(fmt.Sprint(md["markdown"]), "Some text read from contract.pdf") {
		t.Error("the earlier text is gone")
	}
}

func TestSkybitsImport_AFailedExtractLeavesTheInboxClean(t *testing.T) {
	f := newSkyFake(t)
	f.items = []*skyItem{skyFileItem("f-pdf", "Contract", "contract.pdf", "2026-01-01T00:00:00Z")}
	f.extractFault = "extract: the file is damaged"
	f.sync(`{"mode":"apply","full":true}`)
	if r := f.manifestRow("f-pdf"); r["status"] != "failed:error" || r["fail_count"] != float64(1) {
		t.Errorf("row = %v, want failed:error once", r)
	}
	if len(f.files) != 0 || len(f.removed) == 0 {
		t.Errorf("inbox = %v after a failure (removed %v)", f.files, f.removed)
	}
}

func TestSkybitsImport_AZipExportIsRefused(t *testing.T) {
	f := skyOneDoc(t)
	f.zipExport = true
	f.sync(`{"mode":"apply","full":true}`)
	if r := f.manifestRow("d1"); r["status"] != "failed:error" || !strings.Contains(fmt.Sprint(r["reason"]), "zip") {
		t.Errorf("row = %v, want a zip refusal", r)
	}
	if n := len(f.callsTo("File", "download")); n != 0 {
		t.Errorf("a zip was downloaded %d times", n)
	}
}

// TestSkybitsImport_FiveFailuresArePersistentAndWaitForAFullRun.
func TestSkybitsImport_FiveFailuresArePersistentAndWaitForAFullRun(t *testing.T) {
	f := newSkyFake(t)
	f.items = []*skyItem{skyFileItem("f-pdf", "Contract", "contract.pdf", "2026-01-01T00:00:00Z")}
	f.extractFault = "extract: the file is damaged"
	f.sync(`{"mode":"apply","full":true}`)
	for i := 0; i < 4; i++ {
		f.sync(`{"mode":"apply"}`)
	}
	if r := f.manifestRow("f-pdf"); r["status"] != "failed:persistent" || r["fail_count"] != float64(5) {
		t.Fatalf("row = %v, want failed:persistent after 5", r)
	}
	f.calls = nil
	f.sync(`{"mode":"apply"}`)
	if n := len(f.callsTo("Agent", "")); n != 0 {
		t.Errorf("an incremental run retried a persistent failure")
	}
	f.extractFault = ""
	f.sync(`{"mode":"apply","full":true}`)
	if r := f.manifestRow("f-pdf"); r["status"] != "imported" {
		t.Errorf("a full run did not retry it: %v", r)
	}
}
