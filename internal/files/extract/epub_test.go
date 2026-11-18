package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// epubOf builds a book: the mimetype entry stored first, as OCF requires,
// then the entries deflated, in order.
func epubOf(t testing.TB, entries ...entry) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(epubMediaType)); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func epubContainer(opf string) entry {
	return xmlEntry("META-INF/container.xml", `<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="`+opf+`" media-type="application/oebps-package+xml"/></rootfiles></container>`)
}

func xhtml(body string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops"><head><title>Ignored page title</title></head><body>` + body + `</body></html>`
}

// epub3Book is an EPUB 3 book: a nav document, a cover image shown again by
// an SVG cover page, a chapter with a heading, a picture and a footnote, a
// chapter with no heading, a non-linear appendix, and refined metadata.
func epub3Book(t testing.TB) []byte {
	png := tinyPNG(t)
	opf := `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="uid"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
<dc:identifier id="uid">urn:uuid:0b6f3c3e-1111-4222-8333-944445555666</dc:identifier>
<dc:identifier id="isbn">9780000000002</dc:identifier><meta refines="#isbn" property="identifier-type" scheme="onix:codelist5">15</meta>
<dc:title id="t1">Кобзар</dc:title><meta refines="#t1" property="title-type">main</meta>
<dc:title id="t2">Вибране</dc:title><meta refines="#t2" property="title-type">subtitle</meta>
<dc:creator id="c1">Тарас Шевченко</dc:creator><meta refines="#c1" property="role" scheme="marc:relators">aut</meta>
<dc:creator id="c2">Vera Rich</dc:creator><meta refines="#c2" property="role" scheme="marc:relators">trl</meta>
<dc:creator id="c3">An Illustrator</dc:creator><meta refines="#c3" property="role" scheme="marc:relators">ill</meta>
<dc:language>uk</dc:language><dc:date>1840-04-18</dc:date><dc:publisher>Видавництво</dc:publisher>
<dc:subject>Poetry</dc:subject><dc:subject>Poetry</dc:subject>
<dc:description>&lt;p&gt;A &lt;b&gt;first&lt;/b&gt; collection.&lt;/p&gt;</dc:description>
<meta property="belongs-to-collection" id="col">Classics</meta><meta refines="#col" property="collection-type">series</meta><meta refines="#col" property="group-position">3</meta>
<meta property="dcterms:modified">2026-01-01T00:00:00Z</meta>
</metadata><manifest>
<item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
<item id="cover-img" href="images/cover.png" media-type="image/png" properties="cover-image"/>
<item id="pic" href="images/pic.png" media-type="image/png"/>
<item id="cover" href="text/cover.xhtml" media-type="application/xhtml+xml"/>
<item id="ch1" href="text/ch1.xhtml" media-type="application/xhtml+xml"/>
<item id="ch2" href="text/ch%202.xhtml" media-type="application/xhtml+xml"/>
<item id="app" href="text/appendix.xhtml" media-type="application/xhtml+xml"/>
</manifest><spine>
<itemref idref="cover"/><itemref idref="app" linear="no"/><itemref idref="ch1"/><itemref idref="ch2"/>
</spine></package>`
	nav := xhtml(`<nav epub:type="toc" id="toc"><h1>Contents</h1><ol>
<li><a href="text/ch1.xhtml">Перший розділ</a><ol><li><a href="text/ch1.xhtml#s">Section</a></li></ol></li>
<li><a href="text/ch%202.xhtml">Другий розділ</a></li>
<li><a href="text/appendix.xhtml">Додаток</a></li></ol></nav>`)
	return epubOf(t,
		epubContainer("OEBPS/content.opf"),
		xmlEntry("OEBPS/content.opf", opf),
		entry{"OEBPS/nav.xhtml", []byte(nav)},
		entry{"OEBPS/images/cover.png", png},
		entry{"OEBPS/images/pic.png", png},
		entry{"OEBPS/text/cover.xhtml", []byte(xhtml(`<div><svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><image xlink:href="../images/cover.png"/></svg></div>`))},
		entry{"OEBPS/text/ch1.xhtml", []byte(xhtml(`<h1>Кобзар</h1><h1>Чигрине</h1>
<p>Text with a note<a epub:type="noteref" href="#n1">1</a> and # not a heading.</p>
<p><img src="../images/pic.png" alt="A picture"/></p>
<aside epub:type="footnote" id="n1"><p>The note text.</p></aside>
<h2>Section</h2><p>More.</p>`))},
		entry{"OEBPS/text/ch 2.xhtml", []byte(xhtml(`<p>Only a paragraph here.</p>`))},
		entry{"OEBPS/text/appendix.xhtml", []byte(xhtml(`<h1>Appendix</h1><p>Extra.</p>`))},
	)
}

