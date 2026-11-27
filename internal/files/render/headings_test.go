package render

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const headingLookStyle = `{"loomcycle_style": 1, "colors": {"heading": "#0f3d6e"},
 "headings": {"h1": {"rule": true, "uppercase": true}, "h2": {"weight": "semibold"}, "h3": {"weight": "regular"}}}`

func headingDoc(t *testing.T) *Doc {
	t.Helper()
	b := NewBuilder("Ann Lee")
	b.Doc.Blocks = b.Body("Intro.")
	b.AddSection()
	b.Doc.Sections = []*Section{{ID: "s1", Title: "Experience", Level: 2, Blocks: b.Body("Text."),
		Children: []*Section{{ID: "s2", Title: "Acme", Level: 3, Blocks: b.Body("More.")}}}}
	d, _ := b.Finish()
	st, err := ParseStyle(headingLookStyle)
	if err != nil {
		t.Fatal(err)
	}
	d.Style = st
	return d
}

func TestParseStyle_HeadingsTakeOnlyTheirKeys(t *testing.T) {
	for body, want := range map[string]string{
		`{"loomcycle_style":1,"headings":{"h5":{"rule":true}}}`:        "unknown key",
		`{"loomcycle_style":1,"headings":{"h1":{"underline":true}}}`:   "unknown key",
		`{"loomcycle_style":1,"headings":{"h2":{"weight":"heavy"}}}`:   "headings.h2.weight",
		`{"loomcycle_style":1,"headings":{"h1":{"rule":"yes"}}}`:       "not a bool",
		`{"loomcycle_style":1,"headings":{"h4":{"weight":"regular"}}}`: "",
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

// Each writer draws the rule, the capitals and the weight its own way, and
// Word, ODT and HTML keep the heading's text in its own case.
func TestHeadings_EveryFormatDrawsTheLook(t *testing.T) {
	d := headingDoc(t)

	w := docxParts(t, write(t, d, FormatDOCX, Options{}))
	styles := w["word/styles.xml"]
	h1 := styles[strings.Index(styles, `w:styleId="Heading1"`):]
	h1 = h1[:strings.Index(h1, "</w:style>")]
	for _, want := range []string{`<w:pBdr><w:bottom w:val="single" w:sz="6" w:space="1" w:color="0F3D6E"/></w:pBdr>`, "<w:b/><w:caps/>"} {
		if !strings.Contains(h1, want) {
			t.Errorf("Heading1 lacks %q: %s", want, h1)
		}
	}
	h3 := styles[strings.Index(styles, `w:styleId="Heading3"`):]
	if h3 = h3[:strings.Index(h3, "</w:style>")]; strings.Contains(h3, "<w:b/>") {
		t.Errorf("a regular Heading3 is bold: %s", h3)
	}
	if !strings.Contains(w["word/document.xml"], ">Experience<") || xmlWellFormed(styles) != nil {
		t.Error("the heading's text changed case, or styles.xml is not well-formed")
	}

	o := docxParts(t, write(t, d, FormatODT, Options{}))["styles.xml"]
	for _, want := range []string{`fo:border-bottom="0.5pt solid #0f3d6e"`, `fo:font-weight="bold" fo:text-transform="uppercase"`, `fo:font-weight="600"`, `fo:font-weight="normal"`} {
		if !strings.Contains(o, want) {
			t.Errorf("odt styles.xml lacks %q", want)
		}
	}

	h := string(write(t, d, FormatHTML, Options{}))
	for _, want := range []string{"h2{border-bottom:1px solid #0f3d6e;padding-bottom:.15em;text-transform:uppercase;}", "h3{font-weight:600;}", "h4{font-weight:400;}", "<h2>Experience</h2>"} {
		if !strings.Contains(h, want) {
			t.Errorf("html lacks %q", want)
		}
	}

	src := typSource(t, d, Options{})
	for _, want := range []string{`#show heading.where(level: 1): it => { block(width: 100%, stroke: (bottom: 0.5pt + rgb("#0f3d6e")), inset: (bottom: 3pt))[#upper(it)] }`,
		`#show heading.where(level: 2): it => { set text(weight: "semibold"); it }`} {
		if !strings.Contains(src, want) {
			t.Errorf("typst lacks %q", want)
		}
	}
}

// A style without headings writes the headings it did before (G3).
func TestHeadings_NoneLeavesTheBuiltInLook(t *testing.T) {
	d := headingDoc(t)
	d.Style.Headings = nil
	if strings.Contains(typSource(t, d, Options{}), "heading.where") {
		t.Error("typst has a heading rule with no headings set")
	}
	if strings.Contains(string(write(t, d, FormatHTML, Options{})), "h2{border") {
		t.Error("html has a heading rule with no headings set")
	}
	s := docxParts(t, write(t, d, FormatDOCX, Options{}))["word/styles.xml"]
	h1 := s[strings.Index(s, `w:styleId="Heading1"`):]
	if h1 = h1[:strings.Index(h1, "</w:style>")]; strings.Contains(h1, "<w:pBdr>") || strings.Contains(h1, "<w:caps/>") {
		t.Errorf("docx Heading1 has a look with no headings set: %s", h1)
	}
}

// The styled headings typeset. Skipped where typst is not installed.
func TestPDF_TypesetsHeadingLooks(t *testing.T) {
	typst, err := exec.LookPath("typst")
	if err != nil {
		if os.Getenv("LOOMCYCLE_REQUIRE_TYPST") != "" {
			t.Fatal("typst is required here and not on PATH")
		}
		t.Skip("typst not installed")
	}
	txt := pdfText(t, compileTypst(t, typst, headingDoc(t), Options{}))
	if !strings.Contains(txt, "EXPERIENCE") || !strings.Contains(txt, "Acme") {
		t.Errorf("the PDF's headings:\n%s", txt)
	}
}
