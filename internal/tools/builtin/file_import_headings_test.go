package builtin

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// regulationMD is a PDF export as MTB receives it: structure only in the
// numbering, a wrapped section title, and a page break inside a пункт.
const regulationMD = `ПОЛОЖЕННЯ
про організаційну структуру банку
1. Загальні положення
1. Це Положення розроблено відповідно до
вимог законодавства.
2. Положення затверджує Наглядова рада.

7

3. Організаційна структура банку та
порядок її затвердження
3. Банк забезпечує відповідність структури
його бізнес-моделі.
`

// headingsCall imports the regulation with its sections as headings (the
// second one wrapped over two lines) and its пункти as markers.
func headingsCall(ref string) map[string]any {
	return map[string]any{"op": "import", "path": ref,
		"source": map[string]any{"url": "https://tenant.sharepoint.com/sites/board/reg.pdf", "kind": "sharepoint"},
		"headings": []any{
			map[string]any{"line": 3, "level": 2, "as": "heading"},
			map[string]any{"line": 4, "level": 3, "as": "marker", "title": "1. Це Положення розроблено…"},
			map[string]any{"line": 6, "level": 3, "as": "marker", "title": "2. Положення затверджує…"},
			map[string]any{"line": 10, "level": 2, "as": "heading", "lines": 2},
			map[string]any{"line": 12, "level": 3, "as": "marker", "title": "3. Банк забезпечує…"},
		}}
}

// Headings the runtime applies to the recorded file keep the capture record:
// the document is structured AND says, with the runtime's authority, which
// captured file it came from, and that a run added the tree.
func TestFileImport_HeadingsKeepTheCaptureProvenance(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	kept := inbox.Capture(ctx, inbox.Item{Name: "reg.md", MediaType: "text/markdown", Origin: "mcp:ms365/download-bytes"}, []byte(regulationMD))
	out, res := fileOp(t, f, ctx, headingsCall(kept.Record.Ref))
	if res.IsError {
		t.Fatal(res.Text)
	}
	docID := asStr(out["document_id"])
	got, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+docID+`"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	p, _ := got["provenance"].(map[string]any)
	for k, want := range map[string]any{
		"sha256": kept.Record.SHA256, "captured_from": "mcp:ms365/download-bytes", "run_id": kept.Record.RunID,
		"file": "reg.md", "source_kind": "sharepoint",
	} {
		if p[k] != want {
			t.Errorf("provenance %s = %v, want %v", k, p[k], want)
		}
	}
	s, _ := p["structured"].(map[string]any)
	if s["headings"] != float64(5) || s["by"] != tools.RunID(ctx) {
		t.Errorf("provenance structured = %v, want 5 headings by %s", p["structured"], tools.RunID(ctx))
	}
	if tags, _ := got["tags"].([]any); !slices.Contains(tags, any("source/mcp/ms365")) {
		t.Errorf("tags = %v, want the captured file's source tag", got["tags"])
	}
	// root + 2 sections + 3 пункти
	if n := len(importChunkIDs(t, d, ctx, docID)); n != 6 {
		t.Errorf("chunks = %d, want 6", n)
	}
	md, r := docExec(t, d, ctx, `{"op":"export_md","scope":"user","id":"`+docID+`"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	text := asStr(md["markdown"])
	for _, want := range []string{
		"## 1. Загальні положення",
		"### 1. Це Положення розроблено…",
		"## 3. Організаційна структура банку та порядок її затвердження",
		"вимог законодавства.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("export lacks %q:\n%s", want, text)
		}
	}
}

