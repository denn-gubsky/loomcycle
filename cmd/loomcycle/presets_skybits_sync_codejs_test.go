package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// These tests EXECUTE the shipped skybits/sync and skybits/import bodies
// through the real loop and code-js provider against the fake account in
// presets_skybits_fake_test.go, and assert on the store the real Document,
// Path and Memory tools wrote.

const skyDocBody = "Overview of the plan\n\nThe first paragraph explains the plan in a few sentences.\n\nBudget\n\nThe budget section says what it costs and why it is enough."

func skyDocItem(id, title, created string, tags ...string) *skyItem {
	return &skyItem{ID: id, Kind: "doc", Title: title, CreatedAt: created, UpdatedAt: created, Tags: tags, Rev: 4, Body: skyDocBody}
}

func skyFileItem(id, title, filename, created string, tags ...string) *skyItem {
	return &skyItem{ID: id, Kind: "file", Title: title, CreatedAt: created, UpdatedAt: created, Tags: tags,
		File: &skyFile{Filename: filename, Size: 2048, Version: 1}, Body: "Some text read from " + filename + " that is long enough to keep."}
}

// skyCorpus is one item of each kind and outcome.
func skyCorpus() []*skyItem {
	sheet := &skyItem{ID: "s1", Kind: "spreadsheet", Title: "Ledger", CreatedAt: "2026-01-02T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z",
		Tags: []string{"Finance"}, Rev: 3, Cells: 45000, Body: "## Data\n\n| a | b |\n|---|---|\n| 1 | 2 |"}
	png := skyFileItem("f-png", "Whiteboard", "board.png", "2026-01-05T00:00:00Z", "Projects/Alpha")
	png.Body = "Whiteboard notes: ship the beta on Friday"
	blank := skyFileItem("f-blank", "Blank photo", "blank.png", "2026-01-06T00:00:00Z")
	blank.Body = "x"
	icon := &skyItem{ID: "f-icon", Kind: "file", Title: "Icon", CreatedAt: "2026-01-07T00:00:00Z", UpdatedAt: "2026-01-07T00:00:00Z",
		File: &skyFile{Filename: "Icon\r", Size: 0, Version: 1}}
	return []*skyItem{
		skyDocItem("d1", "Плановий звіт", "2026-01-01T00:00:00Z", "Projects/Alpha", "Reports"),
		sheet,
		skyFileItem("f-pdf", "Contract", "contract.pdf", "2026-01-03T00:00:00Z", "Projects/Alpha"),
		skyFileItem("f-pptx", "Kickoff deck", "kickoff.pptx", "2026-01-04T00:00:00Z", "Projects/Alpha"),
		png, blank, icon,
		skyFileItem("f-pages", "Old notes", "notes.pages", "2026-01-08T00:00:00Z"),
		{ID: "c1", Kind: "canvas", Title: "Mood board", CreatedAt: "2026-01-09T00:00:00Z", UpdatedAt: "2026-01-09T00:00:00Z"},
		{ID: "t1", Kind: "doc", Title: "Template", CreatedAt: "2026-01-10T00:00:00Z", UpdatedAt: "2026-01-10T00:00:00Z", Template: true, Body: skyDocBody},
		skyFileItem("f-wav", "Standup", "standup.wav", "2026-01-11T00:00:00Z"),
	}
}

func TestSkybitsSync_PlanWritesNothing(t *testing.T) {
	f := newSkyFake(t)
	f.items = skyCorpus()
	rep := f.sync(`{"mode":"plan"}`)
	if rep["status"] != "planned" || rep["items"] != float64(len(f.items)) {
		t.Fatalf("report = %v", rep)
	}
	for _, c := range f.calls {
		if c.Tool == "Agent" || c.Tool == "File" {
			t.Errorf("plan made a %s call: %v", c.Tool, c.Args)
		}
	}
	// No manifest table was created: plan mode reads, it never writes.
	ctx := f.skyCtx("skybits/sync", "probe")
	res, _ := f.mem.Execute(ctx, json.RawMessage(`{"op":"sql_query","scope":"tenant","statement":"SELECT count(*) FROM skybits_manifest"}`))
	if !res.IsError {
		t.Errorf("plan created the manifest table: %s", res.Text)
	}
	if out, ok := f.doc(`{"op":"get_document","scope":"tenant","path":"/skybits/Projects/Alpha/Planovij-zvit"}`); ok {
		t.Errorf("plan imported a document: %v", out)
	}
}

