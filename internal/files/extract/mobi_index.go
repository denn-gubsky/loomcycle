package extract

import (
	"bytes"
	"encoding/binary"
	"math/bits"
)

// A KF8 book keeps its tables in indexes (RFC GV §4.4): the contents (NCX),
// the skeleton of each XHTML file and the fragments that fill them. An
// index is a header record, then its data records, then the CNCX records
// that hold its strings. The header's TAGX section says which tags an entry
// may carry and how a control byte marks them; each data record lists its
// entries through an IDXT table of offsets. Written from the public
// description (wiki.mobileread.com, "MOBI", "Index"), and checked against
// Calibre's own reader of the books it writes.
//
// The book's text needs none of it, so an index that breaks a bound below
// is refused and the book is read without it, with a note.

// Bounds on one index.
const (
	// maxMOBIIndexRecords caps an index's data records, maxMOBICNCXRecords
	// its string records.
	maxMOBIIndexRecords = 256
	maxMOBICNCXRecords  = 128
	// maxMOBIContents caps the contents (NCX) entries read, and
	// maxMOBIContentsDepth how deep they nest.
	maxMOBIContents      = 5000
	maxMOBIContentsDepth = 16
	// maxKF8Parts caps the skeleton and fragment entries: a part of a
	// book is a file or a fragment of one, and a book of more is read
	// without them.
	maxKF8Parts = 65535
	// maxMOBILabel caps one CNCX string, in bytes.
	maxMOBILabel = 1024
	// maxMOBITagX caps the tag definitions of a TAGX section, and
	// maxMOBIControlBytes the control bytes of an entry.
	maxMOBITagX         = 64
	maxMOBIControlBytes = 4
)

// mobiIndex is one index read: its entries' tag values, in order, and its
// string records.
type mobiIndex struct {
	entries []mobiIndexEntry
	cncx    [][]byte
	// encoding is the index's own text encoding (1252 or UTF-8).
	encoding uint32
}

type mobiIndexEntry []mobiTagValues

type mobiTagValues struct {
	tag  byte
	vals []uint32
}

// value is the i-th value of tag, false when the entry has no such value.
func (e mobiIndexEntry) value(tag byte, i int) (uint32, bool) {
	for _, t := range e {
		if t.tag == tag {
			if i < len(t.vals) {
				return t.vals[i], true
			}
			return 0, false
		}
	}
	return 0, false
}

type mobiTagX struct {
	tag, values, mask byte
	eof               bool
}

// readMOBIIndex reads the index whose header is record first: at most
// maxEntries entries, its records all inside the database.
func readMOBIIndex(db *palmDB, first, maxEntries int) (*mobiIndex, error) {
	head, err := db.record(first, maxPalmRecord0, "index")
	if err != nil {
		return nil, err
	}
	if len(head) < 184 || !bytes.Equal(head[:4], []byte("INDX")) {
		return nil, refusedf("the index header record %d is not an INDX record", first)
	}
	u32 := func(b []byte, off int) uint32 { return binary.BigEndian.Uint32(b[off:]) }
	count, ncncx := u32(head, 24), u32(head, 52)
	if count == 0 || count > maxMOBIIndexRecords {
		return nil, refusedf("the index declares %d data records, not 1 to %d", count, maxMOBIIndexRecords)
	}
	if ncncx > maxMOBICNCXRecords {
		return nil, refusedf("the index declares %d string records, more than %d", ncncx, maxMOBICNCXRecords)
	}
	if int64(first)+1+int64(count)+int64(ncncx) > int64(db.records()) {
		return nil, refusedf("the index's records run past the book")
	}
	cbc, tags, err := parseTagX(head, int64(u32(head, 180)))
	if err != nil {
		return nil, err
	}
	ix := &mobiIndex{encoding: u32(head, 28)}
	for i := 1; i <= int(count); i++ {
		rec, err := db.record(first+i, maxPalmRecord0, "index")
		if err != nil {
			return nil, err
		}
		if ix.entries, err = readIndexRecord(rec, cbc, tags, ix.entries, maxEntries); err != nil {
			return nil, err
		}
	}
	for i := 0; i < int(ncncx); i++ {
		rec, err := db.record(first+1+int(count)+i, maxPalmRecord0, "index string")
		if err != nil {
			return nil, err
		}
		ix.cncx = append(ix.cncx, rec)
	}
	return ix, nil
}

