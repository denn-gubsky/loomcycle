package extract

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// MOBI reads a Mobipocket or Kindle book: MOBI, AZW, AZW3 (KF8) and PRC
// (RFC GV §4.4), written from the format's public description
// (wiki.mobileread.com, "MOBI"). It is a Palm database whose record 0 holds
// the PalmDOC header, the MOBI header and the EXTH metadata block, then the
// text in compressed records, then pictures and indexes. The text is HTML:
// Mobipocket's own for a MOBI 7 book, XHTML for KF8. It is decompressed a
// record at a time and read in blocks through the HTML walker's body-only
// mode, so a book cut at the output limit is not decompressed past the
// block that filled it. A book with DRM is refused.
//
// A hybrid file (a MOBI 7 book, a BOUNDARY record, then a KF8 book) is read
// from its KF8 part. KF8 text is a skeleton of each XHTML file followed by
// the fragments that fill it; the reader takes them in record order, which
// keeps the text's order (RFC GV D5: the skeleton and fragment indexes are
// not applied).
type MOBI struct{}

const mobiMediaType = "application/x-mobipocket-ebook"

func (MOBI) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{mobiMediaType}, []string{".mobi", ".azw", ".azw3", ".prc"})
}

// Bounds on a MOBI book beyond the Palm database's.
const (
	maxMOBIEXTH = 4096
	// maxMOBIPicture caps one picture record; maxMOBIPictures and
	// maxMOBIPictureBytes all the pictures one book hands over.
	maxMOBIPicture      = 16 << 20
	maxMOBIPictures     = 500
	maxMOBIPictureBytes = 64 << 20
	// maxMOBIFontHeading is the longest text a MOBI 7 paragraph in a large
	// font may have and still be read as a heading.
	maxMOBIFontHeading = 120
	// mobiMinBlock is the least text a block holds before a page break
	// (MOBI 7) or a file boundary (KF8) ends it.
	mobiMinBlock = 64 << 10
)

// mobiNone is what a MOBI header's record-number field holds when it names
// no record.
const mobiNone = 0xffffffff

// mobiHeader is what record 0 of a MOBI book (or of a hybrid's KF8 part)
// says. Record numbers are relative to that record 0.
type mobiHeader struct {
	compression, textRecords, encryption    int
	textLength                              int64
	encoding, version                       uint32
	fullName                                []byte
	firstImage, huffRecord, huffCount, fdst uint32
	extraFlags                              uint16
	exth                                    []mobiEXTH
}

type mobiEXTH struct {
	kind  uint32
	value []byte
}

// parseMOBIHeader reads record 0. A field past the MOBI header's declared
// length is absent: it reads as mobiNone (or zero for the flags).
func parseMOBIHeader(rec0 []byte) (*mobiHeader, error) {
	if len(rec0) < 24 || !bytes.Equal(rec0[16:20], []byte("MOBI")) {
		return nil, refusedf("the book's record 0 has no MOBI header")
	}
	hl := int64(binary.BigEndian.Uint32(rec0[20:]))
	end := min(16+hl, int64(len(rec0)))
	u32 := func(off int64) uint32 {
		if off+4 > end {
			return mobiNone
		}
		return binary.BigEndian.Uint32(rec0[off:])
	}
	h := &mobiHeader{
		compression: int(binary.BigEndian.Uint16(rec0[0:])),
		textLength:  int64(binary.BigEndian.Uint32(rec0[4:])),
		textRecords: int(binary.BigEndian.Uint16(rec0[8:])),
		encryption:  int(binary.BigEndian.Uint16(rec0[12:])),
		encoding:    u32(28),
		version:     u32(36),
		firstImage:  u32(0x6c),
		huffRecord:  u32(0x70),
		huffCount:   u32(0x74),
		fdst:        u32(0xc0),
	}
	if hl >= 0xe4 && 0xf4 <= end {
		h.extraFlags = binary.BigEndian.Uint16(rec0[0xf2:])
	}
	if off, n := int64(u32(0x54)), int64(u32(0x58)); off != mobiNone && n != mobiNone && off+n <= int64(len(rec0)) {
		h.fullName = rec0[off : off+n]
	}
	if flags := u32(0x80); flags != mobiNone && flags&0x40 != 0 {
		h.exth = parseEXTH(rec0, 16+hl)
	}
	return h, nil
}

