package render

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const fullStyle = `{
  "loomcycle_style": 1,
  "font": "serif",
  "size": 12,
  "heading_scale": 1.2,
  "colors": {"text": "#112233", "heading": "#445566", "link": "#778899"},
  "page": {"size": "letter", "margin_mm": 20},
  "header": "Acme <b>Internal</b> #set \"x\"",
  "footer": "Confidential ]#panic(\"x\")[",
  "page_numbers": true,
  "logo_width_mm": 40
}`

func TestParseStyle_TakesTheShapeAndAFencedBlock(t *testing.T) {
	s, err := ParseStyle(fullStyle)
	if err != nil {
		t.Fatal(err)
	}
	if !(s.Font == "serif") || s.BaseSize() != 12 || s.headScale() != 1.2 || s.linkColor() != "#778899" || s.marginMM() != 20 ||
		s.PageSize("") != "letter" || s.PageSize("a4") != "a4" || !s.pageNumbers() || s.logoMM() != 40 {
		t.Errorf("parsed style = %+v", s)
	}
	if _, err := ParseStyle("```json\n{\"loomcycle_style\": 1, \"font\": \"sans\"}\n```"); err != nil {
		t.Errorf("a fenced block: %v", err)
	}
	if s, err := ParseStyle(`{"loomcycle_style": 1}`); err != nil || s.BaseSize() != 10.5 || (s.Font == "serif") || s.pageNumbers() != true {
		t.Errorf("an empty style is the built-in look: %+v %v", s, err)
	}
}

// A style that does not parse, or holds a value out of range, is refused,
// and the refusal names the field.
func TestParseStyle_RefusesAndNamesTheField(t *testing.T) {
	for want, body := range map[string]string{
		"loomcycle_style": `{"font": "sans"}`,
		"unknown key":     `{"loomcycle_style": 1, "colour": {"text": "#000000"}}`,
		"font":            `{"loomcycle_style": 1, "font": "comic"}`,
		"size":            `{"loomcycle_style": 1, "size": 30}`,
		"heading_scale":   `{"loomcycle_style": 1, "heading_scale": 3}`,
		"colors.link":     `{"loomcycle_style": 1, "colors": {"link": "blue"}}`,
		"page.size":       `{"loomcycle_style": 1, "page": {"size": "a3"}}`,
		"page.margin_mm":  `{"loomcycle_style": 1, "page": {"margin_mm": 2}}`,
		"header":          `{"loomcycle_style": 1, "header": "two\nlines"}`,
		"logo_width_mm":   `{"loomcycle_style": 1, "logo_width_mm": 500}`,
		"logo and source": `{"loomcycle_style": 1, "logo": "img-1.png"}`,
		"not JSON":        `{"loomcycle_style": 1,`,
		"one JSON value":  `{"loomcycle_style": 1} {}`,
	} {
		_, err := ParseStyle(body)
		if !errors.Is(err, ErrBadStyle) || !strings.Contains(err.Error(), strings.Split(want, " ")[0]) {
			t.Errorf("%s: %v", want, err)
		}
	}
}

// styledDoc is the fixture in a full style, its logo a picture of the Doc.
func styledDoc(t *testing.T) *Doc {
	t.Helper()
	b := NewBuilder("Протокол засідання")
	b.Doc.Anchor = "chunk-root"
	logo := b.Image("image/png", tinyPNG, "logo")
	b.Doc.Blocks = b.Body("Intro.")
	b.AddSection()
	b.Doc.Sections = []*Section{{ID: "s1", Anchor: "chunk-s1", Title: "Порядок денний", Level: 2, Blocks: b.Body("Text with a [link](https://example.com).")}}
	d, _ := b.Finish()
	st, err := ParseStyle(fullStyle)
	if err != nil {
		t.Fatal(err)
	}
	st.Logo, st.Source = logo.Asset, "doc1@3"
	d.Style = st
	return d
}

