package render

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// slottedStyle is a style whose header and footer use slots and every
// placeholder, with markup of each format in its text.
const slottedStyle = `{"loomcycle_style": 1,
 "header": {"left": "{title}", "right": "{date}"},
 "footer": {"left": "Ann <b>Lee</b> #set ]\"x\" {{a}}", "right": "Page {page} of {pages}"}}`

func slottedDoc(t *testing.T, paragraphs int) *Doc {
	t.Helper()
	b := NewBuilder("CV & résumé")
	var md strings.Builder
	for i := 0; i < paragraphs; i++ {
		md.WriteString("A paragraph of the body text, long enough to fill a line of the page and wrap onto the next one as text does.\n\n")
	}
	b.Doc.Blocks = b.Body(md.String())
	d, _ := b.Finish()
	st, err := ParseStyle(slottedStyle)
	if err != nil {
		t.Fatal(err)
	}
	st.Date = "2026-11-27"
	d.Style = st
	return d
}

func TestPageLine_ReadsALineOrThreeSlots(t *testing.T) {
	var l PageLine
	if err := json.Unmarshal([]byte(`"one line"`), &l); err != nil || l != (PageLine{Left: "one line"}) {
		t.Errorf("a string = %+v, %v", l, err)
	}
	if err := json.Unmarshal([]byte(`{"left":"a","center":"b","right":"c"}`), &l); err != nil || l != (PageLine{"a", "b", "c"}) {
		t.Errorf("slots = %+v, %v", l, err)
	}
	if err := json.Unmarshal([]byte(`{"middle":"b"}`), &l); !errors.Is(err, errPageLine) {
		t.Errorf("an unknown slot: %v", err)
	}
	// A left-only line encodes as the string it was, so a style without
	// slots hashes as it did before (the render log's reuse).
	b, _ := json.Marshal(Style{Version: 1, Header: PageLine{Left: "x"}})
	if string(b) != `{"loomcycle_style":1,"colors":{},"page":{},"header":"x"}` {
		t.Errorf("a plain style encodes as %s", b)
	}
}

func TestParseStyle_RefusesAnUnknownPlaceholderAndNamesIt(t *testing.T) {
	for body, want := range map[string]string{
		`{"loomcycle_style":1,"footer":"Page {n}"}`:               "unknown placeholder {n}",
		`{"loomcycle_style":1,"header":{"right":"{page"}}`:        "not closed",
		`{"loomcycle_style":1,"header":{"top":"x"}}`:              "{left, center, right}",
		`{"loomcycle_style":1,"date":"2026-01-01"}`:               "date are not keys",
		`{"loomcycle_style":1,"header":{"left":"a\nb"}}`:          "header must be",
		`{"loomcycle_style":1,"footer":"{{literal}} {page}"}`:     "",
		`{"loomcycle_style":1,"footer":"}{{ {pages} {title} }}"}`: "",
	} {
		_, err := ParseStyle(body)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", body, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: %v, want %q", body, err, want)
		}
	}
}

func TestParsePageText_TextAndCounters(t *testing.T) {
	segs, err := parsePageText("{title} · {{x}} {page}/{pages} {date}", "T\nitle", "D")
	if err != nil {
		t.Fatal(err)
	}
	want := []pageSeg{{Text: "T itle · {x} "}, {Field: fieldPage}, {Text: "/"}, {Field: fieldPages}, {Text: " D"}}
	if len(segs) != len(want) {
		t.Fatalf("segs = %+v", segs)
	}
	for i := range want {
		if segs[i] != want[i] {
			t.Errorf("seg %d = %+v, want %+v", i, segs[i], want[i])
		}
	}
}

// Where no line shows {page}, page numbers go at the end of the footer's
// right slot, where they stood before slots.
func TestPageText_AutomaticNumberGoesRight(t *testing.T) {
	d := slottedDoc(t, 1)
	d.Style.Footer = PageLine{Left: "{title}"}
	_, f := d.pageText()
	if len(f[2]) != 1 || f[2][0].Field != fieldPage {
		t.Errorf("footer right = %+v", f[2])
	}
	off := false
	d.Style.PageNumbers = &off
	if _, f := d.pageText(); len(f[2]) != 0 {
		t.Errorf("page_numbers false still numbers: %+v", f[2])
	}
}

