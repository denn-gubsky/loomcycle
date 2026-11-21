package extract

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/url"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
)

// CHM reads a compiled HTML Help file (RFC GX §4.3): an ITSF container
// whose directory lists the help file's pages and pictures, most of them
// in an LZX-compressed section. The title comes from the file's #SYSTEM
// record and is the one "#" heading; the topics are read in the order its
// table of contents (.hhc) gives, each once, through the HTML walker's
// body-only mode, as an EPUB's documents are. A file with no contents is
// read from its default topic and then its pages in directory order.
//
// Every name inside the file is a key into its own directory: a reference
// is resolved inside it, and one naming another file or a scheme is written
// as a link and never fetched.
type CHM struct{}

const chmMediaType = "application/vnd.ms-htmlhelp"

// Bounds on a help file.
const (
	maxCHMEntries   = 100_000
	maxCHMName      = 1024
	maxCHMChunk     = 64 << 10
	maxCHMSystem    = 1 << 20
	maxCHMTOC       = 10_000
	maxCHMTOCDepth  = 16
	maxCHMResetSpan = 64 << 20
	// maxCHMDecompressed caps what the LZX section decompresses to over
	// one extraction, every topic's reading together; maxCHMText the
	// topics' pages read.
	maxCHMDecompressed = 256 << 20
	maxCHMText         = 64 << 20
	// The picture caps are a MOBI book's (RFC GV): one picture, and all
	// the pictures one file hands over.
	maxCHMPicture      = 16 << 20
	maxCHMPictures     = 500
	maxCHMPictureBytes = 64 << 20
)

// The names of the LZX section's parts, in the uncompressed section.
const (
	chmControlData = "::DataSpace/Storage/MSCompressed/ControlData"
	chmContent     = "::DataSpace/Storage/MSCompressed/Content"
	chmResetTable  = "::DataSpace/Storage/MSCompressed/Transform/{7FC28940-9D31-11D0-9B27-00A0C91E9C7C}/InstanceData/ResetTable"
)

func (CHM) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{chmMediaType}, []string{".chm"})
}

func (CHM) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "chm"}
	f, err := openCHM(ctx, r, size, opts.maxCHMDecompressed())
	if err != nil {
		return rep, err
	}
	sys, err := f.system()
	if err != nil {
		return rep, err
	}
	b := &chmBook{f: f, rep: &rep, opts: opts, sys: sys, fileName: opts.Name}
	meta := &BookMetadata{Title: sys.title, SourceFormat: "chm"}
	if l, ok := chmLCIDs[sys.lcid]; ok {
		meta.Language = l.tag
	}
	rep.Metadata = meta.Clean()
	b.title = title(opts.Name, "Help")
	if rep.Metadata != nil && rep.Metadata.Title != "" {
		b.title = rep.Metadata.Title
	}
	b.w = newWriter(ctx, sink, opts)
	err = b.run()
	if b.cyrillic > 0 && rep.Metadata != nil {
		rep.Metadata.Language = ""
	}
	b.noteSkips()
	rep.finish(b.w)
	return rep, err
}

func (o Options) maxCHMDecompressed() int64 {
	if o.chmMaxDecompressed > 0 {
		return o.chmMaxDecompressed
	}
	return maxCHMDecompressed
}

// chmEntry is one file the directory lists.
type chmEntry struct {
	name           string
	section        uint64
	offset, length uint64
}

// chmFile is an opened help file: its directory, and the LZX section once
// a file in it is read.
type chmFile struct {
	ctx      context.Context
	r        io.ReaderAt
	size     int64
	entries  []chmEntry
	byName   map[string]int // lower-cased name to the first entry of it
	content0 int64          // where the uncompressed section's data begins
	lzx      *chmLZX
	lzxErr   error
	// decoded is what the LZX section has decompressed to so far, every
	// reading together, and maxDecoded its cap.
	decoded, maxDecoded int64
}

