package extract

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
)

// An LZX decoder for the MSCompressed section of a compiled HTML Help file
// (RFC GX §4.3), written from the LZX description in [MS-PATCH] §2 without
// its DELTA extensions: verbatim, aligned-offset and uncompressed blocks,
// the pre-tree, main tree and length tree, the three repeated offsets and
// the E8 call translation.
//
// The output is cut into frames of 32 KiB. The input is a stream of 16-bit
// little-endian words read most significant bit first, padded to a whole
// word at the end of every frame. Every reset interval (a whole number of
// frames) the decoder's state is reset: its trees, its repeated offsets and
// the E8 header. A reader starts at a reset point, which is how a CHM file
// in the middle of the section is read without decompressing what comes
// before it.
//
// Everything is checked before it is used: a Huffman table must be complete
// or empty, a match must lie inside the window and inside what was written
// since the last reset, and a match or a stored run must end inside its
// block and its frame. The total decompressed is bounded by the caller's
// budget, which every frame is charged to before it is decoded.

const (
	lzxFrameSize = 32 << 10
	// lzxMinWindowBits and lzxMaxWindowBits are the window sizes LZX
	// allows: 32 KiB to 2 MiB.
	lzxMinWindowBits = 15
	lzxMaxWindowBits = 21
	lzxNumChars      = 256
	lzxPreTreeSize   = 20
	lzxLengthSize    = 249
	lzxAlignedSize   = 8
	lzxMaxCodeLen    = 16
	// lzxMaxMain is the main tree's largest size: 256 literals and eight
	// lengths for each of the 50 position slots of a 2 MiB window.
	lzxMaxMain = lzxNumChars + 50*8
	// lzxMaxPadWords is how many zero words past the end of its input the
	// decoder may read: a code's lookahead reaches up to 16 bits past the
	// last one, never more than this.
	lzxMaxPadWords = 4
)

const (
	lzxBlockVerbatim     = 1
	lzxBlockAligned      = 2
	lzxBlockUncompressed = 3
)

// lzxPositionSlots is the number of position slots a window of 2^bits has.
var lzxPositionSlots = [lzxMaxWindowBits + 1]int{15: 30, 16: 32, 17: 34, 18: 36, 19: 38, 20: 42, 21: 50}

// lzxExtraBits and lzxPositionBase are the footer bits and the base offset
// of each position slot.
var lzxExtraBits, lzxPositionBase = lzxSlotTables()

func lzxSlotTables() (extra [50]uint8, base [50]uint32) {
	for i := range extra {
		switch {
		case i < 4:
			extra[i] = 0
		case i < 36:
			extra[i] = uint8(i/2 - 1)
		default:
			extra[i] = 17
		}
		if i > 0 {
			base[i] = base[i-1] + 1<<extra[i-1]
		}
	}
	return extra, base
}

// errLZXBudget is the refusal for a stream that decompresses past its
// budget; a test tells it apart from the stream's own faults.
var errLZXBudget = errors.New("the compressed section decompresses past its budget")

// lzxBits reads the input: 16-bit little-endian words, most significant
// bit first. A Huffman code's lookahead may leave a whole word read and
// unused, but a block header's 24-bit length never does: after it the
// buffer holds less than the word being read, so an uncompressed block's
// bytes start where the stream's next whole word is.
type lzxBits struct {
	src *bufio.Reader
	buf uint64 // the unread bits, at the top
	n   uint
	pad int // zero words read past the end
}

// fill makes at least want bits available, want at most 32.
func (b *lzxBits) fill(want uint) error {
	for b.n < want {
		lo, err := b.src.ReadByte()
		var hi byte
		if err == nil {
			hi, err = b.src.ReadByte()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
			if b.pad++; b.pad > lzxMaxPadWords {
				return refusedf("the compressed section ends in the middle of its data")
			}
			lo, hi = 0, 0
		}
		b.buf |= uint64(uint16(lo)|uint16(hi)<<8) << (48 - b.n)
		b.n += 16
	}
	return nil
}

func (b *lzxBits) peek(n uint) uint32 { return uint32(b.buf >> (64 - n)) }