// Bad headings import nothing: the refusal says so, and no document appears.
func TestFileImport_BadHeadingsImportNothing(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	kept := inbox.Capture(ctx, inbox.Item{Name: "reg.md", MediaType: "text/markdown", Origin: "http"}, []byte(regulationMD))
	for name, h := range map[string]map[string]any{
		"out of range":   {"line": 99, "level": 2, "as": "heading"},
		"level 1":        {"line": 3, "level": 1, "as": "heading"},
		"level 7":        {"line": 3, "level": 7, "as": "heading"},
		"blank line":     {"line": 7, "level": 2, "as": "heading"},
		"marker no name": {"line": 4, "level": 3, "as": "marker"},
		"long title":     {"line": 4, "level": 3, "as": "marker", "title": strings.Repeat("я", 301)},
		"title newline":  {"line": 4, "level": 3, "as": "marker", "title": "a\nb"},
		"unknown as":     {"line": 3, "level": 2, "as": "chapter"},
	} {
		_, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": kept.Record.Ref, "headings": []any{h}})
		if !res.IsError || !strings.Contains(res.Text, "Nothing was imported") {
			t.Errorf("%s: want a refusal saying nothing was imported, got %s", name, res.Text)
		}
	}
	unordered := []any{
		map[string]any{"line": 6, "level": 3, "as": "marker", "title": "b"},
		map[string]any{"line": 4, "level": 3, "as": "marker", "title": "a"},
	}
	if _, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": kept.Record.Ref, "headings": unordered}); !res.IsError {
		t.Errorf("unordered headings accepted: %s", res.Text)
	}
	overlap := []any{
		map[string]any{"line": 10, "level": 2, "as": "heading", "lines": 2},
		map[string]any{"line": 11, "level": 3, "as": "marker", "title": "x"},
	}
	if _, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": kept.Record.Ref, "headings": overlap}); !res.IsError {
		t.Errorf("a marker inside a heading's span accepted: %s", res.Text)
	}
	list, r := docExec(t, d, ctx, `{"op":"query_documents","scope":"user"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	if docs, _ := list["documents"].([]any); len(docs) != 0 {
		t.Errorf("a refused import left %d documents", len(docs))
	}
}

// An inbox file with no capture record is named as such, and gets no source
// tag: source/volume/inbox read like a real source.
func TestFileImport_UnrecordedInboxFileIsNamedNotTagged(t *testing.T) {
	f, ctx, d, _ := fileDocRun(t)
	kept := inbox.Capture(ctx, inbox.Item{Name: "n.md", MediaType: "text/markdown", Origin: "mcp:trusted/get"}, []byte("# Notes\n\ntext\n"))
	if err := os.WriteFile(inboxFile(t, tools.EphemeralVolumes(ctx), kept.Record.Ref), []byte("# Notes\n\nchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, res := fileOp(t, f, ctx, map[string]any{"op": "import", "path": kept.Record.Ref,
		"source": map[string]any{"url": "https://tenant.sharepoint.com/n.md", "kind": "sharepoint"}})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if out["provenance_note"] != importNoRecordNote {
		t.Errorf("provenance_note = %v", out["provenance_note"])
	}
	got, _ := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+asStr(out["document_id"])+`"}`)
	tags, _ := got["tags"].([]any)
	for _, tg := range tags {
		if s := asStr(tg); strings.HasPrefix(s, "source/volume") {
			t.Errorf("an unrecorded inbox file was tagged %s", s)
		}
	}
	if !slices.Contains(tags, any("source/sharepoint")) || !slices.Contains(tags, any("untrusted")) {
		t.Errorf("tags = %v, want the given source's tag and untrusted", tags)
	}
	p, _ := got["provenance"].(map[string]any)
	if p["source_url"] != "https://tenant.sharepoint.com/n.md" || p["sha256"] != nil {
		t.Errorf("provenance = %v, want the given source and no capture fields", p)
	}
}

// undoImportHeadings is the inverse the headings guarantee: drop the marker
// lines, strip the heading prefixes, and split each joined span at the
// points the caller recorded. It must give the file's text back unchanged.
func undoImportHeadings(t *testing.T, structured, original string, hs []ImportHeading) string {
	t.Helper()
	orig := strings.Split(original, "\n")
	out := strings.Split(structured, "\n")
	var back []string
	o := 0 // index into out
	for i := 0; i < len(orig); {
		var h *ImportHeading
		for k := range hs {
			if hs[k].Line == i+1 {
				h = &hs[k]
			}
		}
		switch {
		case h == nil:
			back = append(back, out[o])
			o, i = o+1, i+1
		case h.As == importHeadingAsMarker:
			back = append(back, out[o+1]) // skip the inserted line
			o, i = o+2, i+1
		default:
			body := strings.TrimPrefix(out[o], strings.Repeat("#", h.Level)+" ")
			span := max(h.Lines, 1)
			// Split the joined heading at the original lines' lengths.
			for j := range span {
				l := orig[i+j]
				if j < span-1 {
					l = strings.TrimSuffix(l, "\r")
				}
				if !strings.HasPrefix(body, l) {
					t.Fatalf("heading %q does not start with original line %q", body, l)
				}
				back = append(back, l)
				body = strings.TrimPrefix(strings.TrimPrefix(body, l), " ")
			}
			o, i = o+1, i+span
		}
	}
	return strings.Join(back, "\n")
}

