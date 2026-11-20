package extract

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// lzxEnc writes LZX streams for the tests, from the same description the
// decoder follows: 16-bit little-endian words filled most significant bit
// first, one verbatim block per frame here, code lengths sent through a
// pre-tree as changes to the previous block's.
type lzxEnc struct {
	out      []byte
	acc      uint32
	nacc     uint
	numMain  int
	prevMain []byte
	prevLen  []byte
	main     []lzxCode
	length   []lzxCode
	aligned  []lzxCode
}

type lzxCode struct {
	code uint32
	len  uint
}

func newLZXEnc(windowBits int) *lzxEnc {
	n := lzxNumChars + lzxPositionSlots[windowBits]*8
	return &lzxEnc{numMain: n, prevMain: make([]byte, n), prevLen: make([]byte, lzxLengthSize)}
}

func (e *lzxEnc) bits(v uint32, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		e.acc = e.acc<<1 | (v>>uint(i))&1
		if e.nacc++; e.nacc == 16 {
			e.out = append(e.out, byte(e.acc), byte(e.acc>>8))
			e.acc, e.nacc = 0, 0
		}
	}
}

// align pads the word being written with zeros.
func (e *lzxEnc) align() {
	if e.nacc > 0 {
		e.bits(0, 16-e.nacc)
	}
}

// reset is a reset point: the previous lengths are zero again.
func (e *lzxEnc) reset() {
	clear(e.prevMain)
	clear(e.prevLen)
}

// header writes the E8 header bit: translation off, or on with size.
func (e *lzxEnc) header(e8 bool, size uint32) {
	if !e8 {
		e.bits(0, 1)
		return
	}
	e.bits(1, 1)
	e.bits(size, 32)
}

// canonical gives each symbol its code, as the decoder assigns them.
func canonical(lens []byte) []lzxCode {
	var count [lzxMaxCodeLen + 1]uint32
	for _, l := range lens {
		if l > 0 {
			count[l]++
		}
	}
	var next [lzxMaxCodeLen + 2]uint32
	for l := 1; l <= lzxMaxCodeLen; l++ {
		next[l+1] = (next[l] + count[l]) << 1
	}
	out := make([]lzxCode, len(lens))
	for s, l := range lens {
		if l > 0 {
			out[s] = lzxCode{next[l], uint(l)}
			next[l]++
		}
	}
	return out
}

// completeLens gives the symbols used a complete code over size symbols:
// with 2^k >= n > 2^(k-1), 2^k-n of them k-1 bits and the rest k bits.
func completeLens(size int, used ...int) []byte {
	lens := make([]byte, size)
	n := len(used)
	k := 1
	for 1<<k < n {
		k++
	}
	short := 1<<k - n
	for i, s := range used {
		if i < short {
			lens[s] = byte(k - 1)
		} else {
			lens[s] = byte(k)
		}
	}
	return lens
}

func symbolRange(from, to int) []int {
	var s []int
	for i := from; i < to; i++ {
		s = append(s, i)
	}
	return s
}

// preTree writes lens[first:last] as changes to prev[first:last], runs of
// zeros as codes 17 and 18.
func (e *lzxEnc) preTree(prev, lens []byte, first, last int) {
	preLens := completeLens(lzxPreTreeSize, symbolRange(0, lzxPreTreeSize)...)
	for _, l := range preLens {
		e.bits(uint32(l), 4)
	}
	pre := canonical(preLens)
	sym := func(s int) { e.bits(pre[s].code, pre[s].len) }
	for x := first; x < last; {
		run := 0
		for x+run < last && lens[x+run] == 0 && run < 51 {
			run++
		}
		switch {
		case run >= 20:
			sym(18)
			e.bits(uint32(run-20), 5)
			x += run
		case run >= 4:
			run = min(run, 19)
			sym(17)
			e.bits(uint32(run-4), 4)
			x += run
		default:
			sym((int(prev[x]) - int(lens[x]) + 17) % 17)
			x++
		}
	}
}

