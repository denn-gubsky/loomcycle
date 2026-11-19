package extract

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/text/encoding/charmap"
)

// DOC reads a Word 97–2003 binary document, and a simple Word 6/95 one
// (RFC GX §4.2), in pure Go: the OLE2 compound file (cfb.go), then the
// Word format's piece table for the text, the paragraph properties and the
// stylesheet for headings and tables, the footnote and endnote
// sub-documents for notes, and the OLE property sets for metadata
// ([MS-DOC], [MS-OLEPS]). Character formatting is read only to infer
// headings when Options.InferHeadings asks; pictures are counted, macros
// and embedded objects never read.
//
// The text is decoded a piece at a time in document order and written as it
// goes; the first ErrSinkFull ends the reading. Only the notes the written
// text referred to are decoded, at the end.
type DOC struct{}

const docMediaType = "application/msword"

// Bounds on a document (RFC GX §4.2).
const (
	// maxDOCPieces caps the piece table, maxDOCFKPs the paragraph and
	// character property pages listed, maxDOCParas the paragraphs read.
	maxDOCPieces = 100_000
	maxDOCFKPs   = 100_000
	maxDOCParas  = 1_000_000
	// maxDOCText caps the bytes of text the pieces decode from in one pass:
	// pieces may overlap, so the file's size alone does not bound it.
	maxDOCText = 64 << 20
	// maxDOCTableStream caps the table stream, read whole.
	maxDOCTableStream = 64 << 20
	// maxDOCStyles caps the stylesheet entries read.
	maxDOCStyles = 4096
	// maxDOCPara is the text one paragraph collects before what it holds
	// is written as a paragraph of its own; maxDOCLine cuts a heading's
	// text and a cell's.
	maxDOCPara = 1 << 20
	maxDOCLine = 64 << 10
	// maxDOCNote caps one note's text, maxDOCNoteBytes all of them, and
	// maxDOCNoteRefs the references of each kind read.
	maxDOCNote      = 64 << 10
	maxDOCNoteBytes = 8 << 20
	maxDOCNoteRefs  = 100_000
	// maxDOCFieldDepth caps the fields tracked open at once; deeper ones
	// are counted, not kept.
	maxDOCFieldDepth = 64
	// maxDOCInferParas, maxDOCInferText and maxDOCInferRun bound what
	// heading inference holds: the paragraphs, all their text, and the
	// bytes kept of one paragraph (a longer one is never a heading, and
	// only weighs).
	maxDOCInferParas = 100_000
	maxDOCInferText  = 16 << 20
	maxDOCInferRun   = 2048
	// maxDOCProps caps a property set's properties, maxDOCPropString one
	// string value.
	maxDOCProps      = 1000
	maxDOCPropString = 64 << 10
	// maxDOCStyleChain caps the based-on chain followed for a style's
	// character properties.
	maxDOCStyleChain = 16
)

func (DOC) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{docMediaType}, []string{".doc", ".dot"})
}

func (DOC) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "doc"}
	if size > maxCFBBytes {
		return rep, refusedf("the document is over %d bytes", maxCFBBytes)
	}
	head := make([]byte, 8)
	n, _ := r.ReadAt(head, 0)
	head = head[:n]
	// An RTF document saved as .doc and typed a Word document before RTF
	// was sniffed by its header: the RTF reader reads it.
	if bytes.HasPrefix(bytes.TrimPrefix(head, []byte("\xEF\xBB\xBF")), []byte(`{\rtf`)) {
		return RTF{}.Extract(ctx, r, size, sink, opts)
	}
	if !isCFB(head) {
		return rep, refusedf("not a Word document")
	}
	c, err := openCFB(ctx, &ctxReaderAt{ctx: ctx, r: r}, size)
	if err != nil {
		return rep, err
	}
	wd, err := c.stream("WordDocument")
	if err != nil {
		return rep, err
	}
	if wd == nil {
		return rep, refusedf("not a Word document: the compound file has no WordDocument stream")
	}
	d := &docReader{
		ctx: ctx, c: c, wd: wd, rep: &rep,
		w: newWriter(ctx, sink, opts), title: title(opts.Name, "Document"),
		pap: docPageCache{}, chp: docPageCache{},
	}
	if err := d.fib(); err != nil {
		return rep, err
	}
	d.meta = docMetadata(c, &rep)
	if opts.InferHeadings && d.word97 {
		d.inferHeadings()
	}
	err = d.read()
	d.noteSkips()
	rep.Metadata = d.metadata()
	rep.finish(d.w)
	return rep, err
}

