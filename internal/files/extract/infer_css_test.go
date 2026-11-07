package extract

import (
	"strings"
	"testing"
)

// Heading recovery on HTML styled by class rules: an editor's HTML export
// that writes its formatting as `.cN{...}` rules in a <style> block and
// class attributes on the text, with no inline styles, gets the same
// outline as an inline-styled page.

// gdocCSS is a style block in the shape such an export writes: an @import,
// tag rules, list pseudo-elements, one class per formatting, a title class.
const gdocCSS = `@import url(https://fonts.example.com/css?family=Arial);` +
	`ol{margin:0;padding:0}table td,table th{padding:0}` +
	`li.li-bullet-0:before{margin-left:-18pt;white-space:nowrap;display:inline-block;min-width:18pt}` +
	`.c0{color:#000000;font-weight:400;text-decoration:none;vertical-align:baseline;font-size:11pt;font-family:"Arial";font-style:normal}` +
	`.c1{padding-top:0pt;padding-bottom:0pt;line-height:1.15;orphans:2;widows:2;text-align:left}` +
	`.c2{color:#000000;font-weight:700;text-decoration:none;vertical-align:baseline;font-size:11pt;font-family:"Arial";font-style:normal}` +
	`.c3{color:#1f3864;font-weight:400;text-decoration:none;vertical-align:baseline;font-size:16pt;font-family:"Arial";font-style:normal}` +
	`.c4{color:#c00000;font-weight:400;font-size:12pt}` +
	`.c5{background-color:#ffffff;max-width:468pt;padding:72pt 72pt 72pt 72pt}` +
	`.title{padding-top:0pt;color:#000000;font-size:26pt;padding-bottom:3pt;font-family:"Arial";line-height:1.15;page-break-after:avoid;orphans:2;widows:2;text-align:left}` +
	`p{margin:0;color:#000000;font-size:11pt;font-family:"Arial"}` +
	`h1{padding-top:20pt;color:#000000;font-size:20pt;padding-bottom:6pt;font-family:"Arial"}`

func gdocPage(css, body string) string {
	return `<html><head><meta content="text/html; charset=UTF-8" http-equiv="content-type"><style type="text/css">` +
		css + `</style></head><body class="c5 doc-content">` + body + `</body></html>`
}

// cP is a paragraph of one span, classed as such an export classes them.
func cP(class, text string) string {
	return `<p class="c1"><span class="` + class + `">` + text + `</span></p>`
}

func cFiller(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(cP("c0", fillerSentence(i)))
	}
	return b.String()
}

func inferPage(t *testing.T, page string) inferOut {
	t.Helper()
	s, rep := run(t, HTML{}, []byte(page), Options{Name: "export.html", Images: true, InferHeadings: true})
	return inferOut{s.md.String(), rep, len(s.images)}
}

func wantHeadings(t *testing.T, o inferOut, n int, want, absent []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(o.md, w) {
			t.Errorf("missing %q in:\n%s", w, o.md)
		}
	}
	for _, a := range absent {
		if strings.Contains(o.md, a) {
			t.Errorf("unexpected %q in:\n%s", a, o.md)
		}
	}
	if o.rep.InferredHeadings != n {
		t.Errorf("inferred_headings = %d, want %d", o.rep.InferredHeadings, n)
	}
}

// Levels as for inline styles: the real <h1>, then 16pt, then 12pt, then
// bold at body size. "c4 c2" is red, 12pt and NOT bold: .c4 comes later in
// the sheet, so its font-weight wins over .c2's.
func TestInferHeadings_ReadsClassRulesFromAStyleBlock(t *testing.T) {
	body := `<h1 class="c1"><span class="c0">Field report</span></h1>` + cFiller(3) +
		cP("c3", "Section one: Overview") + cFiller(2) +
		cP("c4 c2", "Highlights in colour") + cFiller(1) +
		cP("c2", "Background") + cFiller(1) +
		`<p class="c1"><span class="c2">Partly bold</span><span class="c0"> and plain</span></p>` + cFiller(1)
	o := inferPage(t, gdocPage(gdocCSS, body))
	wantHeadings(t, o, 3, []string{
		"\n## Field report\n",
		"\n### Section one: Overview\n",
		"\n#### Highlights in colour\n",
		"\n##### Background\n",
		"Partly bold and plain",
	}, []string{"# Partly bold"})
}

// The same document styled inline and styled by classes has one outline.
func TestInferHeadings_ClassStyledPageMatchesItsInlineTwin(t *testing.T) {
	classed := gdocPage(gdocCSS, cFiller(3)+cP("c3", "Site survey")+cFiller(1)+cP("c2", "Detail")+cFiller(1))
	inline := htmlPage(hFiller(3) + `<p><span style="font-size:16pt;color:#1f3864">Site survey</span></p>` + hFiller(1) +
		`<p><span style="font-weight:700">Detail</span></p>` + hFiller(1))
	a, b := inferPage(t, classed), inferPage(t, inline)
	if a.rep.InferredHeadings != 2 || b.rep.InferredHeadings != 2 {
		t.Fatalf("inferred: classed %d, inline %d; want 2 each", a.rep.InferredHeadings, b.rep.InferredHeadings)
	}
	for _, w := range []string{"\n## Site survey\n", "\n### Detail\n"} {
		if !strings.Contains(a.md, w) || !strings.Contains(b.md, w) {
			t.Errorf("%q: classed\n%s\ninline\n%s", w, a.md, b.md)
		}
	}
}