// epub2Book is an EPUB 2 book: opf:role and opf:scheme attributes, Calibre's
// series meta, the cover named by <meta name="cover">, an NCX, and an
// endnotes document a chapter's references point into.
func epub2Book(t testing.TB) []byte {
	opf := `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0" unique-identifier="BookId"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf">
<dc:title>The Old Book</dc:title>
<dc:creator opf:role="aut" opf:file-as="Author, Ann">Ann Author</dc:creator>
<dc:creator opf:role="trl">Tom Translator</dc:creator>
<dc:language>en</dc:language>
<dc:date opf:event="modification">2020-02-02</dc:date><dc:date opf:event="publication">May 1999</dc:date>
<dc:identifier id="BookId" opf:scheme="ISBN">0-306-40615-2</dc:identifier>
<dc:identifier opf:scheme="calibre">abc</dc:identifier>
<meta name="calibre:series" content="Old Series"/><meta name="calibre:series_index" content="1.0"/>
<meta name="cover" content="cov"/>
</metadata><manifest>
<item id="ncx" href="toc.ncx" media-type="application/x-dtbncx+xml"/>
<item id="cov" href="cover.png" media-type="image/png"/>
<item id="a" href="a.html" media-type="application/xhtml+xml"/>
<item id="b" href="b.html" media-type="application/xhtml+xml"/>
<item id="notes" href="notes.html" media-type="application/xhtml+xml"/>
</manifest><spine toc="ncx"><itemref idref="a"/><itemref idref="b"/><itemref idref="notes"/></spine></package>`
	ncx := `<?xml version="1.0" encoding="UTF-8"?>
<ncx xmlns="http://www.daisy.org/z3986/2005/ncx/" version="2005-1"><navMap>
<navPoint id="p1" playOrder="1"><navLabel><text>Chapter One</text></navLabel><content src="a.html"/></navPoint>
<navPoint id="p2" playOrder="2"><navLabel><text>Chapter Two</text></navLabel><content src="b.html#start"/></navPoint>
</navMap></ncx>`
	return epubOf(t,
		epubContainer("content.opf"),
		xmlEntry("content.opf", opf),
		xmlEntry("toc.ncx", ncx),
		entry{"cover.png", tinyPNG(t)},
		entry{"a.html", []byte(xhtml(`<p class="chapter">It was a dark night.<a epub:type="noteref" href="notes.html#e1">*</a></p>`))},
		entry{"b.html", []byte(xhtml(`<h2>Two</h2><p>Morning came.</p>`))},
		entry{"notes.html", []byte(xhtml(`<section epub:type="endnotes"><ol><li id="e1"><p>Night, that is.</p></li></ol></section>`))},
	)
}

// An EPUB 3 book reads as one document: its title the one "#", the cover
// first, the spine in order with the non-linear appendix last, a chapter with
// no heading under its contents label, the footnote out of the flow and
// listed under Notes; and its metadata goes to the report.
func TestEPUB_EPUB3IsReadInSpineOrderWithNotesAndCover(t *testing.T) {
	s, rep := run(t, EPUB{}, epub3Book(t), Options{Name: "kobzar.epub", Images: true})
	want := `# Кобзар

![Cover](volume:inbox:extracted/doc/img-1.png)

## Чигрине

Text with a note[1] and # not a heading.

![A picture](volume:inbox:extracted/doc/img-2.png)

### Section

More.

## Другий розділ

Only a paragraph here.

## Supplementary

## Appendix

Extra.

## Notes

[1] The note text.`
	if got := s.md.String(); got != want {
		t.Errorf("markdown:\n%s\n--- want ---\n%s", got, want)
	}
	if len(s.images) != 2 || rep.Images != 2 {
		t.Errorf("images: %d kept, report %d; want the cover and the picture", len(s.images), rep.Images)
	}
	wantMeta := &BookMetadata{
		Title: "Кобзар", Authors: []string{"Тарас Шевченко"}, Translators: []string{"Vera Rich"}, Language: "uk",
		Date: "1840-04-18", Year: 1840, Publisher: "Видавництво",
		Identifiers: []BookIdentifier{{Scheme: "uuid", Value: "0b6f3c3e-1111-4222-8333-944445555666"}, {Scheme: "isbn", Value: "9780000000002"}},
		Series:      "Classics", SeriesIndex: "3", Subjects: []string{"Poetry"}, Description: "A first collection.", SourceFormat: "epub",
	}
	if !reflect.DeepEqual(rep.Metadata, wantMeta) {
		t.Errorf("metadata:\n got %+v\nwant %+v", rep.Metadata, wantMeta)
	}
	if rep.Format != "epub" {
		t.Errorf("format = %q", rep.Format)
	}
}