// openCHM reads the ITSF header and the directory's listing chunks.
func openCHM(ctx context.Context, r io.ReaderAt, size int64, maxDecoded int64) (*chmFile, error) {
	var hdr [0x60]byte
	if size < int64(len(hdr)) {
		return nil, refusedf("the file is too short to be a compiled help file")
	}
	if err := chmReadAt(r, hdr[:], 0); err != nil {
		return nil, err
	}
	if string(hdr[:4]) != "ITSF" {
		return nil, refusedf("the file is not a compiled help file (no ITSF header)")
	}
	if v := binary.LittleEndian.Uint32(hdr[4:]); v != 3 {
		return nil, refusedf("the compiled help file's version %d is not read: version 3 is", v)
	}
	f := &chmFile{ctx: ctx, r: r, size: size, byName: map[string]int{}, maxDecoded: maxDecoded}
	dirOff, dirLen := binary.LittleEndian.Uint64(hdr[0x48:]), binary.LittleEndian.Uint64(hdr[0x50:])
	content0 := binary.LittleEndian.Uint64(hdr[0x58:])
	if !chmInside(dirOff, dirLen, size) || content0 > uint64(size) {
		return nil, refusedf("the compiled help file's header points outside the file")
	}
	f.content0 = int64(content0)
	if err := f.readDirectory(int64(dirOff), int64(dirLen)); err != nil {
		return nil, err
	}
	return f, nil
}

// chmReadAt fills b from off, which the caller checked lies inside the
// file: a reader may answer io.EOF with the last byte.
func chmReadAt(r io.ReaderAt, b []byte, off int64) error {
	n, err := r.ReadAt(b, off)
	if n == len(b) && errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// chmInside reports whether [off, off+n) lies inside a file of size bytes.
func chmInside(off, n uint64, size int64) bool {
	return off <= uint64(size) && n <= uint64(size)-off
}

// readDirectory reads the ITSP header and walks the PMGL listing chunks by
// their chain, each chunk at most once. Index chunks (PMGI) only speed up a
// lookup and are not read.
func (f *chmFile) readDirectory(off, n int64) error {
	var h [0x54]byte
	if n < int64(len(h)) {
		return refusedf("the compiled help file's directory is too short")
	}
	if err := chmReadAt(f.r, h[:], off); err != nil {
		return err
	}
	if string(h[:4]) != "ITSP" {
		return refusedf("the compiled help file's directory has no ITSP header")
	}
	hdrLen := int64(binary.LittleEndian.Uint32(h[8:]))
	chunkSize := int64(binary.LittleEndian.Uint32(h[0x10:]))
	first := binary.LittleEndian.Uint32(h[0x20:])
	nChunks := int64(binary.LittleEndian.Uint32(h[0x2C:]))
	if hdrLen < int64(len(h)) || hdrLen > n || chunkSize < 0x20 || chunkSize > maxCHMChunk {
		return refusedf("the compiled help file's directory header is malformed")
	}
	if nChunks > (n-hdrLen)/chunkSize {
		return refusedf("the compiled help file's directory declares %d chunks, more than it holds", nChunks)
	}
	chunks := off + hdrLen
	visited := map[uint32]bool{}
	chunk := make([]byte, chunkSize)
	for c := first; c != 0xFFFFFFFF; {
		if err := f.ctx.Err(); err != nil {
			return err
		}
		if visited[c] {
			return refusedf("the compiled help file's directory chunks form a loop")
		}
		visited[c] = true
		if int64(c) >= nChunks {
			return refusedf("the compiled help file's directory names chunk %d of %d", c, nChunks)
		}
		if err := chmReadAt(f.r, chunk, chunks+int64(c)*chunkSize); err != nil {
			return err
		}
		if string(chunk[:4]) != "PMGL" {
			return refusedf("the compiled help file's directory chain reaches a chunk that is not a listing")
		}
		free := int64(binary.LittleEndian.Uint32(chunk[4:]))
		if free > chunkSize-20 {
			return refusedf("a directory chunk's free space is larger than the chunk")
		}
		if err := f.readEntries(chunk[20 : chunkSize-free]); err != nil {
			return err
		}
		c = binary.LittleEndian.Uint32(chunk[16:])
	}
	return nil
}

// readEntries reads the directory entries of one listing chunk.
func (f *chmFile) readEntries(b []byte) error {
	for len(b) > 0 {
		nameLen, rest, err := chmEncint(b)
		if err != nil {
			return err
		}
		if nameLen == 0 {
			// The rest of the chunk is padding.
			return nil
		}
		if nameLen > maxCHMName || nameLen > uint64(len(rest)) {
			return refusedf("a directory entry's name is over %d bytes or past its chunk", maxCHMName)
		}
		e := chmEntry{name: string(rest[:nameLen])}
		rest = rest[nameLen:]
		for _, v := range []*uint64{&e.section, &e.offset, &e.length} {
			if *v, rest, err = chmEncint(rest); err != nil {
				return err
			}
		}
		b = rest
		if len(f.entries) == maxCHMEntries {
			return refusedf("the compiled help file lists more than %d entries", maxCHMEntries)
		}
		key := strings.ToLower(e.name)
		if _, dup := f.byName[key]; !dup {
			f.byName[key] = len(f.entries)
		}
		f.entries = append(f.entries, e)
	}
	return nil
}

// chmEncint reads an ENCINT: 7 bits a byte, most significant first, the
// high bit set on every byte but the last. Nine bytes (63 bits) at most.
func chmEncint(b []byte) (uint64, []byte, error) {
	var v uint64
	for i := 0; i < 9; i++ {
		if i >= len(b) {
			return 0, nil, refusedf("a directory entry runs past its chunk")
		}
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return v, b[i+1:], nil
		}
	}
	return 0, nil, refusedf("a directory entry holds a number over 63 bits")
}

