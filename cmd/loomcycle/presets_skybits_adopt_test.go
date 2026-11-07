package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestSkybitsSync_AdoptsAnEarlierImportWithoutReimporting: a tenant already
// holds documents an earlier importer made, with its manifest (the first
// schema) and tag map. The bundle takes them over as they stand: no export,
// download or import touches an adopted item, and every document id, chunk id,
// Path name and the tag map stay byte-identical. Downstream statements and
// links hang off those ids.
func TestSkybitsSync_AdoptsAnEarlierImportWithoutReimporting(t *testing.T) {
	f := newSkyFake(t)
	const day = "2026-01-01T00:00:00Z"
	adopted := []*skyItem{
		{ID: "a-doc", Kind: "doc", Title: "Alpha plan", CreatedAt: "2026-01-01T00:00:01Z", UpdatedAt: day, Tags: []string{"A"}, Rev: 0, Body: skyDocBody},
		{ID: "a-sheet", Kind: "spreadsheet", Title: "Budget", CreatedAt: "2026-01-01T00:00:02Z", UpdatedAt: day, Tags: []string{"A"}, Rev: 2, Body: "| a |\n|---|\n| 1 |"},
		{ID: "a-pdf", Kind: "file", Title: "Spec", CreatedAt: "2026-01-01T00:00:03Z", UpdatedAt: day, File: &skyFile{Filename: "spec.pdf", Size: 900, Version: 1}, Body: "Spec text"},
	}
	png := skyFileItem("i-png", "Photo", "photo.png", "2026-01-01T00:00:04Z")
	png.Body = "A photo of a whiteboard with enough words on it"
	pptx := skyFileItem("i-pptx", "Deck", "deck.pptx", "2026-01-01T00:00:05Z")
	failed := &skyItem{ID: "i-fail", Kind: "spreadsheet", Title: "Stubborn", CreatedAt: "2026-01-01T00:00:06Z", UpdatedAt: day, Rev: 1, Body: "| x |\n|---|\n| 2 |"}
	icon := &skyItem{ID: "i-icon", Kind: "file", Title: "Icon", CreatedAt: "2026-01-01T00:00:07Z", UpdatedAt: day, File: &skyFile{Filename: "Icon\r", Version: 1}}
	mp4 := skyFileItem("i-mp4", "Long call", "call.mp4", "2026-01-01T00:00:08Z")
	mp4.TooLarge = true
	report := skyDocItem("n1", "Report", "2026-01-02T00:00:00Z", "A")
	for _, it := range []*skyItem{png, pptx, icon, mp4} {
		it.UpdatedAt = day
	}
	f.items = append(append([]*skyItem{}, adopted...), png, pptx, failed, icon, mp4, report)

	// The earlier importer's state: its tables exactly, its tag map, and its
	// documents at their stored names under the work key "skybits:<id>".
	f.sql("sql_exec", "CREATE TABLE skybits_manifest (item_id TEXT PRIMARY KEY, kind TEXT, title TEXT, url TEXT, tags_json TEXT, primary_path TEXT, paths_json TEXT, updated_at TEXT, revision TEXT, sha256 TEXT, document_id TEXT, status TEXT, reason TEXT, last_synced_at TEXT, last_run_id TEXT)")
	f.sql("sql_exec", "CREATE TABLE skybits_state (key TEXT PRIMARY KEY, value TEXT)")
	tagMap := `{"A":"/skybits/A"}`
	f.sql("sql_exec", "INSERT INTO skybits_state (key, value) VALUES ($1, $2)", "tag_map_json", tagMap)
	seedDoc := func(id, title, p string) string {
		d := f.mustDoc(fmt.Sprintf(`{"op":"import_md","scope":"tenant","path":%q,"reimport":true,"work_key":"skybits:%s","source_url":"https://skybits.ai/documents/%s","source_kind":"other","markdown":"# %s\n\n## Part\n\nbody of %s"}`, p, id, id, title, id))
		return fmt.Sprint(d["document_id"])
	}
	row := func(id, kind, title, tags, p, rev, sha, doc, status, reason string) {
		paths := "[]"
		if p != "" {
			paths = fmt.Sprintf(`[%q]`, p)
		}
		f.sql("sql_exec", "INSERT INTO skybits_manifest (item_id, kind, title, url, tags_json, primary_path, paths_json, updated_at, revision, sha256, document_id, status, reason, last_synced_at, last_run_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)",
			id, kind, title, "https://skybits.ai/documents/"+id, tags, p, paths, day, rev, sha, doc, status, reason, "2026-01-01T01:00:00Z", "")
	}
	docs := map[string]string{
		"a-doc":   seedDoc("a-doc", "Alpha plan", "/skybits/A/Alpha-plan"),
		"a-sheet": seedDoc("a-sheet", "Budget", "/skybits/A/Budget"),
		"a-pdf":   seedDoc("a-pdf", "Spec", "/skybits/_untagged/Spec"),
		"g1":      seedDoc("g1", "Report", "/skybits/A/Report"),
	}
	row("a-doc", "doc", "Alpha plan", `["A"]`, "/skybits/A/Alpha-plan", "doc:r0", "s1", docs["a-doc"], "imported", "")
	row("a-sheet", "spreadsheet", "Budget", `["A"]`, "/skybits/A/Budget", "sheet:r2", "s2", docs["a-sheet"], "reimported", "")
	row("a-pdf", "file", "Spec", `[]`, "/skybits/_untagged/Spec", "file:v1", "s3", docs["a-pdf"], "unchanged", "")
	row("g1", "doc", "Report", `["A"]`, "/skybits/A/Report", "doc:r1", "s4", docs["g1"], "imported", "")
	row("i-png", "file", "Photo", `[]`, "/skybits/_untagged/Photo", "file:v1", "", "", "stub:ocr_unavailable", "")
	row("i-pptx", "file", "Deck", `[]`, "/skybits/_untagged/Deck", "file:v1", "", "", "skipped:deferred_pptx", "")
	row("i-fail", "spreadsheet", "Stubborn", `[]`, "/skybits/_untagged/Stubborn", "", "", "", "failed", "The agent document service is unavailable; try again shortly")
	row("i-icon", "file", "Icon", `[]`, "/skybits/_untagged/Icon", "file:v1", "", "", "skipped:icon_artefact", "")
	row("i-mp4", "file", "Long call", `[]`, "/skybits/_untagged/Long-call", "file:v1", "", "", "stub:too_large", "")

	chunks := func() string {
		var all []string
		for _, id := range []string{"a-doc", "a-sheet", "a-pdf"} {
			q := f.mustDoc(fmt.Sprintf(`{"op":"query_chunks","scope":"tenant","document_id":%q,"limit":100}`, docs[id]))
			b, _ := json.Marshal(q["chunks"])
			all = append(all, string(b))
		}
		return strings.Join(all, "\n")
	}
	manifestBefore := f.sql("sql_query", "SELECT * FROM skybits_manifest ORDER BY item_id")
	chunksBefore := chunks()

	// Plan: reads only.
	rep := f.sync(`{"mode":"plan"}`)
	for _, x := range rep["todo"].([]any) {
		if id := x.(map[string]any)["id"]; id == "a-doc" || id == "a-sheet" || id == "a-pdf" {
			t.Errorf("plan would touch adopted item %v", id)
		}
	}
	if after := f.sql("sql_query", "SELECT * FROM skybits_manifest ORDER BY item_id"); fmt.Sprint(after) != fmt.Sprint(manifestBefore) {
		t.Error("plan changed the manifest")
	}
	if len(f.callsTo("Agent", "")) != 0 || len(f.callsTo("File", "")) != 0 {
		t.Error("plan dispatched work")
	}

	// Apply.
	f.calls = nil
	rep = f.sync(`{"mode":"apply"}`)
	if rep["status"] != "complete" || rep["full"] != true {
		t.Fatalf("apply = %v, want a complete run, full because no full run is on record", rep)
	}
	for _, c := range f.calls {
		for _, k := range []string{"doc_url", "doc_urls"} {
			if v, ok := c.Args[k]; ok && (v == "a-doc" || v == "a-sheet" || v == "a-pdf") {
				t.Errorf("%s was called for adopted item %v", c.Tool, v)
			}
		}
		if c.Tool == "File" && strings.Contains(fmt.Sprint(c.Args["work_key"]), "skybits:a-") {
			t.Errorf("File %v touched an adopted item", c.Args)
		}
	}
	for id, p := range map[string]string{"a-doc": "/skybits/A/Alpha-plan", "a-sheet": "/skybits/A/Budget", "a-pdf": "/skybits/_untagged/Spec"} {
		if got := f.docAt(p); got != docs[id] {
			t.Errorf("%s now names %s, want %s", p, got, docs[id])
		}
		if r := f.manifestRow(id); r["status"] == "" || r["document_id"] != docs[id] {
			t.Errorf("%s row = %v", id, r)
		}
	}
	if got := chunks(); got != chunksBefore {
		t.Error("an adopted document's chunks changed")
	}
	st := f.sql("sql_query", "SELECT key, value FROM skybits_state ORDER BY key")
	state := map[string]string{}
	for _, r := range st["rows"].([]any) {
		kv := r.([]any)
		state[fmt.Sprint(kv[0])] = fmt.Sprint(kv[1])
	}
	if state["tag_map_json"] != tagMap || state["schema_version"] != "2" || state["last_full_at"] == "" {
		t.Errorf("state = %v", state)
	}
	// The removed item keeps its name; the new item with the same title gets
	// the next one.
	if r := f.manifestRow("n1"); r["primary_path"] != "/skybits/A/Report-doc" || r["status"] != "imported" {
		t.Errorf("new Report row = %v, want /skybits/A/Report-doc", r)
	}
	if f.docAt("/skybits/A/Report") != docs["g1"] {
		t.Error("the removed item's name was taken")
	}
	if r := f.manifestRow("g1"); r["source_status"] != "removed" {
		t.Errorf("g1 = %v, want marked removed", r)
	}
	for id, want := range map[string]string{"i-png": "imported", "i-pptx": "imported", "i-fail": "imported", "i-icon": "skipped:icon_artefact", "i-mp4": "stub:too_large"} {
		if r := f.manifestRow(id); r["status"] != want {
			t.Errorf("%s = %v, want %s", id, r, want)
		}
	}
	if r := f.manifestRow("i-mp4"); r["document_id"] == "" {
		t.Error("the too_large row got no SourceStub document")
	}
}