// parseEXTH reads the EXTH block at off: what does not fit in record 0 is
// left out, and at most maxMOBIEXTH entries are read.
func parseEXTH(rec0 []byte, off int64) []mobiEXTH {
	if off < 0 || off+12 > int64(len(rec0)) || !bytes.Equal(rec0[off:off+4], []byte("EXTH")) {
		return nil
	}
	n := min(binary.BigEndian.Uint32(rec0[off+8:]), maxMOBIEXTH)
	var out []mobiEXTH
	p := off + 12
	for i := uint32(0); i < n && p+8 <= int64(len(rec0)); i++ {
		kind := binary.BigEndian.Uint32(rec0[p:])
		size := int64(binary.BigEndian.Uint32(rec0[p+4:]))
		if size < 8 || p+size > int64(len(rec0)) {
			break
		}
		out = append(out, mobiEXTH{kind: kind, value: rec0[p+8 : p+size]})
		p += size
	}
	return out
}

// exthUint is the first EXTH entry of kind as a 32-bit number.
func (h *mobiHeader) exthUint(kind uint32) (uint32, bool) {
	for _, e := range h.exth {
		if e.kind == kind && len(e.value) == 4 {
			return binary.BigEndian.Uint32(e.value), true
		}
	}
	return 0, false
}

// text decodes a value of the book's own text encoding.
func (h *mobiHeader) text(b []byte) string {
	if h.encoding == 1252 {
		return string(decodeCP1252(b))
	}
	return string(bytes.ToValidUTF8(b, []byte("�")))
}

// metadata is what the EXTH block says (RFC GV §4.4), else the MOBI header's
// full name or the database's name as the title.
func (h *mobiHeader) metadata(ctx context.Context, dbName, format string) *BookMetadata {
	m := &BookMetadata{SourceFormat: format}
	for _, e := range h.exth {
		v := h.text(e.value)
		switch e.kind {
		case 100:
			m.Authors = append(m.Authors, v)
		case 101:
			m.Publisher = v
		case 103:
			// Often HTML: its text, its markup bounded before parsing.
			m.Description = htmlFragmentText(ctx, v)
		case 104:
			m.Identifiers = append(m.Identifiers, BookIdentifier{Scheme: "isbn", Value: v})
		case 105:
			m.Subjects = append(m.Subjects, v)
		case 106:
			m.Date = v
		case 113:
			m.Identifiers = append(m.Identifiers, BookIdentifier{Scheme: "asin", Value: v})
		case 503:
			m.Title = v
		case 524:
			m.Language = v
		}
		if len(m.Authors) > MaxMetaList || len(m.Subjects) > MaxMetaList || len(m.Identifiers) > MaxMetaList {
			break
		}
	}
	if strings.TrimSpace(m.Title) == "" {
		m.Title = h.text(h.fullName)
	}
	if strings.TrimSpace(m.Title) == "" {
		m.Title = h.text([]byte(dbName))
	}
	return m
}

// check refuses a header this reader cannot read: DRM, an unknown text
// encoding or compression.
func (h *mobiHeader) check() error {
	if h.encryption != 0 {
		return refusedf("the book is encrypted (DRM)")
	}
	if h.encoding != 1252 && h.encoding != 65001 {
		return refusedf("the book's text encoding %d is not windows-1252 or UTF-8", h.encoding)
	}
	switch h.compression {
	case palmStored, palmLZ77, palmHuffman:
		return nil
	}
	return refusedf("the book's compression %d is not one this reader knows", h.compression)
}

func (MOBI) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "mobi"}
	db, err := openPalmDB(ctx, r, size)
	if err != nil {
		return rep, err
	}
	switch db.kind {
	case "BOOKMOBI":
	case "TEXtREAd":
		// A .prc that is PalmDOC.
		return PalmDOC{}.Extract(ctx, r, size, sink, opts)
	default:
		return rep, refusedf("the file is not a MOBI book")
	}
	rec0, err := db.record(0, maxPalmRecord0, "header")
	if err != nil {
		return rep, err
	}
	h7, err := parseMOBIHeader(rec0)
	if err != nil {
		return rep, err
	}
	if err := h7.check(); err != nil {
		return rep, err
	}
	b := &mobiBook{db: db, opts: opts, rep: &rep, h: h7}
	if h7.firstImage != mobiNone {
		b.firstImage = int64(h7.firstImage)
	} else {
		b.firstImage = -1
	}
	if h7.version >= 8 {
		b.kf8 = true
	} else if err := b.findKF8(); err != nil {
		return rep, err
	}
	format := "mobi"
	if b.kf8 {
		format = "azw3"
	}
	rep.Format = format
	rep.Metadata = b.h.metadata(ctx, db.name, format).Clean()
	if rep.Metadata == nil || rep.Metadata.Title == "" {
		// A hybrid's KF8 part may carry no EXTH of its own.
		if m := h7.metadata(ctx, db.name, format).Clean(); m != nil {
			rep.Metadata = m
		}
	}
	if err := b.openText(); err != nil {
		return rep, err
	}
	b.title = title(opts.Name, "Book")
	if rep.Metadata != nil && rep.Metadata.Title != "" {
		b.title = rep.Metadata.Title
	}
	b.w = newWriter(ctx, sink, opts)
	err = b.run()
	b.noteSkips()
	rep.finish(b.w)
	return rep, err
}

