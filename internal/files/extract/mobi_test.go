package extract

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
)

// palmDBOf builds a Palm database: the header, the record list and the
// records, in order.
func palmDBOf(name, kind string, records ...[]byte) []byte {
	head := make([]byte, palmHeaderSize)
	copy(head, name)
	copy(head[60:], kind)
	binary.BigEndian.PutUint16(head[76:], uint16(len(records)))
	off := palmHeaderSize + 8*len(records) + 2
	var list []byte
	for i, r := range records {
		e := make([]byte, 8)
		binary.BigEndian.PutUint32(e, uint32(off))
		binary.BigEndian.PutUint32(e[4:], uint32(2*i))
		list = append(list, e...)
		off += len(r)
	}
	out := append(head, list...)
	out = append(out, 0, 0)
	for _, r := range records {
		out = append(out, r...)
	}
	return out
}

// lz77Compress compresses one record with PalmDOC's LZ77, using every kind of
// code the format has: back-references, space pairs, plain bytes and
// literal runs.
func lz77Compress(b []byte) []byte {
	var out []byte
	plain := func(c byte) bool { return c == 0 || (c >= 0x09 && c <= 0x7f) }
	for i := 0; i < len(b); {
		best, dist := 0, 0
		for d := 1; d <= 2047 && d <= i; d++ {
			n := 0
			for n < 10 && i+n < len(b) && b[i+n] == b[i+n-d] {
				n++
			}
			if n > best {
				best, dist = n, d
			}
		}
		switch c := b[i]; {
		case best >= 3:
			v := 0x8000 | dist<<3 | (best - 3)
			out = append(out, byte(v>>8), byte(v))
			i += best
		case c == ' ' && i+1 < len(b) && b[i+1] >= 0x40 && b[i+1] <= 0x7f:
			out = append(out, b[i+1]^0x80)
			i += 2
		case plain(c):
			out = append(out, c)
			i++
		default:
			n := 1
			for n < 8 && i+n < len(b) && !plain(b[i+n]) {
				n++
			}
			out = append(out, byte(n))
			out = append(out, b[i:i+n]...)
			i += n
		}
	}
	return out
}

// mobiSpec is a MOBI book to build: its header fields, EXTH entries and
// text, which is cut into records of recordSize and compressed.
type mobiSpec struct {
	name                   string
	text                   []byte
	compression            uint16
	encryption             uint16
	encoding, version      uint32
	recordSize             int
	extraFlags             uint16
	exth                   []mobiEXTH
	pictures               [][]byte
	huff                   *testHuff // compresses the text when set
	textLength             int       // overrides the declared length
	firstImageOverride     uint32
	fdst                   uint32
	textRecordsOverride    int
	extraRecordsBeforeText [][]byte
	// skelIndex, fragIndex and ncxIndex are a KF8 book's index records,
	// placed after the pictures and named by the header.
	skelIndex, fragIndex, ncxIndex [][]byte
	// fdstRecord, when set, is placed after them as the flow table.
	fdstRecord []byte
}