func TestApplyImportHeadings_BodyIsTheFilesOwn(t *testing.T) {
	hs := []ImportHeading{
		{Line: 3, Level: 2, As: "heading"},
		{Line: 4, Level: 3, As: "marker", Title: "1. Це Положення розроблено…"},
		{Line: 6, Level: 3, As: "marker", Title: "  2. Положення затверджує…  "},
		{Line: 10, Level: 2, As: "heading", Lines: 2},
		{Line: 12, Level: 6, As: "marker", Title: "3. Банк забезпечує…"},
	}
	got, err := applyImportHeadings(regulationMD, hs)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"ПОЛОЖЕННЯ",
		"про організаційну структуру банку",
		"## 1. Загальні положення",
		"### 1. Це Положення розроблено…",
		"1. Це Положення розроблено відповідно до",
		"вимог законодавства.",
		"### 2. Положення затверджує…",
		"2. Положення затверджує Наглядова рада.",
		"",
		"7",
		"",
		"## 3. Організаційна структура банку та порядок її затвердження",
		"###### 3. Банк забезпечує…",
		"3. Банк забезпечує відповідність структури",
		"його бізнес-моделі.",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("structured text:\n%s\nwant:\n%s", got, want)
	}
	if back := undoImportHeadings(t, got, regulationMD, hs); back != regulationMD {
		t.Fatalf("undoing the headings gave:\n%q\nwant the file:\n%q", back, regulationMD)
	}
}

func TestApplyImportHeadings_CRLFSpanDropsOnlyTheJoinedBreaks(t *testing.T) {
	text := "Стаття 5. Права\r\nта обовʼязки\r\nтекст\r\n"
	got, err := applyImportHeadings(text, []ImportHeading{{Line: 1, Level: 2, As: "heading", Lines: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "## Стаття 5. Права та обовʼязки\r\nтекст\r\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestApplyImportHeadings_Refusals(t *testing.T) {
	for name, tc := range map[string]struct {
		hs   []ImportHeading
		want string
	}{
		"line 0":            {[]ImportHeading{{Line: 0, Level: 2, As: "heading"}}, "lines 1 to 13"},
		"past the end":      {[]ImportHeading{{Line: 14, Level: 2, As: "heading"}}, "lines 1 to 13"},
		"span past the end": {[]ImportHeading{{Line: 13, Level: 2, As: "heading", Lines: 2}}, "past the file's last line"},
		"span too long":     {[]ImportHeading{{Line: 1, Level: 2, As: "heading", Lines: 11}}, "lines 11 is not 1 to 10"},
		"blank heading":     {[]ImportHeading{{Line: 9, Level: 2, As: "heading"}}, "blank"},
		"blank in span":     {[]ImportHeading{{Line: 6, Level: 2, As: "heading", Lines: 2}}, "line 7 is blank"},
		"level 1":           {[]ImportHeading{{Line: 1, Level: 1, As: "heading"}}, "level 1"},
		"level 7":           {[]ImportHeading{{Line: 1, Level: 7, As: "heading"}}, "level 7"},
		"heading title":     {[]ImportHeading{{Line: 1, Level: 2, As: "heading", Title: "x"}}, "takes no title"},
		"marker lines":      {[]ImportHeading{{Line: 1, Level: 2, As: "marker", Title: "x", Lines: 2}}, "takes no lines"},
		"marker no title":   {[]ImportHeading{{Line: 1, Level: 2, As: "marker", Title: "   "}}, "needs a title"},
		"title tab":         {[]ImportHeading{{Line: 1, Level: 2, As: "marker", Title: "a\tb"}}, "one line"},
		"title separator":   {[]ImportHeading{{Line: 1, Level: 2, As: "marker", Title: "a b"}}, "one line"},
		"title 301":         {[]ImportHeading{{Line: 1, Level: 2, As: "marker", Title: strings.Repeat("я", 301)}}, "301 characters"},
		"unknown as":        {[]ImportHeading{{Line: 1, Level: 2, As: "chapter"}}, `"chapter"`},
		"same line twice":   {[]ImportHeading{{Line: 3, Level: 2, As: "marker", Title: "a"}, {Line: 3, Level: 2, As: "heading"}}, "not after the previous entry"},
		"inside a span":     {[]ImportHeading{{Line: 10, Level: 2, As: "heading", Lines: 2}, {Line: 11, Level: 3, As: "marker", Title: "x"}}, "ends at line 11"},
		"too many":          {make([]ImportHeading, importHeadingsMax+1), "at most 10000"},
	} {
		if _, err := applyImportHeadings(regulationMD, tc.hs); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
	if _, err := applyImportHeadings(regulationMD, []ImportHeading{{Line: 300, Level: 2, As: "marker", Title: strings.Repeat("я", 300)}}); err == nil {
		t.Error("an out-of-range line passed because its title was valid")
	}
	if _, err := applyImportHeadings(regulationMD, []ImportHeading{{Line: 4, Level: 2, As: "marker", Title: strings.Repeat("я", 300)}}); err != nil {
		t.Errorf("a 300-character title was refused: %v", err)
	}
}
