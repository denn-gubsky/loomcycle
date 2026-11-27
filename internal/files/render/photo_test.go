package render

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// photoDoc is a CV-shaped Doc whose first block is a picture, in a style
// that places a photo.
func photoDoc(t *testing.T, photo string) *Doc {
	t.Helper()
	b := NewBuilder("Ann Lee")
	pic := b.Image("image/png", tinyPNG, "portrait")
	b.Doc.Blocks = append([]Node{{Kind: KindParagraph, Children: []Node{pic}}}, b.Body("Engineer in Kyiv.")...)
	b.AddSection()
	b.Doc.Sections = []*Section{{ID: "s1", Title: "Experience", Level: 2, Blocks: b.Body("Text.")}}
	d, _ := b.Finish()
	st, err := ParseStyle(`{"loomcycle_style":1,"photo":` + photo + `}`)
	if err != nil {
		t.Fatal(err)
	}
	d.Style = st
	if !d.TakePhoto() {
		t.Fatal("no photo taken")
	}
	return d
}

func TestParseStyle_PhotoTakesOnlyItsValues(t *testing.T) {
	for body, want := range map[string]string{
		`{"loomcycle_style":1,"photo":{"place":"top"}}`:                                 "photo.place",
		`{"loomcycle_style":1,"photo":{"width_mm":70}}`:                                 "photo.width_mm",
		`{"loomcycle_style":1,"photo":{"shape":"oval"}}`:                                "photo.shape",
		`{"loomcycle_style":1,"photo":{"float":"left"}}`:                                "unknown key",
		`{"loomcycle_style":1,"photo_image":"img-1.png"}`:                               "not keys of a style",
		`{"loomcycle_style":1,"photo":{"place":"left","width_mm":15,"shape":"circle"}}`: "",
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

// The first picture before the first heading becomes the photo and leaves
// the flow: alone among the root's blocks, or a first section that is only
// a picture (an image chunk); a picture after text in a section is not.
func TestTakePhoto_TheFirstPictureBeforeAHeading(t *testing.T) {
	d := photoDoc(t, `{}`)
	if d.Style.PhotoImage == "" || len(d.Blocks) != 1 || d.Blocks[0].Kind != KindParagraph || len(d.Blocks[0].Children) == 0 || d.Blocks[0].Children[0].Kind == KindImage {
		t.Errorf("photo %q, blocks %+v", d.Style.PhotoImage, d.Blocks)
	}

	b := NewBuilder("CV")
	pic := b.Image("image/png", tinyPNG, "portrait")
	b.AddSection()
	b.Doc.Sections = []*Section{{ID: "p", Title: "Photo", Level: 2, Blocks: []Node{pic}}, {ID: "s", Title: "Experience", Level: 2, Blocks: b.Body("x")}}
	d, _ = b.Finish()
	d.Style = &Style{Version: 1}
	if !d.TakePhoto() || len(d.Sections) != 1 || d.Sections[0].Title != "Experience" {
		t.Errorf("an image section was not taken: %+v", d.Sections)
	}

	b = NewBuilder("CV")
	b.AddSection()
	b.Doc.Sections = []*Section{{ID: "s", Title: "Experience", Level: 2, Blocks: append(b.Body("text"), b.Image("image/png", tinyPNG, "x"))}}
	d, _ = b.Finish()
	d.Style = &Style{Version: 1}
	if d.TakePhoto() {
		t.Error("a picture after text was taken")
	}
}

// Every format places the photo beside the title block, once.
func TestPhoto_EveryFormatPlacesIt(t *testing.T) {
	d := photoDoc(t, `{"place":"right","width_mm":30,"shape":"circle"}`)

	h := string(write(t, d, FormatHTML, Options{}))
	if strings.Count(h, "<img") != 1 || !strings.Contains(h, `<img class="photo photo-right photo-circle" src="data:image/png`) ||
		!strings.Contains(h, `<p class="photo-clear"></p>`) || !strings.Contains(h, "img.photo{width:30mm") {
		t.Errorf("html:\n%s", h)
	}
	if i, j := strings.Index(h, `class="photo-clear"`), strings.Index(h, "<h2"); i < 0 || j < 0 || i > j {
		t.Error("the photo is not cleared before the first heading")
	}
	assertInertHTML(t, h)

	w := docxParts(t, write(t, d, FormatDOCX, Options{}))
	doc := w["word/document.xml"]
	for _, want := range []string{`<wp:anchor `, `<wp:align>right</wp:align>`, `<wp:wrapSquare wrapText="bothSides"/>`, `cx="1080000"`} {
		if !strings.Contains(doc, want) {
			t.Errorf("document.xml lacks %q", want)
		}
	}
	if strings.Contains(doc, "<w:tbl>") || strings.Count(doc, "<w:drawing>") != 1 || xmlWellFormed(doc) != nil {
		t.Error("the photo is in a table, drawn twice, or document.xml is not well-formed")
	}

	o := docxParts(t, write(t, d, FormatODT, Options{}))
	if c := o["content.xml"]; !strings.Contains(c, `draw:style-name="FrPhotoRight"`) || !strings.Contains(c, `text:anchor-type="paragraph"`) ||
		!strings.Contains(c, `style:name="FrPhotoRight"`) || strings.Count(c, "<draw:frame") != 1 || xmlWellFormed(c) != nil {
		t.Errorf("odt content.xml:\n%s", c)
	}

	src := typSource(t, d, Options{})
	if !strings.Contains(src, `#grid(columns: (1fr, 30mm), column-gutter: 1.2em, [`) || !strings.Contains(src, `#box(clip: true, radius: 50%)[#image("images/`) {
		t.Errorf("typst:\n%s", src)
	}
}

func TestPhoto_AboveTheTitle(t *testing.T) {
	d := photoDoc(t, `{"place":"above"}`)
	if h := string(write(t, d, FormatHTML, Options{})); !strings.Contains(h, `class="photo photo-above"`) || strings.Contains(h, "photo-clear\"></p>") {
		t.Errorf("html:\n%s", h)
	}
	if doc := docxParts(t, write(t, d, FormatDOCX, Options{}))["word/document.xml"]; strings.Contains(doc, "<wp:anchor") || !strings.Contains(doc, "<wp:inline") {
		t.Error("an above photo is anchored in Word")
	}
	if src := typSource(t, d, Options{}); strings.Contains(src, "#grid(") {
		t.Error("an above photo is in a grid")
	}
}

// The grid typesets beside the title, with a title page too (where it holds
// only the title, since a page break cannot sit in a grid). Skipped where
// typst is not installed.
func TestPDF_TypesetsAPhoto(t *testing.T) {
	typst, err := exec.LookPath("typst")
	if err != nil {
		if os.Getenv("LOOMCYCLE_REQUIRE_TYPST") != "" {
			t.Fatal("typst is required here and not on PATH")
		}
		t.Skip("typst not installed")
	}
	for _, c := range []struct {
		photo string
		opts  Options
	}{
		{`{"place":"right"}`, Options{}},
		{`{"place":"left","shape":"circle"}`, Options{}},
		{`{"place":"right"}`, Options{TitlePage: true, TOC: true}},
	} {
		txt := pdfText(t, compileTypst(t, typst, photoDoc(t, c.photo), c.opts))
		if !strings.Contains(txt, "Ann Lee") || !strings.Contains(txt, "Engineer in Kyiv.") {
			t.Errorf("%s %+v:\n%s", c.photo, c.opts, txt)
		}
	}
}
