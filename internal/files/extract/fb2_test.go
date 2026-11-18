package extract

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

// fb2Text is a book with everything the reader handles: metadata in
// title-info and publish-info (and a document-info that is not read), a
// cover, a body title, an epigraph, nested sections, a poem, a table, a
// picture referred to before its binary, two notes referred to and one not,
// and the binaries at the end, one with its base64 in indented lines.
func fb2Text(t testing.TB, enc string) string {
	b64 := base64.StdEncoding.EncodeToString(tinyPNG(t))
	var lines strings.Builder
	for i := 0; i < len(b64); i += 20 {
		lines.WriteString("\n    " + b64[i:min(i+20, len(b64))])
	}
	return `<?xml version="1.0" encoding="` + enc + `"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0" xmlns:l="http://www.w3.org/1999/xlink">
<description><title-info><genre>poetry</genre><genre>prose_classic</genre>
<author><first-name>Тарас</first-name><middle-name>Григорович</middle-name><last-name>Шевченко</last-name></author>
<author><nickname>Кобзар</nickname></author>
<book-title>Кобзар</book-title>
<annotation><p>Перша <emphasis>збірка</emphasis>.</p><p>Друга.</p></annotation>
<keywords>вірші, класика</keywords>
<date value="1840-04-18">1840</date>
<coverpage><image l:href="#cover.png"/></coverpage>
<lang>uk</lang>
<translator><first-name>Vera</first-name><last-name>Rich</last-name></translator>
<sequence name="Класика" number="3"/>
</title-info>
<document-info><author><nickname>maker</nickname></author><id>x</id></document-info>
<publish-info><publisher>Видавництво</publisher><year>1999</year><isbn>978-0-00-000000-2</isbn></publish-info>
</description>
<body><title><p>Тарас Шевченко</p><p>Кобзар</p></title>
<epigraph><p>Думи мої, думи мої</p><text-author>Т. Ш.</text-author></epigraph>
<section><title><p>Перший розділ</p></title>
<p>Text with a note<a l:href="#n1" type="note">1</a> and # not a heading.</p>
<p>Some <strong>bold</strong> and <emphasis>italic</emphasis> &lt;b&gt; words.</p>
<empty-line/>
<subtitle>Interlude</subtitle>
<poem><title><p>Вірш</p></title><stanza><v>Перший рядок</v><v>- Другий рядок</v></stanza><text-author>Автор</text-author></poem>
<image l:href="#pic.png"/>
<section><title><p>Підрозділ</p></title><cite><p>A quote.</p><text-author>Somebody</text-author></cite>
<table><tr><th>Name</th><th>Value</th></tr><tr><td>a|b</td><td>1</td></tr></table>
</section>
</section>
<section><title><p>Другий</p></title><p>Second<a l:href="#n2" type="note">2</a>.</p></section>
</body>
<body name="notes"><title><p>Примітки</p></title>
<section id="n1"><title><p>1</p></title><p>The note text.</p></section>
<section id="n2"><title><p>2</p></title><p>Second</p><p>note.</p></section>
<section id="n3"><title><p>3</p></title><p>Never referred to.</p></section>
</body>
<binary id="cover.png" content-type="image/png">` + b64 + `</binary>
<binary id="pic.png" content-type="image/png">` + lines.String() + `
</binary>
</FictionBook>`
}

func fb2Encoded(t testing.TB, label string, e encoding.Encoding) []byte {
	t.Helper()
	out, err := e.NewEncoder().Bytes([]byte(fb2Text(t, label)))
	if err != nil {
		t.Fatalf("encode as %s: %v", label, err)
	}
	return out
}

const fb2Want = `# Кобзар

[picture 1]

Тарас Шевченко Кобзар

> Думи мої, думи мої

> — Т. Ш.

## Перший розділ

Text with a note[1] and # not a heading.

Some bold and italic \<b> words.

**Interlude**

**Вірш**

Перший рядок\
\- Другий рядок

— Автор

[picture 2]

### Підрозділ

> A quote.

> — Somebody

| Name | Value |
| --- | --- |
| a\|b | 1 |

## Другий

Second[2].

## Pictures

![Picture 1: Cover](volume:inbox:extracted/doc/img-1.png)

![Picture 2](volume:inbox:extracted/doc/img-2.png)

## Notes

[1] The note text.

[2] Second note.`