// Slots and placeholders reach every paged format as text plus the
// format's own counters, and no caller text becomes markup, a field or
// Typst code.
func TestPageText_EveryFormatWritesSlotsAndCounters(t *testing.T) {
	d := slottedDoc(t, 3)

	w := docxParts(t, write(t, d, FormatDOCX, Options{}))
	ftr := w["word/footer1.xml"]
	for _, want := range []string{`<w:fldSimple w:instr=" PAGE ">`, `<w:fldSimple w:instr=" NUMPAGES ">`, `Ann &lt;b&gt;Lee&lt;/b&gt; #set ]&#34;x&#34; {a}`, `w:val="center"`, `w:val="right"`} {
		if !strings.Contains(ftr, want) {
			t.Errorf("footer1.xml lacks %q:\n%s", want, ftr)
		}
	}
	if !strings.Contains(w["word/header1.xml"], "CV &amp; résumé") || !strings.Contains(w["word/header1.xml"], "2026-11-27") {
		t.Errorf("header1.xml = %s", w["word/header1.xml"])
	}
	for name, body := range w {
		n := strings.Count(body, "<w:fldSimple")
		if name == "word/footer1.xml" && n != 2 || name != "word/footer1.xml" && n != 0 {
			t.Errorf("%s holds %d fields", name, n)
		}
		if strings.HasSuffix(name, ".xml") && xmlWellFormed(body) != nil {
			t.Errorf("%s is not well-formed", name)
		}
	}

	o := docxParts(t, write(t, d, FormatODT, Options{}))["styles.xml"]
	for _, want := range []string{`<text:page-number text:select-page="current">1</text:page-number>`, `<text:page-count>1</text:page-count>`, `style:name="Header_Footer_Slots"`, `<text:tab/>`} {
		if !strings.Contains(o, want) {
			t.Errorf("odt styles.xml lacks %q:\n%s", want, o)
		}
	}
	if xmlWellFormed(o) != nil {
		t.Error("odt styles.xml is not well-formed")
	}

	src := typSource(t, d, Options{})
	for _, want := range []string{"#grid(columns: (1fr, auto, 1fr)", "#context counter(page).display()", "#context counter(page).final().first()"} {
		if !strings.Contains(src, want) {
			t.Errorf("typst lacks %q", want)
		}
	}
	if code := outsideStrings(t, src); strings.Contains(code, "Lee") || strings.Contains(code, "#set ]") {
		t.Errorf("page text reached the Typst source outside a string:\n%s", code)
	}

	// HTML has no pages: "Page {page} of {pages}" goes, the rest stays.
	h := string(write(t, d, FormatHTML, Options{}))
	if !strings.Contains(h, `<p class="style-footer">Ann &lt;b&gt;Lee&lt;/b&gt; #set ]&#34;x&#34; {a}</p>`) {
		t.Errorf("html footer:\n%s", h)
	}
	if !strings.Contains(h, `<p class="style-header style-slots"><span>CV &amp; résumé</span><span></span><span>2026-11-27</span></p>`) {
		t.Errorf("html header:\n%s", h)
	}
	assertInertHTML(t, h)
}

func TestWebPageText_DropsTheCounterWithItsLeadIn(t *testing.T) {
	d := slottedDoc(t, 1)
	for line, want := range map[string]string{
		"Ann Lee · Page {page} of {pages}": "Ann Lee",
		"Page {page}":                      "",
		"Ann Lee | {page}":                 "Ann Lee",
		"Ann Lee":                          "Ann Lee",
		"A, B · C {page}":                  "A, B",
	} {
		if got := d.webPageText(PageLine{Left: line})[0]; got != want {
			t.Errorf("%q → %q, want %q", line, got, want)
		}
	}
}

// A 3-page PDF reads "Page 3 of 3" on its last page. Skipped where typst
// is not installed; CI's render-office job requires it.
func TestPDF_PageOfPages(t *testing.T) {
	typst, err := exec.LookPath("typst")
	if err != nil {
		if os.Getenv("LOOMCYCLE_REQUIRE_TYPST") != "" {
			t.Fatal("typst is required here and not on PATH")
		}
		t.Skip("typst not installed")
	}
	d := slottedDoc(t, 60)
	txt := pdfText(t, compileTypst(t, typst, d, Options{}))
	pages := strings.Count(txt, "Page 1 of ")
	if pages != 1 {
		t.Fatalf("want one first page, got text:\n%s", txt)
	}
	i := strings.Index(txt, "Page 1 of ")
	total := strings.Fields(txt[i+len("Page 1 of "):])[0]
	if total == "1" || !strings.Contains(txt, "Page "+total+" of "+total) {
		t.Errorf("total pages %q, text:\n%s", total, txt)
	}
	for _, want := range []string{`Ann \<b>Lee\</b> #set ]"x" {a}`, "CV & résumé", "2026-11-27"} {
		if !strings.Contains(txt, want) {
			t.Errorf("the PDF lacks %q", want)
		}
	}
}
