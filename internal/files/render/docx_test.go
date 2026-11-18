package render

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
)

// mdSink collects an extraction's Markdown and counts its pictures.
type mdSink struct {
	md     strings.Builder
	images int
}

func (s *mdSink) Text(md string) error { s.md.WriteString(md); return nil }
func (s *mdSink) Image(img extract.Image) (string, error) {
	s.images++
	return img.Name, nil
}

func docxParts(t *testing.T, b []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	parts := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		parts[f.Name] = string(data)
	}
	return parts
}

// A rendered DOCX read back by loomcycle's own extractor gives the same
// headings, text, table cells and pictures (RFC GR §7).
func TestWriteDOCX_RoundTripsThroughTheExtractor(t *testing.T) {
	d := fixtureDoc(t)
	b := write(t, d, FormatDOCX, Options{TOC: true})
	sink := &mdSink{}
	rep, err := extract.DOCX{}.Extract(context.Background(), bytes.NewReader(b), int64(len(b)), sink, extract.Options{Images: true})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	md := sink.md.String()
	for _, want := range []string{
		"# Протокол засідання — Board minutes",
		"## Порядок денний", "## Рішення", "### Деталі", "#### Підрозділ у тексті",
		"Перше питання", "вкладений пункт", "Цитата з", "Last paragraph.",
		"Іван", "a \\| pipe", "fmt.Println",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("the extraction lacks %q:\n%s", want, md)
		}
	}
	if sink.images != 1 || rep.Tables != 1 {
		t.Errorf("pictures %d, tables %d; want 1 and 1:\n%s", sink.images, rep.Tables, md)
	}
	if !strings.Contains(md, "1. Перше питання") || !strings.Contains(md, "- вкладений пункт") {
		t.Errorf("lists lost their kind:\n%s", md)
	}
}

func TestWriteDOCX_CarriesTheProperties(t *testing.T) {
	parts := docxParts(t, write(t, fixtureDoc(t), FormatDOCX, Options{PageSize: "letter"}))
	core := parts["docProps/core.xml"]
	for _, want := range []string{"<dc:title>Протокол засідання — Board minutes</dc:title>", "<cp:contentStatus>draft</cp:contentStatus>",
		"<dc:language>uk</dc:language>", "2026-11-16T00:00:00Z"} {
		if !strings.Contains(core, want) {
			t.Errorf("core properties lack %q:\n%s", want, core)
		}
	}
	doc := parts["word/document.xml"]
	if !strings.Contains(doc, `w:w="12240" w:h="15840"`) {
		t.Error("letter page size not set")
	}
	if !strings.Contains(doc, `<w:rStyle w:val="StatusLabel"/></w:rPr><w:t xml:space="preserve">Draft</w:t>`) {
		t.Error("no Draft label beside the title")
	}
	if !strings.Contains(doc, `<w:hyperlink w:anchor="chunk_s2">`) || !strings.Contains(doc, `w:name="chunk_s2"`) {
		t.Error("an internal link has no bookmark to land on")
	}
	if !strings.Contains(parts["word/styles.xml"], `w:ascii="Noto Sans"`) {
		t.Error("the body font is not Noto Sans")
	}
	if parts["word/media/img-1.png"] == "" {
		t.Error("the picture is not in word/media")
	}
}

func TestWriteDOCX_SameDocWritesSameBytes(t *testing.T) {
	a := write(t, fixtureDoc(t), FormatDOCX, Options{TOC: true})
	b := write(t, fixtureDoc(t), FormatDOCX, Options{TOC: true})
	if !bytes.Equal(a, b) {
		t.Fatal("two renders of the same document differ")
	}
}

// Text is only ever text runs: no field, macro, altChunk or external part
// but a safe hyperlink, whatever a body holds (RFC GR §4.7).
func TestWriteDOCX_BodyTextNeverBecomesAField(t *testing.T) {
	b := NewBuilder(`Title <w:fldSimple w:instr="INCLUDETEXT x"/> {INCLUDEPICTURE "http://evil"}`)
	b.AddSection()
	body := injection + "\n\n<w:altChunk r:id=\"x\"/> <w:instrText>HYPERLINK</w:instrText> \x01\x02 control\x0b chars \uFFFE"
	b.Doc.Sections = []*Section{{Anchor: "chunk-a", Title: `]]><w:fldSimple/>`, Level: 2, Blocks: b.Body(body)}}
	b.Doc.Status = `draft</cp:contentStatus><evil/>`
	d, _ := b.Finish()
	parts := docxParts(t, write(t, d, FormatDOCX, Options{}))
	for name, body := range parts {
		// The schemes may appear as text; what must not appear is markup.
		for _, bad := range []string{"<w:fldSimple", "<w:instrText", "<w:altChunk", "<evil"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains %q", name, bad)
			}
		}
	}
	rels := parts["word/_rels/document.xml.rels"]
	for _, l := range strings.Split(rels, "<Relationship ") {
		if strings.Contains(l, `TargetMode="External"`) && !strings.Contains(l, `Target="http`) && !strings.Contains(l, `Target="mailto:`) {
			t.Errorf("an external relationship to something else: %s", l)
		}
	}
	// Every part must still be well-formed XML: an invalid character would
	// make Word refuse the file.
	for name, body := range parts {
		if !strings.HasSuffix(name, ".xml") && !strings.HasSuffix(name, ".rels") {
			continue
		}
		if err := xmlWellFormed(body); err != nil {
			t.Errorf("%s is not well-formed: %v", name, err)
		}
	}
}

