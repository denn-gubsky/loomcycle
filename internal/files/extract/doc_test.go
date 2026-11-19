package extract

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// The LibreOffice-made fixtures (testdata/doc/README.md) read to their
// golden Markdown and metadata.
func TestDOC_LibreOfficeFixturesMatchTheirGoldens(t *testing.T) {
	for _, tc := range []struct {
		file  string
		infer bool
	}{{"report.doc", false}, {"cyrillic.doc", false}, {"plain.doc", true}} {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile("testdata/doc/" + tc.file)
			if err != nil {
				t.Fatal(err)
			}
			s, rep := run(t, DOC{}, data, Options{Name: tc.file, Images: true, InferHeadings: tc.infer})
			golden(t, "doc/"+tc.file+".md", s.md.String())
			m, err := json.MarshalIndent(struct {
				Format           string        `json:"format"`
				Tables           int           `json:"tables"`
				InferredHeadings int           `json:"inferred_headings,omitempty"`
				Notes            []string      `json:"notes,omitempty"`
				Metadata         *BookMetadata `json:"metadata"`
			}{rep.Format, rep.Tables, rep.InferredHeadings, rep.Notes, rep.Metadata}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, "doc/"+tc.file+".json", string(m)+"\n")
		})
	}
}

// docTestPiece is one piece of a test document's text: 8-bit in [MS-DOC]'s
// compressed form, or UTF-16.
type docTestPiece struct {
	text       string
	compressed bool
}

// docTestSpec builds a Word 97 document: a FIB with the counts and the
// piece table this reader needs, and nothing else (no styles, no
// paragraph properties, no notes).
type docTestSpec struct {
	pieces []docTestPiece
	flags  uint16
	nFib   uint16
	// noTable leaves the table stream out; extra adds streams.
	noTable bool
	// samePlace stores only the first piece's text: every piece is read
	// from where the first one is, as a hostile piece table may say.
	samePlace bool
	// alternateBold gives the text character properties (a CHPX bin
	// table and its pages) that turn bold on and off at every character.
	alternateBold bool
	extra     []cfbStreamSpec
}

// docEncode8 is text in the compressed 8-bit form: Latin-1, but for the
// characters [MS-DOC] maps to bytes 0x82–0x9F.
func docEncode8(t testing.TB, s string) []byte {
	back := map[rune]byte{}
	for b, r := range docCompressed {
		back[r] = b
	}
	var out []byte
	for _, r := range s {
		switch b, ok := back[r]; {
		case ok:
			out = append(out, b)
		case r < 0x100:
			out = append(out, byte(r))
		default:
			t.Fatalf("%q has no 8-bit form", r)
		}
	}
	return out
}

func (s docTestSpec) build(t testing.TB) []byte {
	t.Helper()
	return s.cfb(t).data
}

