package render

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func fontDoc(t *testing.T, style string) *Doc {
	t.Helper()
	b := NewBuilder("Ann Lee")
	b.Doc.Blocks = b.Body("Intro text.")
	b.AddSection()
	b.Doc.Sections = []*Section{{ID: "s1", Title: "Experience", Level: 2, Blocks: b.Body("Text.")}}
	d, _ := b.Finish()
	st, err := ParseStyle(style)
	if err != nil {
		t.Fatal(err)
	}
	d.Style = st
	return d
}

func TestParseStyle_FontsAreTheClosedSet(t *testing.T) {
	for _, f := range []string{"sans", "serif", "inter", "calibri", "arial", "times"} {
		if _, err := ParseStyle(`{"loomcycle_style":1,"font":"` + f + `","heading_font":"` + f + `"}`); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	for body, want := range map[string]string{
		`{"loomcycle_style":1,"font":"Calibri"}`:       "font must be",
		`{"loomcycle_style":1,"heading_font":"comic"}`: "heading_font must be",
	} {
		if _, err := ParseStyle(body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", body, err)
		}
	}
}

// A style asking for Calibri writes a Word and ODT file that name Calibri,
// with Carlito as its stand-in, and a PDF source that asks for Carlito; the
// heading font, when it differs, goes on the headings only.
func TestFonts_EveryFormatNamesTheLook(t *testing.T) {
	d := fontDoc(t, `{"loomcycle_style":1,"font":"calibri","heading_font":"inter"}`)

	w := docxParts(t, write(t, d, FormatDOCX, Options{}))
	if s := w["word/styles.xml"]; !strings.Contains(s, `<w:rFonts w:ascii="Calibri" w:hAnsi="Calibri" w:eastAsia="Calibri" w:cs="Calibri"/>`) ||
		!strings.Contains(s, `w:styleId="Heading1"`) || !strings.Contains(s[strings.Index(s, `w:styleId="Heading1"`):], `<w:rFonts w:ascii="Inter"`) {
		t.Errorf("styles.xml:\n%s", s)
	}
	if ft := w["word/fontTable.xml"]; !strings.Contains(ft, `<w:font w:name="Calibri"><w:altName w:val="Carlito"/>`) || !strings.Contains(ft, `<w:font w:name="Inter"><w:altName w:val="Arial"/>`) || xmlWellFormed(ft) != nil {
		t.Errorf("fontTable.xml:\n%s", ft)
	}

	o := docxParts(t, write(t, d, FormatODT, Options{}))
	if s := o["styles.xml"]; !strings.Contains(s, `<style:font-face style:name="Calibri" svg:font-family="'Calibri'"`) ||
		!strings.Contains(s, `style:font-name="Calibri"`) || !strings.Contains(s, `style:font-name="Inter"`) || xmlWellFormed(s) != nil {
		t.Errorf("odt styles.xml:\n%s", s)
	}
	if !strings.Contains(o["content.xml"], `style:name="Calibri"`) {
		t.Error("odt content.xml does not declare the font")
	}

	h := string(write(t, d, FormatHTML, Options{}))
	for _, want := range []string{`font-family:Calibri,Carlito,"Noto Sans",sans-serif`, `h2,h3,h4,h5,h6{font-family:Inter,"Noto Sans",Arial,sans-serif}`} {
		if !strings.Contains(h, want) {
			t.Errorf("html lacks %q", want)
		}
	}

	src := typSource(t, d, Options{})
	for _, want := range []string{`#set text(font: ("Carlito", "Noto Sans", "DejaVu Sans")`, `#show heading: set text(font: ("Inter", "Noto Sans", "DejaVu Sans"))`} {
		if !strings.Contains(src, want) {
			t.Errorf("typst lacks %q", want)
		}
	}
}

// A heading font the same as the text's sets nothing extra.
func TestFonts_SameHeadingFontAddsNothing(t *testing.T) {
	d := fontDoc(t, `{"loomcycle_style":1,"font":"serif","heading_font":"serif"}`)
	if strings.Contains(typSource(t, d, Options{}), "#show heading: set text(font") {
		t.Error("typst sets a heading font equal to the text's")
	}
	if ft := docxParts(t, write(t, d, FormatDOCX, Options{}))["word/fontTable.xml"]; ft != docxFontTable {
		t.Errorf("font table changed for a Noto style:\n%s", ft)
	}
}

// The PDF embeds the bundled twin: Carlito for calibri, Inter for the
// headings. Needs the fonts installed, as the worker image and CI's
// render-office job have them (LOOMCYCLE_REQUIRE_RENDER_FONTS).
func TestPDF_EmbedsTheStylesFonts(t *testing.T) {
	typst, err := exec.LookPath("typst")
	if err != nil || os.Getenv("LOOMCYCLE_REQUIRE_RENDER_FONTS") == "" {
		if os.Getenv("LOOMCYCLE_REQUIRE_RENDER_FONTS") != "" {
			t.Fatal("typst is required here and not on PATH")
		}
		t.Skip("set LOOMCYCLE_REQUIRE_RENDER_FONTS where typst and the style fonts are installed")
	}
	pdf := string(compileTypst(t, typst, fontDoc(t, `{"loomcycle_style":1,"font":"calibri","heading_font":"inter"}`), Options{}))
	for _, want := range []string{"Carlito-Regular", "Inter-"} {
		if !strings.Contains(pdf, want) {
			t.Errorf("the PDF does not embed %s", want)
		}
	}
	pdf = string(compileTypst(t, typst, fontDoc(t, `{"loomcycle_style":1,"font":"arial","heading_font":"times"}`), Options{}))
	for _, want := range []string{"LiberationSans", "LiberationSerif"} {
		if !strings.Contains(pdf, want) {
			t.Errorf("the PDF does not embed %s", want)
		}
	}
}