// TestSkybitsNames_GoldenTable pins the naming of invented titles and tags:
// transliteration, cleanup, the 64-character cut, kind suffixes and counters.
func TestSkybitsNames_GoldenTable(t *testing.T) {
	f := newSkyFake(t)
	long := strings.Repeat("Long title ", 10)
	f.items = []*skyItem{
		skyDocItem("n1", "Звіт & план [чернетка]", "2026-01-01T00:00:01Z", "Проєкти/Альфа "),
		skyDocItem("n2", "Notes", "2026-01-01T00:00:02Z"),
		{ID: "n3", Kind: "spreadsheet", Title: "Notes", CreatedAt: "2026-01-01T00:00:03Z", UpdatedAt: "2026-01-01T00:00:03Z"},
		skyDocItem("n4", "Notes", "2026-01-01T00:00:04Z"),
		skyDocItem("n5", long, "2026-01-01T00:00:05Z"),
		skyDocItem("n6", long, "2026-01-01T00:00:06Z"),
		skyDocItem("n7", "Shared plan", "2026-01-01T00:00:07Z", "owner@example.test//Team/Q1"),
		skyDocItem("n8", "Café “déjà vu”: 2/3", "2026-01-01T00:00:08Z"),
	}
	rep := f.sync(`{"mode":"plan"}`)
	got := map[string]string{}
	for _, x := range rep["todo"].([]any) {
		m := x.(map[string]any)
		got[fmt.Sprint(m["id"])] = fmt.Sprint(m["path"])
	}
	cut := strings.TrimRight(strings.Repeat("Long-title-", 10)[:64], "-")
	want := map[string]string{
		"n1": "/skybits/Proyekti/Alfa/Zvit-and-plan-chernetka",
		"n2": "/skybits/_untagged/Notes",
		"n3": "/skybits/_untagged/Notes-sheet",
		"n4": "/skybits/_untagged/Notes-doc",
		"n5": "/skybits/_untagged/" + cut,
		"n6": "/skybits/_untagged/" + strings.TrimRight(cut[:60], "-") + "-doc",
		"n7": "/skybits/_shared/owner-example.test/Team/Q1/Shared-plan",
		"n8": "/skybits/_untagged/Cafe-deja-vu-2-3",
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s → %q, want %q", id, got[id], w)
		}
	}
}
