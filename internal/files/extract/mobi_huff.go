package extract

import (
	"bytes"
	"encoding/binary"
)

// HUFF/CDIC is the compression of MOBI books made by Amazon's own tools
// (compression type 17480), written here from its public description
// (wiki.mobileread.com, "MOBI", "HUFF/CDIC"). A HUFF record holds the code
// tables of a canonical Huffman code; CDIC records hold the phrases the
// codes index. A phrase is either literal text or itself HUFF-coded, so
// decoding one may decode others: the classic expansion bomb. Decoding is
// bounded by the depth of that recursion, the output of each record, and
// the work all the expansions of one record may do together.

// Bounds on a HUFF/CDIC book.
const (
	// maxHuffDepth bounds how deep one phrase may expand into others.
	maxHuffDepth = 32
	// maxHuffWork bounds the codes one text record's decoding may read and
	// the bytes it may produce, at every depth together: what a phrase that
	// expands into many copies of another, each expanding again, would
	// otherwise multiply.
	maxHuffWork = 4 << 20
	// maxHuffRecord caps the HUFF record and each CDIC record, and
	// maxHuffRecords their number.
	maxHuffRecord  = 1 << 20
	maxHuffRecords = 256
	// maxHuffCache caps the expanded phrases kept for reuse; past it a
	// phrase is expanded again where it is used, under maxHuffWork.
	maxHuffCache = 16 << 20
)

// huffCode is one entry of the HUFF record's 256-entry table, indexed by a
// code's first eight bits: the code's length, whether that length is
// final, and the largest code of that length, left-aligned to 32 bits.
type huffCode struct {
	length  int
	term    bool
	maxCode uint64
}

type huffDict struct {
	first [256]huffCode
	// minCode and maxCode are, by code length 1..32, the smallest and the
	// largest code, left-aligned to 32 bits.
	minCode, maxCode [33]uint64
	phrases          [][]byte
	literal          []bool
	// expanded is a phrase's decoded text once decoded; expanding marks
	// one being decoded, so a phrase that refers to itself is refused.
	expanded  [][]byte
	expanding []bool
	cached    int
}

// loadHuff reads the HUFF record and the CDIC records after it.
func loadHuff(huff []byte, cdics [][]byte) (*huffDict, error) {
	if len(huff) < 24 || !bytes.Equal(huff[:4], []byte("HUFF")) {
		return nil, refusedf("the book's HUFF record is not one")
	}
	firstAt := int64(binary.BigEndian.Uint32(huff[8:]))
	limitsAt := int64(binary.BigEndian.Uint32(huff[12:]))
	if firstAt+256*4 > int64(len(huff)) || limitsAt+64*4 > int64(len(huff)) {
		return nil, refusedf("the book's HUFF tables run past the record")
	}
	d := &huffDict{}
	for i := range d.first {
		v := binary.BigEndian.Uint32(huff[firstAt+int64(4*i):])
		n := int(v & 0x1f)
		if n == 0 {
			return nil, refusedf("the book's HUFF table has a code of length 0")
		}
		d.first[i] = huffCode{length: n, term: v&0x80 != 0, maxCode: (uint64(v>>8)+1)<<(32-n) - 1}
	}
	for n := 1; n <= 32; n++ {
		lo := binary.BigEndian.Uint32(huff[limitsAt+int64(8*(n-1)):])
		hi := binary.BigEndian.Uint32(huff[limitsAt+int64(8*(n-1)+4):])
		d.minCode[n] = uint64(lo) << (32 - n)
		d.maxCode[n] = (uint64(hi)+1)<<(32-n) - 1
	}
	for _, c := range cdics {
		if err := d.loadCDIC(c); err != nil {
			return nil, err
		}
	}
	if len(d.phrases) == 0 {
		return nil, refusedf("the book's CDIC records hold no phrases")
	}
	d.expanded = make([][]byte, len(d.phrases))
	d.expanding = make([]bool, len(d.phrases))
	return d, nil
}

