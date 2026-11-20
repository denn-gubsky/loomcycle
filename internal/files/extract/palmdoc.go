package extract

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// A Palm database (PDB) is the container MOBI, AZW, AZW3, PRC and PalmDOC
// books share (RFC GV §4.4): a 78-byte header, then a list of records, each
// an offset into the file. Record 0 describes the book; the text follows in
// records of a few KiB, each compressed on its own. Written from the public
// description of the format (wiki.mobileread.com, "PDB" and "PalmDOC").

// PalmDOC reads a PalmDOC e-text (type and creator "TEXtREAd"): plain text
// in records, stored or compressed with PalmDOC's LZ77. It has no markup, so
// each block of lines is a paragraph under the database's name as the title.
type PalmDOC struct{}

const palmDOCMediaType = "application/x-palm-database"

func (PalmDOC) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{palmDOCMediaType}, []string{".pdb"})
}

// Bounds on a Palm database and its text.
const (
	palmHeaderSize = 78
	// maxPalmRecord0 caps the header record: a MOBI book's is a few KiB
	// (Calibre pads it to about 9).
	maxPalmRecord0 = 1 << 20
	// maxPalmTextRecord caps one text record as stored, and
	// maxPalmTextOut what it decompresses to: the record size a book
	// declares is 4,096 by convention, and one declared past
	// maxPalmTextOut is refused.
	maxPalmTextRecord = 128 << 10
	maxPalmTextOut    = 64 << 10
	// maxPalmText caps a book's text decompressed, all records together.
	maxPalmText = 64 << 20
	// palmChunk is the most text one block of a book is read in: a block
	// ends at a natural boundary (a page break, a file of a KF8 book, a
	// blank line) or, failing one, near this size.
	palmChunk = 1 << 20
)

// palmDB is a Palm database's record list: record i is the file's bytes
// from offs[i] to offs[i+1], the last ending at the file's end.
type palmDB struct {
	ctx  context.Context
	r    io.ReaderAt
	name string // the database name, as stored (NUL-terminated)
	kind string // type and creator, 8 bytes
	offs []int64
}

// openPalmDB reads the header and the record list. A list whose offsets do
// not strictly increase or leave the file is refused: records are read by
// these offsets, and an overlapping or out-of-order list is what a crafted
// file uses to make one record be read many times.
func openPalmDB(ctx context.Context, r io.ReaderAt, size int64) (*palmDB, error) {
	if size < palmHeaderSize {
		return nil, refusedf("the file is too short for a Palm database")
	}
	head := make([]byte, palmHeaderSize)
	if _, err := r.ReadAt(head, 0); err != nil {
		return nil, fmt.Errorf("extract: reading the Palm database header: %w", err)
	}
	n := int(binary.BigEndian.Uint16(head[76:]))
	if n == 0 {
		return nil, refusedf("the Palm database holds no records")
	}
	listEnd := int64(palmHeaderSize + 8*n)
	if listEnd > size {
		return nil, refusedf("the Palm database's record list runs past the file")
	}
	list := make([]byte, 8*n)
	if _, err := r.ReadAt(list, palmHeaderSize); err != nil {
		return nil, fmt.Errorf("extract: reading the Palm database's record list: %w", err)
	}
	db := &palmDB{ctx: ctx, r: r, kind: string(head[60:68]), offs: make([]int64, n+1)}
	name := head[:32]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	db.name = string(name)
	prev := listEnd - 1
	for i := 0; i < n; i++ {
		off := int64(binary.BigEndian.Uint32(list[8*i:]))
		if off <= prev || off >= size {
			return nil, refusedf("the Palm database's record offsets are out of order or outside the file (record %d)", i)
		}
		db.offs[i] = off
		prev = off
	}
	db.offs[n] = size
	return db, nil
}

func (db *palmDB) records() int { return len(db.offs) - 1 }

// size is record i's length; 0 for a record that does not exist.
func (db *palmDB) size(i int) int64 {
	if i < 0 || i >= db.records() {
		return 0
	}
	return db.offs[i+1] - db.offs[i]
}