// mobiBook is one book being read: the part read (MOBI 7, or KF8 with its
// record 0 at base), its text, and what was written.
type mobiBook struct {
	db   *palmDB
	opts Options
	rep  *Report
	w    *writer
	h    *mobiHeader // the header of the part read
	base int         // its record 0
	kf8  bool
	// firstImage is the record number of the first picture, -1 for none.
	firstImage int64
	text       *palmText
	title      string
	written    bool
	// pictures and pictureBytes count what was handed over;
	// skippedPictures what the caps left out; unreadable the blocks of
	// text the HTML parser could not read.
	pictures, skippedPictures, unreadable int
	pictureBytes                          int64
}

// findKF8 switches a hybrid file to its KF8 part: EXTH 121 names the KF8
// record 0, which follows a BOUNDARY record. A pointer that does not lead
// to one is not a hybrid, and the MOBI 7 part is read with a note.
func (b *mobiBook) findKF8() error {
	k, ok := b.h.exthUint(121)
	if !ok || k == mobiNone {
		return nil
	}
	i := int(k)
	if i < 2 || i >= b.db.records() {
		b.rep.note("the book names a KF8 part it does not hold; its MOBI 7 text was read")
		return nil
	}
	boundary, err := b.db.record(i-1, maxPalmRecord0, "boundary")
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(boundary, []byte("BOUNDARY")) {
		b.rep.note("the book names a KF8 part it does not hold; its MOBI 7 text was read")
		return nil
	}
	rec0, err := b.db.record(i, maxPalmRecord0, "header")
	if err != nil {
		return err
	}
	h8, err := parseMOBIHeader(rec0)
	if err != nil || h8.version < 8 {
		b.rep.note("the book's KF8 part has no KF8 header; its MOBI 7 text was read")
		return nil
	}
	if err := h8.check(); err != nil {
		return err
	}
	// A hybrid keeps its pictures once, in the MOBI 7 part, where that
	// part's header points; the KF8 header's own number is not used.
	b.h, b.base, b.kf8 = h8, i, true
	return nil
}

// openText prepares the text records of the part read: HUFF/CDIC tables
// when it is so compressed, and for KF8 the end of the text flow (the
// stylesheets that follow it are not text).
func (b *mobiBook) openText() error {
	h := b.h
	t := &palmText{
		db:          b.db,
		first:       b.base + 1,
		count:       h.textRecords,
		compression: h.compression,
		extraFlags:  h.extraFlags,
		limit:       h.textLength,
		maxText:     b.opts.maxPalmText(),
	}
	if err := t.check(b.db); err != nil {
		return err
	}
	if h.compression == palmHuffman {
		d, err := b.loadHuff()
		if err != nil {
			return err
		}
		t.huff = d
	}
	if b.kf8 {
		if end, ok := b.textFlowEnd(); ok {
			t.limit = min(t.limit, end)
		} else {
			b.rep.note("the book's flow table could not be read; all of its text records were read as text")
		}
	}
	b.text = t
	return nil
}

func (b *mobiBook) loadHuff() (*huffDict, error) {
	h := b.h
	if h.huffRecord == mobiNone || h.huffCount < 2 || h.huffCount > maxHuffRecords {
		return nil, refusedf("the book is HUFF/CDIC compressed and its HUFF records are not named")
	}
	first := b.base + int(h.huffRecord)
	if first <= b.base || first+int(h.huffCount) > b.db.records() {
		return nil, refusedf("the book's HUFF records are outside the book")
	}
	huff, err := b.db.record(first, maxHuffRecord, "HUFF")
	if err != nil {
		return nil, err
	}
	var cdics [][]byte
	for i := 1; i < int(h.huffCount); i++ {
		c, err := b.db.record(first+i, maxHuffRecord, "CDIC")
		if err != nil {
			return nil, err
		}
		cdics = append(cdics, c)
	}
	// The book's decoding may do a few times the work its text cap
	// allows: 4 MiB per record would let 65,535 records do 256 GiB.
	return loadHuff(b.db.ctx, huff, cdics, 4*b.opts.maxPalmText())
}