// lookup finds a file by its name in the directory, ignoring case as Help
// does.
func (f *chmFile) lookup(name string) (chmEntry, bool) {
	i, ok := f.byName[strings.ToLower(name)]
	if !ok {
		return chmEntry{}, false
	}
	return f.entries[i], true
}

// errCHMTooLarge says a file is past the cap its reader gave.
var errCHMTooLarge = errors.New("the file is over its cap")

// read reads a file of the help file, refusing one over limit bytes before
// any of it is read.
func (f *chmFile) read(e chmEntry, limit int64) ([]byte, error) {
	if e.length > uint64(limit) {
		return nil, errCHMTooLarge
	}
	switch e.section {
	case 0:
		if e.offset > uint64(f.size) || !chmInside(uint64(f.content0)+e.offset, e.length, f.size) {
			return nil, refusedf("the file %q lies outside the compiled help file", cutLabel(e.name))
		}
		b := make([]byte, e.length)
		if err := chmReadAt(f.r, b, f.content0+int64(e.offset)); err != nil {
			return nil, err
		}
		return b, nil
	case 1:
		z, err := f.openLZX()
		if err != nil {
			return nil, err
		}
		return z.read(f, int64(e.offset), int64(e.length))
	}
	return nil, refusedf("the file %q is in section %d, which a compiled help file does not have", cutLabel(e.name), e.section)
}

// readNamed reads a file of the uncompressed section the LZX section is
// described by.
func (f *chmFile) readNamed(name string, limit int64) ([]byte, error) {
	e, ok := f.lookup(name)
	if !ok {
		return nil, refusedf("the compiled help file has no %s", name)
	}
	if e.section != 0 {
		return nil, refusedf("the compiled help file's %s is not in its uncompressed section", name)
	}
	b, err := f.read(e, limit)
	if errors.Is(err, errCHMTooLarge) {
		return nil, refusedf("the compiled help file's %s is over %d bytes", name, limit)
	}
	return b, err
}

// chmLZX is the LZX-compressed section: its parameters, its reset table,
// and the decoder the last reading left, which the next one continues when
// it reads further on in the same stretch.
type chmLZX struct {
	windowBits    int
	resetInterval int64
	blockLen      int64
	resets        []int64 // the compressed offset of each block
	total         int64   // the section's length decompressed
	content       chmEntry

	// dec is the decoder a reading may continue, nil when none may;
	// spare the one decoder made, restarted at each reset point so its
	// window is allocated once.
	dec, spare *lzxDecoder
	// at is the section offset the decoder's frame starts at, and frame
	// that frame, while a decoder is open.
	at    int64
	frame []byte
}

// openLZX reads the LZX section's control data and reset table once.
func (f *chmFile) openLZX() (*chmLZX, error) {
	if f.lzx != nil || f.lzxErr != nil {
		return f.lzx, f.lzxErr
	}
	f.lzx, f.lzxErr = f.readLZX()
	return f.lzx, f.lzxErr
}

