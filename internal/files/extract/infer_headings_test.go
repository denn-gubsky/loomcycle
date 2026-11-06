package extract

import (
	"strings"
	"testing"
)

// Heading recovery (RFC GB §6): with Options.InferHeadings, a DOCX or HTML
// paragraph that looks like a heading (a short line, bold, larger or
// coloured) becomes one. Each case runs on both formats: DOCX first, HTML
// for parity.

func hFiller(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString("<p>" + fillerSentence(i) + "</p>\n")
	}
	return b.String()
}

func htmlPage(body string) string {
	return `<!doctype html><html><head><title>Notes</title></head><body>` + body + `</body></html>`
}

type inferOut struct {
	md  string
	rep Report
	n   int // images handed to the sink
}

func inferDOCX(t *testing.T, body string, on bool) inferOut {
	t.Helper()
	s, rep := run(t, DOCX{}, inferDOCXOf(t, body), Options{Name: "notes.docx", Images: true, InferHeadings: on})
	return inferOut{s.md.String(), rep, len(s.images)}
}

func inferHTML(t *testing.T, body string, on bool) inferOut {
	t.Helper()
	s, rep := run(t, HTML{}, []byte(htmlPage(body)), Options{Name: "notes.html", Images: true, InferHeadings: on})
	return inferOut{s.md.String(), rep, len(s.images)}
}

// inferCase is one document in both formats and what its Markdown must and
// must not hold with the option on.
type inferCase struct {
	name       string
	docx, html string
	want       []string
	absent     []string
	headings   int
}

func (c inferCase) check(t *testing.T) {
	t.Helper()
	for _, f := range []struct {
		format string
		out    inferOut
	}{{"docx", inferDOCX(t, c.docx, true)}, {"html", inferHTML(t, c.html, true)}} {
		for _, w := range c.want {
			if !strings.Contains(f.out.md, w) {
				t.Errorf("%s %s: missing %q in:\n%s", c.name, f.format, w, f.out.md)
			}
		}
		for _, a := range c.absent {
			if strings.Contains(f.out.md, a) {
				t.Errorf("%s %s: unexpected %q in:\n%s", c.name, f.format, a, f.out.md)
			}
		}
		if f.out.rep.InferredHeadings != c.headings {
			t.Errorf("%s %s: inferred_headings = %d, want %d", c.name, f.format, f.out.rep.InferredHeadings, c.headings)
		}
	}
}

func TestInferHeadings_PromotesAWholeBoldLine(t *testing.T) {
	inferCase{name: "bold",
		docx:     wFiller(3) + wP("", wrun{text: "Background", bold: true}) + wFiller(1),
		html:     hFiller(3) + `<p><b>Background</b></p>` + hFiller(1),
		want:     []string{"\n## Background\n"},
		headings: 1,
	}.check(t)
}

func TestInferHeadings_PromotesALineAtLeastRatioLarger(t *testing.T) {
	inferCase{name: "size",
		docx:     wFiller(3) + wP("", wrun{text: "Site survey", sz: 28}) + wFiller(1),
		html:     hFiller(3) + `<p style="font-size:14pt">Site survey</p>` + hFiller(1),
		want:     []string{"\n## Site survey\n"},
		headings: 1,
	}.check(t)
}

func TestInferHeadings_PromotesAColouredBoldLine(t *testing.T) {
	// The bold comes from a character style in the DOCX, the colour from
	// the run: both inherited and direct formatting are read.
	inferCase{name: "colour+bold",
		docx:     wFiller(3) + wP("", wrun{text: "Key result", style: "Strong", color: "C00000"}) + wFiller(1),
		html:     hFiller(3) + `<p><strong style="color:#C00000">Key result</strong></p>` + hFiller(1),
		want:     []string{"\n## Key result\n"},
		headings: 1,
	}.check(t)
}

func TestInferHeadings_PromotesAColouredSlightlyLargerLine(t *testing.T) {
	// 12pt over an 11pt body (13pt over 12pt) is under the 1.15 ratio:
	// only the colour makes it a heading.
	inferCase{name: "colour+larger",
		docx:     wFiller(3) + wP("", wrun{text: "Highlights", sz: 24, color: "C00000"}) + wFiller(1),
		html:     hFiller(3) + `<p style="font-size:13pt;color:#c00000">Highlights</p>` + hFiller(1),
		want:     []string{"\n## Highlights\n"},
		headings: 1,
	}.check(t)
	inferCase{name: "larger alone",
		docx:     wFiller(3) + wP("", wrun{text: "Highlights", sz: 24}) + wFiller(1),
		html:     hFiller(3) + `<p style="font-size:13pt">Highlights</p>` + hFiller(1),
		want:     []string{"\n\nHighlights\n"},
		absent:   []string{"# Highlights"},
		headings: 0,
	}.check(t)
}