// textFlowEnd is where a KF8 book's first flow, its text, ends: the FDST
// record lists the flows as start and end offsets into the decompressed
// text.
func (b *mobiBook) textFlowEnd() (int64, bool) {
	if b.h.fdst == mobiNone {
		return 0, false
	}
	i := b.base + int(b.h.fdst)
	if i <= b.base || i >= b.db.records() {
		return 0, false
	}
	rec, err := b.db.record(i, maxPalmRecord0, "flow table")
	if err != nil || len(rec) < 12 || !bytes.Equal(rec[:4], []byte("FDST")) {
		return 0, false
	}
	at := int64(binary.BigEndian.Uint32(rec[4:]))
	if binary.BigEndian.Uint32(rec[8:]) == 0 || at+8 > int64(len(rec)) {
		return 0, false
	}
	start, end := binary.BigEndian.Uint32(rec[at:]), binary.BigEndian.Uint32(rec[at+4:])
	if start != 0 || end == 0 {
		return 0, false
	}
	return int64(end), true
}

// run writes the title, then the text a block at a time; a write the sink
// refuses ends the book there, and no record past that block is read.
func (b *mobiBook) run() error {
	if err := b.emit(heading(1, b.title)); err != nil {
		return err
	}
	split := mobiPageBreak
	if b.kf8 {
		split = kf8NextFile
	}
	for {
		chunk, err := b.text.chunk(split)
		if err != nil || chunk == nil {
			return err
		}
		if err := b.block(chunk); err != nil {
			return err
		}
	}
}

func (b *mobiBook) emit(md string) error {
	if b.written {
		md = "\n\n" + md
	}
	b.written = true
	return b.w.text(md)
}

// block reads one block of the book's HTML through the walker's body-only
// mode and writes it. A block the parser cannot read is skipped with a
// note; a refusal (a block past the parser's bounds) refuses the book.
func (b *mobiBook) block(chunk []byte) error {
	if b.h.encoding == 1252 {
		chunk = decodeCP1252(chunk)
	} else if !utf8.Valid(chunk) {
		chunk = bytes.ToValidUTF8(chunk, []byte("�"))
	}
	doc, err := parseHTMLPage(b.w.ctx, bytes.NewReader(chunk), int64(len(chunk)))
	if err != nil {
		if b.w.ctx.Err() != nil || IsRefused(err) {
			return err
		}
		b.unreadable++
		return nil
	}
	if !b.kf8 {
		mobiFontHeadings(doc)
	}
	h := &htmlState{w: b.w, rep: b.rep, body: &htmlBody{
		title:   b.title,
		picture: b.picture,
		ownRef:  b.pictureRef,
	}}
	if err := h.page(doc, maxCSSWork); err != nil {
		return err
	}
	if len(h.blocks) == 0 {
		return nil
	}
	err = h.write(b.written)
	b.written = true
	return err
}

func (b *mobiBook) noteSkips() {
	if b.skippedPictures > 0 {
		b.rep.note("%d pictures past the book's picture caps (%d pictures, %d bytes) were left out", b.skippedPictures, maxMOBIPictures, maxMOBIPictureBytes)
	}
	if b.unreadable > 0 {
		b.rep.note("%d blocks of the book's text could not be read and were skipped", b.unreadable)
	}
}

// pictureRef names the picture an <img> points to by the book's own means:
// MOBI 7's recindex attribute (counted from 1) or KF8's kindle:embed URL (a
// base-32 number, counted from 1, on an <img> or an SVG <image>). The name is "record:<n>", n the record
// it resolves to, or "record:-1" when it resolves to none.
func (b *mobiBook) pictureRef(n *html.Node) (string, bool) {
	var k int64
	if v := attrOf(n, "recindex"); v != "" {
		i, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
		if err != nil {
			return "record:-1", true
		}
		k = i
	} else if src := cmp.Or(imageSrc(n), svgHref(n)); len(src) > len("kindle:embed:") && strings.EqualFold(src[:len("kindle:embed:")], "kindle:embed:") {
		digits := src[len("kindle:embed:"):]
		if i := strings.IndexAny(digits, "?#"); i >= 0 {
			digits = digits[:i]
		}
		i, err := strconv.ParseInt(digits, 32, 32)
		if err != nil || len(digits) > 6 {
			return "record:-1", true
		}
		k = i
	} else {
		return "", false
	}
	if b.firstImage < 0 || k < 1 {
		return "record:-1", true
	}
	return "record:" + strconv.FormatInt(b.firstImage+k-1, 10), true
}