func (f *chmFile) readLZX() (*chmLZX, error) {
	ctl, err := f.readNamed(chmControlData, 4096)
	if err != nil {
		return nil, err
	}
	if len(ctl) < 24 || string(ctl[4:8]) != "LZXC" {
		return nil, refusedf("the compressed section's control data is not LZX's")
	}
	version := binary.LittleEndian.Uint32(ctl[8:])
	reset, window := int64(binary.LittleEndian.Uint32(ctl[12:])), int64(binary.LittleEndian.Uint32(ctl[16:]))
	if version == 2 {
		// Version 2 counts both in 32 KiB frames.
		reset, window = reset*lzxFrameSize, window*lzxFrameSize
	}
	z := &chmLZX{resetInterval: reset}
	for bits := lzxMinWindowBits; bits <= lzxMaxWindowBits; bits++ {
		if window == 1<<bits {
			z.windowBits = bits
		}
	}
	if z.windowBits == 0 {
		return nil, refusedf("the compressed section's LZX window of %d bytes is not one LZX allows", window)
	}
	if reset <= 0 || reset%lzxFrameSize != 0 || reset > maxCHMResetSpan {
		return nil, refusedf("the compressed section's reset interval of %d bytes is not a whole number of frames up to %d bytes", reset, maxCHMResetSpan)
	}
	rt, err := f.readNamed(chmResetTable, 8<<20)
	if err != nil {
		return nil, err
	}
	if len(rt) < 40 {
		return nil, refusedf("the compressed section's reset table is too short")
	}
	n, entrySize, hdrLen := uint64(binary.LittleEndian.Uint32(rt[4:])), binary.LittleEndian.Uint32(rt[8:]), uint64(binary.LittleEndian.Uint32(rt[12:]))
	total, blockLen := binary.LittleEndian.Uint64(rt[16:]), binary.LittleEndian.Uint64(rt[32:])
	if entrySize != 8 || hdrLen < 40 || hdrLen > uint64(len(rt)) || n > (uint64(len(rt))-hdrLen)/8 {
		return nil, refusedf("the compressed section's reset table is malformed")
	}
	if blockLen == 0 || blockLen%lzxFrameSize != 0 || uint64(reset)%blockLen != 0 {
		return nil, refusedf("the compressed section's reset table counts blocks of %d bytes, which do not divide its reset interval", blockLen)
	}
	if total > 1<<40 {
		return nil, refusedf("the compressed section declares %d bytes", total)
	}
	z.blockLen, z.total = int64(blockLen), int64(total)
	content, ok := f.lookup(chmContent)
	if !ok || content.section != 0 || !chmInside(uint64(f.content0)+content.offset, content.length, f.size) || content.offset > uint64(f.size) {
		return nil, refusedf("the compressed section's content is missing or outside the file")
	}
	z.content = content
	z.resets = make([]int64, n)
	for i := range z.resets {
		o := binary.LittleEndian.Uint64(rt[hdrLen+uint64(i)*8:])
		if o > content.length {
			return nil, refusedf("the compressed section's reset table points past its content")
		}
		z.resets[i] = int64(o)
	}
	return z, nil
}

// read decompresses [off, off+n) of the section: from the reset point at
// or before off, or onwards from where the last reading stopped when that
// is in the same stretch, so neither the start of the section nor a stretch
// already passed is decompressed again.
func (z *chmLZX) read(f *chmFile, off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off > z.total || n > z.total-off {
		return nil, refusedf("a file lies past the end of the compressed section")
	}
	out := make([]byte, 0, n)
	if n == 0 {
		return out, nil
	}
	rp := off - off%z.resetInterval
	if z.dec == nil || z.at > off || z.at+int64(len(z.frame)) <= rp {
		if err := z.start(f, rp); err != nil {
			return nil, err
		}
	}
	for int64(len(out)) < n {
		pos := off + int64(len(out))
		if pos >= z.at+int64(len(z.frame)) {
			z.at += int64(len(z.frame))
			fr, err := z.dec.next()
			if err != nil {
				z.dec = nil
				if errors.Is(err, io.EOF) {
					return nil, refusedf("the compressed section ends before a file in it")
				}
				return nil, err
			}
			z.frame = fr
			continue
		}
		if pos < z.at {
			return nil, refusedf("the compressed section could not be read back to a file in it")
		}
		k := min(int64(len(z.frame))-(pos-z.at), n-int64(len(out)))
		out = append(out, z.frame[pos-z.at:pos-z.at+k]...)
	}
	return out, nil
}

// start opens a decoder at the reset point rp.
func (z *chmLZX) start(f *chmFile, rp int64) error {
	i := rp / z.blockLen
	if i >= int64(len(z.resets)) {
		return refusedf("the compressed section's reset table has no entry for offset %d", rp)
	}
	c := z.resets[i]
	src := io.NewSectionReader(f.r, f.content0+int64(z.content.offset)+c, int64(z.content.length)-c)
	z.dec, z.frame = nil, nil
	if z.spare != nil {
		if err := z.spare.restart(src, rp); err != nil {
			return err
		}
	} else {
		dec, err := newLZXDecoder(f.ctx, src, z.windowBits, z.resetInterval, rp, z.total, &f.decoded, f.maxDecoded)
		if err != nil {
			return err
		}
		z.spare = dec
	}
	z.dec, z.at = z.spare, rp
	return nil
}

// chmSystem is what the #SYSTEM file says.
type chmSystem struct {
	contents, defaultTopic, title, compiledFile string
	lcid                                        uint32
	cp                                          encoding.Encoding // nil when the language's code page is not known
}