// docPiece is one entry of the piece table: CPs [cp0, cp1) are stored from
// byte fc of the WordDocument stream, two bytes a character (wide) or one.
// An 8-bit piece of Word 97 uses [MS-DOC]'s compressed mapping; a Word 6/95
// document's one piece uses the code page its language names (cm).
type docPiece struct {
	cp0, cp1 int
	fc       int64
	wide     bool
	cm       *charmap.Charmap
}

// docStyle is one stylesheet entry: its built-in identifier, kind and base,
// and the paragraph and character properties it sets.
type docStyle struct {
	sti, stk, base int
	papx, chpx     []byte
	ok             bool
}

// docPageCache holds the last property page read.
type docPageCache struct {
	pn    uint32
	valid bool
	rgfc  []uint32
	props [][]byte
}

// docBTE is a bin table: aFC[i]..aFC[i+1] is in property page pn[i].
type docBTE struct {
	fc []uint32
	pn []uint32
}

type docNoteRef struct {
	edn bool
	i   int
}

type docReader struct {
	ctx    context.Context
	c      *cfbFile
	wd     *cfbStream
	rep    *Report
	w      *writer
	title  string
	meta   BookMetadata
	word97 bool

	pieces                []docPiece
	ccpText, ccpFtn       int
	ccpHdd, ccpAtn        int
	ccpEdn                int
	styles                []docStyle
	papBTE, chpBTE        docBTE
	pap, chp              docPageCache
	ftnTxt, ednTxt        []uint32
	refs                  map[int]docNoteRef
	fkpRead               int
	decoded               int64
	inferred              []int // heading levels inferred per paragraph, nil when off
	inferring             bool
	inferParas            []inferPara
	inferText             int
	pictures, deepFields  int
	droppedNotes, cutHead int
	styleCache            map[int]runFmt

	// The paragraph being read.
	para      strings.Builder
	paraCut   bool
	fields    []bool // open fields, true while in the instruction
	instr     int    // open fields in their instruction
	fieldOver int    // fields open past maxDOCFieldDepth
	paraN     int
	runs      []fmtRun

	// The table being written.
	cell      []string
	cellBytes int
	row       []string
	tbl       *rtfTable

	started bool
	notes   []docNoteRef // the notes referred to, in the order written
}

// fib reads the File Information Block: the version, the encryption flags,
// the table stream and what it holds.
func (d *docReader) fib() error {
	base := make([]byte, 32)
	if _, err := d.wd.ReadAt(base, 0); err != nil {
		return refusedf("the Word document's header is cut short")
	}
	ident := binary.LittleEndian.Uint16(base[0:])
	nFib := binary.LittleEndian.Uint16(base[2:])
	lid := binary.LittleEndian.Uint16(base[6:])
	flags := binary.LittleEndian.Uint16(base[10:])
	const (
		fComplex     = 1 << 2
		fEncrypted   = 1 << 8
		fWhichTblStm = 1 << 9
		fObfuscated  = 1 << 15
	)
	if ident != 0xA5EC && ident != 0xA5DC {
		return refusedf("not a Word document: the WordDocument stream has no Word header")
	}
	if flags&(fEncrypted|fObfuscated) != 0 {
		return refusedf("the document is encrypted")
	}
	switch {
	case nFib >= 0x00C1:
		d.word97 = true
		return d.fib97(flags&fWhichTblStm != 0)
	case nFib >= 101 && nFib <= 105:
		if flags&fComplex != 0 {
			return refusedf("a Word 6/95 document saved with fast save (a complex file) is not read")
		}
		return d.fib6(lid)
	default:
		return refusedf("Word documents older than Word 6 (nFib %d) are not read", nFib)
	}
}