func (b *lzxBits) drop(n uint) {
	b.buf <<= n
	b.n -= n
}

// read returns the next n bits, n at most 32.
func (b *lzxBits) read(n uint) (uint32, error) {
	if n == 0 {
		return 0, nil
	}
	if err := b.fill(n); err != nil {
		return 0, err
	}
	v := b.peek(n)
	b.drop(n)
	return v, nil
}

// align drops what is left of the word being read. A code's lookahead may
// have read a whole word more, which stays.
func (b *lzxBits) align() { b.drop(b.n % 16) }

// rawByte reads a byte of an uncompressed block, which starts on a word.
func (b *lzxBits) rawByte() (byte, error) {
	c, err := b.src.ReadByte()
	if errors.Is(err, io.EOF) {
		return 0, refusedf("the compressed section ends in the middle of a stored block")
	}
	return c, err
}

// lzxTree is one Huffman tree: its code lengths, and a table indexed by
// the next maxLen bits giving each code's symbol and length.
type lzxTree struct {
	lens   []byte
	table  []uint32 // symbol<<5 | length
	maxLen uint
	empty  bool
}

func newLZXTree(size int) *lzxTree { return &lzxTree{lens: make([]byte, size)} }

// build makes the decoding table from the code lengths: canonical codes,
// shorter first and in symbol order within a length. A table must be
// complete (every bit pattern decodes) or empty; an incomplete or
// over-subscribed one is refused before any symbol is read from it.
func (t *lzxTree) build(what string) error {
	var count [lzxMaxCodeLen + 1]int
	var kraft uint64
	t.maxLen = 0
	for _, l := range t.lens {
		if l > lzxMaxCodeLen {
			return refusedf("the %s tree has a code longer than %d bits", what, lzxMaxCodeLen)
		}
		if l > 0 {
			count[l]++
			kraft += 1 << (lzxMaxCodeLen - l)
			t.maxLen = max(t.maxLen, uint(l))
		}
	}
	t.empty = kraft == 0
	if t.empty {
		return nil
	}
	if kraft != 1<<lzxMaxCodeLen {
		return refusedf("the %s tree is not a complete Huffman code", what)
	}
	var next [lzxMaxCodeLen + 2]uint32
	for l := 1; l <= lzxMaxCodeLen; l++ {
		next[l+1] = (next[l] + uint32(count[l])) << 1
	}
	size := 1 << t.maxLen
	if cap(t.table) < size {
		t.table = make([]uint32, size)
	}
	t.table = t.table[:size]
	for sym, l := range t.lens {
		if l == 0 {
			continue
		}
		code := next[l]
		next[l]++
		shift := t.maxLen - uint(l)
		e := uint32(sym)<<5 | uint32(l)
		for i := code << shift; i < (code+1)<<shift; i++ {
			t.table[i] = e
		}
	}
	return nil
}

// lzxDecoder decodes an LZX stream from one reset point onwards, a frame at
// a time.
type lzxDecoder struct {
	ctx  context.Context
	bits lzxBits

	window     []byte
	windowSize int64
	mask       int64
	numMain    int

	// pos is the section offset of the frame being decoded, cur of the
	// next byte; total is the section's length, which makes the last frame
	// short.
	pos, cur, total int64
	// resetInterval is how many bytes lie between reset points, a whole
	// number of frames; sinceReset how many were decoded since the last.
	resetInterval, sinceReset int64

	r0, r1, r2 uint32
	headerRead bool
	e8Size     int64 // the E8 translation size; 0 translates nothing

	blockType      int
	blockRemaining int64
	blockOdd       bool // the stored block just read had an odd length

	pre, main, length, aligned *lzxTree

	frame []byte
	// budget is charged each frame's size before it is decoded; it is
	// shared by every decoder one extraction starts.
	budget *int64
	limit  int64
}