// system reads #SYSTEM. A help file without one is read with no title and
// no contents.
func (f *chmFile) system() (chmSystem, error) {
	var s chmSystem
	e, ok := f.lookup("/#SYSTEM")
	if !ok {
		return s, nil
	}
	b, err := f.read(e, maxCHMSystem)
	if errors.Is(err, errCHMTooLarge) {
		return s, refusedf("the compiled help file's #SYSTEM is over %d bytes", maxCHMSystem)
	}
	if err != nil {
		return s, err
	}
	var raw [7][]byte
	if len(b) >= 4 {
		b = b[4:]
	}
	for len(b) >= 4 {
		code, n := binary.LittleEndian.Uint16(b), int(binary.LittleEndian.Uint16(b[2:]))
		if 4+n > len(b) {
			break
		}
		data := b[4 : 4+n]
		b = b[4+n:]
		switch {
		case code == 4 && len(data) >= 4:
			s.lcid = binary.LittleEndian.Uint32(data)
		case code < uint16(len(raw)) && raw[code] == nil:
			if i := bytes.IndexByte(data, 0); i >= 0 {
				data = data[:i]
			}
			raw[code] = data
		}
	}
	if l, ok := chmLCIDs[s.lcid]; ok {
		s.cp = rtfCodePages[l.cp]
	}
	str := func(v []byte) string {
		cp := s.cp
		if !utf8.Valid(v) && westernOrNone(cp) {
			if c := cyrillicCodePage(v, 1); c != nil {
				cp = c
			}
		}
		if utf8.Valid(v) || cp == nil {
			return string(bytes.ToValidUTF8(v, []byte("�")))
		}
		out, err := cp.NewDecoder().Bytes(v)
		if err != nil {
			return ""
		}
		return string(out)
	}
	s.contents, s.defaultTopic, s.title, s.compiledFile = str(raw[0]), str(raw[2]), str(raw[3]), str(raw[6])
	return s, nil
}

// chmLCIDs maps the languages a help file is commonly written in to a BCP
// 47 tag and the code page its 8-bit text is in. Others are read as UTF-8,
// or windows-1252 when they are not.
var chmLCIDs = map[uint32]struct {
	tag string
	cp  int
}{
	0x0409: {"en", 1252}, 0x0809: {"en", 1252}, 0x0c09: {"en", 1252}, 0x1009: {"en", 1252},
	0x0407: {"de", 1252}, 0x040c: {"fr", 1252}, 0x0410: {"it", 1252}, 0x040a: {"es", 1252}, 0x0c0a: {"es", 1252},
	0x0413: {"nl", 1252}, 0x0416: {"pt", 1252}, 0x0816: {"pt", 1252}, 0x041d: {"sv", 1252}, 0x0406: {"da", 1252},
	0x0414: {"nb", 1252}, 0x040b: {"fi", 1252},
	0x0419: {"ru", 1251}, 0x0422: {"uk", 1251}, 0x0423: {"be", 1251}, 0x0402: {"bg", 1251},
	0x0415: {"pl", 1250}, 0x0405: {"cs", 1250}, 0x040e: {"hu", 1250}, 0x041b: {"sk", 1250},
	0x0424: {"sl", 1250}, 0x041a: {"hr", 1250}, 0x0418: {"ro", 1250},
}

// chmBook writes one help file.
type chmBook struct {
	f        *chmFile
	w        *writer
	rep      *Report
	opts     Options
	sys      chmSystem
	title    string
	fileName string
	written  bool
	// textBytes counts the topics' pages read; pictures and pictureBytes
	// the pictures handed over.
	textBytes             int64
	pictures              int
	pictureBytes          int64
	missing, unreadable   int
	badCharset            []string // the first few encodings, each once
	badPages              int
	guessedCharset        int
	cyrillic              int    // unmarked pages read as Cyrillic against the file's Western language
	cyrillicName          string // the code page they were read in
	skippedPictures, deep int
	tocCut                bool
}