// fib97 reads a Word 97–2003 FIB's counts and table positions.
func (d *docReader) fib97(table1 bool) error {
	hdr := make([]byte, min(d.wd.size, 4096))
	if _, err := d.wd.ReadAt(hdr, 0); err != nil {
		return refusedf("the Word document's header is cut short")
	}
	at := 32
	u16 := func() (int, bool) {
		if at+2 > len(hdr) {
			return 0, false
		}
		v := int(binary.LittleEndian.Uint16(hdr[at:]))
		at += 2
		return v, true
	}
	csw, ok := u16()
	if !ok {
		return refusedf("the Word document's header is cut short")
	}
	at += 2 * csw
	cslw, ok := u16()
	if !ok {
		return refusedf("the Word document's header is cut short")
	}
	lw := at
	at += 4 * cslw
	cb, ok := u16()
	if !ok || at > len(hdr) {
		return refusedf("the Word document's header is cut short")
	}
	rgLw := func(i int) int {
		if i >= cslw || lw+4*i+4 > len(hdr) {
			return 0
		}
		return int(int32(binary.LittleEndian.Uint32(hdr[lw+4*i:])))
	}
	d.ccpText, d.ccpFtn, d.ccpHdd, d.ccpAtn, d.ccpEdn = rgLw(3), rgLw(4), rgLw(5), rgLw(7), rgLw(8)
	for _, n := range []int{d.ccpText, d.ccpFtn, d.ccpHdd, d.ccpAtn, d.ccpEdn} {
		if n < 0 || n > maxDOCText {
			return refusedf("the Word document's character counts are damaged")
		}
	}
	blob := at
	fcLcb := func(i int) (int, int) {
		o := blob + 8*i
		if i >= cb || o+8 > len(hdr) {
			return 0, 0
		}
		return int(binary.LittleEndian.Uint32(hdr[o:])), int(binary.LittleEndian.Uint32(hdr[o+4:]))
	}

	name := "0Table"
	if table1 {
		name = "1Table"
	}
	table, err := d.c.readStream(name, maxDOCTableStream)
	if err != nil {
		return err
	}
	if table == nil {
		return refusedf("the Word document's table stream %s is missing", name)
	}
	part := func(i int) []byte {
		fc, lcb := fcLcb(i)
		if lcb == 0 || fc < 0 || lcb < 0 || fc > len(table) || lcb > len(table)-fc {
			return nil
		}
		return table[fc : fc+lcb]
	}
	const (
		iStshf       = 1
		iPlcffndRef  = 2
		iPlcffndTxt  = 3
		iPlcfBteChpx = 12
		iPlcfBtePapx = 13
		iClx         = 33
		iPlcfendRef  = 46
		iPlcfendTxt  = 47
	)
	if err := d.clx(part(iClx)); err != nil {
		return err
	}
	d.styles = docStyles(part(iStshf))
	var err2 error
	if d.papBTE, err2 = docBin(part(iPlcfBtePapx)); err2 != nil {
		return err2
	}
	if d.chpBTE, err2 = docBin(part(iPlcfBteChpx)); err2 != nil {
		return err2
	}
	d.refs = map[int]docNoteRef{}
	d.ftnTxt = d.notePLCs(part(iPlcffndRef), part(iPlcffndTxt), false)
	d.ednTxt = d.notePLCs(part(iPlcfendRef), part(iPlcfendTxt), true)
	return nil
}

// fib6 reads a Word 6/95 FIB: its text is one run of 8-bit characters from
// fcMin, in the code page of the document's language.
func (d *docReader) fib6(lid uint16) error {
	hdr := make([]byte, 0x58)
	if _, err := d.wd.ReadAt(hdr, 0); err != nil {
		return refusedf("the Word document's header is cut short")
	}
	fcMin := int64(binary.LittleEndian.Uint32(hdr[0x18:]))
	fcMac := int64(binary.LittleEndian.Uint32(hdr[0x1C:]))
	ccp := int64(int32(binary.LittleEndian.Uint32(hdr[0x34:])))
	if ccp < 0 || fcMin > fcMac || fcMac > d.wd.size || ccp > fcMac-fcMin {
		return refusedf("the Word document's text positions are damaged")
	}
	cm, err := docLangCodePage(lid)
	if err != nil {
		return err
	}
	d.ccpText = int(ccp)
	d.pieces = []docPiece{{cp0: 0, cp1: int(ccp), fc: fcMin, cm: cm}}
	return nil
}

