package render

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"archive/zip"

	"golang.org/x/net/html"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// tinyPNG is a 1x1 PNG.
var tinyPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

// fixtureDoc is a document with every block kind, Cyrillic and Latin text,
// internal and external links, and pictures.
func fixtureDoc(t *testing.T) *Doc {
	t.Helper()
	b := NewBuilder("Протокол засідання — Board minutes")
	b.Links = func(dest string) (string, string, bool) {
		if dest == "loom:chunk/s2" {
			return "", "chunk-s2", true
		}
		return ExternalOnly(dest)
	}
	b.Doc.Anchor = "chunk-root"
	b.Doc.Status = "draft"
	b.Doc.Provenance = "From minutes.docx (SharePoint)"
	b.Doc.Date = "2026-11-16"
	b.Doc.Language = "uk"
	b.Doc.Blocks = b.Body("Вступ: *курсив*, **жирний**, ~~закреслений~~ and `code span`.\nA soft break, and a hard  \nbreak.")
	s1 := &Section{ID: "s1", Anchor: "chunk-s1", Title: "Порядок денний", Level: 2}
	b.AddSection()
	s1.Blocks = b.Body(`1. Перше питання
2. Друге питання
   - вкладений пункт
   - ще один

- [x] done
- [ ] open

> Цитата з **виділенням**.

| Name | Votes | Note |
|:-----|------:|:----:|
| Іван | 3 | a \| pipe |
| Ann | 10 | ok |

See [section two](loom:chunk/s2), [the site](https://example.com/a?b=1&c=2), and [mail](mailto:board@example.com).

---

` + "```go\nfunc main() { fmt.Println(\"<b>\") }\n```" + `
`)
	b.AddSection()
	s2 := &Section{ID: "s2", Anchor: "chunk-s2", Title: "Рішення", Level: 2}
	s2.Blocks = append(s2.Blocks, b.Image("image/png", tinyPNG, "Схема [1]"))
	s2.Blocks = append(s2.Blocks, b.Diagram("mermaid", "graph TD; A-->B"))
	s2.Blocks = append(s2.Blocks, b.Body("#### Підрозділ у тексті\n\nТекст.")...)
	b.AddSection()
	s21 := &Section{ID: "s21", Anchor: "chunk-s21", Title: "Деталі", Level: 3}
	s21.Blocks = b.Body("Last paragraph.")
	s2.Children = []*Section{s21}
	b.Doc.Sections = []*Section{s1, s2}
	d, _ := b.Finish()
	return d
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("golden %s: %v (run with -update)", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file (run with -update to accept):\n%s", name, got)
	}
}

func write(t *testing.T, d *Doc, format string, opts Options) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, d, format, opts); err != nil {
		t.Fatalf("write %s: %v", format, err)
	}
	return buf.Bytes()
}

func TestWriteMarkdown_MatchesGolden(t *testing.T) {
	golden(t, "fixture.md", write(t, fixtureDoc(t), FormatMarkdown, Options{TOC: true, AssetDir: "fixture"}))
}

func TestWriteHTML_MatchesGolden(t *testing.T) {
	golden(t, "fixture.html", write(t, fixtureDoc(t), FormatHTML, Options{TOC: true}))
}

func TestWrite_SameDocWritesSameBytes(t *testing.T) {
	for _, f := range []string{FormatMarkdown, FormatHTML} {
		a := write(t, fixtureDoc(t), f, Options{TOC: true})
		b := write(t, fixtureDoc(t), f, Options{TOC: true})
		if !bytes.Equal(a, b) {
			t.Errorf("%s: two writes of the same document differ", f)
		}
	}
}