func (b *chmBook) noteSkips() {
	if b.missing > 0 {
		b.rep.note("%d contents entries name a page the file does not hold, and were skipped", b.missing)
	}
	if b.unreadable > 0 {
		b.rep.note("%d pages could not be read and were skipped", b.unreadable)
	}
	if b.badPages > 0 {
		b.rep.note("%d pages in an encoding that is not read (%s) were skipped", b.badPages, strings.Join(b.badCharset, ", "))
	}
	if b.cyrillic > 0 {
		b.rep.note("%d of its pages (or its contents file) name no encoding and are Cyrillic text, read as %s; the file's language (LCID %#04x) is not what its pages are in, so no language is reported", b.cyrillic, b.cyrillicName, b.sys.lcid)
	}
	if b.guessedCharset > 0 {
		b.rep.note("%d pages name no encoding and the file's language gives none: they were read as windows-1252", b.guessedCharset)
	}
	if b.skippedPictures > 0 {
		b.rep.note("%d pictures past the file's picture caps (%d pictures, %d bytes) were left out", b.skippedPictures, maxCHMPictures, maxCHMPictureBytes)
	}
	if b.deep > 0 {
		b.rep.note("%d contents entries deeper than %d levels were skipped", b.deep, maxCHMTOCDepth)
	}
	if b.tocCut {
		b.rep.note("only the first %d contents entries were read", maxCHMTOC)
	}
}

func (b *chmBook) emit(md string) error {
	if b.written {
		md = "\n\n" + md
	}
	b.written = true
	return b.w.text(md)
}

// run writes the title and then the topics, stopping at the first block
// the sink will not take: nothing after it is read or decompressed.
func (b *chmBook) run() error {
	if err := b.emit(heading(1, b.title)); err != nil {
		return err
	}
	toc, err := b.contents()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	if len(toc) > 0 {
		for _, e := range toc {
			level := min(e.depth+1, 6)
			if e.local == "" {
				// A contents entry with no page of its own heads the
				// entries under it.
				if e.name != "" {
					if err := b.emit(heading(level, e.name)); err != nil {
						return err
					}
				}
				continue
			}
			p, ok := b.resolve("/", e.local)
			if _, held := b.f.lookup(p); !ok || !held {
				b.missing++
				continue
			}
			if seen[strings.ToLower(p)] {
				continue
			}
			seen[strings.ToLower(p)] = true
			if err := b.topic(p, e.name, level); err != nil {
				return err
			}
		}
		return nil
	}
	var order []string
	if p, ok := b.resolve("/", b.sys.defaultTopic); ok && b.sys.defaultTopic != "" {
		if _, held := b.f.lookup(p); held {
			order = append(order, p)
		}
	}
	for _, e := range b.f.entries {
		if chmPage(e.name) {
			order = append(order, e.name)
		}
	}
	for _, p := range order {
		if seen[strings.ToLower(p)] {
			continue
		}
		seen[strings.ToLower(p)] = true
		if err := b.topic(p, "", 2); err != nil {
			return err
		}
	}
	return nil
}

// chmPage reports whether a directory name is a page of the help file's
// own: an HTML file outside the system names (#..., $..., ::...).
func chmPage(name string) bool {
	if !strings.HasPrefix(name, "/") || strings.HasPrefix(name, "/#") || strings.HasPrefix(name, "/$") {
		return false
	}
	ext := strings.ToLower(path.Ext(name))
	return ext == ".htm" || ext == ".html"
}