func (s docTestSpec) cfb(t testing.TB) *testCFB {
	t.Helper()
	nFib := s.nFib
	if nFib == 0 {
		nFib = 0x00C1
	}
	wd := make([]byte, 1024)
	binary.LittleEndian.PutUint16(wd[0:], 0xA5EC)
	binary.LittleEndian.PutUint16(wd[2:], nFib)
	binary.LittleEndian.PutUint16(wd[6:], 0x0409)
	binary.LittleEndian.PutUint16(wd[10:], s.flags|1<<9) // the 1Table stream
	binary.LittleEndian.PutUint16(wd[32:], 14)
	binary.LittleEndian.PutUint16(wd[62:], 22)
	binary.LittleEndian.PutUint16(wd[152:], 93)
	cps := []uint32{0}
	var pcds []byte
	cp := 0
	for i, p := range s.pieces {
		fc := uint32(len(wd))
		var raw uint32
		if s.samePlace && i > 0 {
			raw = binary.LittleEndian.Uint32(pcds[2:])
			cp += utf8.RuneCountInString(p.text)
		} else if p.compressed {
			wd = append(wd, docEncode8(t, p.text)...)
			raw = fc*2 | 1<<30
			cp += len(docEncode8(t, p.text))
		} else {
			u := utf16.Encode([]rune(p.text))
			for _, c := range u {
				wd = binary.LittleEndian.AppendUint16(wd, c)
			}
			raw = fc
			cp += len(u)
		}
		cps = append(cps, uint32(cp))
		pcd := make([]byte, 8)
		binary.LittleEndian.PutUint32(pcd[2:], raw)
		pcds = append(pcds, pcd...)
	}
	binary.LittleEndian.PutUint32(wd[76:], uint32(cp)) // ccpText
	plc := []byte{}
	for _, c := range cps {
		plc = binary.LittleEndian.AppendUint32(plc, c)
	}
	plc = append(plc, pcds...)
	clx := append([]byte{0x02}, binary.LittleEndian.AppendUint32(nil, uint32(len(plc)))...)
	clx = append(clx, plc...)
	binary.LittleEndian.PutUint32(wd[154+8*33:], 0)
	binary.LittleEndian.PutUint32(wd[154+8*33+4:], uint32(len(clx)))
	table := clx
	if s.alternateBold {
		// The text is one 8-bit piece from byte 1024; each FKP page holds
		// 101 one-character runs, every other one bold (sprmCFBold 1).
		textEnd := uint32(len(wd))
		var aFC, pns []uint32
		for len(wd)%512 != 0 {
			wd = append(wd, 0)
		}
		for fc := uint32(1024); fc < textEnd; {
			crun := min(101, int(textEnd-fc))
			page := make([]byte, 512)
			for j := 0; j <= crun; j++ {
				binary.LittleEndian.PutUint32(page[4*j:], fc+uint32(j))
			}
			for j := 0; j < crun; j++ {
				if j%2 == 1 {
					page[4*(crun+1)+j] = 250
				}
			}
			copy(page[500:], []byte{3, 0x35, 0x08, 1})
			page[511] = byte(crun)
			aFC = append(aFC, fc)
			pns = append(pns, uint32(len(wd)/512))
			wd = append(wd, page...)
			fc += uint32(crun)
		}
		aFC = append(aFC, textEnd)
		bte := []byte{}
		for _, v := range append(aFC, pns...) {
			bte = binary.LittleEndian.AppendUint32(bte, v)
		}
		binary.LittleEndian.PutUint32(wd[154+8*12:], uint32(len(table)))
		binary.LittleEndian.PutUint32(wd[154+8*12+4:], uint32(len(bte)))
		table = append(append([]byte(nil), table...), bte...)
	}
	streams := []cfbStreamSpec{{"WordDocument", wd}}
	if !s.noTable {
		streams = append(streams, cfbStreamSpec{"1Table", table})
	}
	streams = append(streams, s.extra...)
	return buildCFB(t, streams...)
}

// word6Doc builds a Word 6/95 document: one run of 8-bit text from fcMin.
func word6Doc(t testing.TB, text []byte, nFib, lid, flags uint16) []byte {
	wd := make([]byte, 0x400)
	binary.LittleEndian.PutUint16(wd[0:], 0xA5DC)
	binary.LittleEndian.PutUint16(wd[2:], nFib)
	binary.LittleEndian.PutUint16(wd[6:], lid)
	binary.LittleEndian.PutUint16(wd[10:], flags)
	binary.LittleEndian.PutUint32(wd[0x18:], 0x400)
	binary.LittleEndian.PutUint32(wd[0x1C:], uint32(0x400+len(text)))
	binary.LittleEndian.PutUint32(wd[0x34:], uint32(len(text)))
	return buildCFB(t, cfbStreamSpec{"WordDocument", append(wd, text...)}).data
}

func extractDOC(t *testing.T, data []byte, opts Options) (string, Report, error) {
	t.Helper()
	s := &memSink{}
	if opts.Name == "" {
		opts.Name = "t.doc"
	}
	rep, err := DOC{}.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), s, opts)
	return s.md.String(), rep, err
}

// An encrypted or obfuscated document is refused by name, before its
// (unreadable) text is decoded.
func TestDOC_EncryptedIsRefused(t *testing.T) {
	for name, flags := range map[string]uint16{"encrypted": 1 << 8, "obfuscated": 1 << 15} {
		doc := docTestSpec{pieces: []docTestPiece{{text: "secret words\r"}}, flags: flags}.build(t)
		md, _, err := extractDOC(t, doc, Options{})
		if !IsRefused(err) || !strings.Contains(err.Error(), "the document is encrypted") {
			t.Errorf("%s: err = %v, want the document refused as encrypted", name, err)
		}
		if strings.Contains(md, "secret") {
			t.Errorf("%s: text was written: %q", name, md)
		}
	}
	w6 := word6Doc(t, []byte("secret\r"), 104, 0x0409, 1<<8)
	if _, _, err := extractDOC(t, w6, Options{}); !IsRefused(err) || !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("Word 95: err = %v, want the document refused as encrypted", err)
	}
}