// block writes a verbatim (or, with aligned lengths, an aligned-offset)
// block header and its trees.
func (e *lzxEnc) block(size int, mainLens, lenLens, alignedLens []byte) {
	t := uint32(lzxBlockVerbatim)
	if alignedLens != nil {
		t = lzxBlockAligned
	}
	e.bits(t, 3)
	e.bits(uint32(size), 24)
	if alignedLens != nil {
		for _, l := range alignedLens {
			e.bits(uint32(l), 3)
		}
		e.aligned = canonical(alignedLens)
	}
	e.preTree(e.prevMain, mainLens, 0, lzxNumChars)
	e.preTree(e.prevMain, mainLens, lzxNumChars, e.numMain)
	e.preTree(e.prevLen, lenLens, 0, lzxLengthSize)
	copy(e.prevMain, mainLens)
	copy(e.prevLen, lenLens)
	e.main, e.length = canonical(mainLens), canonical(lenLens)
}

// stored writes an uncompressed block: its header, the padding to a word,
// the repeated offsets, the bytes, and a pad byte when their count is odd.
func (e *lzxEnc) stored(data []byte, r0, r1, r2 uint32) {
	e.bits(lzxBlockUncompressed, 3)
	e.bits(uint32(len(data)), 24)
	if e.nacc == 0 {
		e.bits(0, 16)
	}
	e.align()
	for _, r := range []uint32{r0, r1, r2} {
		e.out = append(e.out, byte(r), byte(r>>8), byte(r>>16), byte(r>>24))
	}
	e.out = append(e.out, data...)
	if len(data)%2 == 1 {
		e.out = append(e.out, 0)
	}
}

func (e *lzxEnc) lit(c byte) { e.bits(e.main[c].code, e.main[c].len) }

// match writes a match of length bytes through position slot slot, with
// footer the slot's verbatim bits (none for slots 0 to 3).
func (e *lzxEnc) match(slot, length int, footer uint32) {
	hdr := min(length-2, 7)
	m := e.main[lzxNumChars+slot*8+hdr]
	e.bits(m.code, m.len)
	if hdr == 7 {
		l := e.length[length-9]
		e.bits(l.code, l.len)
	}
	if slot >= 3 {
		e.bits(footer, uint(lzxExtraBits[slot]))
	}
}

// literalsLZX compresses data as literals only, one verbatim block per
// frame, resetting every resetInterval bytes, and returns the stream and
// the compressed offset each frame starts at.
func literalsLZX(data []byte, windowBits int, resetInterval int) ([]byte, []int64) {
	e := newLZXEnc(windowBits)
	lens := completeLens(e.numMain, symbolRange(0, lzxNumChars)...)
	var frames []int64
	for pos := 0; pos < len(data); pos += lzxFrameSize {
		frames = append(frames, int64(len(e.out)))
		if pos%resetInterval == 0 {
			e.reset()
			e.header(false, 0)
		}
		f := data[pos:min(pos+lzxFrameSize, len(data))]
		e.block(len(f), lens, make([]byte, lzxLengthSize), nil)
		for _, c := range f {
			e.lit(c)
		}
		e.align()
	}
	return e.out, frames
}

// decodeLZX decodes a whole stream from its start.
func decodeLZX(t testing.TB, stream []byte, windowBits int, resetInterval, total, limit int64) ([]byte, error) {
	t.Helper()
	var budget int64
	d, err := newLZXDecoder(context.Background(), bytes.NewReader(stream), windowBits, resetInterval, 0, total, &budget, limit)
	if err != nil {
		return nil, err
	}
	var out []byte
	for {
		f, err := d.next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, f...)
	}
}

func TestLZX_LiteralFramesRoundTrip(t *testing.T) {
	data := []byte(strings.Repeat("The keeper sums each column. ", 4000)) // 3.5 frames
	stream, _ := literalsLZX(data, 15, 2*lzxFrameSize)
	got, err := decodeLZX(t, stream, 15, 2*lzxFrameSize, int64(len(data)), 1<<30)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("err %v, got %d bytes equal %t", err, len(got), bytes.Equal(got, data))
	}
}