// topic reads one page and writes it. label is its contents entry, the
// heading it gets at level when it has none of its own; with no contents,
// the page's own <title>.
func (b *chmBook) topic(p, label string, level int) error {
	if err := b.w.ctx.Err(); err != nil {
		return err
	}
	e, _ := b.f.lookup(p)
	if e.length > maxHTMLBytes {
		return refusedf("the page %q is over %d bytes", cutLabel(p), maxHTMLBytes)
	}
	if b.textBytes+int64(e.length) > maxCHMText {
		return refusedf("the help file's pages are over %d bytes", maxCHMText)
	}
	b.textBytes += int64(e.length)
	data, err := b.f.read(e, maxHTMLBytes)
	if errors.Is(err, errCHMTooLarge) {
		return refusedf("the page %q is over %d bytes", cutLabel(p), maxHTMLBytes)
	}
	if err != nil {
		// A refusal is the file's: hostile, not damaged.
		if b.w.ctx.Err() != nil || IsRefused(err) {
			return err
		}
		b.unreadable++
		return nil
	}
	data, ok := b.decode(data, 3)
	if !ok {
		return nil
	}
	doc, err := parseHTMLPage(b.w.ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		if b.w.ctx.Err() != nil || IsRefused(err) {
			return err
		}
		b.unreadable++
		return nil
	}
	if label == "" {
		if t := findElement(doc, "title"); t != nil {
			label = oneLine(strings.TrimSpace(textOf(t)))
		}
	}
	dir := path.Dir(p)
	h := &htmlState{w: b.w, rep: b.rep, infer: b.opts.InferHeadings, body: &htmlBody{
		title:      b.title,
		label:      label,
		labelLevel: level,
		picture:    func(ref string) ([]byte, bool) { return b.picture(dir, ref) },
		ownRef:     func(img *html.Node) (string, bool) { return b.ownRef(dir, img) },
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

// decode returns a page as UTF-8: in the encoding its <meta> names, else as
// UTF-8 when it is valid UTF-8, else as windows-1251 or KOI8-R when it is
// Cyrillic text and the file's language is Western or unknown, else in the
// code page of the file's language. A page in an encoding not on the fixed
// list is skipped.
// minRuns is how many Cyrillic runs make unmarked text Cyrillic
// (cyrillicCodePage): a page has many words, the contents file's names few.
func (b *chmBook) decode(data []byte, minRuns int) ([]byte, bool) {
	var enc encoding.Encoding
	if label := htmlMetaCharset(data); label != "" {
		switch {
		case label == "utf-8" || label == "utf8":
			return bytes.ToValidUTF8(data, []byte("�")), true
		case label == "us-ascii":
			enc = charmap.Windows1252
		case fb2Encodings[label] != nil:
			enc = fb2Encodings[label]
		default:
			b.badPages++
			if len(b.badCharset) < 10 && !slices.Contains(b.badCharset, cutLabel(label)) {
				b.badCharset = append(b.badCharset, cutLabel(label))
			}
			return nil, false
		}
	} else if utf8.Valid(data) {
		return data, true
	} else if c := cyrillicCodePage(data, minRuns); c != nil && westernOrNone(b.sys.cp) {
		// A Russian help file compiled on English Windows says its
		// language is English and names no encoding on its pages.
		enc = c
		b.cyrillic++
		b.cyrillicName = c.String()
	} else if enc = b.sys.cp; enc == nil {
		b.guessedCharset++
		enc = charmap.Windows1252
	}
	out, err := enc.NewDecoder().Bytes(data)
	if err != nil {
		return bytes.ToValidUTF8(data, []byte("�")), true
	}
	return out, true
}

// resolve resolves a reference a page in dir makes to a file of this help
// file: relative to dir, or absolute from the file's root; an ms-its: or
// mk:@MSITStore: reference only when it names this same file. Fragments
// and queries are dropped. Any other scheme is not resolved.
func (b *chmBook) resolve(dir, ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if inner, ok := chmSameFileRef(ref, b.sys.compiledFile, b.fileName); ok {
		ref, dir = inner, "/"
	} else if hasScheme(ref) {
		return "", false
	}
	if i := strings.IndexAny(ref, "#?"); i >= 0 {
		ref = ref[:i]
	}
	ref = strings.ReplaceAll(ref, "\\", "/")
	if u, err := url.PathUnescape(ref); err == nil {
		ref = u
	}
	if ref == "" || strings.ContainsRune(ref, 0) {
		return "", false
	}
	if !strings.HasPrefix(ref, "/") {
		ref = path.Join(dir, ref)
	}
	p := path.Clean("/" + ref)
	return p, p != "/"
}

// chmSameFileRef returns the path inside a reference of the ms-its: or
// mk:@MSITStore: form, when the file it names is this one: by the compiled
// file's own name from #SYSTEM, or the name it was kept under.
func chmSameFileRef(ref, compiled, kept string) (string, bool) {
	low := strings.ToLower(ref)
	var rest string
	switch {
	case strings.HasPrefix(low, "ms-its:"):
		rest = ref[len("ms-its:"):]
	case strings.HasPrefix(low, "mk:@msitstore:"):
		rest = ref[len("mk:@msitstore:"):]
	default:
		return "", false
	}
	i := strings.Index(rest, "::")
	if i < 0 {
		return "", false
	}
	name := chmBase(rest[:i])
	if name == "" || (name != chmBase(compiled) && name != chmBase(kept)) {
		return "", false
	}
	return rest[i+2:], true
}

// chmBase is a help file's name without its folder or .chm, lower-cased.
func chmBase(name string) string {
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	if name == "." || name == "/" {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(name), ".chm")
}

// ownRef names the picture an <img> points to with an ms-its: or
// mk:@MSITStore: reference to this same file.
func (b *chmBook) ownRef(dir string, img *html.Node) (string, bool) {
	src := strings.TrimSpace(imageSrc(img))
	if _, ok := chmSameFileRef(src, b.sys.compiledFile, b.fileName); !ok {
		return "", false
	}
	return b.resolve(dir, src)
}

// picture reads a picture a page in dir names, while pictures are kept and
// under the caps. Only a picture the file holds is read; one that is not a
// known image type is not passed on.
func (b *chmBook) picture(dir, ref string) ([]byte, bool) {
	if !b.opts.Images {
		return nil, false
	}
	p, ok := b.resolve(dir, ref)
	if !ok {
		return nil, false
	}
	e, ok := b.f.lookup(p)
	if !ok {
		return nil, false
	}
	if e.length > maxCHMPicture || b.pictures >= maxCHMPictures || b.pictureBytes+int64(e.length) > maxCHMPictureBytes {
		b.skippedPictures++
		return nil, false
	}
	data, err := b.f.read(e, maxCHMPicture)
	if err != nil {
		return nil, false
	}
	if mt, _ := imageType(data); mt == "" {
		return nil, false
	}
	b.pictures++
	b.pictureBytes += int64(len(data))
	return data, true
}

// chmTOCEntry is one entry of the table of contents: its name, the page it
// opens (none for a heading over other entries) and how deep it is.
type chmTOCEntry struct {
	name, local string
	depth       int
}

// contents reads the table of contents #SYSTEM names, or the one .hhc file
// the directory lists when it names none. No contents is no error: the
// pages are then read in directory order.
func (b *chmBook) contents() ([]chmTOCEntry, error) {
	var e chmEntry
	found := false
	if p, ok := b.resolve("/", b.sys.contents); ok && b.sys.contents != "" {
		e, found = b.f.lookup(p)
	}
	if !found {
		for _, c := range b.f.entries {
			if strings.HasPrefix(c.name, "/") && strings.EqualFold(path.Ext(c.name), ".hhc") {
				e, found = c, true
				break
			}
		}
	}
	if !found {
		return nil, nil
	}
	data, err := b.f.read(e, maxHTMLBytes)
	if errors.Is(err, errCHMTooLarge) {
		return nil, refusedf("the table of contents is over %d bytes", maxHTMLBytes)
	}
	if err != nil {
		if b.w.ctx.Err() != nil || IsRefused(err) {
			return nil, err
		}
		b.unreadable++
		return nil, nil
	}
	data, ok := b.decode(data, 1)
	if !ok {
		return nil, nil
	}
	if err := countHTML(b.w.ctx, &ctxReader{ctx: b.w.ctx, r: bytes.NewReader(data)}); err != nil {
		return nil, err
	}
	return b.parseTOC(data), nil
}

// parseTOC reads a sitemap's entries with the HTML tokenizer: each
// <object type="text/sitemap"> gives one, its Name and Local params, at the
// depth of the <ul> lists around it.
func (b *chmBook) parseTOC(data []byte) []chmTOCEntry {
	var out []chmTOCEntry
	z := html.NewTokenizer(bytes.NewReader(data))
	depth := 0
	var cur *chmTOCEntry
	for tokens := 1; ; tokens++ {
		if tokens%4096 == 0 && b.w.ctx.Err() != nil {
			return out
		}
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return out
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
		default:
			continue
		}
		name, hasAttr := z.TagName()
		attrs := map[string]string{}
		for hasAttr {
			var k, v []byte
			k, v, hasAttr = z.TagAttr()
			key := strings.ToLower(string(k))
			if _, dup := attrs[key]; !dup {
				attrs[key] = string(v)
			}
		}
		switch tag := string(name); {
		case tag == "ul" && tt == html.StartTagToken:
			depth++
		case tag == "ul" && tt == html.EndTagToken:
			depth = max(0, depth-1)
		case tag == "object" && tt != html.EndTagToken:
			cur = nil
			if strings.EqualFold(strings.TrimSpace(attrs["type"]), "text/sitemap") {
				cur = &chmTOCEntry{depth: max(depth, 1)}
			}
		case tag == "param" && cur != nil && tt != html.EndTagToken:
			v := strings.TrimSpace(attrs["value"])
			switch strings.ToLower(strings.TrimSpace(attrs["name"])) {
			case "name":
				if cur.name == "" {
					cur.name = oneLine(v)
				}
			case "local":
				if cur.local == "" {
					cur.local = v
				}
			}
		case tag == "object" && tt == html.EndTagToken && cur != nil:
			e := *cur
			cur = nil
			if e.depth > maxCHMTOCDepth {
				b.deep++
				continue
			}
			if len(out) == maxCHMTOC {
				b.tocCut = true
				return out
			}
			out = append(out, e)
		}
	}
}

// westernOrNone reports whether a help file's own code page is windows-1252
// or unknown: the cases where its unmarked pages may still be Cyrillic.
func westernOrNone(cp encoding.Encoding) bool {
	return cp == nil || cp == encoding.Encoding(charmap.Windows1252)
}