// record reads record i, refused when it is longer than limit.
func (db *palmDB) record(i int, limit int64, what string) ([]byte, error) {
	if err := db.ctx.Err(); err != nil {
		return nil, err
	}
	if i < 0 || i >= db.records() {
		return nil, refusedf("the book names %s record %d, and holds %d records", what, i, db.records())
	}
	n := db.size(i)
	if n > limit {
		return nil, refusedf("the book's %s record %d is %d bytes, more than %d", what, i, n, limit)
	}
	b := make([]byte, n)
	if _, err := db.r.ReadAt(b, db.offs[i]); err != nil && err != io.EOF {
		return nil, fmt.Errorf("extract: reading record %d: %w", i, err)
	}
	return b, nil
}

// PalmDOC's compression field.
const (
	palmStored  = 1
	palmLZ77    = 2
	palmHuffman = 17480 // HUFF/CDIC, MOBI only
)

// palmLZ77Decode decompresses one PalmDOC record (LZ77 with PalmDOC's byte
// codes) and appends it to out. It refuses a copy reaching before the start
// of the record's output and output past limit bytes: a record never
// legitimately decompresses past its declared size.
func palmLZ77Decode(out, in []byte, limit int) ([]byte, error) {
	start := len(out)
	for i := 0; i < len(in); {
		c := in[i]
		i++
		switch {
		case c == 0 || (c >= 0x09 && c <= 0x7f):
			out = append(out, c)
		case c <= 0x08:
			// The next c bytes are literal.
			n := int(c)
			if i+n > len(in) {
				return nil, refusedf("a text record's literal run ends past the record")
			}
			out = append(out, in[i:i+n]...)
			i += n
		case c >= 0xc0:
			// A space and then the character c ^ 0x80.
			out = append(out, ' ', c^0x80)
		default:
			// 0x80..0xbf: two bytes, an 11-bit distance back into this
			// record's output and a length of 3 to 10.
			if i >= len(in) {
				return nil, refusedf("a text record ends inside a back-reference")
			}
			pair := int(c)<<8 | int(in[i])
			i++
			dist, n := (pair>>3)&0x7ff, pair&7+3
			if dist == 0 || dist > len(out)-start {
				return nil, refusedf("a text record refers back before its start")
			}
			// Byte by byte: the copy may overlap what it writes.
			for k := 0; k < n; k++ {
				out = append(out, out[len(out)-dist])
			}
		}
		if len(out)-start > limit {
			return nil, refusedf("a text record decompresses past %d bytes", limit)
		}
	}
	return out, nil
}

// trimTrailing strips the trailing entries a MOBI text record may carry
// after its compressed text, as the header's extra-data flags say: for each
// flag bit 1..15 set, an entry whose size (itself included) is a
// variable-width integer read backward from its last byte; then, for bit 0,
// the bytes of a character split across records, counted in the low two
// bits of the last byte, plus that byte.
func trimTrailing(rec []byte, flags uint16) ([]byte, error) {
	for bit := 1; bit < 16; bit++ {
		if flags&(1<<bit) == 0 {
			continue
		}
		n, ok := backwardVarint(rec)
		if !ok || n > len(rec) {
			return nil, refusedf("a text record's trailing entry is larger than the record")
		}
		rec = rec[:len(rec)-n]
	}
	if flags&1 != 0 {
		if len(rec) == 0 {
			return nil, refusedf("a text record's trailing entry is larger than the record")
		}
		n := int(rec[len(rec)-1]&3) + 1
		if n > len(rec) {
			return nil, refusedf("a text record's trailing entry is larger than the record")
		}
		rec = rec[:len(rec)-n]
	}
	return rec, nil
}

// backwardVarint reads the variable-width integer that ends rec: seven bits
// a byte, read from the last byte backward, the byte with its high bit set
// being the integer's first. At most four bytes are read.
func backwardVarint(rec []byte) (int, bool) {
	v, shift := 0, 0
	for i := len(rec) - 1; i >= 0 && i >= len(rec)-4; i-- {
		b := rec[i]
		v |= int(b&0x7f) << shift
		shift += 7
		if b&0x80 != 0 {
			break
		}
	}
	return v, len(rec) > 0
}

