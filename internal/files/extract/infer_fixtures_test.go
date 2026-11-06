package extract

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// Fixtures for heading recovery (File op=extract structure.infer_headings):
// documents whose headings are direct formatting rather than heading
// styles, as an editor that exports to DOCX or HTML writes them. The text is
// invented.

// wrun is one w:r of a fixture paragraph.
type wrun struct {
	text  string
	bold  bool
	sz    int    // half-points; 0 inherits
	color string // hex; "" inherits
	style string // a character style (w:rStyle)
	tab   bool   // a w:tab before the text
}

func (r wrun) xml() string {
	var rpr strings.Builder
	if r.style != "" {
		rpr.WriteString(`<w:rStyle w:val="` + r.style + `"/>`)
	}
	if r.bold {
		rpr.WriteString(`<w:b/>`)
	}
	if r.color != "" {
		rpr.WriteString(`<w:color w:val="` + r.color + `"/>`)
	}
	if r.sz > 0 {
		rpr.WriteString(`<w:sz w:val="` + itoa(r.sz) + `"/>`)
	}
	out := `<w:r>`
	if rpr.Len() > 0 {
		out += `<w:rPr>` + rpr.String() + `</w:rPr>`
	}
	if r.tab {
		out += `<w:tab/>`
	}
	return out + `<w:t xml:space="preserve">` + r.text + `</w:t></w:r>`
}

// wP is a paragraph: ppr is the inner XML of its w:pPr.
func wP(ppr string, runs ...wrun) string {
	var b strings.Builder
	b.WriteString(`<w:p>`)
	if ppr != "" {
		b.WriteString(`<w:pPr>` + ppr + `</w:pPr>`)
	}
	for _, r := range runs {
		b.WriteString(r.xml())
	}
	b.WriteString(`</w:p>`)
	return b.String()
}

func plainRun(text string) wrun { return wrun{text: text} }

// wFiller is n plain body paragraphs, each a sentence.
func wFiller(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(wP("", plainRun(fillerSentence(i))))
	}
	return b.String()
}

func fillerSentence(i int) string {
	return "Paragraph " + itoa(i+1) + " describes the survey work in plain words, at body size, and ends with a full stop."
}

// wImageP is a paragraph holding only a picture.
func wImageP(rid, descr string) string {
	return `<w:p><w:r><w:drawing><wp:inline><wp:docPr id="1" name="Picture" descr="` + descr +
		`"/><a:graphic><a:graphicData><pic:pic><pic:blipFill><a:blip r:embed="` + rid +
		`"/></pic:blipFill></pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r></w:p>`
}

// inferStyles: an 11pt body (docDefaults), real heading styles that carry
// their own formatting, a paragraph style that is a heading only by its
// look (Accent: 14pt navy), and a bold character style (Strong).
const inferStyles = `<w:styles ` + nsW + `>` +
	`<w:docDefaults><w:rPrDefault><w:rPr><w:sz w:val="22"/></w:rPr></w:rPrDefault></w:docDefaults>` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:basedOn w:val="Normal"/><w:rPr><w:b/><w:sz w:val="40"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:basedOn w:val="Normal"/><w:rPr><w:b/><w:sz w:val="32"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Accent"><w:name w:val="Accent"/><w:basedOn w:val="Normal"/><w:rPr><w:color w:val="1F3864"/><w:sz w:val="28"/></w:rPr></w:style>` +
	`<w:style w:type="character" w:styleId="Strong"><w:name w:val="Strong"/><w:rPr><w:b/></w:rPr></w:style>` +
	`</w:styles>`