func TestSkybitsSync_ApplyImportsEachKindAndStubsTheRest(t *testing.T) {
	f := newSkyFake(t)
	f.items = skyCorpus()
	f.pendingPolls = 3 // every export polls: the repeat-call guard must not stop it
	f.structure = func(payload string) any {
		// The doc's two bold-looking lines become sections.
		return map[string]any{"headings": []any{
			map[string]any{"line": 3, "level": 2, "text": "Overview of the plan"},
			map[string]any{"line": 7, "level": 2, "text": "Budget"},
		}, "notes": "two sections"}
	}
	rep := f.sync(`{"mode":"apply","full":true}`)
	if rep["status"] != "complete" {
		t.Fatalf("status = %v (%v)", rep["status"], rep["failure"])
	}
	want := map[string]string{
		"d1": "imported", "s1": "imported", "f-pdf": "imported", "f-pptx": "imported", "f-png": "imported",
		"f-blank": "stub:no_text", "f-icon": "skipped:icon_artefact", "f-pages": "stub:unsupported_type:pages",
		"c1": "stub:canvas_not_supported", "t1": "skipped:template", "f-wav": "imported",
	}
	for id, status := range want {
		row := f.manifestRow(id)
		if row == nil || row["status"] != status {
			t.Errorf("%s row = %v, want status %s", id, row, status)
		}
	}

	// The doc: at its first tag's folder, an extra name under its second tag,
	// provenance naming Skybits and the revision, the outline applied.
	d := f.mustDoc(`{"op":"get_document","scope":"tenant","path":"/skybits/Projects/Alpha/Planovij-zvit"}`)
	if row := f.manifestRow("d1"); row["document_id"] != d["document_id"] || row["revision"] != "doc:r4" {
		t.Errorf("d1 row %v does not name document %v at doc:r4", row, d["document_id"])
	}
	if d2 := f.mustDoc(`{"op":"get_document","scope":"tenant","path":"/skybits/Reports/Planovij-zvit"}`); d2["document_id"] != d["document_id"] {
		t.Errorf("the second tag's name points at %v, want %v", d2["document_id"], d["document_id"])
	}
	prov, _ := d["provenance"].(map[string]any)
	if prov["source_kind"] != "skybits" || prov["source_url"] != "https://skybits.ai/documents/d1" || prov["source_revision"] != "doc:r4" {
		t.Errorf("provenance = %v", prov)
	}
	md := f.mustDoc(fmt.Sprintf(`{"op":"export_md","scope":"tenant","id":%q}`, d["document_id"]))
	text := fmt.Sprint(md["markdown"])
	if !strings.Contains(text, "## Overview of the plan") || !strings.Contains(text, "## Budget") {
		t.Errorf("the structurer's outline was not applied:\n%s", text)
	}
	tags := fmt.Sprint(d["tags"])
	for _, tg := range []string{"source/skybits", "skybits/kind/doc", "skybits/tag/Projects/Alpha", "skybits/tag/Reports"} {
		if !strings.Contains(tags, tg) {
			t.Errorf("doc tags %s lack %s", tags, tg)
		}
	}

	// Extract options per kind.
	for _, c := range f.callsTo("File", "extract") {
		p := fmt.Sprint(c.Args["path"])
		switch {
		case strings.HasSuffix(p, ".png"):
			if c.Args["ocr"] != "always" {
				t.Errorf("image extract ocr = %v, want always", c.Args["ocr"])
			}
		default:
			if c.Args["ocr"] != "never" {
				t.Errorf("%s extract ocr = %v, want never (no page renderer)", p, c.Args["ocr"])
			}
		}
		if strings.HasSuffix(p, ".docx") && c.Args["structure"] == nil {
			t.Errorf("the doc's extract did not ask for infer_headings")
		}
		if strings.HasSuffix(p, ".xlsx") && c.Args["max_rows"] != float64(200) {
			t.Errorf("the large sheet's extract max_rows = %v, want 200", c.Args["max_rows"])
		}
	}

	// Stubs are SourceStub documents with the reason; the Icon has no document.
	stub := f.mustDoc(`{"op":"get_document","scope":"tenant","path":"/skybits/_untagged/Old-notes"}`)
	root := f.mustDoc(fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, stub["root_chunk_id"]))
	if root["type"] != "SourceStub" || fmt.Sprint(root["fields"]) == "" || !strings.Contains(fmt.Sprint(root["fields"]), "unsupported_type:pages") {
		t.Errorf("stub root = %v", root)
	}
	if row := f.manifestRow("f-icon"); row["document_id"] != "" {
		t.Errorf("the Icon artefact got a document: %v", row)
	}

	// Inbox hygiene: every file a child wrote was removed.
	if len(f.files) != 0 {
		t.Errorf("files left in the inbox: %v", f.files)
	}
	// The signed download URL is never kept.
	if strings.Contains(fmt.Sprint(prov), "secret-sig") {
		t.Errorf("provenance keeps the signed URL: %v", prov)
	}

	// The monthly report is a SyncReport document under the root.
	ls := f.mustDoc(`{"op":"query_documents","scope":"tenant","limit":100}`)
	found := false
	for _, x := range ls["documents"].([]any) {
		m := x.(map[string]any)
		if strings.HasPrefix(fmt.Sprint(m["title"]), "Skybits sync report ") {
			found = true
			r := f.mustDoc(fmt.Sprintf(`{"op":"get_document","scope":"tenant","id":%q}`, m["document_id"]))
			rc := f.mustDoc(fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, r["root_chunk_id"]))
			if rc["type"] != "SyncReport" {
				t.Errorf("report root type = %v", rc["type"])
			}
		}
	}
	if !found {
		t.Error("no report document")
	}
}