// An EPUB 2 book takes its chapter labels from the NCX, its roles and
// identifier schemes from opf: attributes, its series from Calibre's meta,
// and its notes from an endnotes document another chapter points into.
func TestEPUB_EPUB2TakesNCXLabelsAndOPF2Metadata(t *testing.T) {
	s, rep := run(t, EPUB{}, epub2Book(t), Options{Name: "old.epub"})
	want := `# The Old Book

## Chapter One

It was a dark night.[1]

### Two

Morning came.

## Notes

[1] Night, that is.`
	if got := s.md.String(); got != want {
		t.Errorf("markdown:\n%s\n--- want ---\n%s", got, want)
	}
	if len(s.images) != 0 {
		t.Errorf("images were kept with Images off: %d", len(s.images))
	}
	wantMeta := &BookMetadata{
		Title: "The Old Book", Authors: []string{"Ann Author"}, Translators: []string{"Tom Translator"}, Language: "en",
		Date: "May 1999", Year: 1999,
		Identifiers: []BookIdentifier{{Scheme: "isbn", Value: "0-306-40615-2"}, {Scheme: "calibre", Value: "abc"}},
		Series:      "Old Series", SeriesIndex: "1.0", SourceFormat: "epub",
	}
	if !reflect.DeepEqual(rep.Metadata, wantMeta) {
		t.Errorf("metadata:\n got %+v\nwant %+v", rep.Metadata, wantMeta)
	}
}

// readLog is an io.ReaderAt that records every range read from it.
type readLog struct {
	r      *bytes.Reader
	mu     sync.Mutex
	ranges [][2]int64
}

func (l *readLog) ReadAt(p []byte, off int64) (int, error) {
	l.mu.Lock()
	l.ranges = append(l.ranges, [2]int64{off, off + int64(len(p))})
	l.mu.Unlock()
	return l.r.ReadAt(p, off)
}

// touched reports whether any read overlapped [from, to).
func (l *readLog) touched(from, to int64) bool {
	for _, r := range l.ranges {
		if r[0] < to && r[1] > from {
			return true
		}
	}
	return false
}

// A cut stops the reading, not only the writing: once the sink refuses,
// the spine items after the one being written are never opened, so a sample
// of a long book costs what the sample costs (RFC GV §4.2, G3).
func TestEPUB_MaxCharsDoesNotOpenLaterSpineItems(t *testing.T) {
	const chapters = 6
	var manifest, spine strings.Builder
	files := []entry{epubContainer("content.opf")}
	var chapterFiles []entry
	for i := range chapters {
		id := string(rune('a' + i))
		manifest.WriteString(`<item id="` + id + `" href="` + id + `.xhtml" media-type="application/xhtml+xml"/>`)
		spine.WriteString(`<itemref idref="` + id + `"/>`)
		chapterFiles = append(chapterFiles, entry{id + ".xhtml", []byte(xhtml(`<h1>Chapter ` + id + `</h1><p>` + strings.Repeat("слово ", 400) + `</p>`))})
	}
	files = append(files, xmlEntry("content.opf", `<package xmlns="http://www.idpf.org/2007/opf" version="3.0"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Long</dc:title></metadata><manifest>`+manifest.String()+`</manifest><spine>`+spine.String()+`</spine></package>`))
	// Incompressible padding last: zip.NewReader reads up to 64 KiB from
	// the end looking for the directory, which must not land on a chapter.
	pad := make([]byte, 80<<10)
	for i, x := 0, uint32(1); i < len(pad); i++ {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		pad[i] = byte(x)
	}
	chapterFiles = append(chapterFiles, entry{"padding.bin", pad})
	data := epubOf(t, append(files, chapterFiles...)...)

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	// Where each chapter's entry lies: its local header through its data.
	span := map[string][2]int64{}
	for _, f := range zr.File {
		off, err := f.DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		// The local header is 30 bytes and the name, before the data.
		span[f.Name] = [2]int64{max(off-30-int64(len(f.Name)), 0), off + int64(f.CompressedSize64)}
	}

	log := &readLog{r: bytes.NewReader(data)}
	// Room for the title and part of the first chapter only.
	s := &memSink{limit: 1500}
	rep, err := EPUB{}.Extract(context.Background(), log, int64(len(data)), s, Options{Name: "long.epub"})
	if !errors.Is(err, ErrSinkFull) {
		t.Fatalf("Extract = %v, want ErrSinkFull", err)
	}
	if rep.Format != "epub" || !strings.Contains(s.md.String(), "# Long") {
		t.Errorf("the report or the text so far was lost: %+v\n%s", rep, s.md.String())
	}
	if sp := span["a.xhtml"]; !log.touched(sp[0], sp[1]) {
		t.Fatal("the first chapter was not read: the log does not see entry reads")
	}
	for i := 1; i < chapters; i++ {
		name := string(rune('a'+i)) + ".xhtml"
		if sp := span[name]; log.touched(sp[0], sp[1]) {
			t.Errorf("%s was read after the sink refused the first chapter", name)
		}
	}
}