// records builds the book's records: record 0, the text records, the
// HUFF/CDIC records when compressed so, then the pictures.
func (s mobiSpec) records() [][]byte {
	if s.recordSize == 0 {
		s.recordSize = 4096
	}
	if s.encoding == 0 {
		s.encoding = 65001
	}
	if s.version == 0 {
		s.version = 6
	}
	if s.compression == 0 {
		s.compression = palmLZ77
	}
	var texts [][]byte
	for i := 0; i < len(s.text); i += s.recordSize {
		chunk := s.text[i:min(i+s.recordSize, len(s.text))]
		var rec []byte
		switch s.compression {
		case palmLZ77:
			rec = lz77Compress(chunk)
		case palmHuffman:
			rec = s.huff.encode(chunk)
		default:
			rec = append([]byte(nil), chunk...)
		}
		// Trailing entries: a multibyte entry (0x00: no bytes carried
		// over), then a one-byte TBS entry for flag bit 1.
		if s.extraFlags&1 != 0 {
			rec = append(rec, 0x00)
		}
		if s.extraFlags&2 != 0 {
			rec = append(rec, 0x81)
		}
		texts = append(texts, rec)
	}
	recs := [][]byte{nil}
	recs = append(recs, texts...)
	huffAt := uint32(mobiNone)
	huffCount := uint32(0)
	if s.huff != nil {
		huffAt = uint32(len(recs))
		recs = append(recs, s.huff.huffRecord())
		recs = append(recs, s.huff.cdicRecord())
		huffCount = 2
	}
	firstImage := uint32(mobiNone)
	if len(s.pictures) > 0 {
		firstImage = uint32(len(recs))
		recs = append(recs, s.pictures...)
	}
	if s.firstImageOverride != 0 {
		firstImage = s.firstImageOverride
	}
	indexAt := func(recs0 *[][]byte, ix [][]byte) uint32 {
		if len(ix) == 0 {
			return mobiNone
		}
		at := uint32(len(*recs0))
		*recs0 = append(*recs0, ix...)
		return at
	}
	skelAt := indexAt(&recs, s.skelIndex)
	fragAt := indexAt(&recs, s.fragIndex)
	ncxAt := indexAt(&recs, s.ncxIndex)
	if s.fdstRecord != nil {
		s.fdst = indexAt(&recs, [][]byte{s.fdstRecord})
	}
	textLen := len(s.text)
	if s.textLength != 0 {
		textLen = s.textLength
	}
	declared := min(s.recordSize, 0xffff)
	nText := len(texts)
	if s.textRecordsOverride != 0 {
		nText = s.textRecordsOverride
	}
	const hl = 248
	r0 := make([]byte, 16+hl)
	binary.BigEndian.PutUint16(r0[0:], s.compression)
	binary.BigEndian.PutUint32(r0[4:], uint32(textLen))
	binary.BigEndian.PutUint16(r0[8:], uint16(nText))
	binary.BigEndian.PutUint16(r0[10:], uint16(declared))
	binary.BigEndian.PutUint16(r0[12:], s.encryption)
	copy(r0[16:], "MOBI")
	binary.BigEndian.PutUint32(r0[20:], hl)
	binary.BigEndian.PutUint32(r0[24:], 2)
	binary.BigEndian.PutUint32(r0[28:], s.encoding)
	binary.BigEndian.PutUint32(r0[36:], s.version)
	binary.BigEndian.PutUint32(r0[0x54:], mobiNone)
	binary.BigEndian.PutUint32(r0[0x6c:], firstImage)
	binary.BigEndian.PutUint32(r0[0x70:], huffAt)
	binary.BigEndian.PutUint32(r0[0x74:], huffCount)
	fdst := s.fdst
	if fdst == 0 {
		fdst = mobiNone
	}
	binary.BigEndian.PutUint32(r0[0xc0:], fdst)
	binary.BigEndian.PutUint16(r0[0xf2:], s.extraFlags)
	binary.BigEndian.PutUint32(r0[0xf4:], ncxAt)
	binary.BigEndian.PutUint32(r0[0xf8:], fragAt)
	binary.BigEndian.PutUint32(r0[0xfc:], skelAt)
	binary.BigEndian.PutUint32(r0[0x100:], mobiNone)
	binary.BigEndian.PutUint32(r0[0x104:], mobiNone)
	if len(s.exth) > 0 {
		binary.BigEndian.PutUint32(r0[0x80:], 0x40)
		var body []byte
		for _, e := range s.exth {
			h := make([]byte, 8)
			binary.BigEndian.PutUint32(h, e.kind)
			binary.BigEndian.PutUint32(h[4:], uint32(8+len(e.value)))
			body = append(body, h...)
			body = append(body, e.value...)
		}
		head := []byte("EXTH")
		head = binary.BigEndian.AppendUint32(head, uint32(12+len(body)))
		head = binary.BigEndian.AppendUint32(head, uint32(len(s.exth)))
		r0 = append(r0, head...)
		r0 = append(r0, body...)
	}
	recs[0] = r0
	return recs
}

func (s mobiSpec) build() []byte {
	name := s.name
	if name == "" {
		name = "Test_Book"
	}
	return palmDBOf(name, "BOOKMOBI", s.records()...)
}

func exth(kind uint32, v string) mobiEXTH { return mobiEXTH{kind: kind, value: []byte(v)} }

// testHuff is a HUFF/CDIC dictionary built the way the format describes:
// phrases 0..254 have 8-bit codes (byte 255-i), phrases 255..510 16-bit
// codes (0x00, then 510-i), so both the table's final 8-bit codes and its
// longer codes, found through the per-length limits, are decoded.
type testHuff struct {
	phrases [][]byte
	literal []bool
}

func (h *testHuff) add(b []byte, literal bool) int {
	h.phrases = append(h.phrases, b)
	h.literal = append(h.literal, literal)
	return len(h.phrases) - 1
}