// picture reads the picture record pictureRef named, while pictures are
// kept and under the caps. A record that is not a picture (a font, a
// resource) is not passed on.
func (b *mobiBook) picture(ref string) ([]byte, bool) {
	if !b.opts.Images {
		return nil, false
	}
	// 32 bits: a record index is at most 65,535 (the PalmDB count is 16
	// bits), and the bound below is checked before any conversion to int.
	i, err := strconv.ParseInt(strings.TrimPrefix(ref, "record:"), 10, 32)
	if err != nil || b.firstImage < 0 || i < b.firstImage || i >= int64(b.db.records()) {
		return nil, false
	}
	n := b.db.size(int(i))
	if b.pictures >= maxMOBIPictures || b.pictureBytes+n > maxMOBIPictureBytes {
		b.skippedPictures++
		return nil, false
	}
	data, err := b.db.record(int(i), maxMOBIPicture, "picture")
	if err != nil {
		return nil, false
	}
	if mt, _ := imageType(data); mt == "" {
		return nil, false
	}
	b.pictures++
	b.pictureBytes += n
	return data, true
}

// mobiPageBreak splits MOBI 7 text before a <mbp:pagebreak>, the first
// one at least mobiMinBlock into the block: a book of a page break every
// few bytes is parsed in blocks of that size, not one tiny page at a time.
func mobiPageBreak(b []byte, from int) int {
	return indexFold(b, max(from, mobiMinBlock), "<mbp:pagebreak")
}

// kf8NextFile splits KF8 text where an XHTML file's skeleton starts (an XML
// declaration, a doctype or an <html> tag), the first one at least
// mobiMinBlock into the block. Files after the first in a block parse into
// its body, their heads adding nothing the walker writes. Each marker is
// searched for once, from where the last search stopped, so the scan is
// linear in the text however many markers it holds.
func kf8NextFile(b []byte, from int) int {
	from = max(from, mobiMinBlock)
	i := -1
	for _, m := range []string{"<?xml", "<!doctype", "<html"} {
		end := len(b)
		if i >= 0 {
			end = i // only an earlier match can win
		}
		if j := indexFold(b[:end], from, m); j >= 0 {
			i = j
		}
	}
	return i
}

// mobiFontHeadings turns MOBI 7's headings back into headings. Mobipocket
// HTML has none: a converter writes a heading as a paragraph whose whole
// text is in a large font, <p><font size="5"><b>Chapter</b></font></p>.
// Such a block, one line of at most maxMOBIFontHeading characters, in font
// size 4 or more and bold (or 5 or more), becomes h1 for size 6 or 7, h2
// for 5 and h3 for 4: the sizes Calibre gives h1, h2 and h3.
func mobiFontHeadings(doc *html.Node) {
	budget := maxXMLNodes
	var walk func(n *html.Node, depth int)
	walk = func(n *html.Node, depth int) {
		if depth > maxXMLDepth {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if budget--; budget < 0 {
				return
			}
			if c.Type != html.ElementNode {
				continue
			}
			if (c.Data == "p" || c.Data == "div") && fontHeading(c) {
				continue
			}
			walk(c, depth+1)
		}
	}
	walk(doc, 0)
}

// fontHeading rewrites block p into a heading when its content is one run
// in a large font, and reports whether it did.
func fontHeading(p *html.Node) bool {
	size, bold := 0, false
	n := p
	for {
		only := soleElement(n)
		if only == nil {
			break
		}
		switch only.Data {
		case "font":
			if s, err := strconv.Atoi(strings.TrimSpace(attrOf(only, "size"))); err == nil {
				size = max(size, s)
			}
		case "b", "strong":
			bold = true
		case "i", "em", "u", "span":
		default:
			return false
		}
		n = only
	}
	if size < 4 || (size == 4 && !bold) {
		return false
	}
	t := oneLine(textOf(p))
	if t == "" || utf8.RuneCountInString(t) > maxMOBIFontHeading {
		return false
	}
	level := 3
	switch {
	case size >= 6:
		level = 1
	case size == 5:
		level = 2
	}
	p.Data = "h" + strconv.Itoa(level)
	p.DataAtom = [...]atom.Atom{atom.H1, atom.H2, atom.H3}[level-1]
	return true
}

// soleElement is n's one element child when everything else in n is
// whitespace, else nil.
func soleElement(n *html.Node) *html.Node {
	var only *html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode && strings.TrimSpace(c.Data) == "" {
			continue
		}
		if c.Type != html.ElementNode || only != nil {
			return nil
		}
		only = c
	}
	return only
}