// matchLZX is a stream of one frame: literal "ab", then a match through
// slot with footer, then literals to fill the frame.
func matchLZX(windowBits, slot, length int, footer uint32, aligned bool) ([]byte, int) {
	return prefixMatchLZX("ab", windowBits, slot, length, footer, aligned)
}

// prefixMatchLZX is matchLZX with the literals before the match given.
func prefixMatchLZX(prefix string, windowBits, slot, length int, footer uint32, aligned bool) ([]byte, int) {
	e := newLZXEnc(windowBits)
	e.header(false, 0)
	used := append(symbolRange(0, lzxNumChars), lzxNumChars+slot*8+min(length-2, 7))
	mainLens := completeLens(e.numMain, used...)
	lenLens := completeLens(lzxLengthSize, symbolRange(0, lzxLengthSize)...)
	var alignedLens []byte
	if aligned {
		alignedLens = completeLens(lzxAlignedSize, symbolRange(0, lzxAlignedSize)...)
	}
	const size = 120
	e.block(size, mainLens, lenLens, alignedLens)
	for _, c := range []byte(prefix) {
		e.lit(c)
	}
	if aligned && lzxExtraBits[slot] >= 3 {
		hdr := min(length-2, 7)
		m := e.main[lzxNumChars+slot*8+hdr]
		e.bits(m.code, m.len)
		if hdr == 7 {
			l := e.length[length-9]
			e.bits(l.code, l.len)
		}
		extra := uint(lzxExtraBits[slot])
		e.bits(footer>>3, extra-3)
		a := e.aligned[footer&7]
		e.bits(a.code, a.len)
	} else {
		e.match(slot, length, footer)
	}
	for i := len(prefix) + length; i < size; i++ {
		e.lit('z')
	}
	e.align()
	return e.out, size
}

// A match decodes through a repeated offset and through an aligned-offset
// footer alike: the copy is of the bytes the offset names.
func TestLZX_MatchesCopyTheBytesTheirOffsetNames(t *testing.T) {
	// Slot 0 is the repeated offset R0, 1 at a reset: "ab" then "bbbb".
	stream, n := matchLZX(15, 0, 4, 0, false)
	got, err := decodeLZX(t, stream, 15, lzxFrameSize, int64(n), 1<<30)
	if err != nil || string(got[:6]) != "abbbbb" {
		t.Fatalf("R0 match: err %v, got %q", err, got[:min(len(got), 6)])
	}
	// Slot 3 has no footer: offset 3-2 = 1 as well.
	stream, n = matchLZX(15, 3, 3, 0, false)
	if got, err = decodeLZX(t, stream, 15, lzxFrameSize, int64(n), 1<<30); err != nil || string(got[:5]) != "abbbb" {
		t.Fatalf("slot 3: err %v, got %q", err, got[:min(len(got), 5)])
	}
	// Slot 2 (R2, 1 at a reset) through an aligned block: the block's
	// aligned tree is read and the copy is the same.
	stream, n = matchLZX(15, 2, 5, 0, true)
	if got, err = decodeLZX(t, stream, 15, lzxFrameSize, int64(n), 1<<30); err != nil || string(got[:7]) != "abbbbbb" {
		t.Fatalf("aligned R2: err %v, got %q", err, got[:min(len(got), 7)])
	}
	// Slot 8 has 3 footer bits, all from the aligned tree in an aligned
	// block: base 16 + 5 - 2 = offset 19.
	prefix := "abcdefghijklmnopqrstuvwxyz"
	stream, n = prefixMatchLZX(prefix, 15, 8, 4, 5, true)
	if got, err = decodeLZX(t, stream, 15, lzxFrameSize, int64(n), 1<<30); err != nil || string(got[26:30]) != "hijk" {
		t.Fatalf("aligned footer: err %v, got %q", err, got)
	}
	// Slot 12 has 5: two verbatim bits, then three from the aligned tree:
	// base 64 + (2<<3 | 3) - 2 = offset 81, from a longer prefix.
	long := strings.Repeat("0123456789", 9)
	stream, n = prefixMatchLZX(long, 15, 12, 3, 2<<3|3, true)
	if got, err = decodeLZX(t, stream, 15, lzxFrameSize, int64(n), 1<<30); err != nil || string(got[90:93]) != long[9:12] {
		t.Fatalf("aligned with verbatim bits: err %v, got %q", err, got)
	}
}