// solidPNG is a 2x2 PNG of one colour, so two pictures can differ.
func solidPNG(t testing.TB, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for x := range 2 {
		for y := range 2 {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// inferDOCXOf wraps body in a document with inferStyles, a bullet list
// numbering, and pictures: rId9 (deco.png), rId10 (photo.png), and rId11..13
// three parts holding deco.png's bytes again.
func inferDOCXOf(t testing.TB, body string) []byte {
	t.Helper()
	deco := solidPNG(t, color.RGBA{R: 31, G: 56, B: 100, A: 255})
	photo := solidPNG(t, color.RGBA{G: 200, A: 255})
	doc := `<w:document ` + nsW + ` ` + nsR + ` ` + nsA + ` ` + nsWP + ` ` + nsPic + `><w:body>` + body + `<w:sectPr/></w:body></w:document>`
	numbering := `<w:numbering ` + nsW + `><w:abstractNum w:abstractNumId="0"><w:lvl w:ilvl="0"><w:numFmt w:val="bullet"/></w:lvl></w:abstractNum>` +
		`<w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num></w:numbering>`
	return zipOf(t,
		xmlEntry("[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`),
		xmlEntry("_rels/.rels", relsXML([3]string{"rId1", "officeDocument", "word/document.xml"})),
		xmlEntry("word/document.xml", doc),
		xmlEntry("word/_rels/document.xml.rels", relsXML(
			[3]string{"rId1", "styles", "styles.xml"},
			[3]string{"rId2", "numbering", "numbering.xml"},
			[3]string{"rId9", "image", "media/deco.png"},
			[3]string{"rId10", "image", "media/photo.png"},
			[3]string{"rId11", "image", "media/deco-a.png"},
			[3]string{"rId12", "image", "media/deco-b.png"},
			[3]string{"rId13", "image", "media/deco-c.png"},
		)),
		xmlEntry("word/styles.xml", inferStyles),
		xmlEntry("word/numbering.xml", numbering),
		entry{name: "word/media/deco.png", data: deco},
		entry{name: "word/media/photo.png", data: photo},
		entry{name: "word/media/deco-a.png", data: deco},
		entry{name: "word/media/deco-b.png", data: deco},
		entry{name: "word/media/deco-c.png", data: deco},
	)
}

// gdocBody is a field report as an editor without heading styles exports
// it: the title is the one real heading (Heading 1), section titles are
// 14pt navy paragraphs (one by direct formatting, one by a look-only
// paragraph style), sub-sections are lone bold lines (one by direct
// formatting, one coloured through a bold character style), a 12pt coloured
// line, a tab and a run of non-breaking spaces, an empty paragraph, a
// divider picture repeated three times and a photo twice.
func gdocBody() string {
	return wP(`<w:pStyle w:val="Heading1"/>`, plainRun("Quarterly field report")) +
		wFiller(3) +
		wP("", wrun{text: "Section one: Overview", sz: 28, color: "1F3864"}) +
		wFiller(2) +
		wImageP("rId9", "divider") +
		wP("", wrun{text: "Background", bold: true}) +
		wP("", plainRun("Alpha"), wrun{text: "Beta   Gamma", tab: true}) +
		wP("", plainRun("   ")) +
		wImageP("rId10", "site photo") +
		wImageP("rId9", "divider") +
		wP(`<w:pStyle w:val="Accent"/>`, plainRun("Section two: Findings")) +
		wFiller(2) +
		wImageP("rId10", "site photo") +
		wP("", wrun{text: "Highlights in colour", sz: 24, color: "C00000"}) +
		wFiller(1) +
		wP("", wrun{text: "Key result", style: "Strong", color: "C00000"}) +
		wFiller(1) +
		wImageP("rId9", "divider")
}

// gdocHTML is gdocBody as a web page: the same structure through inline
// styles and <b>/<strong>, at the browser's 12pt (16px) body size.
func gdocHTML() string {
	var filler func(n int) string
	k := 0
	filler = func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteString("<p>" + fillerSentence(k) + "</p>\n")
			k++
		}
		return b.String()
	}
	return `<!doctype html><html><head><title>Field report</title></head><body>
<h1>Quarterly field report</h1>
` + filler(3) + `<p><span style="font-size:14pt;color:#1F3864">Section one: Overview</span></p>
` + filler(2) + `<p><img src="https://example.com/deco.png" alt="divider"></p>
<p><b>Background</b></p>
<p>Alpha	Beta&nbsp;&nbsp;&nbsp;Gamma</p>
<p>&nbsp;&nbsp;</p>
<p><img src="https://example.com/photo.png" alt="site photo"></p>
<p><img src="https://example.com/deco.png" alt="divider"></p>
<p style="font-size: 18.67px; color: rgb(31, 56, 100)">Section two: Findings</p>
` + filler(2) + `<p><img src="https://example.com/photo.png" alt="site photo"></p>
<p style="font-size:13pt;color:#c00000">Highlights in colour</p>
` + filler(1) + `<p><strong style="color:#C00000">Key result</strong></p>
` + filler(1) + `<p><img src="https://example.com/deco.png" alt="divider"></p>
</body></html>`
}

// Without the option, the formatted documents read exactly as they did
// before heading recovery existed: the golden files were written by the
// extractor as it was then.
func TestInferHeadings_OffIsTodaysOutput(t *testing.T) {
	s, _ := run(t, DOCX{}, inferDOCXOf(t, gdocBody()), Options{Name: "field-report.docx", Images: true})
	golden(t, "gdoc.docx.md", s.md.String())
	h, _ := run(t, HTML{}, []byte(gdocHTML()), Options{Name: "field-report.html", Images: true})
	golden(t, "gdoc.html.md", h.md.String())
}
