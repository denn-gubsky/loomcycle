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

// A rendered ODT read back by loomcycle's own extractor gives the same
// headings, text, table cells and pictures (RFC GR P4a, D17).
func TestWriteODT_RoundTripsThroughTheExtractor(t *testing.T) {
	b := write(t, fixtureDoc(t), FormatODT, Options{TOC: true})
	sink := &mdSink{}
	rep, err := extract.ODT{}.Extract(context.Background(), bytes.NewReader(b), int64(len(b)), sink, extract.Options{Images: true})
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

func TestWriteODT_SameDocWritesSameBytes(t *testing.T) {
	a := write(t, fixtureDoc(t), FormatODT, Options{TOC: true})
	b := write(t, fixtureDoc(t), FormatODT, Options{TOC: true})
	if !bytes.Equal(a, b) {
		t.Fatal("two renders of the same document differ")
	}
}

// The package is one an office suite opens: mimetype first, stored, with no
// extra field, and a manifest naming every part.
func TestWriteODT_ContainerIsWellFormed(t *testing.T) {
	assertOCFMimetype(t, write(t, fixtureDoc(t), FormatODT, Options{}), odtMediaType)
	parts := docxParts(t, write(t, fixtureDoc(t), FormatODT, Options{}))
	man := parts["META-INF/manifest.xml"]
	for name := range parts {
		if name == "mimetype" || name == "META-INF/manifest.xml" {
			continue
		}
		if !strings.Contains(man, `manifest:full-path="`+name+`"`) {
			t.Errorf("the manifest does not name %s", name)
		}
	}
}

// odtElements are the elements the ODT writer writes. Text from the Doc can
// only ever be character data among them.
var odtElements = map[string]bool{
	"document-content": true, "document-styles": true, "document-meta": true, "manifest": true, "file-entry": true,
	"font-face-decls": true, "font-face": true, "automatic-styles": true, "styles": true, "master-styles": true,
	"style": true, "default-style": true, "text-properties": true, "paragraph-properties": true, "table-properties": true,
	"table-cell-properties": true, "graphic-properties": true, "page-layout": true, "page-layout-properties": true,
	"master-page": true, "list-style": true, "list-level-style-bullet": true, "list-level-style-number": true,
	"list-level-properties": true, "list-level-label-alignment": true, "body": true, "text": true, "meta": true,
	"generator": true, "title": true, "language": true, "creation-date": true, "date": true, "user-defined": true,
	"p": true, "h": true, "span": true, "a": true, "s": true, "tab": true, "line-break": true, "bookmark-start": true,
	"bookmark-end": true, "list": true, "list-item": true, "table": true, "table-column": true, "table-header-rows": true,
	"table-row": true, "table-cell": true, "frame": true, "image": true,
}

// Text is only ever text: no field, script, event, form or object, and no
// link but an anchor, http, https or mailto, whatever a body holds.
func TestWriteODT_BodyTextNeverBecomesMarkup(t *testing.T) {
	b := NewBuilder(`Title <text:page-number/> <office:scripts/> ]]>`)
	b.AddSection()
	body := injection + "\n\n<text:database-display/> <script:event-listener/> \x01\x02 control\x0b chars \uFFFE\n\n" +
		"[js](javascript:alert(1)) [file](file:///etc/passwd)\n\n```\n  two  spaces\tand a tab\n```"
	b.Doc.Sections = []*Section{{Anchor: "chunk-a", Title: `]]><text:p/>`, Level: 2, Blocks: b.Body(body)}}
	b.Doc.Status = `draft</meta:user-defined><evil/>`
	b.Doc.Language = `uk"><evil/>`
	d, _ := b.Finish()
	parts := docxParts(t, write(t, d, FormatODT, Options{TOC: true}))
	for name, body := range parts {
		if !strings.HasSuffix(name, ".xml") {
			continue
		}
		dec := xmlDecoder(body)
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%s is not well-formed: %v", name, err)
			}
			se, ok := tok.(xml.StartElement)
			if !ok {
				continue
			}
			if !odtElements[se.Name.Local] {
				t.Errorf("%s holds an element the writer never writes: %s", name, se.Name.Local)
			}
			for _, a := range se.Attr {
				if a.Name.Local != "href" {
					continue
				}
				v := a.Value
				if !(strings.HasPrefix(v, "#") || strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://") ||
					strings.HasPrefix(v, "mailto:") || strings.HasPrefix(v, "Pictures/img-")) {
					t.Errorf("%s links to %q", name, v)
				}
			}
		}
	}
	if !strings.Contains(parts["content.xml"], `two <text:s text:c="1"/>spaces<text:tab/>and a tab`) {
		t.Errorf("a code block's spaces and tab were not kept:\n%s", parts["content.xml"])
	}
}