// docLangCodePage is the code page a Word 6/95 document's language (its
// lid) writes 8-bit text in, from the fixed list of decoders (RFC GX §4.1).
func docLangCodePage(lid uint16) (*charmap.Charmap, error) {
	switch lid & 0x3FF {
	case 0x00:
		return charmap.Windows1252, nil
	case 0x19, 0x22, 0x23, 0x02, 0x2F: // Russian, Ukrainian, Belarusian, Bulgarian, Macedonian
		return charmap.Windows1251, nil
	case 0x1A: // Croatian, Serbian, Bosnian: Cyrillic Serbian is 1251
		if lid == 0x0C1A || lid == 0x1C1A {
			return charmap.Windows1251, nil
		}
		return charmap.Windows1250, nil
	case 0x05, 0x15, 0x0E, 0x1B, 0x24, 0x18, 0x1C: // Czech, Polish, Hungarian, Slovak, Slovenian, Romanian, Albanian
		return charmap.Windows1250, nil
	case 0x09, 0x07, 0x0C, 0x0A, 0x10, 0x13, 0x16, 0x1D, 0x06, 0x14, 0x0B, 0x03, 0x0F, 0x2D, 0x36, 0x38, 0x3E, 0x21, 0x56:
		// English, German, French, Spanish, Italian, Dutch, Portuguese,
		// Swedish, Danish, Norwegian, Finnish, Catalan, Icelandic, Basque,
		// Afrikaans, Faroese, Malay, Indonesian, Galician
		return charmap.Windows1252, nil
	}
	return nil, refusedf("the code page of the document's language (0x%04x) is not read", lid)
}

// clx reads the piece table out of the CLX: property runs (Prc) first,
// then the one Pcdt.
func (d *docReader) clx(b []byte) error {
	if len(b) == 0 {
		return refusedf("the Word document has no piece table")
	}
	for i := 0; i < len(b); {
		switch b[i] {
		case 0x01:
			if i+3 > len(b) {
				return refusedf("the Word document's piece table is damaged")
			}
			n := int(int16(binary.LittleEndian.Uint16(b[i+1:])))
			if n < 0 || i+3+n > len(b) {
				return refusedf("the Word document's piece table is damaged")
			}
			i += 3 + n
		case 0x02:
			if i+5 > len(b) {
				return refusedf("the Word document's piece table is damaged")
			}
			lcb := int(binary.LittleEndian.Uint32(b[i+1:]))
			if lcb < 4 || lcb > len(b)-i-5 || (lcb-4)%12 != 0 {
				return refusedf("the Word document's piece table is damaged")
			}
			return d.plcPcd(b[i+5 : i+5+lcb])
		default:
			return refusedf("the Word document's piece table is damaged")
		}
	}
	return refusedf("the Word document has no piece table")
}

func (d *docReader) plcPcd(b []byte) error {
	n := (len(b) - 4) / 12
	if n > maxDOCPieces {
		return refusedf("the Word document has over %d pieces", maxDOCPieces)
	}
	cp := func(i int) int { return int(binary.LittleEndian.Uint32(b[4*i:])) }
	pcd := b[4*(n+1):]
	for i := 0; i < n; i++ {
		// The CPs are shared, one piece's end the next one's start, so
		// pieces never overlap in CP; each must not run backwards.
		a, z := cp(i), cp(i+1)
		if z < a || z > maxDOCText*4 {
			return refusedf("the Word document's piece table is damaged")
		}
		raw := binary.LittleEndian.Uint32(pcd[8*i+2:])
		p := docPiece{cp0: a, cp1: z}
		if raw&(1<<30) != 0 {
			p.fc = int64(raw&0x3FFFFFFF) / 2
		} else {
			p.fc, p.wide = int64(raw&0x3FFFFFFF), true
		}
		d.pieces = append(d.pieces, p)
	}
	return nil
}

// docBin reads a PlcBte: n+1 FCs, then n page numbers.
func docBin(b []byte) (docBTE, error) {
	if len(b) < 4 {
		return docBTE{}, nil
	}
	if (len(b)-4)%8 != 0 {
		return docBTE{}, refusedf("the Word document's property tables are damaged")
	}
	n := (len(b) - 4) / 8
	if n > maxDOCFKPs {
		return docBTE{}, refusedf("the Word document has over %d property pages", maxDOCFKPs)
	}
	t := docBTE{fc: make([]uint32, n+1), pn: make([]uint32, n)}
	for i := range t.fc {
		t.fc[i] = binary.LittleEndian.Uint32(b[4*i:])
	}
	for i := range t.pn {
		t.pn[i] = binary.LittleEndian.Uint32(b[4*(n+1)+4*i:]) & 0x3FFFFF
	}
	return t, nil
}