// parseTagX reads the TAGX section at off: the control byte count and the
// tag definitions, each four bytes (tag, values per entry, bit mask, end of
// a control byte).
func parseTagX(head []byte, off int64) (int, []mobiTagX, error) {
	if off < 0 || off+12 > int64(len(head)) || !bytes.Equal(head[off:off+4], []byte("TAGX")) {
		return 0, nil, refusedf("the index has no TAGX section")
	}
	size := int64(binary.BigEndian.Uint32(head[off+4:]))
	cbc := int64(binary.BigEndian.Uint32(head[off+8:]))
	if size < 12 || (size-12)%4 != 0 || off+size > int64(len(head)) || (size-12)/4 > maxMOBITagX {
		return 0, nil, refusedf("the index's TAGX section is %d bytes, not 12 plus up to %d tags of 4", size, maxMOBITagX)
	}
	if cbc < 1 || cbc > maxMOBIControlBytes {
		return 0, nil, refusedf("the index declares %d control bytes, not 1 to %d", cbc, maxMOBIControlBytes)
	}
	var tags []mobiTagX
	eofs := int64(0)
	for p := off + 12; p < off+size; p += 4 {
		t := mobiTagX{tag: head[p], values: head[p+1], mask: head[p+2], eof: head[p+3]&1 != 0}
		if t.eof {
			eofs++
		} else if t.values == 0 || t.mask == 0 {
			return 0, nil, refusedf("the index's tag %d has no values or no mask", t.tag)
		}
		tags = append(tags, t)
	}
	// Each control byte's tags end with an end marker: one per byte, the
	// last tag being one, so every tag has its control byte.
	if eofs != cbc {
		return 0, nil, refusedf("the index's TAGX section ends %d control bytes, and declares %d", eofs, cbc)
	}
	if !tags[len(tags)-1].eof {
		return 0, nil, refusedf("the index's TAGX section has a tag after its last control byte")
	}
	return int(cbc), tags, nil
}