// Every format but Markdown applies the style, and its header and footer
// lines stay text: no markup, field or Typst code comes from them.
func TestStyle_EveryFormatAppliesIt(t *testing.T) {
	d := styledDoc(t)

	h := string(write(t, d, FormatHTML, Options{}))
	for _, want := range []string{`"Noto Serif"`, "font-size:12pt", "color:#112233", "h1,h2,h3,h4,h5,h6{color:#445566}", "a{color:#778899}",
		`<p class="style-header">Acme &lt;b&gt;Internal&lt;/b&gt; #set &#34;x&#34;</p>`, `<p class="style-footer">Confidential`, `<img class="logo"`, "img.logo{width:40mm"} {
		if !strings.Contains(h, want) {
			t.Errorf("html lacks %q", want)
		}
	}
	assertInertHTML(t, h)

	book := docxParts(t, write(t, d, FormatEPUB, Options{}))
	if css := book["OEBPS/style.css"]; !strings.Contains(css, `"Noto Serif"`) || !strings.Contains(css, "a{color:#778899}") {
		t.Errorf("epub css lacks the style:\n%s", css)
	}
	if p := book["OEBPS/text/s001.xhtml"]; !strings.Contains(p, `<p class="style-header">Acme &lt;b&gt;Internal&lt;/b&gt;`) || !strings.Contains(p, `class="style-footer"`) {
		t.Errorf("an epub page lacks the header or footer:\n%s", p)
	}
	if p := book["OEBPS/text/title.xhtml"]; !strings.Contains(p, `<img class="logo" src="../images/`) {
		t.Errorf("the epub title page has no logo:\n%s", p)
	}

	w := docxParts(t, write(t, d, FormatDOCX, Options{}))
	if s := w["word/styles.xml"]; !strings.Contains(s, `w:ascii="Noto Serif"`) || !strings.Contains(s, `<w:color w:val="112233"/><w:sz w:val="24"/>`) ||
		!strings.Contains(s, `<w:color w:val="445566"/>`) || !strings.Contains(s, `<w:color w:val="778899"/>`) {
		t.Errorf("styles.xml lacks the style:\n%s", s)
	}
	if !strings.Contains(w["word/document.xml"], `<w:headerReference w:type="default" r:id="rHdr1"/>`) || !strings.Contains(w["word/document.xml"], `w:left="1133"`) {
		t.Error("the section has no header reference or the style's margin")
	}
	if !strings.Contains(w["word/header1.xml"], "Acme &lt;b&gt;Internal&lt;/b&gt;") {
		t.Errorf("header1.xml = %s", w["word/header1.xml"])
	}
	for name, body := range w {
		n := strings.Count(body, "<w:fldSimple")
		switch {
		case name == "word/footer1.xml" && (n != 1 || !strings.Contains(body, `<w:fldSimple w:instr=" PAGE ">`)):
			t.Errorf("the footer's fields = %d, want the one PAGE field:\n%s", n, body)
		case name != "word/footer1.xml" && n != 0:
			t.Errorf("%s holds a field", name)
		}
		if (strings.HasSuffix(name, ".xml") || strings.HasSuffix(name, ".rels")) && xmlWellFormed(body) != nil {
			t.Errorf("%s is not well-formed", name)
		}
	}

	o := docxParts(t, write(t, d, FormatODT, Options{}))
	if s := o["styles.xml"]; !strings.Contains(s, `style:font-name="Noto Serif" fo:font-size="12pt" fo:color="#112233"`) ||
		!strings.Contains(s, `<style:header><text:p text:style-name="Header_Footer">Acme &lt;b&gt;Internal&lt;/b&gt;`) ||
		!strings.Contains(s, `<text:page-number text:select-page="current">`) || !strings.Contains(s, `fo:margin-left="2.000cm"`) {
		t.Errorf("odt styles.xml lacks the style:\n%s", s)
	}
	if xmlWellFormed(o["styles.xml"]) != nil || xmlWellFormed(o["content.xml"]) != nil {
		t.Error("an odt part is not well-formed")
	}

	src := typSource(t, d, Options{})
	for _, want := range []string{`margin: 20mm`, `"Noto Serif"`, `size: 12pt`, `fill: rgb("#112233")`, `counter(page).display()`, `paper: "us-letter"`, `#image("images/`} {
		if !strings.Contains(src, want) {
			t.Errorf("typst lacks %q:\n%s", want, src)
		}
	}
	if code := outsideStrings(t, src); strings.Contains(code, "Acme") || strings.Contains(code, "panic") {
		t.Errorf("a style line reached the Typst source outside a string:\n%s", code)
	}

	// Markdown is the same with a style or without.
	plain := *d
	plain.Style = nil
	if !bytes.Equal(write(t, d, FormatMarkdown, Options{}), write(t, &plain, FormatMarkdown, Options{})) {
		t.Error("the style changed the Markdown")
	}
}