// loadCDIC adds one CDIC record's phrases: a header (the phrase count of
// the whole dictionary and the bits a record's share is indexed by), an
// offset per phrase, and each phrase as a length (its top bit saying the
// phrase is literal text) and its bytes. Phrases stay slices of the record.
func (d *huffDict) loadCDIC(c []byte) error {
	if len(c) < 16 || !bytes.Equal(c[:4], []byte("CDIC")) {
		return refusedf("the book's CDIC record is not one")
	}
	head := int(binary.BigEndian.Uint32(c[4:]))
	total := int(binary.BigEndian.Uint32(c[8:]))
	bits := int(binary.BigEndian.Uint32(c[12:]))
	if head < 16 || head > len(c) || bits < 1 || bits > 16 {
		return refusedf("the book's CDIC header is not one this reader knows")
	}
	n := min(1<<bits, total-len(d.phrases))
	if n <= 0 || head+2*n > len(c) {
		return refusedf("the book's CDIC phrase table runs past the record")
	}
	for i := 0; i < n; i++ {
		at := head + int(binary.BigEndian.Uint16(c[head+2*i:]))
		if at+2 > len(c) {
			return refusedf("a CDIC phrase starts past its record")
		}
		v := binary.BigEndian.Uint16(c[at:])
		end := at + 2 + int(v&0x7fff)
		if end > len(c) {
			return refusedf("a CDIC phrase runs past its record")
		}
		d.phrases = append(d.phrases, c[at+2:end])
		d.literal = append(d.literal, v&0x8000 != 0)
	}
	return nil
}

// decode decodes one text record and appends it to out, refusing output
// past limit bytes.
func (d *huffDict) decode(out, in []byte, limit int) ([]byte, error) {
	if d == nil {
		return nil, refusedf("the book is HUFF/CDIC compressed and names no HUFF record")
	}
	work := 0
	return d.unpack(out, in, limit, 0, &work)
}

// unpack decodes data, a run of codes, appending each code's phrase to out.
func (d *huffDict) unpack(out, data []byte, limit, depth int, work *int) ([]byte, error) {
	if depth > maxHuffDepth {
		return nil, refusedf("the book's HUFF/CDIC phrases nest deeper than %d", maxHuffDepth)
	}
	start := len(out)
	// A 64-bit window over the data, 32 bits of which are the next code:
	// shift is how far the window must move down to bring them to its low
	// 32 bits. The data past its end reads as zero bits.
	left := len(data) * 8
	pos, shift := 0, 32
	window := be64(data, pos)
	for {
		if shift <= 0 {
			pos += 4
			window = be64(data, pos)
			shift += 32
		}
		code := (window >> shift) & 0xffffffff
		e := d.first[code>>24]
		n, maxCode := e.length, e.maxCode
		if !e.term {
			for n < 32 && code < d.minCode[n] {
				n++
			}
			if code < d.minCode[n] {
				return nil, refusedf("the book's HUFF codes are not a code this reader can decode")
			}
			maxCode = d.maxCode[n]
		}
		shift -= n
		if left -= n; left < 0 {
			break
		}
		i := (maxCode - code) >> (32 - n)
		if i >= uint64(len(d.phrases)) {
			return nil, refusedf("a HUFF code names a phrase the CDIC records do not hold")
		}
		phrase, err := d.phrase(int(i), limit, depth, work)
		if err != nil {
			return nil, err
		}
		// Each code counts too, so a phrase of codes that expand to nothing
		// is bounded as well.
		if *work += 1 + len(phrase); *work > maxHuffWork {
			return nil, refusedf("a text record's HUFF/CDIC phrases expand past %d bytes", maxHuffWork)
		}
		if len(out)-start+len(phrase) > limit {
			return nil, refusedf("a text record decompresses past %d bytes", limit)
		}
		out = append(out, phrase...)
	}
	return out, nil
}

// phrase is phrase i decoded: as stored when literal, else expanded (and
// kept for reuse while the cache has room).
func (d *huffDict) phrase(i, limit, depth int, work *int) ([]byte, error) {
	if d.literal[i] {
		return d.phrases[i], nil
	}
	if d.expanded[i] != nil {
		return d.expanded[i], nil
	}
	if d.expanding[i] {
		return nil, refusedf("a HUFF/CDIC phrase expands into itself")
	}
	d.expanding[i] = true
	b, err := d.unpack(nil, d.phrases[i], limit, depth+1, work)
	d.expanding[i] = false
	if err != nil {
		return nil, err
	}
	if b == nil {
		b = []byte{}
	}
	if d.cached+len(b) <= maxHuffCache {
		d.cached += len(b)
		d.expanded[i] = b
	}
	return b, nil
}

// be64 is the eight bytes of b at pos as a big-endian number, bytes past
// b's end read as zero.
func be64(b []byte, pos int) uint64 {
	var v uint64
	for k := 0; k < 8; k++ {
		v <<= 8
		if pos+k < len(b) {
			v |= uint64(b[pos+k])
		}
	}
	return v
}