// notePLCs reads one kind of note's reference and text tables: each
// reference's CP is kept in refs, and the text table returned.
func (d *docReader) notePLCs(ref, txt []byte, edn bool) []uint32 {
	if len(ref) < 4 || (len(ref)-4)%6 != 0 || len(txt) < 8 || len(txt)%4 != 0 {
		return nil
	}
	n := (len(ref) - 4) / 6
	if n+2 > len(txt)/4 {
		n = len(txt)/4 - 2
	}
	if n > maxDOCNoteRefs {
		d.rep.note("only the first %d notes of each kind are read", maxDOCNoteRefs)
		n = maxDOCNoteRefs
	}
	for i := 0; i < n; i++ {
		cp := int(binary.LittleEndian.Uint32(ref[4*i:]))
		if _, dup := d.refs[cp]; !dup {
			d.refs[cp] = docNoteRef{edn: edn, i: i}
		}
	}
	texts := make([]uint32, n+1)
	for i := range texts {
		texts[i] = binary.LittleEndian.Uint32(txt[4*i:])
	}
	return texts
}

// docStyles reads the stylesheet (STSH): for each style its sti, kind,
// base and property exceptions. A damaged entry is left out, not refused.
func docStyles(b []byte) []docStyle {
	if len(b) < 2 {
		return nil
	}
	cbStshi := int(binary.LittleEndian.Uint16(b))
	if cbStshi < 4 || 2+cbStshi > len(b) {
		return nil
	}
	stshi := b[2 : 2+cbStshi]
	cstd := int(binary.LittleEndian.Uint16(stshi))
	cbBase := int(binary.LittleEndian.Uint16(stshi[2:]))
	if cbBase < 10 {
		return nil
	}
	cstd = min(cstd, maxDOCStyles)
	out := make([]docStyle, 0, cstd)
	at := 2 + cbStshi
	for i := 0; i < cstd && at+2 <= len(b); i++ {
		cb := int(binary.LittleEndian.Uint16(b[at:]))
		at += 2
		if cb == 0 {
			out = append(out, docStyle{})
			continue
		}
		if at+cb > len(b) {
			break
		}
		out = append(out, docStd(b[at:at+cb], cbBase))
		at += cb
	}
	return out
}

// docStd reads one STD: StdfBase, the name, then the UPXs its kind has.
func docStd(s []byte, cbBase int) docStyle {
	if len(s) < 10 || cbBase > len(s) {
		return docStyle{}
	}
	st := docStyle{
		sti:  int(binary.LittleEndian.Uint16(s) & 0x0FFF),
		stk:  int(binary.LittleEndian.Uint16(s[2:]) & 0x000F),
		base: int(binary.LittleEndian.Uint16(s[2:]) >> 4),
		ok:   true,
	}
	at := cbBase
	if at+2 > len(s) {
		return st
	}
	// The name: a count, that many UTF-16 characters, and a terminator.
	at += 2 + 2*int(binary.LittleEndian.Uint16(s[at:])) + 2
	cupx := int(binary.LittleEndian.Uint16(s[4:]) & 0x000F)
	upx := func() []byte {
		if at%2 == 1 {
			at++
		}
		if at+2 > len(s) {
			return nil
		}
		n := int(binary.LittleEndian.Uint16(s[at:]))
		at += 2
		if at+n > len(s) {
			at = len(s)
			return nil
		}
		v := s[at : at+n]
		at += n
		return v
	}
	switch st.stk {
	case 1: // paragraph style: its PAPX (istd first), then its CHPX
		if cupx >= 1 {
			if p := upx(); len(p) >= 2 {
				st.papx = p[2:]
			}
		}
		if cupx >= 2 {
			st.chpx = upx()
		}
	case 2: // character style
		if cupx >= 1 {
			st.chpx = upx()
		}
	}
	return st
}