func TestWriteODT_StopsAtTheOutputCap(t *testing.T) {
	d := incompressibleDoc(t)
	w := &countingDiscard{}
	if err := Write(context.Background(), w, d, FormatODT, Options{}); err == nil || w.n > MaxOutput {
		t.Fatalf("err = %v after %d bytes", err, w.n)
	}
}

// incompressibleDoc is a Doc whose text passes MaxOutput even compressed.
func incompressibleDoc(t *testing.T) *Doc {
	t.Helper()
	b := NewBuilder("x")
	pic := b.Image("image/png", tinyPNG, "p")
	d, _ := b.Finish()
	var sb strings.Builder
	seed := uint32(1)
	for sb.Len() < MaxOutput*5/4 {
		seed = seed*1664525 + 1013904223
		sb.WriteByte(byte('!' + seed>>24%90))
	}
	d.Blocks = []Node{pic, {Kind: KindParagraph, Children: []Node{{Kind: KindText, Text: sb.String()}}}}
	return d
}

// assertOCFMimetype checks a zip container's first entry is mimetype,
// stored, with no extra field, holding want (ODF 1.3 §3.3, EPUB OCF 3.3).
func assertOCFMimetype(t *testing.T, b []byte, want string) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	f := zr.File[0]
	if f.Name != "mimetype" || f.Method != zip.Store || len(f.Extra) != 0 {
		t.Fatalf("first entry %q method %d extra %d bytes", f.Name, f.Method, len(f.Extra))
	}
	// The local header too: a reader takes the type from its fixed offset.
	if !bytes.Equal(b[30:30+len("mimetype")], []byte("mimetype")) || string(b[38:38+len(want)]) != want {
		t.Fatalf("the local header does not start with mimetype and %q: %q", want, b[:38+len(want)])
	}
}

// The rendered ODT opens in LibreOffice. Skipped where soffice is not
// installed; the CI job that installs it sets LOOMCYCLE_REQUIRE_SOFFICE.
func TestODT_OpensInLibreOffice(t *testing.T) {
	soffice, err := exec.LookPath("soffice")
	if err != nil {
		if os.Getenv("LOOMCYCLE_REQUIRE_SOFFICE") != "" {
			t.Fatal("soffice is required here and not on PATH")
		}
		t.Skip("soffice not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixture.odt"), write(t, embedDoc(t), FormatODT, Options{TOC: true, TitlePage: true}), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, soffice, "-env:UserInstallation=file://"+filepath.ToSlash(filepath.Join(dir, "profile")),
		"--headless", "--norestore", "--convert-to", "txt:Text (encoded):UTF8", "--outdir", dir, filepath.Join(dir, "fixture.odt"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("soffice: %v\n%s", err, out)
	}
	txt, err := os.ReadFile(filepath.Join(dir, "fixture.txt"))
	if err != nil {
		t.Fatalf("soffice did not convert the file: %v", err)
	}
	for _, want := range []string{"Протокол засідання", "Порядок денний", "вкладений пункт", "Іван", "Last paragraph."} {
		if !strings.Contains(string(txt), want) {
			t.Errorf("the ODT as LibreOffice reads it lacks %q:\n%s", want, txt)
		}
	}
}