func (h *testHuff) code(i int) []byte {
	if i <= 254 {
		return []byte{byte(255 - i)}
	}
	return []byte{0x00, byte(510 - i)}
}

func (h *testHuff) codes(is ...int) []byte {
	var out []byte
	for _, i := range is {
		out = append(out, h.code(i)...)
	}
	return out
}

// encode codes text as phrases: each word and each run of spaces or
// punctuation is a literal phrase of its own.
func (h *testHuff) encode(text []byte) []byte {
	var out []byte
	for _, tok := range tokens(string(text)) {
		i := -1
		for k, p := range h.phrases {
			if h.literal[k] && string(p) == tok {
				i = k
				break
			}
		}
		if i < 0 {
			i = h.add([]byte(tok), true)
		}
		out = append(out, h.code(i)...)
	}
	return out
}

// tokens splits s into runs of letters or digits and single other bytes.
func tokens(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		j := i + 1
		alnum := func(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
		if alnum(s[i]) {
			for j < len(s) && alnum(s[j]) {
				j++
			}
		}
		out = append(out, s[i:j])
		i = j
	}
	return out
}

func (h *testHuff) huffRecord() []byte {
	r := []byte("HUFF")
	r = binary.BigEndian.AppendUint32(r, 24)
	r = binary.BigEndian.AppendUint32(r, 24)
	r = binary.BigEndian.AppendUint32(r, 24+1024)
	r = binary.BigEndian.AppendUint32(r, 0)
	r = binary.BigEndian.AppendUint32(r, 0)
	for b := 0; b < 256; b++ {
		if b == 0 {
			r = binary.BigEndian.AppendUint32(r, 8) // length 8, not final
			continue
		}
		r = binary.BigEndian.AppendUint32(r, 255<<8|0x80|8)
	}
	for n := 1; n <= 32; n++ {
		lo, hi := uint32(0), uint32(0)
		switch {
		case n >= 8 && n <= 15:
			lo = 1 << (n - 8) // above every code that starts 0x00
		case n == 16:
			lo, hi = 0, 510
		}
		r = binary.BigEndian.AppendUint32(r, lo)
		r = binary.BigEndian.AppendUint32(r, hi)
	}
	return r
}

func (h *testHuff) cdicRecord() []byte {
	n := len(h.phrases)
	r := []byte("CDIC")
	r = binary.BigEndian.AppendUint32(r, 16)
	r = binary.BigEndian.AppendUint32(r, uint32(n))
	r = binary.BigEndian.AppendUint32(r, 9)
	var body []byte
	offs := make([]byte, 2*n)
	for i, p := range h.phrases {
		binary.BigEndian.PutUint16(offs[2*i:], uint16(2*n+len(body)))
		v := uint16(len(p))
		if h.literal[i] {
			v |= 0x8000
		}
		body = binary.BigEndian.AppendUint16(body, v)
		body = append(body, p...)
	}
	return append(append(r, offs...), body...)
}

// mobi7Text is Mobipocket HTML as a converter writes it: the title and
// headings in large fonts, a picture by recindex, page breaks, a list.
const mobi7Text = `<html><head><guide><reference type="toc" title="Contents" filepos=0000000300 /></guide></head><body>` +
	`<p><font size="7"><b>Le Grand Livre</b></font></p><p><img recindex="00001" align="baseline"></p><p><img recindex="00009"></p>` +
	`<mbp:pagebreak/><p><font size="5"><b>Chapitre premier</b></font></p><p>Il était une fois, à Paris, un café — “le Lanterne”. # not a heading</p>` +
	`<p><font size="4"><b>Une section</b></font></p><p>Second <b>bold</b> paragraph &amp; more.</p><ul><li>un</li><li>deux</li></ul>` +
	`<mbp:pagebreak/><p><font size="5"><b>Chapitre deux</b></font></p><p><font size="5">A large paragraph that is not bold but large.</font></p><p>Fin.</p></body></html>`

const mobi7Want = "# Le Grand Livre\n\n![](volume:inbox:extracted/doc/img-1.png)\n\n[image]\n\n### Chapitre premier\n\nIl était une fois, à Paris, un café — “le Lanterne”. # not a heading\n\n#### Une section\n\nSecond bold paragraph & more.\n\n- un\n- deux\n\n### Chapitre deux\n\n### A large paragraph that is not bold but large.\n\nFin."