// docSprms calls fn for each property modifier of a grpprl with its
// operand, as far as the grpprl holds whole ones.
func docSprms(g []byte, fn func(sprm uint16, op []byte)) {
	for i := 0; i+2 <= len(g); {
		sprm := binary.LittleEndian.Uint16(g[i:])
		i += 2
		var n int
		switch sprm >> 13 {
		case 0, 1:
			n = 1
		case 2, 4, 5:
			n = 2
		case 3:
			n = 4
		case 7:
			n = 3
		case 6:
			if i >= len(g) {
				return
			}
			switch sprm {
			case 0xD608: // sprmTDefTable: a two-byte length, less one
				if i+2 > len(g) {
					return
				}
				n = int(binary.LittleEndian.Uint16(g[i:])) + 1
			case 0xC615: // sprmPChgTabs: a one-byte length, 255 marks a longer form
				if g[i] == 255 {
					if i+2 > len(g) {
						return
					}
					del := int(g[i+1])
					if i+2+4*del >= len(g) {
						return
					}
					add := int(g[i+2+4*del])
					n = 1 + 1 + 4*del + 1 + 3*add
				} else {
					n = 1 + int(g[i])
				}
			default:
				n = 1 + int(g[i])
			}
		}
		if i+n > len(g) {
			return
		}
		fn(sprm, g[i:i+n])
		i += n
	}
}

// Paragraph property modifiers read.
const (
	sprmPFInTable = 0x2416
	sprmPFTtp     = 0x2417
	sprmPItap     = 0x6649
	sprmPOutLvl   = 0x2640
	sprmCFBold    = 0x0835
	sprmCHps      = 0x4A43
)

// docPara is what a paragraph's properties say this reader needs.
type docPara struct {
	istd    int
	inTable bool
	ttp     bool
	outline int // 0-8, or -1
}

// papAt is the properties of the paragraph whose text holds byte fc.
func (d *docReader) papAt(fc int64) docPara {
	p := docPara{outline: -1}
	g := d.propsAt(&d.papBTE, &d.pap, fc, true)
	if len(g) < 2 {
		return p
	}
	p.istd = int(binary.LittleEndian.Uint16(g))
	docSprms(g[2:], func(sprm uint16, op []byte) {
		switch sprm {
		case sprmPFInTable:
			p.inTable = op[0] != 0
		case sprmPFTtp:
			p.ttp = op[0] != 0
		case sprmPItap:
			if binary.LittleEndian.Uint32(op) > 0 {
				p.inTable = true
			}
		case sprmPOutLvl:
			if op[0] <= 8 {
				p.outline = int(op[0])
			}
		}
	})
	return p
}

// propsAt is the grpprl of the property page run holding fc: a PAPX (istd
// first) when pap, a CHPX otherwise; nil when none.
func (d *docReader) propsAt(bte *docBTE, cache *docPageCache, fc int64, pap bool) []byte {
	n := len(bte.pn)
	if n == 0 || fc < 0 || fc > 0xFFFFFFFF {
		return nil
	}
	f := uint32(fc)
	i := sort.Search(n, func(i int) bool { return bte.fc[i+1] > f }) // first page ending past fc
	if i == n || bte.fc[i] > f {
		return nil
	}
	if !cache.valid || cache.pn != bte.pn[i] {
		if !d.loadPage(bte.pn[i], cache, pap) {
			return nil
		}
	}
	k := sort.Search(len(cache.props), func(k int) bool { return cache.rgfc[k+1] > f })
	if k == len(cache.props) || cache.rgfc[k] > f {
		return nil
	}
	return cache.props[k]
}

// loadPage reads a 512-byte FKP: crun in its last byte, crun+1 FCs, then
// crun offsets (a PAPX's with its 12-byte PHE beside it).
func (d *docReader) loadPage(pn uint32, cache *docPageCache, pap bool) bool {
	cache.valid = false
	d.fkpRead++
	if d.fkpRead > maxDOCParas {
		return false
	}
	page := make([]byte, 512)
	if _, err := d.wd.ReadAt(page, int64(pn)*512); err != nil {
		return false
	}
	crun := int(page[511])
	step := 1
	if pap {
		step = 13
	}
	if crun == 0 || 4*(crun+1)+step*crun > 511 {
		return false
	}
	cache.pn, cache.valid = pn, true
	cache.rgfc = make([]uint32, crun+1)
	for i := range cache.rgfc {
		cache.rgfc[i] = binary.LittleEndian.Uint32(page[4*i:])
	}
	cache.props = make([][]byte, crun)
	for j := 0; j < crun; j++ {
		off := int(page[4*(crun+1)+step*j]) * 2
		if off == 0 || off >= 511 {
			continue
		}
		if pap {
			cb := int(page[off])
			at, size := off+1, 2*cb-1
			if cb == 0 {
				if off+1 >= 511 {
					continue
				}
				at, size = off+2, 2*int(page[off+1])
			}
			if size > 0 && at+size <= 511 {
				cache.props[j] = page[at : at+size]
			}
		} else {
			cb := int(page[off])
			if off+1+cb <= 511 {
				cache.props[j] = page[off+1 : off+1+cb]
			}
		}
	}
	return true
}

