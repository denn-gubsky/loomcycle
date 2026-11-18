package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
)

// typstInjection is Typst syntax a body, title, alt text or cell might hold:
// every piece of it must reach the PDF as text (RFC GR §7).
const typstInjection = `PWN1 #import "@preview/evil:1.0.0": * PWN2 #read("/etc/passwd") PWN3 #include "x.typ"
PWN4 $x^2$ <lbl> @ref PWN5 #{ panic("boom") } #set page(width: 1pt) PWN6 // a comment
PWN7 /* block */ \ back\slash "quote" PWN8 #let x = 1 PWN9 = not a heading`

func typSource(t *testing.T, d *Doc, opts Options) string {
	t.Helper()
	var buf bytes.Buffer
	if _, err := WriteTypst(context.Background(), &buf, d, opts); err != nil {
		t.Fatalf("write typst: %v", err)
	}
	return buf.String()
}

// outsideStrings is src with every string literal and the // comment lines
// removed: what Typst reads as markup or code.
func outsideStrings(t *testing.T, src string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.SplitAfter(src, "\n") {
		if strings.HasPrefix(line, "//") {
			continue
		}
		in := false
		for i := 0; i < len(line); i++ {
			c := line[i]
			switch {
			case in && c == '\\':
				i++
			case c == '"':
				in = !in
			case !in:
				b.WriteByte(c)
			}
		}
		if in {
			t.Fatalf("a string literal runs past its line: %q", line)
		}
	}
	return b.String()
}

func injectionDoc(t *testing.T) *Doc {
	t.Helper()
	b := NewBuilder("Title PWN10 " + typstInjection)
	b.Doc.Anchor = "chunk-root"
	b.Doc.Status = "review"
	b.Doc.Provenance = `PWN12 #read("x")`
	b.Doc.Language = `uk"); #panic("PWN13`
	b.Doc.Date = `2026-11-16"); #panic("PWN14`
	pic := b.Image("image/png", tinyPNG, `PWN15 ", alt: none); #panic("x`)
	b.AddSection()
	body := typstInjection + "\n\n| PWN16 #a | b |\n|---|---|\n| `PWN17 #raw` | [PWN18 #link](https://example.com/\"#x) |\n\n" +
		"```typ\"); #panic(\"PWN19\n#import \"x\"\n```\n\n- PWN20 $item$\n\n> PWN21 #quote"
	b.Doc.Sections = []*Section{{Anchor: "chunk-a", Title: `PWN22 ]#panic("x")[ <chunk-a>`, Level: 2, Blocks: append([]Node{pic}, b.Body(body)...)}}
	d, _ := b.Finish()
	return d
}

// No text from the Doc ever reaches Typst outside a string literal, and the
// writer itself never emits an import, include, file read or package.
func TestWriteTypst_BodyTextStaysInsideStringLiterals(t *testing.T) {
	src := typSource(t, injectionDoc(t), Options{TOC: true})
	code := outsideStrings(t, src)
	if strings.Contains(code, "PWN") {
		t.Errorf("text from the document reached Typst outside a string:\n%s", code)
	}
	for _, bad := range []string{"#import", "#include", "read(", "json(", "@preview", "panic(", "eval("} {
		if strings.Contains(code, bad) {
			t.Errorf("the source contains %q outside a string:\n%s", bad, code)
		}
	}
	if strings.Contains(src, `lang: "uk"`) || !strings.Contains(src, "date: none") {
		t.Errorf("a malformed language or date was written:\n%s", src)
	}
}

func TestWriteTypst_SameDocWritesSameBytes(t *testing.T) {
	a := typSource(t, fixtureDoc(t), Options{TOC: true})
	b := typSource(t, fixtureDoc(t), Options{TOC: true})
	if a != b {
		t.Fatal("two renders of the same document differ")
	}
	if !strings.Contains(a, "date: datetime(year: 2026, month: 11, day: 16)") || !strings.Contains(a, `lang: "uk"`) {
		t.Errorf("the properties are missing:\n%s", a)
	}
}

// Typst refuses a link to a label that is missing or defined twice, so each
// anchor is labelled once and a link to an unknown one is its text.
func TestWriteTypst_LinksOnlyToLabelsThatExistOnce(t *testing.T) {
	b := NewBuilder("x")
	b.Doc.Anchor = "chunk-root"
	b.AddSection()
	b.AddSection()
	link := func(anchor, text string) Node {
		return Node{Kind: KindParagraph, Children: []Node{{Kind: KindLink, Anchor: anchor, Children: []Node{{Kind: KindText, Text: text}}}}}
	}
	b.Doc.Sections = []*Section{
		{Anchor: "chunk-a", Title: "A", Level: 2, Blocks: []Node{link("chunk-gone", "gone"), link("chunk-a", "here")}},
		{Anchor: "chunk-a", Title: "A again", Level: 2},
	}
	d, _ := b.Finish()
	src := typSource(t, d, Options{})
	if n := strings.Count(src, "<chunk-a>"); n != 2 { // one label, one link
		t.Errorf("<chunk-a> appears %d times, want a label and a link:\n%s", n, src)
	}
	if strings.Contains(src, "chunk-gone") {
		t.Errorf("a link to a missing label was written:\n%s", src)
	}
}