// A book whose encryption.xml encrypts its content documents is refused:
// its text cannot be read without the key.
func TestEPUB_EncryptedContentIsRefused(t *testing.T) {
	enc := xmlEntry("META-INF/encryption.xml", `<encryption xmlns="urn:oasis:names:tc:opendocument:xmlns:container" xmlns:enc="http://www.w3.org/2001/04/xmlenc#">
<enc:EncryptedData><enc:EncryptionMethod Algorithm="http://www.w3.org/2001/04/xmlenc#aes128-cbc"/><enc:CipherData><enc:CipherReference URI="OEBPS/text/ch1.xhtml"/></enc:CipherData></enc:EncryptedData>
</encryption>`)
	for name, book := range map[string][]byte{
		"content":   withEntries(t, epub3Book(t), enc),
		"malformed": withEntries(t, epub3Book(t), entry{"META-INF/encryption.xml", []byte("<encryption><unclosed>")}),
	} {
		_, err := EPUB{}.Extract(context.Background(), bytes.NewReader(book), int64(len(book)), &memSink{}, Options{})
		if !IsRefused(err) || !strings.Contains(err.Error(), "encrypted (DRM)") {
			t.Errorf("%s: Extract = %v, want a DRM refusal", name, err)
		}
	}
}

// Font obfuscation is not DRM: a book whose only encrypted files are fonts
// is read as any other.
func TestEPUB_ObfuscatedFontsAloneAreRead(t *testing.T) {
	enc := xmlEntry("META-INF/encryption.xml", `<encryption xmlns="urn:oasis:names:tc:opendocument:xmlns:container" xmlns:enc="http://www.w3.org/2001/04/xmlenc#">
<enc:EncryptedData><enc:EncryptionMethod Algorithm="http://www.idpf.org/2008/embedding"/><enc:CipherData><enc:CipherReference URI="OEBPS/fonts/Serif.otf"/></enc:CipherData></enc:EncryptedData>
<enc:EncryptedData><enc:EncryptionMethod Algorithm="http://ns.adobe.com/pdf/enc#RC"/><enc:CipherData><enc:CipherReference URI="OEBPS/fonts/Sans%20Bold.TTF"/></enc:CipherData></enc:EncryptedData>
</encryption>`)
	book := withEntries(t, epub3Book(t), enc)
	s, _ := run(t, EPUB{}, book, Options{})
	if !strings.Contains(s.md.String(), "The note text.") {
		t.Errorf("the book was not read:\n%s", s.md.String())
	}
}