// skyImported is a fake whose corpus has been imported by one full apply.
func skyImported(t *testing.T) *skyFake {
	t.Helper()
	f := newSkyFake(t)
	f.items = skyCorpus()
	if rep := f.sync(`{"mode":"apply","full":true}`); rep["status"] != "complete" {
		t.Fatalf("first apply: %v", rep)
	}
	f.calls = nil
	return f
}

func (f *skyFake) docAt(p string) string {
	f.t.Helper()
	out, ok := f.doc(fmt.Sprintf(`{"op":"get_document","scope":"tenant","path":%q}`, p))
	if !ok {
		return ""
	}
	return fmt.Sprint(out["document_id"])
}

func (f *skyFake) skybitsCalls() int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c.Tool, "mcp__skybits__") && c.Tool != "mcp__skybits__find_documents" {
			n++
		}
	}
	return n
}

func TestSkybitsSync_SecondApplyChangesNothing(t *testing.T) {
	f := skyImported(t)
	before := f.manifestRow("d1")
	rep := f.sync(`{"mode":"apply"}`)
	if rep["full"] != false || rep["status"] != "complete" {
		t.Fatalf("second apply = %v, want an incremental, complete run", rep)
	}
	if n := f.skybitsCalls(); n != 0 || len(f.callsTo("Agent", "")) != 0 {
		t.Errorf("an unchanged account cost %d Skybits calls and %d spawns", n, len(f.callsTo("Agent", "")))
	}
	if after := f.manifestRow("d1"); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("the row moved: %v → %v", before, after)
	}
}

func TestSkybitsSync_AnEditIsReimportedIntoTheSameDocument(t *testing.T) {
	f := skyImported(t)
	id := f.docAt("/skybits/Projects/Alpha/Planovij-zvit")
	it := f.item("d1")
	it.UpdatedAt, it.Rev = "2026-02-01T00:00:00Z", 5
	it.Body = skyDocBody + "\n\nA new closing paragraph."
	rep := f.sync(`{"mode":"apply"}`)
	row := f.manifestRow("d1")
	if row["status"] != "reimported" || row["document_id"] != id || row["revision"] != "doc:r5" || row["updated_at"] != "2026-02-01T00:00:00Z" {
		t.Errorf("row = %v (report %v), want reimported into %s at doc:r5", row, rep["results"], id)
	}
	if got := f.docAt("/skybits/Projects/Alpha/Planovij-zvit"); got != id {
		t.Errorf("the name now points at %s, want %s", got, id)
	}
	md := f.mustDoc(fmt.Sprintf(`{"op":"export_md","scope":"tenant","id":%q}`, id))
	if !strings.Contains(fmt.Sprint(md["markdown"]), "A new closing paragraph.") {
		t.Errorf("the edit is not in the document:\n%v", md["markdown"])
	}
}

func TestSkybitsSync_UnchangedBytesOnlyMoveTheRow(t *testing.T) {
	f := skyImported(t)
	it := f.item("f-pdf")
	it.UpdatedAt = "2026-02-02T00:00:00Z" // touched, same bytes
	f.sync(`{"mode":"apply"}`)
	row := f.manifestRow("f-pdf")
	if row["status"] != "unchanged" || row["updated_at"] != "2026-02-02T00:00:00Z" {
		t.Errorf("row = %v, want unchanged with the new updated_at", row)
	}
	if n := len(f.callsTo("File", "import")); n != 0 {
		t.Errorf("unchanged bytes were imported %d times", n)
	}
}