// headingLevel is the paragraph's heading level, 1-9, from its style's
// built-in identifier (Heading 1-9) or its own outline level; 0 for body
// text.
func (d *docReader) headingLevel(p docPara) int {
	if p.outline >= 0 {
		return p.outline + 1
	}
	if p.istd >= 0 && p.istd < len(d.styles) {
		if s := d.styles[p.istd]; s.ok && s.stk == 1 && s.sti >= 1 && s.sti <= 9 {
			return s.sti
		}
	}
	return 0
}

// styleFmt is the bold and size a paragraph style gives its text, its
// based-on chain followed from the root.
func (d *docReader) styleFmt(istd int) (bold bool, hps int) {
	var chain []docStyle
	seen := map[int]bool{}
	for i := istd; i >= 0 && i < len(d.styles) && !seen[i] && len(chain) < maxDOCStyleChain; i = d.styles[i].base {
		seen[i] = true
		if !d.styles[i].ok {
			break
		}
		chain = append(chain, d.styles[i])
	}
	hps = 20 // [MS-DOC]'s default, 10 points
	for k := len(chain) - 1; k >= 0; k-- {
		docSprms(chain[k].chpx, func(sprm uint16, op []byte) {
			switch sprm {
			case sprmCFBold:
				bold = docToggle(op[0], bold)
			case sprmCHps:
				hps = int(binary.LittleEndian.Uint16(op))
			}
		})
	}
	return bold, hps
}

// docToggle applies a toggle property's operand to the value it has.
func docToggle(op byte, v bool) bool {
	switch op {
	case 0:
		return false
	case 1:
		return true
	case 0x81:
		return !v
	}
	return v
}

// metadata is the document's properties, cleaned.
func (d *docReader) metadata() *BookMetadata {
	m := d.meta
	m.SourceFormat = "doc"
	return m.Clean()
}

// noteSkips reports what was left out.
func (d *docReader) noteSkips() {
	if d.pictures > 0 {
		d.rep.note("%d pictures or drawn objects were left out", d.pictures)
	}
	if d.deepFields > 0 {
		d.rep.note("fields nested over %d deep were read as text", maxDOCFieldDepth)
	}
	if d.droppedNotes > 0 {
		d.rep.note("%d notes past %d bytes of notes were left out", d.droppedNotes, maxDOCNoteBytes)
	}
	if d.cutHead > 0 {
		d.rep.note("%d headings were cut at %d bytes", d.cutHead, maxDOCLine)
	}
}

// docMetadata reads the summary property sets. A damaged or missing one
// leaves its fields empty: metadata is never a reason to refuse a document.
func docMetadata(c *cfbFile, rep *Report) BookMetadata {
	var m BookMetadata
	if b, err := c.readStream("\x05SummaryInformation", maxCFBStream); err == nil && b != nil {
		props := docPropertySet(b)
		m.Title = props.str(2)
		if s := props.str(3); s != "" {
			m.Subjects = append(m.Subjects, s)
		}
		if a := props.str(4); a != "" {
			m.Authors = append(m.Authors, a)
		}
		if k := props.str(5); k != "" {
			m.Subjects = append(m.Subjects, strings.FieldsFunc(k, func(r rune) bool { return r == ',' || r == ';' })...)
		}
		m.Description = props.str(6)
		if t, ok := props.times[12]; ok && t.Year() >= 1 && t.Year() <= 9999 {
			m.Date = t.Format("2006-01-02")
		}
		if props.badCodePage != 0 {
			rep.note("the document's properties in code page %d were not read", props.badCodePage)
		}
	}
	if b, err := c.readStream("\x05DocumentSummaryInformation", maxCFBStream); err == nil && b != nil {
		m.Publisher = docPropertySet(b).str(0x0F)
	}
	return m
}