func TestInferHeadings_ClassRulesCascade(t *testing.T) {
	for _, c := range []struct {
		name, css, body string
		heading         bool
	}{
		{"tag.class is read", `p.lead{font-weight:bold}`, `<p class="lead">Lead in</p>`, true},
		{"span.class is read", `span.k{font-weight:bold}`, `<p><span class="k">Lead in</span></p>`, true},
		{"tag.class on another tag is not", `div.lead{font-weight:bold}`, `<p class="lead">Lead in</p>`, false},
		{"a selector list reads its simple parts", `td .x, .k , em{font-weight:bold}`, `<p><span class="k">Lead in</span></p>`, true},
		{"a later rule overrides an earlier", `.k{font-weight:bold}.k{font-weight:400}`, `<p class="k">Lead in</p>`, false},
		{"tag.class outranks a later .class", `p.k{font-weight:bold}.k{font-weight:normal}`, `<p class="k">Lead in</p>`, true},
		{"an inline style outranks a class", `.k{font-weight:bold}`, `<p class="k" style="font-weight:normal">Lead in</p>`, false},
		{"a descendant selector is ignored", `div .k{font-weight:bold}`, `<div><p class="k">Lead in</p></div>`, false},
		{"a compound class selector is ignored", `.k.j{font-weight:bold}`, `<p class="k j">Lead in</p>`, false},
		{"a pseudo-class is ignored", `.k:first-child{font-weight:bold}`, `<p class="k">Lead in</p>`, false},
		{"an id selector is ignored", `#k{font-weight:bold}`, `<p id="k">Lead in</p>`, false},
		{"a tag rule is ignored", `p{font-weight:bold}`, `<p>Lead in</p>`, false},
		{"a rule inside @media is ignored", `@media print{.k{font-weight:bold}}`, `<p class="k">Lead in</p>`, false},
		{"a rule after @media is read", `@media print{.j{color:red}}.k{font-weight:bold}`, `<p class="k">Lead in</p>`, true},
		{"a comment is not a rule", `/* .k{font-weight:bold} */`, `<p class="k">Lead in</p>`, false},
		{"a style block in the body is read", ``, `<style>.k{font-weight:bold}</style><p class="k">Lead in</p>`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := inferPage(t, gdocPage(c.css, cFiller(3)+c.body+cFiller(1)))
			n := 0
			if c.heading {
				n = 1
			}
			if got := strings.Contains(o.md, "\n## Lead in\n"); got != c.heading || o.rep.InferredHeadings != n {
				t.Errorf("heading = %v (inferred %d), want %v:\n%s", got, o.rep.InferredHeadings, c.heading, o.md)
			}
		})
	}
}

// Style sheets are someone else's text: only so much of them is read.
// A rule past the bound is not, and a class attribute names only so many
// classes.
func TestInferHeadings_ClassRulesAreBounded(t *testing.T) {
	pad := strings.Repeat(".pad{color:#123456}", maxCSSRules)
	o := inferPage(t, gdocPage(pad+`.k{font-weight:bold}`, cFiller(3)+`<p class="k">Lead in</p>`+cFiller(1)))
	if strings.Contains(o.md, "# Lead in") {
		t.Errorf("a rule past %d rules was read:\n%s", maxCSSRules, o.md)
	}
	big := `/*` + strings.Repeat("x", maxCSSBytes) + `*/.k{font-weight:bold}`
	o = inferPage(t, gdocPage(big, cFiller(3)+`<p class="k">Lead in</p>`+cFiller(1)))
	if strings.Contains(o.md, "# Lead in") {
		t.Errorf("a rule past %d bytes of style sheet was read:\n%s", maxCSSBytes, o.md)
	}
	many := strings.Repeat("z ", maxCSSClasses) + "k"
	o = inferPage(t, gdocPage(`.k{font-weight:bold}`, cFiller(3)+`<p class="`+many+`">Lead in</p>`+cFiller(1)))
	if strings.Contains(o.md, "# Lead in") {
		t.Errorf("a class past the first %d was read:\n%s", maxCSSClasses, o.md)
	}
}

// With the option off, a style block changes nothing.
func TestInferHeadings_OffIgnoresClassRules(t *testing.T) {
	page := gdocPage(gdocCSS, cFiller(3)+cP("c2", "Background")+cFiller(1))
	s, rep := run(t, HTML{}, []byte(page), Options{Name: "export.html"})
	if rep.InferredHeadings != 0 || strings.Contains(s.md.String(), "# Background") {
		t.Errorf("report %+v, md:\n%s", rep, s.md.String())
	}
}