// A compound file that is not a Word document (a spreadsheet, a deck, a
// mail message share the container) and bytes that are no compound file
// are refused as not a Word document.
func TestDOC_NotAWordDocumentIsRefused(t *testing.T) {
	for name, data := range map[string][]byte{
		"xls":   buildCFB(t, cfbStreamSpec{"Workbook", []byte("\x09\x08\x10\x00")}, cfbStreamSpec{"\x05SummaryInformation", nil}).data,
		"ppt":   buildCFB(t, cfbStreamSpec{"PowerPoint Document", []byte("x")}).data,
		"bytes": []byte("PK\x03\x04 not a compound file at all"),
	} {
		_, _, err := extractDOC(t, data, Options{Name: "x.doc"})
		if !IsRefused(err) || !strings.Contains(err.Error(), "not a Word document") {
			t.Errorf("%s: err = %v, want a refusal as not a Word document", name, err)
		}
	}
	// A WordDocument stream with no Word header is not one either.
	bad := buildCFB(t, cfbStreamSpec{"WordDocument", []byte("not a FIB")}).data
	if _, _, err := extractDOC(t, bad, Options{}); !IsRefused(err) || !strings.Contains(err.Error(), "not a Word document") {
		t.Errorf("no Word header: err = %v", err)
	}
}

