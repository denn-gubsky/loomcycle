package render

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestWriteEPUB_SameDocWritesSameBytes(t *testing.T) {
	a := write(t, fixtureDoc(t), FormatEPUB, Options{TOC: true})
	b := write(t, fixtureDoc(t), FormatEPUB, Options{TOC: true})
	if !bytes.Equal(a, b) {
		t.Fatal("two renders of the same document differ")
	}
}

// The book is a well-formed EPUB 3 container (D18): mimetype first and
// stored, container.xml naming the package, a manifest that lists exactly
// the files there are, one page for the title and one per top-level
// section, and every link in the contents landing on an id of its page.
func TestWriteEPUB_ContainerIsWellFormed(t *testing.T) {
	b := write(t, fixtureDoc(t), FormatEPUB, Options{})
	assertOCFMimetype(t, b, epubMediaType)
	parts := docxParts(t, b)
	if !strings.Contains(parts["META-INF/container.xml"], `full-path="OEBPS/content.opf"`) {
		t.Fatal("container.xml does not name the package")
	}
	opf := parts["OEBPS/content.opf"]
	listed := map[string]bool{}
	for _, m := range regexp.MustCompile(`href="([^"]+)"`).FindAllStringSubmatch(opf, -1) {
		listed["OEBPS/"+m[1]] = true
		if parts["OEBPS/"+m[1]] == "" {
			t.Errorf("the manifest lists %s, which is not in the book", m[1])
		}
	}
	for name := range parts {
		if strings.HasPrefix(name, "OEBPS/") && name != "OEBPS/content.opf" && !listed[name] {
			t.Errorf("%s is in the book but not in the manifest", name)
		}
	}
	for _, want := range []string{"OEBPS/text/title.xhtml", "OEBPS/text/s001.xhtml", "OEBPS/text/s002.xhtml", "OEBPS/images/img-1.png"} {
		if _, ok := parts[want]; !ok {
			t.Errorf("the book has no %s", want)
		}
	}
	if !strings.Contains(opf, `<dc:language>uk</dc:language>`) || !strings.Contains(opf, `<meta property="dcterms:modified">2026-11-16T00:00:00Z</meta>`) {
		t.Errorf("the package metadata lacks the language or modified date:\n%s", opf)
	}
	// Every link inside the book lands on an id of the page it names.
	ids := map[string]map[string]bool{}
	for name, body := range parts {
		if strings.HasSuffix(name, ".xhtml") {
			ids[name] = idsOf(t, name, body)
		}
	}
	for name, body := range parts {
		if !strings.HasSuffix(name, ".xhtml") {
			continue
		}
		for _, m := range regexp.MustCompile(`href="([^"#:]*)#([^"]+)"`).FindAllStringSubmatch(body, -1) {
			target := name
			if m[1] != "" {
				target = path.Join(path.Dir(name), m[1])
			}
			if !ids[target][m[2]] {
				t.Errorf("%s links to %s#%s, which has no such id", name, target, m[2])
			}
		}
	}
	// A link to a part of the document in another page goes to that page.
	if !strings.Contains(parts["OEBPS/text/s001.xhtml"], `href="s002.xhtml#chunk-s2"`) {
		t.Errorf("the link to section two does not name its page:\n%s", parts["OEBPS/text/s001.xhtml"])
	}
}

func idsOf(t *testing.T, name, body string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	dec := xmlDecoder(body)
	dec.Strict = true
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("%s is not well-formed XML: %v", name, err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			for _, a := range se.Attr {
				if a.Name.Local == "id" {
					out[a.Value] = true
				}
			}
		}
	}
}

// xhtmlElements are the elements the EPUB writer writes, and xhtmlAttrs the
// attributes. Text from the Doc can only ever be character data among them.
var (
	xhtmlElements = map[string]bool{
		"html": true, "head": true, "meta": true, "title": true, "link": true, "body": true, "header": true, "nav": true,
		"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "span": true, "p": true, "section": true,
		"ul": true, "ol": true, "li": true, "a": true, "pre": true, "code": true, "figure": true, "figcaption": true,
		"blockquote": true, "hr": true, "table": true, "tr": true, "th": true, "td": true, "em": true, "strong": true,
		"del": true, "br": true, "img": true,
	}
	xhtmlAttrs = map[string]bool{
		"id": true, "class": true, "href": true, "src": true, "alt": true, "lang": true, "charset": true, "rel": true,
		"type": true, "start": true, "style": true, "xmlns": true, "epub": true,
	}
)