// A picture whose header reads and whose data does not would stop the
// typesetter; it is its alt text, and the caller is not asked to write it.
func TestWriteTypst_APictureThatDoesNotDecodeIsItsAltText(t *testing.T) {
	b := NewBuilder("x")
	broken := append([]byte(nil), tinyPNG[:len(tinyPNG)-20]...)
	n := b.Image("image/png", broken, "broken")
	ok := b.Image("image/png", tinyPNG, "fine")
	d, _ := b.Finish()
	d.Blocks = []Node{n, ok}
	src := typSource(t, d, Options{})
	if !strings.Contains(src, `#"[broken]"`) || strings.Count(src, "#image(") != 1 {
		t.Errorf("the broken picture was embedded:\n%s", src)
	}
	used, err := WriteTypst(context.Background(), &bytes.Buffer{}, d, Options{})
	if err != nil || len(used) != 1 || used[0].Name != d.Images[1].Name {
		t.Errorf("used pictures = %v", used)
	}
}

func TestWrite_PDFIsLeftToTheHelper(t *testing.T) {
	err := Write(context.Background(), &bytes.Buffer{}, fixtureDoc(t), FormatPDF, Options{})
	if !errors.Is(err, ErrUnsupported) || !Supported(FormatPDF) || MediaType(FormatPDF) != "application/pdf" {
		t.Fatalf("err = %v", err)
	}
}

// compileTypst typesets d with the typst on PATH and returns the PDF.
func compileTypst(t *testing.T, typst string, d *Doc, opts Options) []byte {
	t.Helper()
	dir := t.TempDir()
	var src bytes.Buffer
	used, err := WriteTypst(context.Background(), &src, d, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.typ"), src.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "images"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, im := range used {
		if err := os.WriteFile(filepath.Join(dir, TypstImagePath(im.Name)), im.Data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, typst, "compile", "--root", dir, filepath.Join(dir, "main.typ"), filepath.Join(dir, "out.pdf")).CombinedOutput()
	if err != nil {
		t.Fatalf("typst compile: %v\n%s", err, out)
	}
	pdf, err := os.ReadFile(filepath.Join(dir, "out.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	return pdf
}

func pdfText(t *testing.T, pdf []byte) string {
	t.Helper()
	sink := &mdSink{}
	if _, err := (extract.PDF{}).Extract(context.Background(), bytes.NewReader(pdf), int64(len(pdf)), sink, extract.Options{}); err != nil {
		t.Fatalf("extract: %v", err)
	}
	return sink.md.String()
}

// The source compiles, and the PDF holds the document's text, the Typst
// syntax in it included, as loomcycle's own extractor reads it (RFC GR §7).
// Skipped where typst is not installed; the CI job that installs it sets
// LOOMCYCLE_REQUIRE_TYPST so the check cannot silently not run.
func TestPDF_TypesetsAndKeepsTheText(t *testing.T) {
	typst, err := exec.LookPath("typst")
	if err != nil {
		if os.Getenv("LOOMCYCLE_REQUIRE_TYPST") != "" {
			t.Fatal("typst is required here and not on PATH")
		}
		t.Skip("typst not installed")
	}
	for _, opts := range []Options{{TOC: true}, {TitlePage: true, PageSize: "letter"}} {
		txt := pdfText(t, compileTypst(t, typst, fixtureDoc(t), opts))
		for _, want := range []string{"Протокол засідання", "Порядок денний", "Перше питання", "вкладений пункт", "Іван", "Last paragraph", "Draft", "fmt.Println"} {
			if !strings.Contains(txt, want) {
				t.Errorf("%+v: the PDF lacks %q:\n%s", opts, want, txt)
			}
		}
	}
	txt := pdfText(t, compileTypst(t, typst, injectionDoc(t), Options{TOC: true}))
	for _, want := range []string{"PWN1 #import", `#read("/etc/passwd")`, "PWN5 #{ panic", "$x^2$", "PWN10", "PWN12", "PWN16 #a", "PWN17", "PWN18", "PWN20", "PWN21", "PWN22", "Review"} {
		if !strings.Contains(txt, want) {
			t.Errorf("the PDF lacks %q as text:\n%s", want, txt)
		}
	}
}

// A PDF carries the record as an attached file (D21). Its stream is
// compressed; its name and the PDF's attachment structures are not.
func TestPDF_CarriesTheRecordAsAnAttachment(t *testing.T) {
	typst, err := exec.LookPath("typst")
	if err != nil {
		if os.Getenv("LOOMCYCLE_REQUIRE_TYPST") != "" {
			t.Fatal("typst is required here and not on PATH")
		}
		t.Skip("typst not installed")
	}
	pdf := compileTypst(t, typst, embedDoc(t), Options{})
	for _, want := range []string{"loomcycle.json", "/EmbeddedFile"} {
		if !bytes.Contains(pdf, []byte(want)) {
			t.Errorf("the PDF has no %q", want)
		}
	}
	if bytes.Contains(compileTypst(t, typst, fixtureDoc(t), Options{}), []byte("/EmbeddedFile")) {
		t.Error("a PDF rendered without metadata embed carries an attachment")
	}
	// The PDF extractor finds it again (RFC GR P4b.2).
	sink := &pdfRecordSink{}
	if _, err := (extract.PDF{}).Extract(context.Background(), bytes.NewReader(pdf), int64(len(pdf)), sink, extract.Options{}); err != nil {
		t.Fatal(err)
	}
	var e Embed
	if err := json.Unmarshal(sink.record, &e); err != nil || e.DocumentID != "doc1" || len(e.Chunks) != 4 {
		t.Errorf("the extracted record = %s (%v)", sink.record, err)
	}
}

type pdfRecordSink struct {
	mdSink
	record []byte
}

func (s *pdfRecordSink) Record(b []byte)      { s.record = append([]byte(nil), b...) }
func (s *pdfRecordSink) HeadingAnchor(string) {}