func TestInferHeadings_LeavesNonHeadingsAlone(t *testing.T) {
	long := strings.Repeat("abcdefghij", 12) + "k" // 121 characters
	for _, c := range []inferCase{
		{name: "ends with a full stop",
			docx: wP("", wrun{text: "This line ends with a period.", bold: true}),
			html: `<p><b>This line ends with a period.</b></p>`,
			want: []string{"This line ends with a period."}},
		{name: "ends with a comma",
			docx: wP("", wrun{text: "Then,", bold: true}), html: `<p><b>Then,</b></p>`},
		{name: "ends with a semicolon",
			docx: wP("", wrun{text: "Also;", bold: true}), html: `<p><b>Also;</b></p>`},
		{name: "over 120 characters",
			docx: wP("", wrun{text: long, bold: true}), html: `<p><b>` + long + `</b></p>`,
			want: []string{long}},
		{name: "list item",
			docx: wP(`<w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr>`, wrun{text: "Bold item", bold: true}),
			html: `<ul><li><b>Bold item</b></li></ul>`,
			want: []string{"- Bold item"}},
		{name: "table cell",
			docx: `<w:tbl><w:tr><w:tc>` + wP("", wrun{text: "Bold cell", bold: true}) + `</w:tc><w:tc>` + wP("", plainRun("x")) + `</w:tc></w:tr></w:tbl>`,
			html: `<table><tr><td><b>Bold cell</b></td><td>x</td></tr></table>`,
			want: []string{"| Bold cell | x |"}},
		{name: "colour alone",
			docx: wP("", wrun{text: "Coloured only line", color: "1F3864"}),
			html: `<p style="color:#1f3864">Coloured only line</p>`,
			want: []string{"\n\nColoured only line\n"}},
		{name: "partly bold",
			docx: wP("", wrun{text: "Bold", bold: true}, plainRun(" and plain")),
			html: `<p><b>Bold</b> and plain</p>`,
			want: []string{"Bold and plain"}},
		{name: "two lines",
			docx: `<w:p><w:r><w:rPr><w:b/></w:rPr><w:t>First</w:t><w:br/><w:t>Second</w:t></w:r></w:p>`,
			html: `<p><b>First<br>Second</b></p>`,
			want: []string{"First\nSecond"}},
	} {
		c.docx = wFiller(3) + c.docx + wFiller(1)
		c.html = hFiller(3) + c.html + hFiller(1)
		c.absent = append(c.absent, "\n## ", "\n### ")
		c.check(t)
	}
}

// Levels: a real heading keeps its level; inferred ones start one below the
// shallowest real heading, by distinct font size, larger first; bold lines
// at body size are the deepest.
func TestInferHeadings_LevelsFollowRealHeadingsThenSizeThenBold(t *testing.T) {
	for _, f := range []struct {
		format string
		out    inferOut
	}{
		{"docx", inferDOCX(t, gdocBody(), true)},
		{"html", func() inferOut {
			s, rep := run(t, HTML{}, []byte(gdocHTML()), Options{Name: "f.html", Images: true, InferHeadings: true})
			return inferOut{s.md.String(), rep, len(s.images)}
		}()},
	} {
		for _, w := range []string{
			"\n## Quarterly field report\n",
			"\n### Section one: Overview\n",
			"\n### Section two: Findings\n",
			"\n#### Highlights in colour\n",
			"\n##### Background\n",
			"\n##### Key result\n",
		} {
			if !strings.Contains(f.out.md, w) {
				t.Errorf("%s: missing %q in:\n%s", f.format, w, f.out.md)
			}
		}
		if f.out.rep.InferredHeadings != 5 || f.out.rep.DroppedDecorativeImages != 3 {
			t.Errorf("%s: report = %+v, want 5 inferred headings and 3 dropped pictures", f.format, f.out.rep)
		}
	}
}

func TestInferHeadings_NoRealHeadingStartsAtTheFirstLevel(t *testing.T) {
	inferCase{name: "no real heading",
		docx: wFiller(3) + wP("", wrun{text: "Part one", sz: 28}) + wFiller(1) + wP("", wrun{text: "Detail", bold: true}) + wFiller(1),
		html: hFiller(3) + `<p style="font-size:14pt">Part one</p>` + hFiller(1) + `<p><b>Detail</b></p>` + hFiller(1),
		want: []string{"\n## Part one\n", "\n### Detail\n"}, headings: 2,
	}.check(t)
	inferCase{name: "real heading 2",
		docx: wP(`<w:pStyle w:val="Heading2"/>`, plainRun("Chapter")) + wFiller(3) + wP("", wrun{text: "Detail", bold: true}) + wFiller(1),
		html: `<h2>Chapter</h2>` + hFiller(3) + `<p><b>Detail</b></p>` + hFiller(1),
		want: []string{"\n### Chapter\n", "\n#### Detail\n"}, headings: 1,
	}.check(t)
}