func TestSkybitsSync_FullRunMarksTrashedAndRemovedThenRestores(t *testing.T) {
	f := skyImported(t)
	pdf, pptx := f.docAt("/skybits/Projects/Alpha/Contract"), f.docAt("/skybits/Projects/Alpha/Kickoff-deck")
	f.item("f-pdf").Trashed = true
	var kept []*skyItem
	for _, it := range f.items {
		if it.ID != "f-pptx" {
			kept = append(kept, it)
		}
	}
	f.items = kept
	// An incremental run notices nothing it may act on.
	f.sync(`{"mode":"apply"}`)
	if row := f.manifestRow("f-pdf"); row["source_status"] != "present" && row["source_status"] != "" {
		t.Errorf("an incremental run marked %v", row)
	}
	rep := f.sync(`{"mode":"apply","full":true}`)
	if r := f.manifestRow("f-pdf"); r["source_status"] != "trashed" {
		t.Errorf("trashed item row = %v (report %v)", r, rep)
	}
	if r := f.manifestRow("f-pptx"); r["source_status"] != "removed" {
		t.Errorf("removed item row = %v", r)
	}
	for id, want := range map[string]string{pdf: "trashed", pptx: "removed"} {
		d := f.mustDoc(fmt.Sprintf(`{"op":"get_document","scope":"tenant","id":%q}`, id))
		prov, _ := d["provenance"].(map[string]any)
		if prov["source_status"] != want || !strings.Contains(fmt.Sprint(d["tags"]), "source/removed") {
			t.Errorf("document %s: provenance %v tags %v, want %s and source/removed", id, prov, d["tags"], want)
		}
	}
	// Untrashed: present again, and no re-import of unchanged content.
	f.item("f-pdf").Trashed = false
	f.calls = nil
	f.sync(`{"mode":"apply","full":true}`)
	d := f.mustDoc(fmt.Sprintf(`{"op":"get_document","scope":"tenant","id":%q}`, pdf))
	if prov, _ := d["provenance"].(map[string]any); prov["source_status"] != "present" || strings.Contains(fmt.Sprint(d["tags"]), "source/removed") {
		t.Errorf("restored document: provenance %v tags %v", prov, d["tags"])
	}
	if n := len(f.callsTo("File", "import")); n != 0 {
		t.Errorf("restoring re-imported %d items", n)
	}
}

func TestSkybitsSync_MassRemovalMarksNothingAndFails(t *testing.T) {
	f := skyImported(t)
	f.items = f.items[:5] // six of eleven gone: past max(3, 30%)
	if err := f.syncErr(`{"mode":"apply","full":true}`); err == nil || !strings.Contains(err.Error(), "mass-removal guard") {
		t.Fatalf("err = %v, want the mass-removal guard", err)
	}
	for _, id := range []string{"f-blank", "f-pages", "c1", "f-wav"} {
		if r := f.manifestRow(id); r["source_status"] == "removed" {
			t.Errorf("%s was marked removed past the guard", id)
		}
	}
}

func TestSkybitsSync_ThreeTransientFailuresStopTheRun(t *testing.T) {
	f := newSkyFake(t)
	for i := 1; i <= 6; i++ {
		f.items = append(f.items, skyDocItem(fmt.Sprintf("d%d", i), fmt.Sprintf("Doc %d", i), fmt.Sprintf("2026-01-0%dT00:00:00Z", i)))
	}
	busy := "The agent document service is unavailable; try again shortly"
	f.faults["start_export"] = []string{busy, busy, busy}
	rep := f.sync(`{"mode":"apply","full":true}`)
	if rep["status"] != "stopped" || len(f.callsTo("Agent", "parallel_spawn")) != 1 {
		t.Fatalf("status %v after %d batches, want stopped after the first", rep["status"], len(f.callsTo("Agent", "parallel_spawn")))
	}
	for i := 1; i <= 3; i++ {
		if r := f.manifestRow(fmt.Sprintf("d%d", i)); r["status"] != "failed:transient" || r["fail_count"] != float64(1) {
			t.Errorf("d%d = %v, want failed:transient once", i, r)
		}
	}
	if r := f.manifestRow("d4"); r != nil {
		t.Errorf("d4 was dispatched after the breaker: %v", r)
	}
}