// docProps is a property set's string and time values by property id.
type docProps struct {
	strs        map[uint32]string
	times       map[uint32]time.Time
	badCodePage int
}

func (p docProps) str(id uint32) string { return p.strs[id] }

// docCodePages are the code pages a property set's 8-bit strings are read
// in (the fixed list of RFC GX §4.1).
var docCodePages = map[int]*charmap.Charmap{
	1250: charmap.Windows1250, 1251: charmap.Windows1251, 1252: charmap.Windows1252,
	20866: charmap.KOI8R, 21866: charmap.KOI8U,
	28591: charmap.ISO8859_1, 28592: charmap.ISO8859_2, 28595: charmap.ISO8859_5,
	866: charmap.CodePage866,
}

// docPropertySet reads the first property set of a property set stream
// ([MS-OLEPS] §2.21): its code page, then its strings and times, under
// bounds. Anything malformed ends the reading with what was read.
func docPropertySet(b []byte) docProps {
	p := docProps{strs: map[uint32]string{}, times: map[uint32]time.Time{}}
	if len(b) < 48 || binary.LittleEndian.Uint16(b) != 0xFFFE {
		return p
	}
	if binary.LittleEndian.Uint32(b[24:]) < 1 {
		return p
	}
	off := int(binary.LittleEndian.Uint32(b[44:]))
	if off < 0 || off+8 > len(b) {
		return p
	}
	set := b[off:]
	size := int(binary.LittleEndian.Uint32(set))
	if size < 8 || size > len(set) {
		size = len(set)
	}
	set = set[:size]
	count := int(binary.LittleEndian.Uint32(set[4:]))
	count = min(count, maxDOCProps, (len(set)-8)/8)
	type entry struct{ id, at uint32 }
	entries := make([]entry, 0, count)
	codePage := 1252
	for i := 0; i < count; i++ {
		e := entry{binary.LittleEndian.Uint32(set[8+8*i:]), binary.LittleEndian.Uint32(set[12+8*i:])}
		entries = append(entries, e)
		if e.id == 1 && int(e.at)+6 <= len(set) && binary.LittleEndian.Uint16(set[e.at:]) == 0x02 {
			codePage = int(binary.LittleEndian.Uint16(set[e.at+4:]))
		}
	}
	for _, e := range entries {
		at := int(e.at)
		if at < 8 || at+4 > len(set) {
			continue
		}
		v := set[at+4:]
		switch binary.LittleEndian.Uint16(set[at:]) {
		case 0x1E: // VT_LPSTR: a byte count with its terminator, in the code page
			if len(v) < 4 {
				continue
			}
			n := int(binary.LittleEndian.Uint32(v))
			if n < 0 || n > maxDOCPropString || n > len(v)-4 {
				continue
			}
			raw := v[4 : 4+n]
			switch codePage {
			case 1200: // UTF-16LE in an 8-bit string's place
				p.strs[e.id] = docUTF16(raw)
			case 65001:
				p.strs[e.id] = strings.ToValidUTF8(string(bytes.TrimRight(raw, "\x00")), "")
			default:
				cm := docCodePages[codePage]
				if cm == nil {
					p.badCodePage = codePage
					continue
				}
				s, err := cm.NewDecoder().Bytes(bytes.TrimRight(raw, "\x00"))
				if err == nil {
					p.strs[e.id] = string(s)
				}
			}
		case 0x1F: // VT_LPWSTR: a character count with its terminator
			if len(v) < 4 {
				continue
			}
			n := int(binary.LittleEndian.Uint32(v))
			if n < 0 || n > maxDOCPropString/2 || 2*n > len(v)-4 {
				continue
			}
			p.strs[e.id] = docUTF16(v[4 : 4+2*n])
		case 0x40: // VT_FILETIME: 100 ns intervals since 1601
			if len(v) < 8 {
				continue
			}
			ft := binary.LittleEndian.Uint64(v)
			const epochDiff = 116444736000000000 // 1601 to 1970 in 100 ns
			if ft > epochDiff && ft < epochDiff+uint64(9000)*365*24*3600*10_000_000 {
				d := ft - epochDiff
				p.times[e.id] = time.Unix(int64(d/10_000_000), int64(d%10_000_000)*100).UTC()
			}
		}
	}
	return p
}

// docUTF16 decodes UTF-16LE text up to its first NUL.
func docUTF16(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}