// palmText decodes a book's text records in order, one at a time and only
// as far as the reader asks: a book whose output is cut is not
// decompressed past the record that filled it.
type palmText struct {
	db *palmDB
	// first and count are the text records, count of them from first.
	first, count int
	compression  int
	extraFlags   uint16
	huff         *huffDict
	// limit is the text to keep: the length the book declares (and, for a
	// KF8 book, the end of its text flow), at most maxText.
	limit, maxText int64
	next           int   // records read so far
	total          int64 // bytes decoded so far
	taken          int64 // bytes handed out by chunk so far
	buf            []byte
}

// more decodes the next text record into buf, reporting false when the
// text is done.
func (t *palmText) more() (bool, error) {
	if t.next >= t.count || t.total >= t.limit {
		return false, nil
	}
	i := t.first + t.next
	t.next++
	rec, err := t.db.record(i, maxPalmTextRecord, "text")
	if err != nil {
		return false, err
	}
	if rec, err = trimTrailing(rec, t.extraFlags); err != nil {
		return false, err
	}
	before := len(t.buf)
	switch t.compression {
	case palmStored:
		if len(rec) > maxPalmTextOut {
			return false, refusedf("a text record is over %d bytes", maxPalmTextOut)
		}
		t.buf = append(t.buf, rec...)
	case palmLZ77:
		if t.buf, err = palmLZ77Decode(t.buf, rec, maxPalmTextOut); err != nil {
			return false, err
		}
	case palmHuffman:
		if t.buf, err = t.huff.decode(t.buf, rec, maxPalmTextOut); err != nil {
			return false, err
		}
	default:
		return false, refusedf("the book's compression %d is not one this reader knows", t.compression)
	}
	// The declared length is at most maxText (check), and text past it is
	// dropped here, so the book's text never exceeds the cap.
	n := int64(len(t.buf) - before)
	if t.total+n > t.limit {
		// The last record past the declared length holds padding or, in
		// a KF8 book, the flows after the text.
		n = t.limit - t.total
		t.buf = t.buf[:before+int(n)]
	}
	t.total += n
	return true, nil
}

// chunk returns the next block of text: the bytes before the first
// boundary split finds past the start, or, with none within palmChunk
// bytes, a cut near that size; nil when the text is done. split returns
// the boundary's index in b, or -1; from is where a boundary may begin,
// since the bytes before it were searched already.
func (t *palmText) chunk(split func(b []byte, from int) int) ([]byte, error) {
	searched := 1
	for {
		if i := split(t.buf, searched); i > 0 {
			return t.take(i), nil
		}
		if len(t.buf) >= palmChunk {
			return t.take(cutPoint(t.buf[:palmChunk])), nil
		}
		searched = max(1, len(t.buf)-32)
		ok, err := t.more()
		if err != nil {
			return nil, err
		}
		if !ok {
			if len(t.buf) == 0 {
				return nil, nil
			}
			return t.take(len(t.buf)), nil
		}
	}
}

// take removes and returns the first n bytes of buf.
func (t *palmText) take(n int) []byte {
	out := append([]byte(nil), t.buf[:n]...)
	t.buf = append(t.buf[:0], t.buf[n:]...)
	t.taken += int64(n)
	return out
}

// cutPoint is where a block with no natural boundary is cut: after the
// last closing paragraph tag, else after the last tag, else after the last
// line, else at a character boundary.
func cutPoint(b []byte) int {
	for _, end := range []string{"</p>", "</P>"} {
		if i := bytes.LastIndex(b, []byte(end)); i > len(b)/2 {
			return i + len(end)
		}
	}
	if i := bytes.LastIndexByte(b, '>'); i > len(b)/2 {
		return i + 1
	}
	if i := bytes.LastIndexByte(b, '\n'); i > len(b)/2 {
		return i + 1
	}
	n := len(b)
	for k := 0; k < 3 && n > 1 && !utf8.RuneStart(b[n-1]); k++ {
		n--
	}
	if n > 1 && !utf8.RuneStart(b[n-1]) {
		return len(b) // not UTF-8: any byte is a boundary
	}
	if n > 1 && b[n-1] >= 0xc0 {
		n-- // the lead byte of a character the cut would split
	}
	return n
}