func TestSkybitsSync_ATransientFailureIsRetriedOnceAtTheEnd(t *testing.T) {
	f := newSkyFake(t)
	f.items = []*skyItem{skyDocItem("d1", "Doc 1", "2026-01-01T00:00:00Z")}
	f.faults["start_export"] = []string{"export failed: Export failed. Please try again."}
	rep := f.sync(`{"mode":"apply","full":true}`)
	if rep["requeued"] != float64(1) || f.manifestRow("d1")["status"] != "imported" {
		t.Errorf("report %v row %v, want one requeue and the item imported", rep, f.manifestRow("d1"))
	}
}

func TestSkybitsSync_AShortListingStopsBeforeAnything(t *testing.T) {
	f := newSkyFake(t)
	f.items = skyCorpus()
	f.shortBy = 1
	if err := f.syncErr(`{"mode":"apply","full":true}`); err == nil || !strings.Contains(err.Error(), "total_count") {
		t.Fatalf("err = %v, want a short-listing stop", err)
	}
	if len(f.callsTo("Agent", "")) != 0 {
		t.Error("a short listing dispatched imports")
	}
}

func TestSkybitsSync_AnEmptyTextBlockStillParses(t *testing.T) {
	f := newSkyFake(t)
	f.items = []*skyItem{skyDocItem("d1", "Doc 1", "2026-01-01T00:00:00Z")}
	f.emptyText = true
	if rep := f.sync(`{"mode":"plan"}`); rep["items"] != float64(1) {
		t.Errorf("report %v, want the one item listed", rep)
	}
}

func TestSkybitsSync_AHeldLeaseMakesTheRunBusy(t *testing.T) {
	f := newSkyFake(t)
	f.items = skyCorpus()
	ctx := f.skyCtx("skybits/sync", "run-other")
	if res, _ := f.docs.Execute(ctx, json.RawMessage(`{"op":"lease","name":"skybits-sync","ttl_ms":600000,"scope":"tenant"}`)); res.IsError {
		t.Fatal(res.Text)
	}
	rep := f.sync(`{"mode":"apply"}`)
	if rep["status"] != "busy" || len(f.callsTo("Agent", "")) != 0 || len(f.callsTo("mcp__skybits__find_documents", "")) != 0 {
		t.Errorf("report %v; a busy run must stop before listing or importing", rep)
	}
}

func TestSkybitsSync_MovedTagsMoveTheNamesNotTheDocument(t *testing.T) {
	f := skyImported(t)
	id := f.docAt("/skybits/Projects/Alpha/Planovij-zvit")
	f.item("d1").Tags = []string{"Reports", "Archive"}
	rep := f.sync(`{"mode":"apply"}`)
	if rep["retagged"] != float64(1) {
		t.Fatalf("report %v, want one retag", rep)
	}
	if f.docAt("/skybits/Reports/Planovij-zvit") != id || f.docAt("/skybits/Archive/Planovij-zvit") != id {
		t.Error("the new names do not point at the document")
	}
	if got := f.docAt("/skybits/Projects/Alpha/Planovij-zvit"); got != "" {
		t.Errorf("the old name still points at %s", got)
	}
	d := f.mustDoc(fmt.Sprintf(`{"op":"get_document","scope":"tenant","id":%q}`, id))
	tags := fmt.Sprint(d["tags"])
	if strings.Contains(tags, "skybits/tag/Projects/Alpha") || !strings.Contains(tags, "skybits/tag/Archive") {
		t.Errorf("tags = %s", tags)
	}
	if n := len(f.callsTo("File", "import")); n != 0 {
		t.Errorf("a tag move re-imported %d items", n)
	}
	if row := f.manifestRow("d1"); row["primary_path"] != "/skybits/Reports/Planovij-zvit" {
		t.Errorf("row = %v", row)
	}
}

func TestSkybitsSync_ANameHeldByAnotherDocumentIsNeverTaken(t *testing.T) {
	f := newSkyFake(t)
	f.items = []*skyItem{skyDocItem("d1", "Notes", "2026-01-01T00:00:00Z")}
	other := f.mustDoc(`{"op":"import_md","scope":"tenant","path":"/skybits/_untagged/Notes","markdown":"# Someone else's notes\n\nbody"}`)
	f.sync(`{"mode":"apply","full":true}`)
	if got := f.docAt("/skybits/_untagged/Notes"); got != other["document_id"] {
		t.Errorf("the other document lost its name to %s", got)
	}
	if row := f.manifestRow("d1"); row["primary_path"] != "/skybits/_untagged/Notes-doc" || row["status"] != "imported" {
		t.Errorf("row = %v, want the next free name", row)
	}
}