func TestBuilder_ReportCountsWhatWasRendered(t *testing.T) {
	b := NewBuilder("t")
	b.AddSection()
	_ = b.Body("| a |\n|---|\n| 1 |\n\n[x](javascript:alert(1)) ![y](https://example.com/y.png)")
	_ = b.Image("image/png", tinyPNG, "")
	_ = b.Image("image/svg+xml", []byte("<svg/>"), "logo")
	_, rep := b.Finish()
	if rep.Sections != 1 || rep.Tables != 1 || rep.Images != 1 || rep.ImagesSkipped != 2 || rep.LinksFlattened != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

// injection is text that would be markup, script or a renderer instruction
// if a writer passed it on.
const injection = `<script>alert(1)</script> <img src=x onerror=alert(2)> <iframe src="https://evil.example"></iframe>
[click](javascript:alert(3)) [data](data:text/html,<script>alert(4)</script>) [file](file:///etc/passwd)
<a href="javascript:alert(5)">raw link</a>

<div onclick="alert(6)">block html</div>

#import "evil.typ" $x^2$ <label> @ref {{ field }} { QUOTE "x" }`

func TestWriteHTML_BodyTextNeverBecomesMarkup(t *testing.T) {
	b := NewBuilder(`Title <script>alert("t")</script>`)
	b.Doc.Status = `draft"><script>`
	b.Doc.Provenance = `<b>prov</b>`
	b.AddSection()
	b.Doc.Sections = []*Section{{Anchor: "chunk-a", Title: `<img src=x onerror=alert(1)>`, Level: 2, Blocks: b.Body(injection)}}
	b.Doc.Blocks = []Node{b.Image("image/png", tinyPNG, `"><script>alert(7)</script>`)}
	d, _ := b.Finish()
	out := string(write(t, d, FormatHTML, Options{TOC: true}))
	assertInertHTML(t, out)
	if !strings.Contains(out, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("the script tag is not shown as text:\n%s", out)
	}
	if !strings.Contains(out, `<meta http-equiv="Content-Security-Policy" content="default-src 'none'`) {
		t.Error("no content security policy")
	}
}

func TestWriteMarkdown_BodyTextNeverBecomesMarkup(t *testing.T) {
	b := NewBuilder("# not a heading [x](javascript:y)")
	b.AddSection()
	b.Doc.Sections = []*Section{{Anchor: "chunk-a", Title: "* not a list", Level: 2, Blocks: b.Body(injection + "\n\n\\- escaped dash\n\n1\\. not a list")}}
	d, _ := b.Finish()
	out := write(t, d, FormatMarkdown, Options{})
	// Re-parse the Markdown: no raw HTML, no link outside the allowed schemes.
	rb := NewBuilder("x")
	nodes := rb.Body(string(out))
	var walk func(ns []Node)
	walk = func(ns []Node) {
		for _, n := range ns {
			if n.Kind == KindLink && n.Anchor == "" {
				if _, _, ok := ExternalOnly(n.Href); !ok {
					t.Errorf("link to %q survived", n.Href)
				}
			}
			walk(n.Children)
		}
	}
	walk(nodes)
	if rb.Report.LinksFlattened != 0 {
		t.Errorf("the output holds %d links a reader would follow somewhere unsafe:\n%s", rb.Report.LinksFlattened, out)
	}
	assertInertHTML(t, string(write(t, &Doc{Title: "x", Blocks: nodes}, FormatHTML, Options{})))
	if strings.Contains(string(out), "\n# not") || strings.HasPrefix(string(out), "# # ") {
		t.Errorf("a title became a second heading:\n%s", out)
	}
}

func TestMarkdown_RoundTripKeepsTheText(t *testing.T) {
	d := fixtureDoc(t)
	out := write(t, d, FormatMarkdown, Options{})
	again := NewBuilder("x").Body(string(out))
	h := string(write(t, &Doc{Title: "x", Blocks: again}, FormatHTML, Options{}))
	for _, want := range []string{"Перше питання", "вкладений пункт", "a | pipe", "Цитата з <strong>виділенням</strong>", "fmt.Println(&#34;&lt;b&gt;&#34;)", "graph TD; A--&gt;B"} {
		if !strings.Contains(h, want) {
			t.Errorf("round trip lost %q:\n%s", want, out)
		}
	}
}

func TestBundle_RoundTripsAndIsDeterministic(t *testing.T) {
	d := fixtureDoc(t)
	opts := Options{TOC: true, PageSize: "a4"}
	b1, err := EncodeBundle(d, opts)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := EncodeBundle(fixtureDoc(t), opts)
	if !bytes.Equal(b1, b2) {
		t.Fatal("the same document made two different bundles")
	}
	got, gotOpts, err := DecodeBundle(bytes.NewReader(b1), int64(len(b1)))
	if err != nil {
		t.Fatal(err)
	}
	if gotOpts.TOC != true || len(got.Images) != 1 || !bytes.Equal(got.Images[0].Data, tinyPNG) {
		t.Fatalf("decoded %+v %+v", gotOpts, got.Images)
	}
	var direct, viaBundle bytes.Buffer
	_ = Write(context.Background(), &direct, d, FormatHTML, opts)
	if err := RenderBundle(context.Background(), bytes.NewReader(b1), int64(len(b1)), FormatHTML, &viaBundle); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(direct.Bytes(), viaBundle.Bytes()) {
		t.Fatal("rendering the bundle differs from rendering the document")
	}
}

func zipOf(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range entries {
		w, _ := zw.Create(name)
		_, _ = w.Write(data)
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestDecodeBundle_RefusesWhatABuilderNeverMakes(t *testing.T) {
	cases := map[string]map[string][]byte{
		"unknown node kind": {"doc.json": []byte(`{"version":1,"doc":{"title":"x","blocks":[{"k":"script","t":"x"}]}}`)},
		"bad anchor":        {"doc.json": []byte(`{"version":1,"doc":{"title":"x","sections":[{"anchor":"\" onload=x","title":"s","level":2}]}}`)},
		"bad link anchor":   {"doc.json": []byte(`{"version":1,"doc":{"title":"x","blocks":[{"k":"a","anchor":"x\"><script>"}]}}`)},
		"other entry":       {"doc.json": []byte(`{"version":1,"doc":{"title":"x"}}`), "../evil": []byte("x")},
		"bad image name":    {"doc.json": []byte(`{"version":1,"doc":{"title":"x"}}`), "images/../../x.png": []byte("x")},
		"missing image":     {"doc.json": []byte(`{"version":1,"doc":{"title":"x","images":[{"name":"img-1.png","media_type":"image/png"}]}}`)},
		"svg image":         {"doc.json": []byte(`{"version":1,"doc":{"title":"x","images":[{"name":"img-1.png","media_type":"image/svg+xml"}]}}`), "images/img-1.png": []byte("<svg/>")},
		"wrong version":     {"doc.json": []byte(`{"version":9,"doc":{"title":"x"}}`)},
		"no document":       {"images/img-1.png": tinyPNG},
	}
	for name, entries := range cases {
		b := zipOf(t, entries)
		if _, _, err := DecodeBundle(bytes.NewReader(b), int64(len(b))); !errors.Is(err, ErrBadBundle) {
			t.Errorf("%s: err = %v, want ErrBadBundle", name, err)
		}
	}
	if _, _, err := DecodeBundle(bytes.NewReader([]byte("not a zip")), 9); !errors.Is(err, ErrBadBundle) {
		t.Errorf("not a zip: err = %v", err)
	}
}

func TestDecodeBundle_RefusesNestingPastTheCap(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < maxDepth+10; i++ {
		sb.WriteString(`{"k":"quote","c":[`)
	}
	sb.WriteString(`{"k":"p"}`)
	for i := 0; i < maxDepth+10; i++ {
		sb.WriteString(`]}`)
	}
	b := zipOf(t, map[string][]byte{"doc.json": []byte(`{"version":1,"doc":{"title":"x","blocks":[` + sb.String() + `]}}`)})
	if _, _, err := DecodeBundle(bytes.NewReader(b), int64(len(b))); !errors.Is(err, ErrBadBundle) {
		t.Fatalf("err = %v, want ErrBadBundle", err)
	}
}

func TestBuilder_DeepNestingIsCutNotRecursedThrough(t *testing.T) {
	body := strings.Repeat("> ", 5000) + "deep"
	b := NewBuilder("x")
	nodes := b.Body(body)
	d := &Doc{Title: "x", Blocks: nodes}
	for _, f := range []string{FormatMarkdown, FormatHTML} {
		out := write(t, d, f, Options{})
		if !strings.Contains(string(out), "deep") {
			t.Errorf("%s lost the text below the cut", f)
		}
	}
	enc, err := EncodeBundle(d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeBundle(bytes.NewReader(enc), int64(len(enc))); err != nil {
		t.Fatalf("a builder's own deep document is refused: %v", err)
	}
}

func TestBuilder_BodiesPastTheCapAreCut(t *testing.T) {
	b := NewBuilder("x")
	big := strings.Repeat("a", MaxBodyBytes/2+1)
	b.Body(big)
	if n := b.Body(big); n != nil {
		t.Fatal("a body past the cap was kept")
	}
	b.Body("small")
	_, rep := b.Finish()
	if rep.BodiesCut != 2 || len(rep.Notes) != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestBuilder_ImagesPastTheCapsAreSkipped(t *testing.T) {
	b := NewBuilder("x")
	n := b.Image("image/png", make([]byte, MaxImage+1), "big")
	if n.Kind == KindImage || b.Report.ImagesSkipped != 1 {
		t.Fatalf("an oversized picture was kept: %+v", n)
	}
	huge := "![h](data:image/png;base64," + strings.Repeat("A", (MaxImage/3+10)*4) + ")"
	if nodes := b.Body(huge); len(nodes) != 1 || nodes[0].Kind == KindImage {
		t.Fatalf("an oversized data URL was kept: %+v", nodes)
	}
}

func TestWrite_UnknownFormatIsRefused(t *testing.T) {
	if err := Write(context.Background(), &bytes.Buffer{}, &Doc{}, "rtf", Options{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

func TestCapWriter_StopsAtTheOutputCap(t *testing.T) {
	c := &capWriter{w: &bytes.Buffer{}}
	c.n = MaxOutput - 2
	c.WriteString("abc")
	if !errors.Is(c.err, ErrTooLarge) {
		t.Fatalf("err = %v", c.err)
	}
}

// assertInertHTML parses a page and fails on anything that would run or
// fetch: a script, an event handler, a frame, a form, a link or picture
// outside the allowed schemes. Text that merely spells these out is fine.
func assertInertHTML(t *testing.T, page string) {
	t.Helper()
	root, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"html": true, "head": true, "meta": true, "title": true, "style": true, "body": true,
		"article": true, "header": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "span": true,
		"p": true, "nav": true, "ul": true, "ol": true, "li": true, "a": true, "section": true, "pre": true, "code": true,
		"blockquote": true, "table": true, "tbody": true, "tr": true, "th": true, "td": true, "img": true, "hr": true,
		"br": true, "em": true, "strong": true, "del": true, "figure": true, "figcaption": true}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if !allowed[n.Data] {
				t.Errorf("element <%s> in the output:\n%s", n.Data, page)
			}
			for _, a := range n.Attr {
				k, v := strings.ToLower(a.Key), strings.ToLower(strings.TrimSpace(a.Val))
				switch {
				case strings.HasPrefix(k, "on"):
					t.Errorf("event handler %s=%q in the output", a.Key, a.Val)
				case k == "href" && !strings.HasPrefix(v, "#") && !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") && !strings.HasPrefix(v, "mailto:"):
					t.Errorf("link to %q in the output", a.Val)
				case k == "src" && !strings.HasPrefix(v, "data:image/"):
					t.Errorf("picture from %q in the output", a.Val)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
}

func TestWrite_StopsWhenTheContextEnds(t *testing.T) {
	d := fixtureDoc(t)
	for _, f := range []string{FormatMarkdown, FormatHTML} {
		ctx, cancel := context.WithCancel(context.Background())
		w := &cancelAfter{n: 3, cancel: cancel}
		if err := Write(ctx, w, d, f, Options{}); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: err = %v, want context.Canceled", f, err)
		}
		cancel()
	}
}

// cancelAfter cancels its context after n writes.
type cancelAfter struct {
	n      int
	cancel func()
}

func (c *cancelAfter) Write(p []byte) (int, error) {
	if c.n--; c.n <= 0 {
		c.cancel()
	}
	return len(p), nil
}

// One picture a body names many times — a reference definition used over
// and over — is decoded and stored once, not once per use.
func TestBuilder_ARepeatedPictureIsStoredOnce(t *testing.T) {
	pic := base64.StdEncoding.EncodeToString(tinyPNG)
	body := strings.Repeat("![a][r] ", 5000) + "\n\n[r]: data:image/png;base64," + pic + "\n"
	b := NewBuilder("x")
	nodes := b.Body(body)
	if len(b.Doc.Images) != 1 || b.Report.Images != 1 {
		t.Fatalf("a picture used 5000 times was stored %d times", len(b.Doc.Images))
	}
	uses := 0
	var count func(ns []Node)
	count = func(ns []Node) {
		for _, n := range ns {
			if n.Kind == KindImage {
				uses++
			}
			count(n.Children)
		}
	}
	count(nodes)
	if uses != 5000 {
		t.Fatalf("%d of the 5000 uses are kept", uses)
	}
}

func TestBuilder_PicturesPastTheTotalCapAreSkipped(t *testing.T) {
	b := NewBuilder("x")
	one := make([]byte, MaxImage)
	one[0] = 1
	kept := 0
	for i := 0; i < MaxImageBytes/MaxImage+3; i++ {
		if b.Image("image/png", one, "p").Kind == KindImage {
			kept++
		}
	}
	if kept != MaxImageBytes/MaxImage || b.Report.ImagesSkipped != 3 {
		t.Fatalf("kept %d pictures of %d MiB (skipped %d)", kept, MaxImage>>20, b.Report.ImagesSkipped)
	}
}

// countingDiscard counts what it is sent and keeps none of it.
type countingDiscard struct{ n int }

func (c *countingDiscard) Write(p []byte) (int, error) { c.n += len(p); return len(p), nil }

// A paragraph that names one large picture many times stops at the output
// cap while it is written, never assembling the paragraph first.
func TestWriteHTML_ARepeatedPictureStopsAtTheOutputCap(t *testing.T) {
	b := NewBuilder("x")
	pic := b.Image("image/png", make([]byte, 4<<20), "p")
	uses := make([]Node, 5000)
	for i := range uses {
		uses[i] = pic
	}
	d, _ := b.Finish()
	d.Blocks = []Node{{Kind: KindParagraph, Children: uses}}
	w := &countingDiscard{}
	if err := Write(context.Background(), w, d, FormatHTML, Options{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if w.n > MaxOutput {
		t.Fatalf("%d bytes written past the %d cap", w.n, MaxOutput)
	}
}

// A bare <br> is the line break an extraction writes between a table cell's
// paragraphs (GFM cells are one line), so it is the one raw tag read as
// markup: a break in every format. Any other raw HTML, a <br> with
// attributes included, stays text (G5).
func TestBuilder_BareBrInACellIsALineBreak(t *testing.T) {
	b := NewBuilder("Services")
	b.Doc.Blocks = b.Body("| Basic | Other |\n|---|---|\n| No test plan<br>Few runs | x<BR/>y<br />z |\n| a<br class=k>b | <b>bold</b> |\n")
	d, _ := b.Finish()

	h := string(write(t, d, FormatHTML, Options{}))
	for _, want := range []string{"No test plan<br>\nFew runs", "x<br>\ny<br>\nz", "a&lt;br class=k&gt;b", "&lt;b&gt;bold&lt;/b&gt;"} {
		if !strings.Contains(h, want) {
			t.Errorf("html lacks %q:\n%s", want, h)
		}
	}
	if src := typSource(t, d, Options{}); strings.Count(src, "#linebreak()") != 3 {
		t.Errorf("typst has %d line breaks, want 3:\n%s", strings.Count(src, "#linebreak()"), src)
	}
	md := string(write(t, d, FormatMarkdown, Options{}))
	if !strings.Contains(md, "| No test plan<br>Few runs | x<br>y<br>z |") {
		t.Errorf("markdown cell lost its breaks or split the row:\n%s", md)
	}
	// The Markdown reads back as the same table, breaks and all.
	again := string(write(t, &Doc{Title: "x", Blocks: NewBuilder("x").Body(md)}, FormatHTML, Options{}))
	if !strings.Contains(again, "No test plan<br>\nFew runs") {
		t.Errorf("markdown round trip lost the break:\n%s", again)
	}
	// Typst accepts a break inside a table cell (CI's render-office job
	// requires typst; elsewhere this part is skipped).
	if typst, err := exec.LookPath("typst"); err == nil {
		txt := pdfText(t, compileTypst(t, typst, d, Options{}))
		for _, want := range []string{"No test plan", "Few runs"} {
			if !strings.Contains(txt, want) {
				t.Errorf("the PDF lacks %q:\n%s", want, txt)
			}
		}
	}
}