func TestInferHeadings_DropsAPictureRepeatedThreeTimes(t *testing.T) {
	d := inferDOCX(t, wFiller(1)+wImageP("rId9", "divider")+wImageP("rId9", "divider")+wImageP("rId9", "divider")+wImageP("rId10", "site photo")+wImageP("rId10", "site photo"), true)
	h := inferHTML(t, hFiller(1)+strings.Repeat(`<img src="https://example.com/deco.png" alt="divider">`, 3)+strings.Repeat(`<img src="https://example.com/photo.png" alt="site photo">`, 2), true)
	for _, o := range []struct {
		format string
		out    inferOut
	}{{"docx", d}, {"html", h}} {
		if strings.Contains(o.out.md, "divider") || strings.Count(o.out.md, "site photo") != 2 || o.out.rep.DroppedDecorativeImages != 3 {
			t.Errorf("%s: dropped %d, md:\n%s", o.format, o.out.rep.DroppedDecorativeImages, o.out.md)
		}
	}
	if d.n != 2 || d.rep.Images != 2 {
		t.Errorf("docx: %d images handed over, report %+v; want the photo twice", d.n, d.rep)
	}
}

// A picture stored as three parts with the same bytes is one picture
// repeated.
func TestInferHeadings_DropsTheSamePictureUnderDifferentParts(t *testing.T) {
	d := inferDOCX(t, wFiller(1)+wImageP("rId11", "divider")+wImageP("rId12", "divider")+wImageP("rId13", "divider"), true)
	if strings.Contains(d.md, "divider") || d.rep.DroppedDecorativeImages != 3 || d.n != 0 {
		t.Errorf("dropped %d, images %d, md:\n%s", d.rep.DroppedDecorativeImages, d.n, d.md)
	}
}

func TestInferHeadings_CleansWhitespace(t *testing.T) {
	inferCase{name: "whitespace",
		docx: wFiller(1) + wP("", plainRun("Alpha"), wrun{text: "Beta   Gamma", tab: true}) +
			wP("", plainRun("  ")) + wP("", wrun{text: " ", tab: true}) + wFiller(1),
		html:   hFiller(1) + "<p>Alpha\tBeta&nbsp;&nbsp;&nbsp;Gamma</p><p>&nbsp;&nbsp;</p><p>\t&nbsp;</p>" + hFiller(1),
		want:   []string{"\n\nAlpha Beta Gamma\n\nParagraph 1"},
		absent: []string{" "},
	}.check(t)
}

// A document whose headings are real heading styles reads the same with
// the option on: nothing in it looks like an inferred heading.
func TestInferHeadings_RealHeadingStylesUnchanged(t *testing.T) {
	s, rep := run(t, DOCX{}, boardDOCX(t, tinyPNG(t)), Options{Name: "board.docx", Images: true, InferHeadings: true})
	golden(t, "board.docx.md", s.md.String())
	if rep.InferredHeadings != 0 || rep.DroppedDecorativeImages != 0 {
		t.Errorf("report = %+v", rep)
	}
	page := `<!doctype html><html><head><title>Новини правління</title><script>steal()</script></head><body>
<h1>Засідання</h1><p>Рішення: затвердити
   бюджет</p><h3>Деталі</h3>
<ul><li>Перше<ul><li>Вкладене</li></ul></li><li># не заголовок</li></ul>
<table><tr><th>Стаття</th><th>Сума</th></tr><tr><td>a|b</td><td>1</td></tr></table>
<img src="https://example.com/logo (1).png" alt="Логотип"><img src="data:image/png;base64,AAAA" alt="вбудоване">
<pre>code ` + "```" + ` inside</pre><style>p{}</style></body></html>`
	h, _ := run(t, HTML{}, []byte(page), Options{Name: "page.html", InferHeadings: true})
	golden(t, "page.html.md", h.md.String())
}

// The option is read only by DOCX and HTML; with it off nothing is counted.
func TestInferHeadings_OffCountsNothing(t *testing.T) {
	_, hrep := run(t, HTML{}, []byte(gdocHTML()), Options{Name: "f.html", Images: true})
	for _, rep := range []Report{inferDOCX(t, gdocBody(), false).rep, hrep} {
		if rep.InferredHeadings != 0 || rep.DroppedDecorativeImages != 0 {
			t.Errorf("report = %+v", rep)
		}
	}
}