var fb2WantMeta = &BookMetadata{
	Title:        "Кобзар",
	Authors:      []string{"Тарас Григорович Шевченко", "Кобзар"},
	Translators:  []string{"Vera Rich"},
	Language:     "uk",
	Date:         "1840-04-18",
	Year:         1840,
	Publisher:    "Видавництво",
	Identifiers:  []BookIdentifier{{Scheme: "isbn", Value: "978-0-00-000000-2"}},
	Series:       "Класика",
	SeriesIndex:  "3",
	Subjects:     []string{"poetry", "prose_classic", "вірші", "класика"},
	Description:  "Перша збірка. Друга.",
	SourceFormat: "fb2",
}

func checkFB2Book(t *testing.T, data []byte, name string) {
	t.Helper()
	s, rep := run(t, FB2{}, data, Options{Name: name, Images: true})
	if got := s.md.String(); got != fb2Want {
		t.Errorf("markdown:\n%s\n--- want ---\n%s", got, fb2Want)
	}
	if !reflect.DeepEqual(rep.Metadata, fb2WantMeta) {
		t.Errorf("metadata:\n%+v\nwant\n%+v", rep.Metadata, fb2WantMeta)
	}
	if rep.Format != "fb2" || rep.Images != 2 || rep.Tables != 1 || len(s.images) != 2 {
		t.Errorf("report %+v, %d images", rep, len(s.images))
	}
	for _, img := range s.images {
		if img.MediaType != "image/png" || !bytes.Equal(img.Data, tinyPNG(t)) {
			t.Errorf("picture %s: %s, %d bytes", img.Name, img.MediaType, len(img.Data))
		}
	}
}

func TestFB2_BookReadsAsMarkdownAndMetadata(t *testing.T) {
	checkFB2Book(t, []byte(fb2Text(t, "UTF-8")), "book.fb2")
}

func TestFB2_Windows1251IsRead(t *testing.T) {
	data := fb2Encoded(t, "windows-1251", charmap.Windows1251)
	if bytes.Contains(data, []byte("Кобзар")) {
		t.Fatal("the fixture is still UTF-8")
	}
	checkFB2Book(t, data, "book.fb2")
}

func TestFB2_EncodingsOfTheListAreRead(t *testing.T) {
	for label, e := range map[string]encoding.Encoding{
		"koi8-u":   charmap.KOI8U,
		"UTF-16":   unicode.UTF16(unicode.LittleEndian, unicode.UseBOM),
		"utf-16be": unicode.UTF16(unicode.BigEndian, unicode.UseBOM),
		"utf-8":    unicode.UTF8BOM,
	} {
		t.Run(label, func(t *testing.T) { checkFB2Book(t, fb2Encoded(t, label, e), "book.fb2") })
	}
}

func TestFB2_UnknownEncodingIsRefusedByName(t *testing.T) {
	for _, label := range []string{"Shift_JIS", "x-mac-cyrillic"} {
		book := strings.Replace(fb2Text(t, label), "Кобзар", "Kobzar", -1)
		_, err := extractBytes(FB2{}, "b.fb2", []byte(book))
		if !IsRefused(err) || !strings.Contains(err.Error(), label) {
			t.Errorf("%s: err = %v, want a refusal naming it", label, err)
		}
	}
	// UTF-16 declared without the byte order mark that says which.
	_, err := extractBytes(FB2{}, "b.fb2", []byte(`<?xml version="1.0" encoding="UTF-16"?><FictionBook/>`))
	if !IsRefused(err) {
		t.Errorf("UTF-16 with no byte order mark: err = %v", err)
	}
}