// A name inside the book that is absolute or climbs out of the package is
// never followed: the item is skipped with a note, the rest read.
func TestEPUB_PathOutsideThePackageIsSkipped(t *testing.T) {
	opf := `<package xmlns="http://www.idpf.org/2007/opf" version="3.0"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Escape</dc:title></metadata><manifest>
<item id="up" href="../../etc/passwd" media-type="application/xhtml+xml"/>
<item id="abs" href="/etc/passwd" media-type="application/xhtml+xml"/>
<item id="url" href="https://example.com/x.xhtml" media-type="application/xhtml+xml"/>
<item id="gone" href="missing.xhtml" media-type="application/xhtml+xml"/>
<item id="ok" href="ok.xhtml" media-type="application/xhtml+xml"/>
</manifest><spine><itemref idref="up"/><itemref idref="abs"/><itemref idref="url"/><itemref idref="gone"/><itemref idref="ok"/></spine></package>`
	book := epubOf(t, epubContainer("OEBPS/content.opf"), xmlEntry("OEBPS/content.opf", opf),
		entry{"etc/passwd", []byte("root:x:0:0")},
		entry{"OEBPS/ok.xhtml", []byte(xhtml(`<p>Inside. <img src="../../../etc/passwd" alt="up"/><img src="/etc/passwd" alt="abs"/></p>`))})
	s, rep := run(t, EPUB{}, book, Options{Images: true})
	md := s.md.String()
	if strings.Contains(md, "root:") || len(s.images) != 0 {
		t.Fatalf("a file outside the package was read:\n%s", md)
	}
	if !strings.Contains(md, "Inside.") {
		t.Errorf("the item inside the package was not read:\n%s", md)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "4 manifest items name a file outside the book") {
		t.Errorf("notes %q: want the four items' skip counted", rep.Notes)
	}
}

// A book that inflates past the archive's bounds is refused by the zip
// limits every zip format reads through.
func TestEPUB_ZipBombIsRefused(t *testing.T) {
	book := withEntries(t, epub3Book(t), entry{"OEBPS/text/ch1.xhtml.bak", bytes.Repeat([]byte("a"), minZipBudget+1)})
	_, err := EPUB{}.Extract(context.Background(), bytes.NewReader(book), int64(len(book)), &memSink{}, Options{})
	if !IsRefused(err) {
		t.Fatalf("Extract = %v, want a refusal", err)
	}
}

// withEntries is a book with more entries added after its own.
func withEntries(t testing.TB, book []byte, extra ...entry) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(book), int64(len(book)))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, f := range zr.File {
		if err := zw.Copy(f); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range extra {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The registry picks the EPUB reader by type and by name.
func TestEPUB_RegistryPicksItByTypeAndName(t *testing.T) {
	for _, c := range [][2]string{{"application/epub+zip", ""}, {"", "Book.EPUB"}} {
		e, ok := Default().For(c[0], c[1])
		if _, isEPUB := e.(EPUB); !ok || !isEPUB {
			t.Errorf("For(%q, %q) = %T, want EPUB", c[0], c[1], e)
		}
	}
}

// Metadata is untrusted text: control and format characters go, each value
// and list is bounded, repeats drop, a year is read from a date, and nothing
// left means no metadata.
func TestBookMetadata_CleanBoundsAndStripsEveryValue(t *testing.T) {
	long := strings.Repeat("я", MaxMetaString+50)
	many := make([]string, MaxMetaList+10)
	for i := range many {
		many[i] = "a" + strings.Repeat("b", i)
	}
	m := (&BookMetadata{
		Title: "Line\none\x00\x1b[31m ‮evil", Authors: append([]string{"", " x ", "x"}, many...), Publisher: long,
		Date: "c. 1923?", Description: strings.Repeat("d", MaxMetaDescription*2),
		Identifiers: []BookIdentifier{{Scheme: "ISBN", Value: "1"}, {Scheme: "isbn", Value: "1"}, {Scheme: "x", Value: ""}},
	}).Clean()
	if m.Title != "Line one[31m evil" {
		t.Errorf("title = %q", m.Title)
	}
	if len(m.Authors) != MaxMetaList || m.Authors[0] != "x" {
		t.Errorf("authors = %d, first %q", len(m.Authors), m.Authors[0])
	}
	if n := len([]rune(m.Publisher)); n != MaxMetaString {
		t.Errorf("publisher is %d characters", n)
	}
	if n := len([]rune(m.Description)); n != MaxMetaDescription {
		t.Errorf("description is %d characters", n)
	}
	if m.Year != 1923 {
		t.Errorf("year = %d", m.Year)
	}
	if len(m.Identifiers) != 1 || m.Identifiers[0] != (BookIdentifier{Scheme: "isbn", Value: "1"}) {
		t.Errorf("identifiers = %+v", m.Identifiers)
	}
	if (&BookMetadata{Title: "\x00 \t", SourceFormat: "epub"}).Clean() != nil {
		t.Error("metadata of nothing but a format was kept")
	}
}