// readIndexRecord appends the entries of one data record: each between its
// IDXT offset and the next (the last ending at the IDXT table), offsets
// that strictly increase inside the record.
func readIndexRecord(rec []byte, cbc int, tags []mobiTagX, out []mobiIndexEntry, maxEntries int) ([]mobiIndexEntry, error) {
	if len(rec) < 28 || !bytes.Equal(rec[:4], []byte("INDX")) {
		return nil, refusedf("an index data record is not an INDX record")
	}
	idxt := int64(binary.BigEndian.Uint32(rec[20:]))
	n := int64(binary.BigEndian.Uint32(rec[24:]))
	if n > int64(maxEntries-len(out)) {
		return nil, refusedf("the index holds more than %d entries", maxEntries)
	}
	if idxt < 28 || idxt+4+2*n > int64(len(rec)) || !bytes.Equal(rec[idxt:idxt+4], []byte("IDXT")) {
		return nil, refusedf("an index data record's IDXT table is outside the record")
	}
	for j := int64(0); j < n; j++ {
		start := int64(binary.BigEndian.Uint16(rec[idxt+4+2*j:]))
		end := idxt
		if j+1 < n {
			end = int64(binary.BigEndian.Uint16(rec[idxt+4+2*(j+1):]))
		}
		// Each entry ends where the next starts, so ends past their starts
		// keep the entries in order and apart.
		if start < 28 || end <= start || end > idxt {
			return nil, refusedf("an index entry's offsets are out of order or outside its record")
		}
		e, err := parseIndexEntry(rec[start:end], cbc, tags)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// parseIndexEntry reads one entry: its name (a length byte and the
// bytes, not used here), its control bytes, then each tag's values as
// forward variable-width integers. A tag whose masked bits are all set,
// in a mask of more than one bit, is followed by the byte length of its
// values instead of a count.
func parseIndexEntry(data []byte, cbc int, tags []mobiTagX) (mobiIndexEntry, error) {
	if len(data) < 1 || 1+int(data[0])+cbc > len(data) {
		return nil, refusedf("an index entry is shorter than its name and control bytes")
	}
	data = data[1+int(data[0]):]
	ctrl, data := data[:cbc], data[cbc:]
	type pending struct {
		tag             mobiTagX
		count, byteSize int
	}
	var ps []pending
	ci := 0
	for _, t := range tags {
		if t.eof {
			ci++
			continue
		}
		v := ctrl[ci] & t.mask
		if v == 0 {
			continue
		}
		p := pending{tag: t}
		if v == t.mask && bits.OnesCount8(t.mask) > 1 {
			n, used, ok := forwardVarint(data)
			if !ok {
				return nil, refusedf("an index entry's value size runs past the entry")
			}
			data = data[used:]
			p.byteSize = int(n)
		} else {
			p.count = int(v >> bits.TrailingZeros8(t.mask))
		}
		ps = append(ps, p)
	}
	var e mobiIndexEntry
	for _, p := range ps {
		tv := mobiTagValues{tag: p.tag.tag}
		if p.byteSize > 0 {
			if p.byteSize > len(data) {
				return nil, refusedf("an index entry's values run past the entry")
			}
			for used := 0; used < p.byteSize; {
				v, n, ok := forwardVarint(data[:p.byteSize-used])
				if !ok {
					return nil, refusedf("an index entry's values run past their size")
				}
				tv.vals = append(tv.vals, v)
				data, used = data[n:], used+n
			}
		} else {
			// Each value takes at least a byte: a count past what the
			// entry holds runs out of bytes and is refused.
			for k := 0; k < p.count*int(p.tag.values); k++ {
				v, n, ok := forwardVarint(data)
				if !ok {
					return nil, refusedf("an index entry's values run past the entry")
				}
				tv.vals = append(tv.vals, v)
				data = data[n:]
			}
		}
		e = append(e, tv)
	}
	return e, nil
}

// forwardVarint reads a forward variable-width integer: seven bits a byte,
// most significant first, the byte with its high bit set the last. At most
// four bytes are read (28 bits).
func forwardVarint(b []byte) (uint32, int, bool) {
	var v uint32
	for i := 0; i < len(b) && i < 4; i++ {
		v = v<<7 | uint32(b[i]&0x7f)
		if b[i]&0x80 != 0 {
			return v, i + 1, true
		}
	}
	return 0, 0, false
}

// label is the CNCX string at off: the record off>>16, a length (a
// forward variable-width integer) then the bytes. A string longer than
// maxMOBILabel refuses the index.
func (ix *mobiIndex) label(off uint32) (string, error) {
	r, p := int(off>>16), int(off&0xffff)
	if r >= len(ix.cncx) || p >= len(ix.cncx[r]) {
		return "", refusedf("an index string at %d is outside the index's strings", off)
	}
	rec := ix.cncx[r][p:]
	n, used, ok := forwardVarint(rec)
	if !ok || int64(used)+int64(n) > int64(len(rec)) {
		return "", refusedf("an index string at %d runs past its record", off)
	}
	if n > maxMOBILabel {
		return "", refusedf("an index string at %d is %d bytes, more than %d", off, n, maxMOBILabel)
	}
	b := rec[used : used+int(n)]
	if ix.encoding == 1252 {
		return string(decodeCP1252(b)), nil
	}
	return string(bytes.ToValidUTF8(b, []byte("�"))), nil
}