// newLZXDecoder starts decoding at a reset point: src is the compressed
// stream from that point's offset on, start the section offset it
// decompresses to.
func newLZXDecoder(ctx context.Context, src io.Reader, windowBits int, resetInterval, start, total int64, budget *int64, limit int64) (*lzxDecoder, error) {
	if windowBits < lzxMinWindowBits || windowBits > lzxMaxWindowBits {
		return nil, refusedf("the LZX window of 2^%d bytes is not one LZX allows", windowBits)
	}
	if resetInterval <= 0 || resetInterval%lzxFrameSize != 0 || start%resetInterval != 0 || start < 0 || start > total {
		return nil, refusedf("the LZX reset interval %d does not divide the section into frames", resetInterval)
	}
	ws := int64(1) << windowBits
	return &lzxDecoder{
		ctx:           ctx,
		bits:          lzxBits{src: bufio.NewReaderSize(src, 16<<10)},
		window:        make([]byte, ws),
		windowSize:    ws,
		mask:          ws - 1,
		numMain:       lzxNumChars + lzxPositionSlots[windowBits]*8,
		pos:           start,
		total:         total,
		resetInterval: resetInterval,
		pre:           newLZXTree(lzxPreTreeSize),
		main:          newLZXTree(lzxNumChars + lzxPositionSlots[windowBits]*8),
		length:        newLZXTree(lzxLengthSize),
		aligned:       newLZXTree(lzxAlignedSize),
		frame:         make([]byte, lzxFrameSize),
		budget:        budget,
		limit:         limit,
	}, nil
}

// restart starts the decoder again at another reset point, keeping its
// window and buffers: what the window holds is never read, since a match
// may reach back only to the last reset point.
func (d *lzxDecoder) restart(src io.Reader, start int64) error {
	if start < 0 || start%d.resetInterval != 0 || start > d.total {
		return refusedf("the LZX reset point %d is not one of the section's", start)
	}
	d.bits.src.Reset(src)
	d.bits.buf, d.bits.n, d.bits.pad = 0, 0, 0
	d.pos, d.cur, d.sinceReset = start, start, 0
	d.blockType, d.blockRemaining, d.blockOdd = 0, 0, false
	return nil
}

// reset is the state at a reset point.
func (d *lzxDecoder) reset() {
	d.r0, d.r1, d.r2 = 1, 1, 1
	d.headerRead = false
	d.e8Size = 0
	d.blockType, d.blockRemaining, d.blockOdd = 0, 0, false
	clear(d.main.lens)
	clear(d.length.lens)
}

// next decodes the next frame and returns it, with the E8 translation
// undone; the slice is valid until the next call. It returns io.EOF at the
// end of the section.
func (d *lzxDecoder) next() ([]byte, error) {
	if d.pos >= d.total {
		return nil, io.EOF
	}
	if err := d.ctx.Err(); err != nil {
		return nil, err
	}
	n := min(int64(lzxFrameSize), d.total-d.pos)
	if *d.budget+n > d.limit {
		return nil, refusedf("%v (%d bytes)", errLZXBudget, d.limit)
	}
	*d.budget += n
	if d.sinceReset == 0 {
		if d.blockRemaining > 0 {
			return nil, refusedf("an LZX block runs over a reset point")
		}
		d.reset()
	}
	if !d.headerRead {
		on, err := d.bits.read(1)
		if err != nil {
			return nil, err
		}
		if on == 1 {
			v, err := d.bits.read(32)
			if err != nil {
				return nil, err
			}
			d.e8Size = int64(int32(v))
		}
		d.headerRead = true
	}
	start := d.pos & d.mask
	d.cur = d.pos
	var done int64
	for done < n {
		if d.blockRemaining == 0 {
			if err := d.readBlockHeader(); err != nil {
				return nil, err
			}
			continue
		}
		run := min(d.blockRemaining, n-done)
		var err error
		if d.blockType == lzxBlockUncompressed {
			err = d.stored(run)
		} else {
			err = d.compressed(run)
		}
		if err != nil {
			return nil, err
		}
		d.blockRemaining -= run
		done += run
	}
	d.bits.align()
	out := d.frame[:n]
	copy(out, d.window[start:start+n])
	d.translateE8(out)
	d.pos += n
	if d.sinceReset += n; d.sinceReset >= d.resetInterval {
		d.sinceReset = 0
	}
	return out, nil
}