func TestFB2_DTDAndEntitiesAreRefused(t *testing.T) {
	dtd := `<?xml version="1.0"?><!DOCTYPE FictionBook [<!ENTITY x "` + strings.Repeat("y", 100) + `">]><FictionBook><body><p>&x;</p></body></FictionBook>`
	if _, err := extractBytes(FB2{}, "b.fb2", []byte(dtd)); !IsRefused(err) || !strings.Contains(err.Error(), "DTD") {
		t.Errorf("a DTD: err = %v, want a refusal", err)
	}
	s := &memSink{}
	ent := `<?xml version="1.0"?><FictionBook><body><p>a&nbsp;b &x;</p></body></FictionBook>`
	var f FB2
	if _, err := f.Extract(context.Background(), strings.NewReader(ent), int64(len(ent)), s, Options{}); err == nil {
		t.Errorf("an undeclared entity was read: %q", s.md.String())
	}
}

func TestFB2_DeepNestingIsRefused(t *testing.T) {
	book := `<FictionBook><body>` + strings.Repeat("<section>", maxXMLDepth+10) + `x` + strings.Repeat("</section>", maxXMLDepth+10) + `</body></FictionBook>`
	if _, err := extractBytes(FB2{}, "b.fb2", []byte(book)); !IsRefused(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	// Headings stop at six levels below a refusal's depth.
	ok := `<FictionBook><body>` + strings.Repeat("<section><title><p>T</p></title>", 9) + strings.Repeat("</section>", 9) + `</body></FictionBook>`
	s, _ := run(t, FB2{}, []byte(ok), Options{Name: "b.fb2"})
	if !strings.Contains(s.md.String(), "\n\n###### T") || strings.Contains(s.md.String(), "####### ") {
		t.Errorf("deep headings:\n%s", s.md.String())
	}
}

func TestFB2_RootMustBeFictionBook(t *testing.T) {
	if _, err := extractBytes(FB2{}, "b.fb2", []byte(`<html><body><p>x</p></body></html>`)); !IsRefused(err) {
		t.Errorf("err = %v, want a refusal", err)
	}
}

func fb2WithPicture(n int) []byte {
	pic := make([]byte, n)
	_, _ = rand.Read(pic)
	return []byte(`<FictionBook xmlns:l="http://www.w3.org/1999/xlink"><body><p>Before.</p><image l:href="#big"/></body><binary id="big" content-type="image/png">` +
		base64.StdEncoding.EncodeToString(pic) + `</binary></FictionBook>`)
}

// An oversized picture is refused from its encoded length: the 3 MiB one
// is refused while its base64 streams in, before the decoder has buffered
// it, and the one just over the cap from the length of what was buffered,
// before any of it is decoded.
func TestFB2_Base64PictureOverTheCapIsRefusedBeforeDecoding(t *testing.T) {
	const limit = 1024
	big := fb2WithPicture(3 << 20)
	var err error
	got := allocated(func() {
		_, err = FB2{}.Extract(context.Background(), bytes.NewReader(big), int64(len(big)), &memSink{}, Options{Images: true, fb2MaxPicture: limit})
	})
	if !IsRefused(err) || !strings.Contains(err.Error(), "picture") {
		t.Fatalf("a 3 MiB picture: err = %v, want a refusal", err)
	}
	if got > ceiling(1<<20) {
		t.Errorf("refusing a 3 MiB picture allocated %d KiB: its base64 was read in", got>>10)
	}
	just := fb2WithPicture(limit + 3)
	_, err = FB2{}.Extract(context.Background(), bytes.NewReader(just), int64(len(just)), &memSink{}, Options{Images: true, fb2MaxPicture: limit})
	if !IsRefused(err) {
		t.Errorf("a picture just over the cap: err = %v, want a refusal", err)
	}
	at := fb2WithPicture(limit)
	// At the cap it is decoded; random bytes are no image type, so it is
	// placed as its alt text and counted as left out.
	s, rep := run(t, FB2{}, at, Options{Images: true, fb2MaxPicture: limit})
	if !strings.HasSuffix(s.md.String(), "## Pictures\n\n[image: Picture 1]") || len(rep.Notes) != 1 {
		t.Errorf("a picture at the cap: %q, notes %v", s.md.String(), rep.Notes)
	}
}

// countingReaderAt counts the bytes read out of a file.
type countingReaderAt struct {
	r io.ReaderAt
	n atomic.Int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n.Add(int64(n))
	return n, err
}

// A book cut at its output limit stops reading there: the rest of the
// file, its notes and its pictures, is never read.
func TestFB2_MaxCharsStopsReading(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<FictionBook xmlns:l="http://www.w3.org/1999/xlink"><body><section><title><p>Chapter</p></title>`)
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&b, "<p>Paragraph %d of a long book, with a note<a l:href=\"#n%d\" type=\"note\">%d</a>.</p>\n", i, i, i)
	}
	b.WriteString(`</section></body><body name="notes">`)
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&b, "<section id=\"n%d\"><p>Note %d.</p></section>", i, i)
	}
	b.WriteString(`</body><binary id="x" content-type="image/png">` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 1<<20)) + `</binary></FictionBook>`)
	data := []byte(b.String())
	r := &countingReaderAt{r: bytes.NewReader(data)}
	s := &memSink{limit: 4 << 10}
	rep, err := FB2{}.Extract(context.Background(), r, int64(len(data)), s, Options{Name: "long.fb2", Images: true})
	if err != ErrSinkFull {
		t.Fatalf("err = %v, want ErrSinkFull", err)
	}
	if got := r.n.Load(); got > 64<<10 {
		t.Errorf("read %d of the file's %d bytes after a cut at 4 KiB", got, len(data))
	}
	if strings.Contains(s.md.String(), "## Notes") || rep.Chars == 0 {
		t.Errorf("chars %d, markdown:\n%s", rep.Chars, s.md.String())
	}
}

func TestFB2_ZipOfOneBookIsRead(t *testing.T) {
	z := zipOf(t, entry{"Кобзар.fb2", []byte(fb2Text(t, "UTF-8"))})
	checkFB2Book(t, z, "book.fb2.zip")
	var f FB2
	if !f.Match("", "Book.FB2.zip") || !f.Match(fb2ZipMediaType, "") || f.Match("application/zip", "a.zip") {
		t.Error("FB2 matches .fb2.zip by name and type, and no other zip")
	}
}

func TestFB2_ZipWithTwoEntriesIsRefused(t *testing.T) {
	for name, z := range map[string][]byte{
		"two books":         zipOf(t, entry{"a.fb2", []byte(fb2Text(t, "UTF-8"))}, entry{"b.fb2", []byte(fb2Text(t, "UTF-8"))}),
		"a book and a note": zipOf(t, entry{"a.fb2", []byte(fb2Text(t, "UTF-8"))}, entry{"readme.txt", []byte("x")}),
	} {
		_, err := extractBytes(FB2{}, "a.fb2.zip", z)
		if !IsRefused(err) || !strings.Contains(err.Error(), "an archive of 2 files; unpack it first") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	_, err := extractBytes(FB2{}, "a.fb2.zip", zipOf(t, entry{"a.txt", []byte("x")}))
	if !IsRefused(err) {
		t.Errorf("a zip of one text file: err = %v", err)
	}
}

func TestFB2_ZipEntryOverTheBookCapIsRefused(t *testing.T) {
	// The entry's own bound (a test-sized archive budget here) holds as it
	// streams.
	z := zipOf(t, entry{"a.fb2", []byte(`<FictionBook><body><p>` + strings.Repeat("x", 4<<20) + `</p></body></FictionBook>`)})
	_, err := FB2{}.Extract(context.Background(), bytes.NewReader(z), int64(len(z)), &memSink{},
		Options{limits: &zipLimits{entries: 10, expansion: 8, maxTotal: 1 << 20, floor: 1 << 20, ratio: 1000, ratioFree: 1 << 20}})
	if !IsRefused(err) {
		t.Errorf("err = %v, want a refusal", err)
	}
}

func TestFB2_PictureReadBeforeItsReferenceIsPlacedThere(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(tinyPNG(t))
	book := `<FictionBook xmlns:l="http://www.w3.org/1999/xlink"><binary id="p" content-type="image/png">` + b64 + `</binary>` +
		`<body><p>Before <image l:href="#p" alt="Fig"/> after.</p><image l:href="#p"/><image l:href="http://example.com/x.png"/></body></FictionBook>`
	s, rep := run(t, FB2{}, []byte(book), Options{Name: "early.fb2", Images: true})
	want := "# early\n\nBefore\n\n![Fig](volume:inbox:extracted/doc/img-1.png)\n\nafter.\n\n![Fig](volume:inbox:extracted/doc/img-1.png)\n\n[image]"
	if got := s.md.String(); got != want || rep.Images != 1 {
		t.Errorf("got %d images:\n%s\n--- want ---\n%s", rep.Images, got, want)
	}
}

func TestFB2_ImagesOffWritesAltTextAndDecodesNothing(t *testing.T) {
	s, rep := run(t, FB2{}, []byte(fb2Text(t, "UTF-8")), Options{Name: "b.fb2"})
	md := s.md.String()
	if len(s.images) != 0 || rep.Images != 0 || strings.Contains(md, "Pictures") || !strings.Contains(md, "\n\n[image: Cover]\n\n") {
		t.Errorf("images off:\n%s", md)
	}
}

func TestFB2_NotesBeforeTheTextAreKept(t *testing.T) {
	book := `<FictionBook xmlns:l="http://www.w3.org/1999/xlink"><body name="notes"><section id="a"><p>Early note.</p></section><section id="b"><p>Unused.</p></section></body>` +
		`<body><p>See<a l:href="#a" type="note">*</a>.</p></body><body><p>A second body.</p></body></FictionBook>`
	s, rep := run(t, FB2{}, []byte(book), Options{Name: "n.fb2"})
	want := "# n\n\nSee[1].\n\n## Notes\n\n[1] Early note."
	if got := s.md.String(); got != want {
		t.Errorf("got:\n%s\n--- want ---\n%s", got, want)
	}
	if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "bodies") {
		t.Errorf("notes %v", rep.Notes)
	}
}

// A legacy encoding decodes to more bytes than the file holds (Cyrillic in
// windows-1251 is one byte, in UTF-8 two): what it decodes to is counted
// and refused past its cap, as UTF-16's is.
func TestFB2_DecodedTextOverTheCapIsRefused(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="windows-1251"?><FictionBook><body>`)
	for b.Len() < 48<<10 {
		b.WriteString("<p>Довгий рядок тексту книги.</p>")
	}
	b.WriteString(`</body></FictionBook>`)
	raw, err := charmap.Windows1251.NewEncoder().Bytes([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Name: "b.fb2", fb2MaxDecoded: int64(len(raw)) + 8<<10}
	_, err = FB2{}.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)), &memSink{}, opts)
	if !IsRefused(err) || !strings.Contains(err.Error(), "decodes to more than") {
		t.Errorf("windows-1251: err = %v, want a refusal", err)
	}
	u16, err := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder().Bytes([]byte(strings.Replace(b.String(), "windows-1251", "UTF-16", 1)))
	if err != nil {
		t.Fatal(err)
	}
	opts.fb2MaxDecoded = int64(len(u16)) / 2
	_, err = FB2{}.Extract(context.Background(), bytes.NewReader(u16), int64(len(u16)), &memSink{}, opts)
	if !IsRefused(err) {
		t.Errorf("UTF-16: err = %v, want a refusal", err)
	}
	var f FB2
	if _, err := f.Extract(context.Background(), bytes.NewReader(raw), int64(len(raw)), &memSink{}, Options{}); err != nil {
		t.Errorf("under the default cap: %v", err)
	}
}

// One text node of most of the file is refused while it streams in, before
// the decoder has built it into a token.
func TestFB2_LongTextRunIsRefusedWhileItStreams(t *testing.T) {
	for name, book := range map[string][]byte{
		"text":      []byte(`<FictionBook><body><p>` + strings.Repeat("слово ", 40<<20/11) + `</p></body></FictionBook>`),
		"comment":   []byte(`<FictionBook><!--` + strings.Repeat("c", 40<<20) + `--></FictionBook>`),
		"attribute": []byte(`<FictionBook><body><p id="` + strings.Repeat("v", 40<<20) + `">x</p></body></FictionBook>`),
	} {
		wantRefusedUnder(t, name, 32<<20, func() error {
			_, err := extractBytes(FB2{}, "long.fb2", book)
			return err
		})
	}
}
