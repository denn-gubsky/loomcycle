package render

// The fonts a style may name (RFC HB D1). Each is named by the look a reader
// expects: a PDF embeds the bundled family (from the worker image), and Word
// and ODT name the font a reader's machine is expected to have, with the
// bundled twin as its stand-in. Carlito and Liberation are metric-compatible
// with Calibri, Arial and Times New Roman, so a PDF breaks lines and pages
// where those fonts would.

// fontFace is one font a style may name.
type fontFace struct {
	// typst is the family list a PDF asks Typst for, the bundled family
	// first and Noto behind it for a missing glyph.
	typst string
	// office is the name Word and ODT write, alt Word's stand-in for it.
	office, alt string
	// css is an HTML page's and an EPUB book's stack.
	css string
	// generic is ODF's and Word's family class: swiss or roman.
	generic string
}

var styleFonts = map[string]fontFace{
	"sans":    {typst: `("Noto Sans", "DejaVu Sans", "Libertinus Serif")`, office: "Noto Sans", alt: "Calibri", css: cssSans, generic: "swiss"},
	"serif":   {typst: `("Noto Serif", "DejaVu Serif", "Libertinus Serif")`, office: "Noto Serif", alt: "Georgia", css: cssSerif, generic: "roman"},
	"inter":   {typst: `("Inter", "Noto Sans", "DejaVu Sans")`, office: "Inter", alt: "Arial", css: `Inter,"Noto Sans",Arial,sans-serif`, generic: "swiss"},
	"calibri": {typst: `("Carlito", "Noto Sans", "DejaVu Sans")`, office: "Calibri", alt: "Carlito", css: `Calibri,Carlito,"Noto Sans",sans-serif`, generic: "swiss"},
	"arial":   {typst: `("Liberation Sans", "Noto Sans", "DejaVu Sans")`, office: "Arial", alt: "Liberation Sans", css: `Arial,"Liberation Sans","Noto Sans",sans-serif`, generic: "swiss"},
	"times":   {typst: `("Liberation Serif", "Noto Serif", "DejaVu Serif")`, office: "Times New Roman", alt: "Liberation Serif", css: `"Times New Roman","Liberation Serif","Noto Serif",serif`, generic: "roman"},
}

// fontOK reports a font name a style may use ("" is the built-in sans).
func fontOK(name string) bool {
	_, ok := styleFonts[name]
	return name == "" || ok
}

// bodyFont is the style's text font: sans when it names none.
func (s *Style) bodyFont() fontFace {
	if s == nil || s.Font == "" {
		return styleFonts["sans"]
	}
	return styleFonts[s.Font]
}

// headFont is the style's heading font, and whether it differs from the
// text's (only then does a writer set it).
func (s *Style) headFont() (fontFace, bool) {
	if s == nil || s.HeadingFont == "" || s.HeadingFont == s.Font || (s.Font == "" && s.HeadingFont == "sans") {
		return s.bodyFont(), false
	}
	return styleFonts[s.HeadingFont], true
}

// extraFonts are the fonts the style names beyond the Noto set every file
// declares, for Word's font table and ODT's font declarations.
func (s *Style) extraFonts() []fontFace {
	var out []fontFace
	seen := map[string]bool{"Noto Sans": true, "Noto Serif": true}
	add := func(f fontFace) {
		if !seen[f.office] {
			seen[f.office] = true
			out = append(out, f)
		}
	}
	add(s.bodyFont())
	if f, ok := s.headFont(); ok {
		add(f)
	}
	return out
}
