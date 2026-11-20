package extract

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"unicode/utf16"
)

// An OLE2 Compound File ([MS-CFB]): a FAT file system inside one file, the
// container of Word 97–2003 documents and their property sets (RFC GX §4.2).
// Every structure in it is a chain of sectors, and every chain comes from the
// file: each walk has a visited set and a length bound, every sector number is
// checked against the sectors the file holds, and a stream is read through
// its chain on demand, never assembled whole unless its reader asks.

const (
	// maxCFBBytes caps the file, and maxCFBStream one stream in it.
	maxCFBBytes  = 64 << 20
	maxCFBStream = 64 << 20
	// maxCFBEntries caps the directory.
	maxCFBEntries = 65536

	cfbHeaderSize = 512
	cfbDirEntry   = 128

	cfbMaxRegSect  = 0xFFFFFFFA
	cfbEndOfChain  = 0xFFFFFFFE
	cfbFreeSect    = 0xFFFFFFFF
	cfbNoStream    = 0xFFFFFFFF
	cfbTypeStorage = 1
	cfbTypeStream  = 2
	cfbTypeRoot    = 5
)

// cfbSignature begins every compound file.
var cfbSignature = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

// isCFB reports whether head begins a compound file.
func isCFB(head []byte) bool {
	return len(head) >= len(cfbSignature) && string(head[:len(cfbSignature)]) == string(cfbSignature)
}

type cfbEntry struct {
	name               string
	typ                byte
	left, right, child uint32
	start              uint32
	size               uint64
}

// cfbFile is an opened compound file: its FAT, mini FAT and directory.
type cfbFile struct {
	ctx        context.Context
	r          io.ReaderAt
	size       int64
	sectorSize int
	sectors    uint32 // regular sectors the file holds after its header
	fat        []uint32
	miniFAT    []uint32
	miniCutoff uint64
	dir        []cfbEntry
	mini       *cfbStream // the root entry's stream, holding the mini sectors
}

// cfbStream is one stream read through its chain.
type cfbStream struct {
	c       *cfbFile
	chain   []uint32
	size    int64
	unit    int        // the sector size the chain counts in
	backing *cfbStream // the mini stream, for a stream in mini sectors
}

func (s *cfbStream) Size() int64 { return s.size }