func mobi7Book(t testing.TB, compression uint16, encoding uint32) []byte {
	t.Helper()
	enc := func(s string) []byte {
		if encoding != 1252 {
			return []byte(s)
		}
		b, err := charmap.Windows1252.NewEncoder().Bytes([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	text := enc(mobi7Text)
	return mobiSpec{
		text:        text,
		compression: compression,
		encoding:    encoding,
		recordSize:  256,
		extraFlags:  3,
		pictures:    [][]byte{tinyPNG(t)},
		exth: []mobiEXTH{
			exth(100, "Anne Auteur"), exth(100, "Bob Writer"), {kind: 101, value: enc("Éditions Test")},
			exth(103, "<div><p>A <b>short</b> blurb.</p></div>"), exth(104, "978-0-00-000000-2"),
			exth(105, "Fiction"), exth(106, "1999-05-04"), exth(113, "B000TEST01"),
			exth(503, "Le Grand Livre"), exth(524, "fr"), exth(201, "\x00\x00\x00\x00"),
		},
	}.build()
}

func TestMOBI_Mobi7TextIsRead(t *testing.T) {
	for _, c := range []struct {
		name        string
		compression uint16
		encoding    uint32
	}{
		{"lz77 utf-8", palmLZ77, 65001},
		{"stored utf-8", palmStored, 65001},
		{"lz77 windows-1252", palmLZ77, 1252},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, rep := run(t, MOBI{}, mobi7Book(t, c.compression, c.encoding), Options{Name: "livre.mobi", Images: true})
			if got := s.md.String(); got != mobi7Want {
				t.Errorf("markdown:\n%s\n--- want ---\n%s", got, mobi7Want)
			}
			if len(s.images) != 1 || s.images[0].MediaType != "image/png" {
				t.Errorf("images: %+v", s.images)
			}
			m, _ := json.Marshal(rep.Metadata)
			want := `{"title":"Le Grand Livre","authors":["Anne Auteur","Bob Writer"],"language":"fr","date":"1999-05-04","year":1999,"publisher":"Éditions Test","identifiers":[{"scheme":"isbn","value":"978-0-00-000000-2"},{"scheme":"asin","value":"B000TEST01"}],"subjects":["Fiction"],"description":"A short blurb.","source_format":"mobi"}`
			if string(m) != want || rep.Format != "mobi" {
				t.Errorf("format %q, metadata:\n%s\nwant\n%s", rep.Format, m, want)
			}
		})
	}
}

func TestMOBI_NoPicturesWritesNoPictures(t *testing.T) {
	s, _ := run(t, MOBI{}, mobi7Book(t, palmLZ77, 65001), Options{Name: "livre.mobi"})
	if len(s.images) != 0 || strings.Contains(s.md.String(), "img-") {
		t.Errorf("images %d:\n%s", len(s.images), s.md.String())
	}
}

// The three Calibre-made fixtures (testdata/mobi/README.md): MOBI 7, KF8
// and a hybrid, read from its KF8 part.
func TestMOBI_CalibreFixturesMatchTheirGoldens(t *testing.T) {
	for _, f := range []string{"book-mobi7.mobi", "book-kf8.azw3", "book-hybrid.mobi"} {
		t.Run(f, func(t *testing.T) {
			data, err := os.ReadFile("testdata/mobi/" + f)
			if err != nil {
				t.Fatal(err)
			}
			s, rep := run(t, MOBI{}, data, Options{Name: f, Images: true})
			golden(t, "mobi/"+f+".md", s.md.String())
			m, err := json.MarshalIndent(struct {
				Format   string        `json:"format"`
				Images   int           `json:"images"`
				Notes    []string      `json:"notes,omitempty"`
				Metadata *BookMetadata `json:"metadata"`
			}{rep.Format, rep.Images, rep.Notes, rep.Metadata}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, "mobi/"+f+".json", string(m)+"\n")
		})
	}
}

// KF8 text read in record order is in reading order (RFC GV D5): every
// chapter of the fixture appears once, in the order of the source.
func TestMOBI_KF8TextIsInReadingOrder(t *testing.T) {
	for _, f := range []string{"book-kf8.azw3", "book-hybrid.mobi"} {
		data, err := os.ReadFile("testdata/mobi/" + f)
		if err != nil {
			t.Fatal(err)
		}
		s, _ := run(t, MOBI{}, data, Options{Name: f})
		md := s.md.String()
		last := -1
		for _, want := range []string{"### Chapter 1: Part One", "Chapter 1 paragraph 6.", "### Chapter 2: Part Two", "#### A short section", "### Chapter 3: Part Three", "End of chapter 3.", "### Table of Contents"} {
			i := strings.Index(md, want)
			if i <= last || strings.Count(md, want) != 1 {
				t.Errorf("%s: %q at %d, after %d (count %d)", f, want, i, last, strings.Count(md, want))
			}
			last = i
		}
		if strings.Contains(md, "calibre") || strings.Contains(md, "margin") {
			t.Errorf("%s: the stylesheet flow was read as text:\n%s", f, md)
		}
	}
}

func TestPalmDOC_TextIsRead(t *testing.T) {
	text := "Line one of the first paragraph\nline two.\n\n\nThe second paragraph, café.\r\n\n# not a heading\n"
	cp, err := charmap.Windows1252.NewEncoder().Bytes([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	want := "# A Palm Text\n\nLine one of the first paragraph\nline two.\n\nThe second paragraph, café.\n\n\\# not a heading"
	for _, compression := range []uint16{palmStored, palmLZ77} {
		var recs [][]byte
		for i := 0; i < len(cp); i += 16 {
			chunk := cp[i:min(i+16, len(cp))]
			if compression == palmLZ77 {
				chunk = lz77Compress(chunk)
			}
			recs = append(recs, chunk)
		}
		r0 := make([]byte, 16)
		binary.BigEndian.PutUint16(r0, compression)
		binary.BigEndian.PutUint32(r0[4:], uint32(len(cp)))
		binary.BigEndian.PutUint16(r0[8:], uint16(len(recs)))
		binary.BigEndian.PutUint16(r0[10:], 16)
		book := palmDBOf("A Palm Text", "TEXtREAd", append([][]byte{r0}, recs...)...)
		s, rep := run(t, PalmDOC{}, book, Options{Name: "t.pdb"})
		if s.md.String() != want {
			t.Errorf("compression %d:\n%q\nwant\n%q", compression, s.md.String(), want)
		}
		if rep.Format != "palmdoc" || rep.Metadata == nil || rep.Metadata.Title != "A Palm Text" || rep.Metadata.SourceFormat != "palmdoc" {
			t.Errorf("report %+v metadata %+v", rep, rep.Metadata)
		}
		// A .prc holding a PalmDOC text is read as one through MOBI too.
		if s2, _ := run(t, MOBI{}, book, Options{Name: "t.prc"}); s2.md.String() != want {
			t.Errorf("as .prc: %q", s2.md.String())
		}
	}
}

func TestMOBI_HuffCDICTextIsRead(t *testing.T) {
	h := &testHuff{}
	// Phrase 0 is coded text itself: two phrases that are literal.
	h.add(nil, false)
	a, b := h.add([]byte("<p>Coded "), true), h.add([]byte("twice</p>"), true)
	h.phrases[0] = h.codes(a, b)
	// Enough phrases that some take the 16-bit codes.
	var text, want strings.Builder
	text.WriteString("<html><body><h1>Huff Book</h1>")
	want.WriteString("# Huff Book\n\nCoded twice")
	for i := 0; i < 300; i++ {
		w := "w" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		text.WriteString("<p>" + w + "</p>")
		want.WriteString("\n\n" + w)
	}
	text.WriteString("</body></html>")
	coded := h.encode([]byte(text.String()))
	if len(h.phrases) <= 255 {
		t.Fatalf("only %d phrases: no 16-bit codes are exercised", len(h.phrases))
	}
	// The record is the coded phrase, then the text.
	rec := append(h.codes(0), coded...)
	r := mobiSpec{compression: palmHuffman, huff: h, text: []byte("x"), exth: []mobiEXTH{exth(503, "Huff Book")}}.records()
	r[1] = rec
	binary.BigEndian.PutUint32(r[0][4:], uint32(len("<p>Coded twice</p>")+text.Len()))
	book := palmDBOf("Huff", "BOOKMOBI", r...)
	s, _ := run(t, MOBI{}, book, Options{Name: "h.mobi"})
	md := s.md.String()
	if md != want.String() {
		t.Errorf("markdown:\n%s\n--- want ---\n%s", md, want.String())
	}
}

// offsetReaderAt records the offsets read.
type offsetReaderAt struct {
	r    *bytes.Reader
	mu   sync.Mutex
	offs []int64
}

func (c *offsetReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.mu.Lock()
	c.offs = append(c.offs, off)
	c.mu.Unlock()
	return c.r.ReadAt(p, off)
}

// A book cut at the output limit decompresses no text record past the
// block that filled it (RFC GV §4.4, early stop): of 200 records, a sink
// that takes a few pages reads the records of one block (64 KiB of text),
// not the book.
func TestMOBI_MaxCharsStopsDecompression(t *testing.T) {
	var text strings.Builder
	text.WriteString("<html><body>")
	for i := 0; i < 200; i++ {
		page := "<mbp:pagebreak/><p>Page " + strings.Repeat("text ", 50) + "</p>"
		text.WriteString(page)
		text.WriteString(strings.Repeat(" ", 4096-len(page)%4096))
	}
	book := mobiSpec{text: []byte(text.String()), extraFlags: 3}.build()
	db, err := openPalmDB(context.Background(), bytes.NewReader(book), int64(len(book)))
	if err != nil {
		t.Fatal(err)
	}
	if db.records() < 200 {
		t.Fatalf("%d records", db.records())
	}
	cr := &offsetReaderAt{r: bytes.NewReader(book)}
	s := &memSink{limit: 2000}
	_, err = MOBI{}.Extract(context.Background(), cr, int64(len(book)), s, Options{Name: "b.mobi"})
	if !errors.Is(err, ErrSinkFull) {
		t.Fatalf("err = %v, want ErrSinkFull", err)
	}
	read := 0
	for _, off := range cr.offs {
		for i := 1; i <= 200; i++ {
			if off == db.offs[i] {
				read++
			}
		}
	}
	if want := mobiMinBlock/4096 + 2; read == 0 || read > want {
		t.Errorf("%d of 200 text records were read for a 2000-byte cut, want at most %d", read, want)
	}
}

// A dictionary whose phrases expand into one another deeper than
// maxHuffDepth is refused, as is a phrase that expands into itself; one
// within the depth is read.
func TestMOBI_HuffCDICRecursionIsBounded(t *testing.T) {
	book := func(depth int, self bool) []byte {
		h := &testHuff{}
		for i := 0; i < depth; i++ {
			h.add(nil, false)
		}
		leaf := h.add([]byte("<p>leaf</p>"), true)
		for i := 0; i < depth; i++ {
			h.phrases[i] = h.codes(i + 1)
		}
		if self {
			h.phrases[depth-1] = h.codes(depth - 1)
		}
		_ = leaf
		r := mobiSpec{compression: palmHuffman, huff: h, text: []byte("x")}.records()
		r[1] = h.codes(0)
		binary.BigEndian.PutUint32(r[0][4:], uint32(len("<p>leaf</p>")))
		return palmDBOf("Huff", "BOOKMOBI", r...)
	}
	s, _ := run(t, MOBI{}, book(maxHuffDepth, false), Options{Name: "h.mobi"})
	if !strings.Contains(s.md.String(), "leaf") {
		t.Errorf("a chain %d deep: %q", maxHuffDepth, s.md.String())
	}
	_, err := extractBytes(MOBI{}, "h.mobi", book(maxHuffDepth+1, false))
	if !IsRefused(err) || !strings.Contains(err.Error(), "nest deeper") {
		t.Errorf("a chain %d deep: err = %v, want a refusal", maxHuffDepth+1, err)
	}
	_, err = extractBytes(MOBI{}, "h.mobi", book(3, true))
	if !IsRefused(err) || !strings.Contains(err.Error(), "itself") {
		t.Errorf("a phrase that expands into itself: err = %v, want a refusal", err)
	}
}

// The record list is what every read goes by: offsets that do not strictly
// increase, or that point outside the file, refuse the book before any
// record is read.
func TestMOBI_RecordOffsetsOutOfOrderAreRefused(t *testing.T) {
	good := mobi7Book(t, palmLZ77, 65001)
	if _, err := extractBytes(MOBI{}, "b.mobi", good); err != nil {
		t.Fatalf("the unaltered book: %v", err)
	}
	put := func(i int, off uint32) []byte {
		b := append([]byte(nil), good...)
		binary.BigEndian.PutUint32(b[palmHeaderSize+8*i:], off)
		return b
	}
	at := func(i int) uint32 { return binary.BigEndian.Uint32(good[palmHeaderSize+8*i:]) }
	for name, book := range map[string][]byte{
		"swapped":          put(2, at(1)-1),
		"repeated":         put(2, at(1)),
		"past the file":    put(3, uint32(len(good)+10)),
		"inside the list":  put(0, palmHeaderSize),
		"list past a file": good[:palmHeaderSize+8],
	} {
		_, err := extractBytes(MOBI{}, "b.mobi", book)
		if !IsRefused(err) {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
}

func TestMOBI_EncryptedBookIsRefused(t *testing.T) {
	for _, enc := range []uint16{1, 2} {
		book := mobiSpec{text: []byte("<p>secret</p>"), encryption: enc}.build()
		_, err := extractBytes(MOBI{}, "d.azw", book)
		if !IsRefused(err) || !strings.Contains(err.Error(), "encrypted (DRM)") {
			t.Errorf("encryption %d: err = %v, want a DRM refusal", enc, err)
		}
	}
	// A hybrid whose KF8 part is encrypted is refused too, though its
	// MOBI 7 part is not.
	data, err := os.ReadFile("testdata/mobi/book-hybrid.mobi")
	if err != nil {
		t.Fatal(err)
	}
	db, err := openPalmDB(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	kf8 := -1
	for i := 1; i < db.records(); i++ {
		if bytes.HasPrefix(data[db.offs[i]:], []byte("BOUNDARY")) {
			kf8 = i + 1
		}
	}
	if kf8 < 0 {
		t.Fatal("no BOUNDARY record in the hybrid fixture")
	}
	locked := append([]byte(nil), data...)
	binary.BigEndian.PutUint16(locked[db.offs[kf8]+12:], 2)
	if _, err := extractBytes(MOBI{}, "h.mobi", locked); !IsRefused(err) || !strings.Contains(err.Error(), "DRM") {
		t.Errorf("an encrypted KF8 part: err = %v, want a DRM refusal", err)
	}
}

// A text record is decompressed into a buffer of at most maxPalmTextOut
// (the record size field is 16 bits, so no declared size is past it): a
// record that decompresses past it is refused, and the book's text is
// capped in all.
func TestMOBI_RecordSizeAndTextAreCapped(t *testing.T) {
	// One stored record of 70 KiB.
	big := mobiSpec{text: bytes.Repeat([]byte("a"), 70<<10), recordSize: 70 << 10, compression: palmStored}.build()
	if _, err := extractBytes(MOBI{}, "b.mobi", big); !IsRefused(err) || !strings.Contains(err.Error(), "over") {
		t.Errorf("a 70 KiB stored record: err = %v", err)
	}
	// An LZ77 record of back-references that decompresses to 80 KiB.
	rec := []byte{'a', 'a', 'a'}
	for i := 0; i < (80<<10)/10; i++ {
		rec = append(rec, 0x80, 0x0f) // distance 1, length 10
	}
	r := mobiSpec{text: []byte("x")}.records()
	r[1] = rec
	binary.BigEndian.PutUint32(r[0][4:], 80<<10)
	if _, err := extractBytes(MOBI{}, "b.mobi", palmDBOf("B", "BOOKMOBI", r...)); !IsRefused(err) || !strings.Contains(err.Error(), "decompresses past") {
		t.Errorf("a record decompressing to 80 KiB: err = %v", err)
	}
	// The whole text, under a lowered cap: a declared length past it, and
	// text past it whatever the declared length.
	text := []byte(strings.Repeat("<p>more text</p>", 1000))
	book := mobiSpec{text: text}.build()
	_, err := MOBI{}.Extract(context.Background(), bytes.NewReader(book), int64(len(book)), &memSink{}, Options{palmMaxText: 4096})
	if !IsRefused(err) || !strings.Contains(err.Error(), "declares") {
		t.Errorf("a declared length past the text cap: err = %v", err)
	}
	book = mobiSpec{text: text, textLength: 4000}.build()
	if _, err := extractBytes(MOBI{}, "b.mobi", book); err != nil {
		t.Errorf("a book declaring less text than it holds: %v", err)
	}
}

// A KF8 SVG cover's kindle:embed picture is read; a record a converter
// left out, a picture record past the file or one that is not a picture are
// not passed on; the book is still read.
func TestMOBI_PictureReferencesOutsideThePicturesAreLeftOut(t *testing.T) {
	text := `<p><svg><image xlink:href="kindle:embed:0001?mime=image/png"/></svg></p><p><img recindex="00002" alt="second"></p><p><img recindex="0"></p><p><img recindex="x"></p><p><img src="kindle:embed:zzzzzzzz"></p><p><img src="https://example.org/a.png" alt="remote"></p>`
	book := mobiSpec{text: []byte(text), pictures: [][]byte{tinyPNG(t), []byte("FONT\x00\x00not a picture")}}.build()
	s, rep := run(t, MOBI{}, book, Options{Name: "b.mobi", Images: true})
	// Only the SVG cover is a picture record.
	if len(s.images) != 1 || rep.Images != 1 {
		t.Errorf("%d images kept, want the SVG one", len(s.images))
	}
	if md := s.md.String(); !strings.Contains(md, "[image: second]") || !strings.Contains(md, "[image: remote](https://example.org/a.png)") {
		t.Errorf("markdown:\n%s", md)
	}
}

func TestMOBI_UnknownEncodingAndCompressionAreRefused(t *testing.T) {
	if _, err := extractBytes(MOBI{}, "b.mobi", mobiSpec{text: []byte("x"), encoding: 1251}.build()); !IsRefused(err) || !strings.Contains(err.Error(), "1251") {
		t.Errorf("encoding 1251: err = %v", err)
	}
	if _, err := extractBytes(MOBI{}, "b.mobi", mobiSpec{text: []byte("x"), compression: 7}.build()); !IsRefused(err) || !strings.Contains(err.Error(), "compression 7") {
		t.Errorf("compression 7: err = %v", err)
	}
	if _, err := extractBytes(MOBI{}, "b.mobi", palmDBOf("x", "DataPlkr", []byte("x"))); !IsRefused(err) {
		t.Errorf("another Palm database: err = %v", err)
	}
}

// Finding a KF8 file boundary is linear in the text: a block of nothing but
// XML declarations, unclosed ones, doctypes or <html> tags is scanned once,
// not once per candidate (each candidate used to rescan the block from its
// start, so 4 MiB of declarations took minutes).
func TestMOBI_KF8BoundaryScanIsLinear(t *testing.T) {
	for name, unit := range map[string]string{"declarations": "<?xml?>", "unclosed": "<?xml ", "doctypes": "<!doctype x>", "html": "<html>"} {
		b := []byte(strings.Repeat(unit, (4<<20)/len(unit)) + "<p>x</p><?xml?><html>")
		done := make(chan int, 1)
		go func() { done <- kf8NextFile(b, 1) }()
		select {
		case i := <-done:
			if i < mobiMinBlock {
				t.Errorf("%s: boundary at %d", name, i)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: 4 MiB took more than 10 s to scan", name)
		}
	}
}

// A CDIC record may not declare more phrases than maxHuffPhrases: the
// dictionary's tables are sized by the phrase count.
func TestMOBI_HuffCDICPhraseCountIsCapped(t *testing.T) {
	h := &testHuff{}
	h.add([]byte("a"), true)
	cdic := h.cdicRecord()
	binary.BigEndian.PutUint32(cdic[8:], maxHuffPhrases+1)
	if _, err := loadHuff(context.Background(), h.huffRecord(), [][]byte{cdic}, maxHuffWork); !IsRefused(err) || !strings.Contains(err.Error(), "phrases") {
		t.Errorf("a CDIC declaring %d phrases: err = %v, want a refusal", maxHuffPhrases+1, err)
	}
}

// The HUFF/CDIC work of a whole book is bounded, not only each record's: a
// book of many records of codes that expand to nothing is refused once
// their codes together pass the book's budget.
func TestMOBI_HuffCDICBookWorkIsBounded(t *testing.T) {
	h := &testHuff{}
	empty := h.add([]byte{}, true)
	rec := bytes.Repeat(h.codes(empty), 4096)
	r := mobiSpec{compression: palmHuffman, huff: h, text: bytes.Repeat([]byte("x"), 100*4096)}.records()
	for i := 1; i <= 100; i++ {
		r[i] = rec
	}
	binary.BigEndian.PutUint32(r[0][4:], 1000)
	book := palmDBOf("Empty", "BOOKMOBI", r...)
	_, err := MOBI{}.Extract(context.Background(), bytes.NewReader(book), int64(len(book)), &memSink{}, Options{palmMaxText: 16 << 10})
	if !IsRefused(err) || !strings.Contains(err.Error(), "book's HUFF/CDIC") {
		t.Errorf("100 records of empty codes under a 64 KiB work budget: err = %v, want a refusal", err)
	}
}