// A stored block's bytes come out as they are, its odd length padded, and
// the next block is read from the word after.
func TestLZX_StoredBlockIsCopied(t *testing.T) {
	e := newLZXEnc(15)
	e.header(false, 0)
	e.stored([]byte("odd"), 1, 1, 1)
	lens := completeLens(e.numMain, symbolRange(0, lzxNumChars)...)
	e.block(4, lens, make([]byte, lzxLengthSize), nil)
	for _, c := range []byte("even") {
		e.lit(c)
	}
	e.align()
	got, err := decodeLZX(t, e.out, 15, lzxFrameSize, 7, 1<<30)
	if err != nil || string(got) != "oddeven" {
		t.Fatalf("err %v, got %q", err, got)
	}
}

// With the E8 translation on, the operand after an E8 byte is made
// relative to the byte's own position again.
func TestLZX_E8OperandIsMadeRelativeAgain(t *testing.T) {
	plain := make([]byte, 64)
	plain[20] = 0xE8
	plain[21] = 0x10 // relative +16 from position 20
	coded := append([]byte(nil), plain...)
	coded[21] = 0x10 + 20 // the compressor made it absolute: 36
	e := newLZXEnc(15)
	e.header(true, 1<<20)
	lens := completeLens(e.numMain, symbolRange(0, lzxNumChars)...)
	e.block(len(coded), lens, make([]byte, lzxLengthSize), nil)
	for _, c := range coded {
		e.lit(c)
	}
	e.align()
	got, err := decodeLZX(t, e.out, 15, lzxFrameSize, int64(len(coded)), 1<<30)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("err %v, got % x", err, got[18:26])
	}
}

func TestLZX_IncompleteHuffmanTableIsRefused(t *testing.T) {
	e := newLZXEnc(15)
	e.header(false, 0)
	// Every literal 9 bits long: half the code space is unused.
	lens := make([]byte, e.numMain)
	for i := 0; i < lzxNumChars; i++ {
		lens[i] = 9
	}
	e.block(16, lens, make([]byte, lzxLengthSize), nil)
	e.out = append(e.out, make([]byte, 64)...)
	_, err := decodeLZX(t, e.out, 15, lzxFrameSize, 16, 1<<30)
	if !IsRefused(err) || !strings.Contains(err.Error(), "not a complete Huffman code") {
		t.Fatalf("err = %v, want the incomplete main tree refused", err)
	}
	// An over-subscribed one too: three 1-bit codes.
	lens = make([]byte, e.numMain)
	lens[0], lens[1], lens[2] = 1, 1, 1
	e = newLZXEnc(15)
	e.header(false, 0)
	e.block(16, lens, make([]byte, lzxLengthSize), nil)
	e.out = append(e.out, make([]byte, 64)...)
	if _, err = decodeLZX(t, e.out, 15, lzxFrameSize, 16, 1<<30); !IsRefused(err) || !strings.Contains(err.Error(), "not a complete Huffman code") {
		t.Fatalf("over-subscribed: err = %v, want a refusal", err)
	}
}

func TestLZX_MatchBeforeWindowStartIsRefused(t *testing.T) {
	// "ab" then a match 3 back through slot 4 (base 4, one footer bit:
	// offset 4+1-2 = 3): only two bytes are written.
	stream, n := matchLZX(15, 4, 3, 1, false)
	_, err := decodeLZX(t, stream, 15, lzxFrameSize, int64(n), 1<<30)
	if !IsRefused(err) || !strings.Contains(err.Error(), "before the start of the window") {
		t.Fatalf("err = %v, want the match refused", err)
	}
	// The same through slot 4 with footer 0 (offset 2) is read.
	stream, n = matchLZX(15, 4, 3, 0, false)
	if got, err := decodeLZX(t, stream, 15, lzxFrameSize, int64(n), 1<<30); err != nil || string(got[:5]) != "ababa" {
		t.Fatalf("in-window match: err %v, got %q", err, got[:min(len(got), 5)])
	}
}