// Text is only ever text: no script, no event handler, no style but a
// cell's alignment, no link but inside the book or to http, https or
// mailto, and no picture from outside the book, whatever a body holds.
func TestWriteEPUB_BodyTextNeverBecomesMarkup(t *testing.T) {
	b := NewBuilder(`Title <script>alert(1)</script> ]]>`)
	b.AddSection()
	body := injection + "\n\n\x01\x02 control\x0b chars ￾\n\n[js](javascript:alert(1)) [rel](../../etc/passwd) ![x](https://evil.example/x.png)"
	b.Doc.Sections = []*Section{{Anchor: "chunk-a", Title: `<img src=x onerror=alert(1)>`, Level: 2, Blocks: b.Body(body)}}
	b.Doc.Status = `draft"><script>`
	b.Doc.Language = `uk"><script>`
	d, _ := b.Finish()
	parts := docxParts(t, write(t, d, FormatEPUB, Options{TOC: true}))
	for name, body := range parts {
		if !strings.HasSuffix(name, ".xhtml") {
			continue
		}
		dec := xmlDecoder(body)
		dec.Strict = true
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
			if !xhtmlElements[se.Name.Local] {
				t.Errorf("%s holds an element the writer never writes: %s", name, se.Name.Local)
			}
			for _, a := range se.Attr {
				if !xhtmlAttrs[a.Name.Local] && a.Name.Space != "xmlns" && !(a.Name.Space == "http://www.w3.org/XML/1998/namespace" && a.Name.Local == "lang") {
					t.Errorf("%s holds an attribute the writer never writes: %s:%s", name, a.Name.Space, a.Name.Local)
				}
				v := a.Value
				switch a.Name.Local {
				case "href":
					if !(strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "mailto:") ||
						regexp.MustCompile(`^(text/)?(title|s\d{3})\.xhtml(#[A-Za-z0-9_-]+)?$`).MatchString(v) || v == "../style.css") {
						t.Errorf("%s links to %q", name, v)
					}
				case "src":
					if !regexp.MustCompile(`^\.\./images/img-\d+\.(png|jpg|gif|webp)$`).MatchString(v) {
						t.Errorf("%s loads %q", name, v)
					}
				case "style":
					if !regexp.MustCompile(`^text-align:(left|center|right)$`).MatchString(v) {
						t.Errorf("%s styles with %q", name, v)
					}
				}
			}
		}
	}
	if opf := parts["OEBPS/content.opf"]; !strings.Contains(opf, `<dc:language>und</dc:language>`) {
		t.Errorf("a malformed language reached the package:\n%s", opf)
	}
}

func TestWriteEPUB_StopsAtTheOutputCap(t *testing.T) {
	w := &countingDiscard{}
	if err := Write(context.Background(), w, incompressibleDoc(t), FormatEPUB, Options{}); err == nil || w.n > MaxOutput {
		t.Fatalf("err = %v after %d bytes", err, w.n)
	}
}

// The book passes epubcheck, the reference EPUB validator (D18). Skipped
// where it is not installed; the CI job that installs it sets
// LOOMCYCLE_REQUIRE_EPUBCHECK so the check cannot silently not run.
func TestEPUB_PassesEpubcheck(t *testing.T) {
	epubcheck, err := exec.LookPath("epubcheck")
	if err != nil {
		if os.Getenv("LOOMCYCLE_REQUIRE_EPUBCHECK") != "" {
			t.Fatal("epubcheck is required here and not on PATH")
		}
		t.Skip("epubcheck not installed")
	}
	dir := t.TempDir()
	books := map[string][]byte{
		"fixture.epub":   write(t, fixtureDoc(t), FormatEPUB, Options{TOC: true}),
		"injection.epub": write(t, injectionDoc(t), FormatEPUB, Options{TOC: true, TitlePage: true}),
	}
	for name, b := range books {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		out, err := exec.CommandContext(ctx, epubcheck, p).CombinedOutput()
		cancel()
		if err != nil {
			t.Errorf("epubcheck %s: %v\n%s", name, err, out)
		}
	}
}