// readBlockHeader reads a block's type and length, and its trees or its
// stored repeated offsets.
func (d *lzxDecoder) readBlockHeader() error {
	if d.blockType == lzxBlockUncompressed && d.blockOdd {
		// A stored block of odd length is padded to a whole word.
		if _, err := d.bits.rawByte(); err != nil {
			return err
		}
	}
	d.blockOdd = false
	t, err := d.bits.read(3)
	if err != nil {
		return err
	}
	size, err := d.bits.read(24)
	if err != nil {
		return err
	}
	d.blockType, d.blockRemaining = int(t), int64(size)
	switch d.blockType {
	case lzxBlockAligned:
		for i := range d.aligned.lens {
			v, err := d.bits.read(3)
			if err != nil {
				return err
			}
			d.aligned.lens[i] = byte(v)
		}
		if err := d.aligned.build("aligned offset"); err != nil {
			return err
		}
		fallthrough
	case lzxBlockVerbatim:
		if err := d.readLengths(d.main.lens, 0, lzxNumChars); err != nil {
			return err
		}
		if err := d.readLengths(d.main.lens, lzxNumChars, d.numMain); err != nil {
			return err
		}
		if err := d.main.build("main"); err != nil {
			return err
		}
		if err := d.readLengths(d.length.lens, 0, lzxLengthSize); err != nil {
			return err
		}
		// An empty length tree is allowed: a block whose matches are all
		// shorter than nine bytes needs none.
		return d.length.build("length")
	case lzxBlockUncompressed:
		// The block starts on the next word: what is left of this one is
		// padding, and a whole word of it when none is left.
		if d.bits.n == 0 {
			if _, err := d.bits.read(16); err != nil {
				return err
			}
		}
		d.bits.drop(d.bits.n)
		var r [12]byte
		for i := range r {
			c, err := d.bits.rawByte()
			if err != nil {
				return err
			}
			r[i] = c
		}
		d.r0 = binary.LittleEndian.Uint32(r[0:])
		d.r1 = binary.LittleEndian.Uint32(r[4:])
		d.r2 = binary.LittleEndian.Uint32(r[8:])
		d.blockOdd = size%2 == 1
		return nil
	}
	return refusedf("an LZX block has the unknown type %d", t)
}

// readLengths reads the code lengths lens[first:last] through a pre-tree,
// each as a change to the length the previous block gave the symbol.
func (d *lzxDecoder) readLengths(lens []byte, first, last int) error {
	for i := range d.pre.lens {
		v, err := d.bits.read(4)
		if err != nil {
			return err
		}
		d.pre.lens[i] = byte(v)
	}
	if err := d.pre.build("pre"); err != nil {
		return err
	}
	for x := first; x < last; {
		z, err := d.symbol(d.pre)
		if err != nil {
			return err
		}
		var run int
		var val byte
		switch z {
		case 17, 18:
			bits, base := uint(4), 4
			if z == 18 {
				bits, base = 5, 20
			}
			v, err := d.bits.read(bits)
			if err != nil {
				return err
			}
			run, val = base+int(v), 0
		case 19:
			v, err := d.bits.read(1)
			if err != nil {
				return err
			}
			run = 4 + int(v)
			z2, err := d.symbol(d.pre)
			if err != nil {
				return err
			}
			if z2 > 16 {
				return refusedf("a pre-tree run repeats the run code %d", z2)
			}
			val = byte((int(lens[x]) + 17 - z2) % 17)
		default:
			run, val = 1, byte((int(lens[x])+17-z)%17)
		}
		if x+run > last {
			return refusedf("a pre-tree run writes past the end of its tree")
		}
		for i := 0; i < run; i++ {
			lens[x] = val
			x++
		}
	}
	return nil
}