// A Word 6/95 document saved with fast save is refused by name, as is
// anything older than Word 6; a simple one is read in its language's code
// page.
func TestDOC_Word95ComplexIsRefusedByName(t *testing.T) {
	text, err := charmap.Windows1251.NewEncoder().Bytes([]byte("Книга ліхтарів\rДругий рядок\r"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = extractDOC(t, word6Doc(t, text, 104, 0x0422, 1<<2), Options{})
	if !IsRefused(err) || !strings.Contains(err.Error(), "fast save") {
		t.Fatalf("complex Word 95: err = %v, want it refused by name", err)
	}
	if _, _, err := extractDOC(t, word6Doc(t, text, 45, 0x0422, 0), Options{}); !IsRefused(err) || !strings.Contains(err.Error(), "older than Word 6") {
		t.Fatalf("Word 2: err = %v", err)
	}
	md, rep, err := extractDOC(t, word6Doc(t, text, 104, 0x0422, 0), Options{Name: "w95.doc"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "# w95\n\nКнига ліхтарів\n\nДругий рядок"; md != want || rep.Format != "doc" {
		t.Fatalf("simple Word 95 in windows-1251:\n%q\nwant\n%q", md, want)
	}
	// A language whose code page is not on the list is refused by name.
	if _, _, err := extractDOC(t, word6Doc(t, []byte("x\r"), 104, 0x0411, 0), Options{}); !IsRefused(err) || !strings.Contains(err.Error(), "code page") {
		t.Fatalf("Japanese Word 95: err = %v", err)
	}
}

// A field's instruction (between its begin and separator marks) is left
// out and its result kept, a nested field's too; 8-bit pieces use
// [MS-DOC]'s mapping, and a document's pieces are read in CP order.
func TestDOC_FieldInstructionIsDropped(t *testing.T) {
	doc := docTestSpec{pieces: []docTestPiece{
		{text: "See \x13 HYPERLINK \"http://example.com/\" \x14the \x13 PAGE \x143\x15 page\x15 now.\r", compressed: true},
		{text: "\x93Quoted\x94 \x13 TOC \\o \x15café — ok\r"},
		{text: "“Quoted” ™ dash–end\r", compressed: true},
	}}.build(t)
	md, _, err := extractDOC(t, doc, Options{Name: "f.doc"})
	if err != nil {
		t.Fatal(err)
	}
	want := "# f\n\nSee the 3 page now.\n\nQuoted café — ok\n\n“Quoted” ™ dash–end"
	if md != want {
		t.Fatalf("got\n%q\nwant\n%q", md, want)
	}
}

// furthestReaderAt records the furthest byte read below below.
type furthestReaderAt struct {
	r           io.ReaderAt
	below, most int64
}

func (c *furthestReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if end := off + int64(len(p)); off < c.below && end > c.most {
		c.most = end
	}
	return c.r.ReadAt(p, off)
}

// A sink that is full ends the reading: the pieces after the cut are never
// read from the file (RFC GX G3).
func TestDOC_MaxCharsStopsReading(t *testing.T) {
	var pieces []docTestPiece
	for i := range 200 {
		pieces = append(pieces, docTestPiece{text: strings.Repeat("word ", 400) + "\r", compressed: i%2 == 0})
	}
	c := docTestSpec{pieces: pieces}.cfb(t)
	data := c.data
	// The text starts 1,024 bytes into the WordDocument stream, its pieces
	// one after the other: piece 10 starts past 10 pieces of at least
	// 2,001 bytes each. The table stream follows the WordDocument stream.
	textAt := int64(c.start["WordDocument"]+1)*512 + 1024
	cr := &furthestReaderAt{r: bytes.NewReader(data), below: textAt + 200*2001}
	s := &memSink{limit: 4 << 10}
	_, err := DOC{}.Extract(context.Background(), cr, int64(len(data)), s, Options{Name: "long.doc"})
	if !errors.Is(err, ErrSinkFull) {
		t.Fatalf("err = %v, want ErrSinkFull", err)
	}
	if limit := textAt + 10*2001; cr.most > limit {
		t.Fatalf("read up to byte %d of %d, want nothing past %d: the reading went on after the cut", cr.most, len(data), limit)
	}
}

// A piece table of more pieces than the cap, or whose pieces run
// backwards, is refused before any text is read.
func TestDOC_PieceTableBoundsAreRefused(t *testing.T) {
	many := make([]docTestPiece, maxDOCPieces+1)
	for i := range many {
		many[i] = docTestPiece{text: "a", compressed: true}
	}
	if _, _, err := extractDOC(t, docTestSpec{pieces: many, samePlace: true}.build(t), Options{}); !IsRefused(err) || !strings.Contains(err.Error(), "pieces") {
		t.Fatalf("too many pieces: err = %v", err)
	}
	c := docTestSpec{pieces: []docTestPiece{{text: "ab", compressed: true}, {text: "cd", compressed: true}}}.cfb(t)
	// The table stream follows the WordDocument stream; its PlcPcd's CPs
	// begin five bytes in. The second piece is made to run backwards.
	clx := int(c.start["1Table"]+1) * 512
	binary.LittleEndian.PutUint32(c.data[clx+5+4:], 10)
	if _, _, err := extractDOC(t, c.data, Options{}); !IsRefused(err) || !strings.Contains(err.Error(), "piece table") {
		t.Fatalf("pieces out of order: err = %v", err)
	}
}

// Only the notes the written text referred to are listed, numbered in the
// order they are referred to: a document cut before its endnote's
// reference lists the footnote alone.
func TestDOC_NotesListOnlyTheReferencesWritten(t *testing.T) {
	data, err := os.ReadFile("testdata/doc/report.doc")
	if err != nil {
		t.Fatal(err)
	}
	c, err := openCFB(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := c.stream("WordDocument")
	d := &docReader{ctx: context.Background(), c: c, wd: wd, rep: &Report{}, w: newWriter(context.Background(), &memSink{}, Options{}), title: "t"}
	if err := d.fib(); err != nil {
		t.Fatal(err)
	}
	// Read the main text only up to the paragraph before the endnote's.
	cut := -1
	_ = d.chars(0, d.ccpText, func(cp int, ch rune, _ int64) error {
		if ref, ok := d.refs[cp]; ok && ref.edn {
			cut = cp - 5
		}
		return nil
	})
	if cut < 0 {
		t.Fatal("the fixture has no endnote reference")
	}
	if err := d.chars(0, cut, d.char); err != nil {
		t.Fatal(err)
	}
	if err := d.finish(); err != nil {
		t.Fatal(err)
	}
	md := d.w.sink.(*memSink).md.String()
	if !strings.Contains(md, "[1] Every lantern had a number.") || strings.Contains(md, "Dusk") || strings.Contains(md, "[2]") {
		t.Fatalf("notes of a cut document:\n%s", md)
	}
}