// A worker takes a style from the bundle as the core wrote it, and refuses
// one the core never makes.
func TestStyle_BundleCarriesItAndRefusesAForgedOne(t *testing.T) {
	d := styledDoc(t)
	b, err := EncodeBundle(d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := DecodeBundle(bytes.NewReader(b), int64(len(b)))
	if err != nil || got.Style == nil || got.Style.Footer != d.Style.Footer || got.Style.Logo != d.Style.Logo {
		t.Fatalf("decoded style = %+v (%v)", got.Style, err)
	}
	for name, forge := range map[string]func(s *Style){
		"colour":  func(s *Style) { s.Colors.Text = `red;}body{x` },
		"header":  func(s *Style) { s.Header = PageLine{Left: "a\nb"} },
		"logo":    func(s *Style) { s.Logo = "../../etc/passwd" },
		"version": func(s *Style) { s.Version = 2 },
	} {
		d := styledDoc(t)
		forge(d.Style)
		b, _ := EncodeBundle(d, Options{})
		if _, _, err := DecodeBundle(bytes.NewReader(b), int64(len(b))); !errors.Is(err, ErrBadBundle) {
			t.Errorf("%s: %v, want ErrBadBundle", name, err)
		}
	}
}

// The styled PDF typesets, its header and footer on the page. Skipped where
// typst is not installed; CI's render-office job requires it.
func TestPDF_TypesetsAStyle(t *testing.T) {
	typst, err := exec.LookPath("typst")
	if err != nil {
		if os.Getenv("LOOMCYCLE_REQUIRE_TYPST") != "" {
			t.Fatal("typst is required here and not on PATH")
		}
		t.Skip("typst not installed")
	}
	txt := pdfText(t, compileTypst(t, typst, styledDoc(t), Options{}))
	// The extractor escapes markup characters in the text it reads back.
	for _, want := range []string{`Acme \<b>Internal\</b> #set "x"`, `Confidential ]#panic("x")[ 1`, "Порядок денний"} {
		if !strings.Contains(txt, want) {
			t.Errorf("the PDF lacks %q:\n%s", want, txt)
		}
	}
	_ = context.Background
}

// styledFixture is the full fixture (every block kind) in the full style,
// for the checks an office suite and epubcheck make.
func styledFixture(t *testing.T) *Doc {
	t.Helper()
	d := fixtureDoc(t)
	st, err := ParseStyle(fullStyle)
	if err != nil {
		t.Fatal(err)
	}
	st.Logo = d.Images[0].Name
	// A photo beside the title (RFC HB D4), so the office suite and
	// epubcheck open an anchored picture and a floated one.
	st.Photo = &StylePhoto{Place: "right", WidthMM: 30}
	d.Style = st
	if !d.AddPhoto("image/png", tinyPNG) {
		t.Fatal("the fixture took no photo")
	}
	return d
}