func xmlWellFormed(s string) error {
	dec := xmlDecoder(s)
	for {
		if _, err := dec.Token(); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
	}
}

func TestWriteDOCX_APictureItCannotSizeIsItsAltText(t *testing.T) {
	b := NewBuilder("x")
	n := b.Image("image/png", []byte("not a png at all"), "broken")
	d, _ := b.Finish()
	d.Blocks = []Node{n}
	parts := docxParts(t, write(t, d, FormatDOCX, Options{}))
	if !strings.Contains(parts["word/document.xml"], "[broken]") || parts["word/media/img-1.png"] != "" {
		t.Fatal("an unreadable picture was embedded")
	}
	if err := xmlWellFormed(parts["word/_rels/document.xml.rels"]); err != nil || strings.Contains(parts["word/_rels/document.xml.rels"], "img-1") {
		t.Fatal("a relationship names a picture that is not there")
	}
}

func TestWriteDOCX_StopsAtTheOutputCap(t *testing.T) {
	b := NewBuilder("x")
	pic := b.Image("image/png", tinyPNG, "p")
	d, _ := b.Finish()
	// Incompressible text: past MaxOutput even compressed.
	var sb strings.Builder
	seed := uint32(1)
	for sb.Len() < MaxOutput*5/4 {
		seed = seed*1664525 + 1013904223
		sb.WriteByte(byte('!' + seed>>24%90))
	}
	d.Blocks = []Node{pic, {Kind: KindParagraph, Children: []Node{{Kind: KindText, Text: sb.String()}}}}
	w := &countingDiscard{}
	if err := Write(context.Background(), w, d, FormatDOCX, Options{}); err == nil || w.n > MaxOutput {
		t.Fatalf("err = %v after %d bytes", err, w.n)
	}
}

// The rendered files open in LibreOffice (RFC GR P1 verification). Skipped
// where soffice is not installed; the CI job that installs it sets
// LOOMCYCLE_REQUIRE_SOFFICE so the check cannot silently not run.
func TestDOCX_OpensInLibreOffice(t *testing.T) {
	soffice, err := exec.LookPath("soffice")
	if err != nil {
		if os.Getenv("LOOMCYCLE_REQUIRE_SOFFICE") != "" {
			t.Fatal("soffice is required here and not on PATH")
		}
		t.Skip("soffice not installed")
	}
	dir := t.TempDir()
	files := map[string][]byte{
		"fixture.docx":   write(t, fixtureDoc(t), FormatDOCX, Options{TOC: true}),
		"titlepage.docx": write(t, fixtureDoc(t), FormatDOCX, Options{TitlePage: true, PageSize: "letter"}),
		"embed.docx":     write(t, embedDoc(t), FormatDOCX, Options{}),
		"styled.docx":    write(t, styledFixture(t), FormatDOCX, Options{}),
		"diagrams.docx":  write(t, drawnDoc(t), FormatDOCX, Options{}),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		cmd := exec.CommandContext(ctx, soffice, "-env:UserInstallation=file://"+filepath.ToSlash(filepath.Join(dir, "profile")),
			"--headless", "--norestore", "--convert-to", "txt:Text (encoded):UTF8", "--outdir", dir, filepath.Join(dir, name))
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("soffice %s: %v\n%s", name, err, out)
		}
		txt, err := os.ReadFile(filepath.Join(dir, strings.TrimSuffix(name, ".docx")+".txt"))
		if err != nil {
			t.Fatalf("soffice did not convert %s: %v\n%s", name, err, out)
		}
		for _, want := range []string{"Протокол засідання", "Порядок денний", "вкладений пункт", "Іван", "Last paragraph."} {
			if !strings.Contains(string(txt), want) {
				t.Errorf("%s as LibreOffice reads it lacks %q:\n%s", name, want, txt)
			}
		}
	}
}

func xmlDecoder(s string) *xml.Decoder { return xml.NewDecoder(strings.NewReader(s)) }