// A match may not reach back past a reset point, though the window still
// holds the bytes before it: a reader that starts at that point has none.
func TestLZX_MatchAcrossAResetPointIsRefused(t *testing.T) {
	e := newLZXEnc(16)
	lens := append(symbolRange(0, lzxNumChars), lzxNumChars+0*8+7)
	mainLens := completeLens(e.numMain, lens...)
	lenLens := completeLens(lzxLengthSize, symbolRange(0, lzxLengthSize)...)
	e.header(false, 0)
	e.block(lzxFrameSize, mainLens, lenLens, nil)
	for i := 0; i < lzxFrameSize; i++ {
		e.lit('x')
	}
	e.align()
	// The second frame is a reset point; its first item is a match through
	// R0 = 1, which would copy the first frame's last byte.
	e.reset()
	e.header(false, 0)
	e.block(lzxFrameSize, mainLens, lenLens, nil)
	e.match(0, 257, 0)
	_, err := decodeLZX(t, append(e.out, make([]byte, 64)...), 16, lzxFrameSize, 2*lzxFrameSize, 1<<30)
	if !IsRefused(err) || !strings.Contains(err.Error(), "before the start of the window") {
		t.Fatalf("err = %v, want the match across the reset refused", err)
	}
}

// bombLZX is frames of one byte and 257-byte matches of it, n frames with
// a reset every resetFrames: about 200 bytes a frame for 32 KiB of output.
func bombLZX(windowBits, n, resetFrames int) []byte {
	stream, _ := bombFramesLZX(windowBits, n, resetFrames)
	return stream
}

// bombFramesLZX is bombLZX with the compressed offset of each frame.
func bombFramesLZX(windowBits, n, resetFrames int) ([]byte, []int64) {
	var frames []int64
	e := newLZXEnc(windowBits)
	mainLens := completeLens(e.numMain, 'a', lzxNumChars+0*8+7)
	lenLens := completeLens(lzxLengthSize, symbolRange(0, lzxLengthSize)...)
	for f := 0; f < n; f++ {
		frames = append(frames, int64(len(e.out)))
		left := lzxFrameSize
		if f%resetFrames == 0 {
			e.reset()
			e.header(false, 0)
		}
		e.block(lzxFrameSize, mainLens, lenLens, nil)
		if f%resetFrames == 0 {
			e.lit('a')
			left--
		}
		for left > 0 {
			l := min(257, left)
			if l < 9 {
				for ; l > 0; l-- {
					e.lit('a')
					left--
				}
				continue
			}
			e.match(0, l, 0)
			left -= l
		}
		e.align()
	}
	return e.out, frames
}

func TestLZX_ExpansionIsBounded(t *testing.T) {
	const frames = 64 // 2 MiB of output
	stream := bombLZX(15, frames, 4)
	got, err := decodeLZX(t, stream, 15, 4*lzxFrameSize, frames*lzxFrameSize, 1<<30)
	if err != nil || len(got) != frames*lzxFrameSize || bytes.Count(got, []byte("a")) != len(got) {
		t.Fatalf("the bomb itself: err %v, %d bytes", err, len(got))
	}
	// Under a 1 MiB budget it stops at the frame that would pass it.
	got, err = decodeLZX(t, stream, 15, 4*lzxFrameSize, frames*lzxFrameSize, 1<<20)
	if !IsRefused(err) || !strings.Contains(err.Error(), errLZXBudget.Error()) {
		t.Fatalf("err = %v, want the budget refusal", err)
	}
	if len(got) > 1<<20 {
		t.Fatalf("decoded %d bytes past a budget of %d", len(got), 1<<20)
	}
}