// indexFold is the index of the first case-insensitive match of an ASCII
// marker in b at or after from, or -1.
func indexFold(b []byte, from int, marker string) int {
	if from >= len(b) {
		return -1
	}
	first := marker[0]
	for i := from; i+len(marker) <= len(b); i++ {
		if c := b[i]; c != first && c|0x20 != first {
			continue
		}
		if bytes.EqualFold(b[i:i+len(marker)], []byte(marker)) {
			return i
		}
	}
	return -1
}

// decodeCP1252 is b as UTF-8: windows-1252 bytes decoded (every byte maps
// to a character, so nothing can fail).
func decodeCP1252(b []byte) []byte {
	out, err := charmap.Windows1252.NewDecoder().Bytes(b)
	if err != nil {
		return bytes.ToValidUTF8(b, []byte("�"))
	}
	return out
}

func (PalmDOC) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "palmdoc"}
	db, err := openPalmDB(ctx, r, size)
	if err != nil {
		return rep, err
	}
	if db.kind != "TEXtREAd" {
		return rep, refusedf("the file is not a PalmDOC e-text")
	}
	rec0, err := db.record(0, maxPalmRecord0, "header")
	if err != nil {
		return rep, err
	}
	if len(rec0) < 16 {
		return rep, refusedf("the e-text's header record is too short")
	}
	t := &palmText{
		db:          db,
		first:       1,
		count:       int(binary.BigEndian.Uint16(rec0[8:])),
		compression: int(binary.BigEndian.Uint16(rec0[0:])),
		limit:       int64(binary.BigEndian.Uint32(rec0[4:])),
		maxText:     opts.maxPalmText(),
	}
	if t.compression != palmStored && t.compression != palmLZ77 {
		return rep, refusedf("the e-text's compression %d is not PalmDOC's", t.compression)
	}
	if err := t.check(db); err != nil {
		return rep, err
	}
	name := metaString(string(decodeCP1252([]byte(db.name))), MaxMetaString)
	rep.Metadata = (&BookMetadata{Title: name, SourceFormat: "palmdoc"}).Clean()
	w := newWriter(ctx, sink, opts)
	err = writePalmDOC(w, t, name, opts)
	rep.finish(w)
	return rep, err
}

// check refuses a text-record range the database cannot hold and a
// declared length past the text cap. The declared record size needs no
// check: it is a 16-bit field, so never past maxPalmTextOut, the bound each
// record's output is held to.
func (t *palmText) check(db *palmDB) error {
	if t.count < 0 || t.first+t.count > db.records() {
		return refusedf("the book declares %d text records, and holds %d records", t.count, db.records())
	}
	if t.limit > t.maxText {
		return refusedf("the book declares %d bytes of text, more than %d", t.limit, t.maxText)
	}
	return nil
}

// writePalmDOC writes an e-text's title and then its text a block at a
// time, blocks ending on a blank line where one comes: a run of lines is a
// paragraph, each line kept on its own. It stops at the first write the
// sink refuses and decompresses nothing after it.
func writePalmDOC(w *writer, t *palmText, name string, opts Options) error {
	// The database's name is the text's own title, not a file name: no
	// extension is taken off it.
	if name == "" {
		name = title(opts.Name, "Text")
	}
	if err := w.text(heading(1, name)); err != nil {
		return err
	}
	blankLine := func(b []byte, from int) int {
		if i := bytes.Index(b[max(0, from-1):], []byte("\n\n")); i >= 0 {
			return max(0, from-1) + i + 2
		}
		return -1
	}
	for {
		b, err := t.chunk(blankLine)
		if err != nil || b == nil {
			return err
		}
		if !utf8.Valid(b) {
			b = decodeCP1252(b)
		}
		var paras, cur []string
		end := func() {
			if len(cur) > 0 {
				paras = append(paras, strings.Join(cur, "\n"))
				cur = nil
			}
		}
		for _, l := range strings.Split(normalizeNewlines(string(b)), "\n") {
			if l = strings.TrimRight(escapeLine(l), " \t"); l == "" {
				end()
				continue
			}
			cur = append(cur, l)
		}
		end()
		if len(paras) > 0 {
			if err := w.text("\n\n" + strings.Join(paras, "\n\n")); err != nil {
				return err
			}
		}
	}
}