// symbol decodes one symbol of t.
func (d *lzxDecoder) symbol(t *lzxTree) (int, error) {
	if t.empty {
		return 0, refusedf("an LZX code is read from an empty tree")
	}
	if err := d.bits.fill(t.maxLen); err != nil {
		return 0, err
	}
	e := t.table[d.bits.peek(t.maxLen)]
	d.bits.drop(uint(e & 31))
	return int(e >> 5), nil
}

// stored copies run bytes of an uncompressed block into the window.
func (d *lzxDecoder) stored(run int64) error {
	for i := int64(0); i < run; i++ {
		c, err := d.bits.rawByte()
		if err != nil {
			return err
		}
		d.window[d.cur&d.mask] = c
		d.cur++
	}
	return nil
}

// compressed decodes run bytes of a verbatim or aligned-offset block.
func (d *lzxDecoder) compressed(run int64) error {
	end := d.cur + run
	for d.cur < end {
		m, err := d.symbol(d.main)
		if err != nil {
			return err
		}
		if m < lzxNumChars {
			d.window[d.cur&d.mask] = byte(m)
			d.cur++
			continue
		}
		m -= lzxNumChars
		length, slot := int64(m&7), m>>3
		if length == 7 {
			extra, err := d.symbol(d.length)
			if err != nil {
				return err
			}
			length += int64(extra)
		}
		length += 2
		var off uint32
		switch slot {
		case 0:
			off = d.r0
		case 1:
			off = d.r1
			d.r1 = d.r0
			d.r0 = off
		case 2:
			off = d.r2
			d.r2 = d.r0
			d.r0 = off
		default:
			extra := uint(lzxExtraBits[slot])
			var v uint32
			if d.blockType == lzxBlockAligned && extra >= 3 {
				hi, err := d.bits.read(extra - 3)
				if err != nil {
					return err
				}
				lo, err := d.symbol(d.aligned)
				if err != nil {
					return err
				}
				v = hi<<3 + uint32(lo)
			} else if v, err = d.bits.read(extra); err != nil {
				return err
			}
			off = lzxPositionBase[slot] + v - 2
			d.r2, d.r1, d.r0 = d.r1, d.r0, off
		}
		if length > end-d.cur && !d.lastFrameSlack(length, end, run) {
			return refusedf("an LZX match runs past the end of its block or frame")
		}
		// What the window holds from before this reset point, or from
		// before the window's own start, is not this stream's to read.
		written := d.sinceReset + d.cur - d.pos
		if off == 0 || int64(off) > written || int64(off) > d.windowSize {
			return refusedf("an LZX match reaches %d bytes back, before the start of the window (%d bytes written)", off, written)
		}
		for i := int64(0); i < length; i++ {
			d.window[d.cur&d.mask] = d.window[(d.cur-int64(off))&d.mask]
			d.cur++
		}
	}
	return nil
}

// lastFrameSlack allows a match to run past the end of the section in its
// last frame, while it stays inside a whole frame and its block: a
// compressor that codes the last frame as a full one (Free Pascal's
// chmcmd) ends it with a match into the padding. The bytes past the end
// are decoded into the window and never returned.
func (d *lzxDecoder) lastFrameSlack(length, end, run int64) bool {
	return d.pos+lzxFrameSize > d.total && end == d.total &&
		d.cur+length <= d.pos+lzxFrameSize && d.cur+length-end <= d.blockRemaining-run
}

// translateE8 undoes the E8 call translation in a frame: the 32-bit operand
// after an E8 byte was made absolute by the compressor; it is made relative
// to the byte's own position again. The last 10 bytes of a frame are not
// translated.
func (d *lzxDecoder) translateE8(out []byte) {
	if d.e8Size <= 0 || len(out) <= 10 {
		return
	}
	for i := 0; i < len(out)-10; {
		if out[i] != 0xE8 {
			i++
			continue
		}
		abs := int64(int32(binary.LittleEndian.Uint32(out[i+1:])))
		cur := d.pos + int64(i)
		if abs >= -cur && abs < d.e8Size {
			rel := abs - cur
			if abs < 0 {
				rel = abs + d.e8Size
			}
			binary.LittleEndian.PutUint32(out[i+1:], uint32(int32(rel)))
		}
		i += 5
	}
}