// ReadAt reads the stream's bytes at off, sector by sector of its chain.
func (s *cfbStream) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("cfb: negative offset")
	}
	if off >= s.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off < s.size {
		i := off / int64(s.unit)
		within := int(off % int64(s.unit))
		k := min(len(p)-n, s.unit-within)
		if rest := s.size - off; int64(k) > rest {
			k = int(rest)
		}
		sec := s.chain[i]
		var err error
		if s.backing != nil {
			_, err = s.backing.ReadAt(p[n:n+k], int64(sec)*int64(s.unit)+int64(within))
		} else {
			_, err = s.c.r.ReadAt(p[n:n+k], int64(sec+1)*int64(s.c.sectorSize)+int64(within))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return n, err
		}
		n += k
		off += int64(k)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// openCFB reads a compound file's header, DIFAT, FAT, directory and mini
// FAT. Anything malformed is a refusal.
func openCFB(ctx context.Context, r io.ReaderAt, size int64) (*cfbFile, error) {
	if size > maxCFBBytes {
		return nil, refusedf("the file is over %d bytes", maxCFBBytes)
	}
	if size < cfbHeaderSize {
		return nil, refusedf("not an OLE2 compound file")
	}
	h := make([]byte, cfbHeaderSize)
	if _, err := r.ReadAt(h, 0); err != nil {
		return nil, refusedf("not an OLE2 compound file")
	}
	if !isCFB(h) {
		return nil, refusedf("not an OLE2 compound file")
	}
	le16 := func(o int) uint16 { return binary.LittleEndian.Uint16(h[o:]) }
	le32 := func(o int) uint32 { return binary.LittleEndian.Uint32(h[o:]) }
	major, shift := le16(26), le16(30)
	switch {
	case major == 3 && shift == 9, major == 4 && shift == 12:
	default:
		return nil, refusedf("the compound file's version %d with %d-byte sectors is not read", major, 1<<min(shift, 16))
	}
	if le16(28) != 0xFFFE || le16(32) != 6 {
		return nil, refusedf("the compound file's header is damaged")
	}
	c := &cfbFile{ctx: ctx, r: r, size: size, sectorSize: 1 << shift, miniCutoff: uint64(le32(56))}
	if c.miniCutoff != 4096 {
		return nil, refusedf("the compound file's header is damaged")
	}
	// The header takes the first sector; a last sector cut short still
	// counts, its missing bytes refused when read.
	c.sectors = uint32((size - int64(c.sectorSize) + int64(c.sectorSize) - 1) / int64(c.sectorSize))
	nFAT, firstDir, firstMini, nMini, firstDIFAT, nDIFAT := le32(44), le32(48), le32(60), le32(64), le32(68), le32(72)
	if nFAT > c.sectors || nMini > c.sectors || nDIFAT > c.sectors {
		return nil, refusedf("the compound file's header names more sectors than the file holds")
	}

	// The DIFAT: 109 FAT sector numbers in the header, the rest in a chain
	// of DIFAT sectors, each ending in the next one's number.
	fatSectors := make([]uint32, 0, nFAT)
	for i := 0; i < 109 && uint32(len(fatSectors)) < nFAT; i++ {
		fatSectors = append(fatSectors, le32(76+4*i))
	}
	per := c.sectorSize/4 - 1
	seen := map[uint32]bool{}
	buf := make([]byte, c.sectorSize)
	for sec, walked := firstDIFAT, uint32(0); uint32(len(fatSectors)) < nFAT; walked++ {
		if walked >= nDIFAT || sec > cfbMaxRegSect || sec >= c.sectors || seen[sec] {
			return nil, refusedf("the compound file's DIFAT chain is damaged")
		}
		seen[sec] = true
		if err := c.sector(sec, buf); err != nil {
			return nil, err
		}
		for i := 0; i < per && uint32(len(fatSectors)) < nFAT; i++ {
			fatSectors = append(fatSectors, binary.LittleEndian.Uint32(buf[4*i:]))
		}
		sec = binary.LittleEndian.Uint32(buf[4*per:])
	}

	// The FAT itself, cut to the sectors the file holds: an entry past them
	// names no sector a chain may use.
	entries := min(int(nFAT)*c.sectorSize/4, int(c.sectors))
	c.fat = make([]uint32, 0, entries)
	for _, sec := range fatSectors {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if sec > cfbMaxRegSect || sec >= c.sectors {
			return nil, refusedf("the compound file's FAT is damaged")
		}
		if err := c.sector(sec, buf); err != nil {
			return nil, err
		}
		for i := 0; i < c.sectorSize/4 && len(c.fat) < entries; i++ {
			c.fat = append(c.fat, binary.LittleEndian.Uint32(buf[4*i:]))
		}
	}

	// The directory.
	dirChain, err := c.chain(firstDir, c.fat, maxCFBEntries*cfbDirEntry/c.sectorSize+1)
	if err != nil {
		return nil, err
	}
	if len(dirChain)*c.sectorSize/cfbDirEntry > maxCFBEntries {
		return nil, refusedf("the compound file's directory has over %d entries", maxCFBEntries)
	}
	dir := &cfbStream{c: c, chain: dirChain, size: int64(len(dirChain) * c.sectorSize), unit: c.sectorSize}
	raw := make([]byte, dir.size)
	if _, err := dir.ReadAt(raw, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, refusedf("the compound file's directory is damaged")
	}
	for o := 0; o+cfbDirEntry <= len(raw); o += cfbDirEntry {
		e := raw[o : o+cfbDirEntry]
		nameLen := int(binary.LittleEndian.Uint16(e[64:]))
		var name string
		if nameLen >= 2 && nameLen <= 64 && nameLen%2 == 0 {
			u := make([]uint16, nameLen/2-1)
			for i := range u {
				u[i] = binary.LittleEndian.Uint16(e[2*i:])
			}
			name = string(utf16.Decode(u))
		}
		size := binary.LittleEndian.Uint64(e[120:])
		if major == 3 {
			// A version 3 file's size is 32 bits; older writers left
			// garbage in the high half ([MS-CFB] §2.6.1).
			size &= 0xFFFFFFFF
		}
		c.dir = append(c.dir, cfbEntry{
			name: name, typ: e[66],
			left: binary.LittleEndian.Uint32(e[68:]), right: binary.LittleEndian.Uint32(e[72:]),
			child: binary.LittleEndian.Uint32(e[76:]),
			start: binary.LittleEndian.Uint32(e[116:]), size: size,
		})
	}
	if len(c.dir) == 0 || c.dir[0].typ != cfbTypeRoot {
		return nil, refusedf("the compound file has no root entry")
	}

	// The mini FAT, and the mini stream its sectors live in.
	if nMini > 0 {
		miniChain, err := c.chain(firstMini, c.fat, int(nMini))
		if err != nil {
			return nil, err
		}
		c.miniFAT = make([]uint32, 0, len(miniChain)*c.sectorSize/4)
		for _, sec := range miniChain {
			if err := c.sector(sec, buf); err != nil {
				return nil, err
			}
			for i := 0; i < c.sectorSize/4; i++ {
				c.miniFAT = append(c.miniFAT, binary.LittleEndian.Uint32(buf[4*i:]))
			}
		}
	}
	root := c.dir[0]
	if root.size > 0 && root.start != cfbEndOfChain {
		if root.size > maxCFBStream {
			return nil, refusedf("the compound file's mini stream is over %d bytes", maxCFBStream)
		}
		s, err := c.regular(root.start, int64(root.size))
		if err != nil {
			return nil, err
		}
		c.mini = s
	}
	return c, nil
}

// sector reads regular sector n whole into buf.
func (c *cfbFile) sector(n uint32, buf []byte) error {
	if _, err := c.r.ReadAt(buf, int64(n+1)*int64(c.sectorSize)); err != nil {
		return refusedf("the compound file is cut short")
	}
	return nil
}

// chain follows a chain of the given FAT from start: a loop, a sector the
// table does not hold, or more than limit sectors is a refusal.
func (c *cfbFile) chain(start uint32, fat []uint32, limit int) ([]uint32, error) {
	var out []uint32
	visited := make([]bool, len(fat))
	for sec := start; sec != cfbEndOfChain; sec = fat[sec] {
		if len(out)%4096 == 4095 {
			if err := c.ctx.Err(); err != nil {
				return nil, err
			}
		}
		if sec > cfbMaxRegSect || int(sec) >= len(fat) {
			return nil, refusedf("a chain of the compound file names a sector it does not hold")
		}
		if visited[sec] {
			return nil, refusedf("a chain of the compound file loops")
		}
		if len(out) >= limit {
			return nil, refusedf("a chain of the compound file is longer than its stream")
		}
		visited[sec] = true
		out = append(out, sec)
	}
	return out, nil
}

// regular is a stream of size bytes in regular sectors from start.
func (c *cfbFile) regular(start uint32, size int64) (*cfbStream, error) {
	need := int((size + int64(c.sectorSize) - 1) / int64(c.sectorSize))
	ch, err := c.chain(start, c.fat, need)
	if err != nil {
		return nil, err
	}
	if len(ch) < need {
		return nil, refusedf("a stream of the compound file is longer than its chain")
	}
	return &cfbStream{c: c, chain: ch, size: size, unit: c.sectorSize}, nil
}

// stream opens the stream called name in the root storage: nil when there
// is none. The name is compared as [MS-CFB] compares them, ignoring case.
func (c *cfbFile) stream(name string) (*cfbStream, error) {
	e, ok, err := c.lookup(name)
	if err != nil || !ok {
		return nil, err
	}
	if e.size > maxCFBStream {
		return nil, refusedf("the stream %q is over %d bytes", name, maxCFBStream)
	}
	if e.size == 0 {
		return &cfbStream{c: c, unit: c.sectorSize}, nil
	}
	if e.size >= c.miniCutoff {
		return c.regular(e.start, int64(e.size))
	}
	if c.mini == nil {
		return nil, refusedf("the compound file's mini stream is missing")
	}
	const miniSize = 64
	need := int((e.size + miniSize - 1) / miniSize)
	ch, err := c.chain(e.start, c.miniFAT, need)
	if err != nil {
		return nil, err
	}
	if len(ch) < need {
		return nil, refusedf("a stream of the compound file is longer than its chain")
	}
	for _, s := range ch {
		if int64(s+1)*miniSize > c.mini.size {
			return nil, refusedf("a stream of the compound file lies past its mini stream")
		}
	}
	return &cfbStream{c: c, chain: ch, size: int64(e.size), unit: miniSize, backing: c.mini}, nil
}

// lookup finds a stream among the root storage's children: the red-black
// tree under the root's child walked iteratively, each entry once.
func (c *cfbFile) lookup(name string) (cfbEntry, bool, error) {
	visited := make([]bool, len(c.dir))
	stack := []uint32{c.dir[0].child}
	for len(stack) > 0 {
		i := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if i == cfbNoStream {
			continue
		}
		if int(i) >= len(c.dir) || i == 0 {
			return cfbEntry{}, false, refusedf("the compound file's directory names an entry it does not hold")
		}
		if visited[i] {
			return cfbEntry{}, false, refusedf("the compound file's directory tree loops")
		}
		visited[i] = true
		e := c.dir[i]
		if e.typ == cfbTypeStream && strings.EqualFold(e.name, name) {
			return e, true, nil
		}
		stack = append(stack, e.left, e.right)
	}
	return cfbEntry{}, false, nil
}

// readStream reads a whole stream of at most limit bytes: nil when the
// file has no such stream, a refusal when it is larger.
func (c *cfbFile) readStream(name string, limit int64) ([]byte, error) {
	s, err := c.stream(name)
	if err != nil || s == nil {
		return nil, err
	}
	if s.size > limit {
		return nil, refusedf("the stream %q is over %d bytes", name, limit)
	}
	b := make([]byte, s.size)
	if _, err := s.ReadAt(b, 0); err != nil && !(errors.Is(err, io.EOF) && s.size == 0) {
		return nil, refusedf("the stream %q is cut short", name)
	}
	return b, nil
}
