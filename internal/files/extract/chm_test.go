package extract

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// chmPart is one file of a help file built by buildCHM.
type chmPart struct {
	name       string
	data       []byte
	compressed bool
}

// chmSpec builds a compiled help file from the ITSF description the reader
// follows: the header, an ITSP directory of PMGL chunks, the uncompressed
// section, and the LZX section as literal frames (literalsLZX).
type chmSpec struct {
	parts         []chmPart
	windowBits    int
	resetInterval int
	// chunkSize is the directory chunk size, and perChunk how many
	// entries each chunk holds; next, when set, overrides a chunk's link
	// to the next one.
	chunkSize, perChunk int
	next                map[int]int32
	// lzx, when set, is the compressed section as it is, frames its reset
	// table and total its length; extra are directory entries into it.
	lzx    []byte
	frames []int64
	total  int64
	extra  []chmEntry
}

func chmEncintOf(v uint64) []byte {
	var g []byte
	for {
		g = append([]byte{byte(v & 0x7f)}, g...)
		if v >>= 7; v == 0 {
			break
		}
	}
	for i := 0; i < len(g)-1; i++ {
		g[i] |= 0x80
	}
	return g
}

func (s chmSpec) build() []byte {
	if s.windowBits == 0 {
		s.windowBits = 15
	}
	if s.resetInterval == 0 {
		s.resetInterval = 2 * lzxFrameSize
	}
	if s.chunkSize == 0 {
		s.chunkSize = 4096
	}
	if s.perChunk == 0 {
		s.perChunk = 1 << 30
	}
	var sec0, sec1 []byte
	var entries []chmEntry
	for _, p := range s.parts {
		e := chmEntry{name: p.name, length: uint64(len(p.data))}
		if p.compressed {
			e.section, e.offset = 1, uint64(len(sec1))
			sec1 = append(sec1, p.data...)
		} else {
			e.offset = uint64(len(sec0))
			sec0 = append(sec0, p.data...)
		}
		entries = append(entries, e)
	}
	add0 := func(name string, data []byte) {
		entries = append(entries, chmEntry{name: name, offset: uint64(len(sec0)), length: uint64(len(data))})
		sec0 = append(sec0, data...)
	}
	stream, frames := literalsLZX(sec1, s.windowBits, s.resetInterval)
	total := int64(len(sec1))
	if s.lzx != nil {
		stream, frames, total = s.lzx, s.frames, s.total
		entries = append(entries, s.extra...)
	}
	ctl := make([]byte, 28)
	binary.LittleEndian.PutUint32(ctl[0:], 6)
	copy(ctl[4:], "LZXC")
	binary.LittleEndian.PutUint32(ctl[8:], 2)
	binary.LittleEndian.PutUint32(ctl[12:], uint32(s.resetInterval/lzxFrameSize))
	binary.LittleEndian.PutUint32(ctl[16:], uint32((1<<s.windowBits)/lzxFrameSize))
	binary.LittleEndian.PutUint32(ctl[20:], 1)
	add0(chmControlData, ctl)
	rt := make([]byte, 40, 40+8*len(frames))
	binary.LittleEndian.PutUint32(rt[0:], 2)
	binary.LittleEndian.PutUint32(rt[4:], uint32(len(frames)))
	binary.LittleEndian.PutUint32(rt[8:], 8)
	binary.LittleEndian.PutUint32(rt[12:], 40)
	binary.LittleEndian.PutUint64(rt[16:], uint64(total))
	binary.LittleEndian.PutUint64(rt[24:], uint64(len(stream)))
	binary.LittleEndian.PutUint64(rt[32:], lzxFrameSize)
	for _, f := range frames {
		rt = binary.LittleEndian.AppendUint64(rt, uint64(f))
	}
	add0(chmResetTable, rt)
	add0(chmContent, stream)

	// The listing chunks.
	var chunks [][]byte
	var cur []byte
	n := 0
	flush := func() {
		chunks = append(chunks, cur)
		cur, n = nil, 0
	}
	for _, e := range entries {
		b := append(chmEncintOf(uint64(len(e.name))), e.name...)
		b = append(b, chmEncintOf(e.section)...)
		b = append(b, chmEncintOf(e.offset)...)
		b = append(b, chmEncintOf(e.length)...)
		if n == s.perChunk || 20+len(cur)+len(b) > s.chunkSize {
			flush()
		}
		cur = append(cur, b...)
		n++
	}
	flush()
	var dir []byte
	hdr := make([]byte, 0x54)
	copy(hdr, "ITSP")
	binary.LittleEndian.PutUint32(hdr[4:], 1)
	binary.LittleEndian.PutUint32(hdr[8:], 0x54)
	binary.LittleEndian.PutUint32(hdr[0x10:], uint32(s.chunkSize))
	binary.LittleEndian.PutUint32(hdr[0x18:], 1)
	binary.LittleEndian.PutUint32(hdr[0x1C:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(hdr[0x20:], 0)
	binary.LittleEndian.PutUint32(hdr[0x24:], uint32(len(chunks)-1))
	binary.LittleEndian.PutUint32(hdr[0x2C:], uint32(len(chunks)))
	dir = append(dir, hdr...)
	for i, c := range chunks {
		chunk := make([]byte, s.chunkSize)
		copy(chunk, "PMGL")
		binary.LittleEndian.PutUint32(chunk[4:], uint32(s.chunkSize-20-len(c)))
		prev, next := int32(i-1), int32(i+1)
		if i == len(chunks)-1 {
			next = -1
		}
		if v, ok := s.next[i]; ok {
			next = v
		}
		binary.LittleEndian.PutUint32(chunk[12:], uint32(prev))
		binary.LittleEndian.PutUint32(chunk[16:], uint32(next))
		copy(chunk[20:], c)
		dir = append(dir, chunk...)
	}

	head := make([]byte, 0x60)
	copy(head, "ITSF")
	binary.LittleEndian.PutUint32(head[4:], 3)
	binary.LittleEndian.PutUint32(head[8:], 0x60)
	binary.LittleEndian.PutUint32(head[12:], 1)
	binary.LittleEndian.PutUint32(head[20:], 0x409)
	binary.LittleEndian.PutUint64(head[0x38:], 0x60)
	binary.LittleEndian.PutUint64(head[0x40:], 0x18)
	binary.LittleEndian.PutUint64(head[0x48:], 0x78)
	binary.LittleEndian.PutUint64(head[0x50:], uint64(len(dir)))
	binary.LittleEndian.PutUint64(head[0x58:], uint64(0x78+len(dir)))
	out := append(head, make([]byte, 0x18)...)
	out = append(out, dir...)
	return append(out, sec0...)
}

// chmSystemFile is a #SYSTEM file naming the contents, default topic,
// title, language and compiled file name.
func chmSystemFile(contents, defaultTopic, title string, lcid uint32, compiled string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, 3)
	rec := func(code uint16, data []byte) {
		b = binary.LittleEndian.AppendUint16(b, code)
		b = binary.LittleEndian.AppendUint16(b, uint16(len(data)))
		b = append(b, data...)
	}
	cstr := func(s string) []byte { return append([]byte(s), 0) }
	if contents != "" {
		rec(0, cstr(contents))
	}
	rec(2, cstr(defaultTopic))
	rec(3, cstr(title))
	rec(4, binary.LittleEndian.AppendUint32(make([]byte, 0, 36), lcid))
	rec(6, cstr(compiled))
	return b
}

// chmTOC is a sitemap of one entry a page.
func chmTOC(pages ...string) []byte {
	var b strings.Builder
	b.WriteString("<html><body><ul>")
	for _, p := range pages {
		fmt.Fprintf(&b, `<li><object type="text/sitemap"><param name="Name" value="%s"><param name="Local" value="%s"></object>`, p, p)
	}
	b.WriteString("</ul></body></html>")
	return []byte(b.String())
}

func TestCHM_FixturesMatchTheirGoldens(t *testing.T) {
	for _, f := range []string{"book.chm", "notoc.chm"} {
		t.Run(f, func(t *testing.T) {
			data, err := os.ReadFile("testdata/chm/" + f)
			if err != nil {
				t.Fatal(err)
			}
			s, rep := run(t, CHM{}, data, Options{Name: f, Images: true})
			golden(t, "chm/"+f+".md", s.md.String())
			m, err := json.MarshalIndent(struct {
				Format   string        `json:"format"`
				Images   int           `json:"images"`
				Tables   int           `json:"tables"`
				Notes    []string      `json:"notes,omitempty"`
				Metadata *BookMetadata `json:"metadata"`
			}{rep.Format, rep.Images, rep.Tables, rep.Notes, rep.Metadata}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, "chm/"+f+".json", string(m)+"\n")
		})
	}
}

// bigPages are the pages testdata/chm/src/big/gen.py writes, made again
// here: the same generator, so big.chm is checked against its source.
func bigPages() []string {
	words := strings.Fields("lamp wick glass oil ledger keeper column total entry week page line " +
		"brass hook shelf candle smoke flame light dark night morning road " +
		"bridge river stone wall gate yard field barn wheel cart horse rope " +
		"nail board door window roof chimney stair floor table chair bench " +
		"cup plate bowl spoon knife bread salt water tea letter stamp seal " +
		"clock bell")
	state := 12345
	rnd := func(n int) int {
		state = (state*1103515245 + 12345) % (1 << 31)
		return state % n
	}
	var pages []string
	for ch := 1; ch <= 12; ch++ {
		var paras []string
		for p := 0; p < 40; p++ {
			var sents []string
			for s := 0; s < 6; s++ {
				n := 6 + rnd(8)
				ws := make([]string, n)
				for i := range ws {
					ws[i] = words[rnd(len(words))]
				}
				sent := strings.Join(ws, " ")
				sents = append(sents, strings.ToUpper(sent[:1])+sent[1:]+".")
			}
			paras = append(paras, strings.Join(sents, " "))
		}
		pages = append(pages, fmt.Sprintf("## Chapter %d\n\n%s", ch, strings.Join(paras, "\n\n")))
	}
	return pages
}

// big.chm spans four LZX frames and two reset intervals, written by
// chmcmd: every chapter reads back as its source wrote it.
func TestCHM_ChmcmdFileOfSeveralFramesReadsBack(t *testing.T) {
	data, err := os.ReadFile("testdata/chm/big.chm")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := run(t, CHM{}, data, Options{Name: "big.chm"})
	want := "# The Long Ledger\n\n" + strings.Join(bigPages(), "\n\n")
	if got := s.md.String(); got != want {
		i := 0
		for i < len(got) && i < len(want) && got[i] == want[i] {
			i++
		}
		t.Fatalf("differs at byte %d of %d:\n%q\nwant\n%q", i, len(want), got[i:min(len(got), i+80)], want[i:min(len(want), i+80)])
	}
}

func TestCHM_LaterTopicDoesNotDecompressFromStart(t *testing.T) {
	data, err := os.ReadFile("testdata/chm/big.chm")
	if err != nil {
		t.Fatal(err)
	}
	f, err := openCHM(context.Background(), bytes.NewReader(data), int64(len(data)), maxCHMDecompressed)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := f.lookup("/ch12.htm")
	if !ok {
		t.Fatal("no /ch12.htm")
	}
	page, err := f.read(e, maxHTMLBytes)
	if err != nil || !bytes.Contains(page, []byte("<h1>Chapter 12</h1>")) {
		t.Fatalf("err %v, page %q", err, page[:min(len(page), 80)])
	}
	z := f.lzx
	if z.resetInterval >= z.total || int64(e.offset) < z.resetInterval {
		t.Fatalf("the fixture's last chapter is not past its first reset point (offset %d, interval %d)", e.offset, z.resetInterval)
	}
	// Only the stretch from the reset point before the chapter is read.
	if f.decoded > z.total-z.resetInterval {
		t.Fatalf("reading the last chapter decompressed %d of the section's %d bytes, want at most %d", f.decoded, z.total, z.total-z.resetInterval)
	}
	// A page further on in the same stretch continues the decoder; one
	// before it starts again from its own reset point.
	before := f.decoded
	toc, _ := f.lookup("/toc.hhc")
	if _, err := f.read(toc, maxHTMLBytes); err != nil || f.decoded-before > int64(toc.offset+toc.length)-int64(e.offset) {
		t.Fatalf("err %v: reading on decompressed %d bytes", err, f.decoded-before)
	}
}

// A help file built in the test: the pages and a picture in the LZX
// section, #SYSTEM in the uncompressed one.
func testCHM(pages map[string]string, order []string, extra ...chmPart) []byte {
	parts := []chmPart{{name: "/#SYSTEM", data: chmSystemFile("toc.hhc", order[0], "Test Help", 0x409, "help")}}
	parts = append(parts, chmPart{name: "/toc.hhc", data: chmTOC(order...), compressed: true})
	for _, p := range order {
		parts = append(parts, chmPart{name: "/" + p, data: []byte(pages[p]), compressed: true})
	}
	return chmSpec{parts: append(parts, extra...)}.build()
}

func TestCHM_ExternalImageIsNotFetched(t *testing.T) {
	png := tinyPNG(t)
	page := `<html><body><p>Pictures:</p>
<img src="pic.png" alt="here">
<img src="ms-its:help.chm::/pic.png" alt="this file by name">
<img src="ms-its:other.chm::/pic.png" alt="another file">
<img src="mk:@MSITStore:C:\Windows\Help\other.chm::/pic.png" alt="another file again">
<img src="http://example.invalid/remote.png" alt="remote">
<img src="file:///etc/passwd" alt="local">
<img src="../../../etc/passwd" alt="climbing">
</body></html>`
	data := testCHM(map[string]string{"a.htm": page}, []string{"a.htm"}, chmPart{name: "/pic.png", data: png, compressed: true})
	s, rep := run(t, CHM{}, data, Options{Name: "kept-as.chm", Images: true})
	md := s.md.String()
	if len(s.images) != 2 || rep.Images != 2 {
		t.Fatalf("handed over %d pictures, want the 2 this file holds:\n%s", len(s.images), md)
	}
	for _, want := range []string{"[image: another file](ms-its:other.chm::/pic.png)", "[image: remote](http://example.invalid/remote.png)", "[image: local](file:///etc/passwd)", "[image: climbing]"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
}

func TestCHM_MaxCharsStopsReading(t *testing.T) {
	// Twenty pages of 64 KiB each, a reset interval apiece: reading all of
	// them reads the whole file.
	pages := map[string]string{}
	var order []string
	for i := 0; i < 20; i++ {
		p := fmt.Sprintf("p%02d.htm", i)
		order = append(order, p)
		pages[p] = "<html><body><p>" + strings.Repeat(fmt.Sprintf("Page %d words. ", i), 64<<10/10)[:64<<10-40] + "</p></body></html>"
	}
	data := testCHM(pages, order)
	r := &countingReaderAt{r: bytes.NewReader(data)}
	s := &memSink{limit: 4 << 10}
	_, err := CHM{}.Extract(context.Background(), r, int64(len(data)), s, Options{Name: "big.chm"})
	if !errors.Is(err, ErrSinkFull) {
		t.Fatalf("err = %v, want ErrSinkFull", err)
	}
	if got := r.n.Load(); got > int64(len(data))/4 {
		t.Fatalf("read %d of the file's %d bytes after the output was full", got, len(data))
	}
	full := &countingReaderAt{r: bytes.NewReader(data)}
	if _, err := (CHM{}).Extract(context.Background(), full, int64(len(data)), &memSink{}, Options{Name: "big.chm"}); err != nil || full.n.Load() < int64(len(data))*3/4 {
		t.Fatalf("the whole book: err %v, read %d of %d bytes", err, full.n.Load(), len(data))
	}
}

func TestCHM_PMGLChunkLoopIsRefused(t *testing.T) {
	spec := chmSpec{
		parts:     []chmPart{{name: "/#SYSTEM", data: chmSystemFile("", "a.htm", "T", 0x409, "t")}, {name: "/a.htm", data: []byte("<p>a</p>")}},
		perChunk:  2,
		next:      map[int]int32{1: 0},
		chunkSize: 512,
	}
	_, err := extractBytes(CHM{}, "loop.chm", spec.build())
	if !IsRefused(err) || !strings.Contains(err.Error(), "loop") {
		t.Fatalf("err = %v, want the chunk loop refused", err)
	}
	// A chain that names a chunk past the directory is refused too.
	spec.next = map[int]int32{0: 1000}
	if _, err := extractBytes(CHM{}, "far.chm", spec.build()); !IsRefused(err) || !strings.Contains(err.Error(), "names chunk") {
		t.Fatalf("err = %v, want the chunk past the directory refused", err)
	}
}

// A page that is not in the contents is not read when there are contents,
// a contents entry repeated is read once, and with no contents the default
// topic comes first and then the directory's order.
func TestCHM_ReadsTheContentsInOrderEachPageOnce(t *testing.T) {
	pages := map[string]string{
		"b.htm": "<html><head><title>B</title></head><body><p>second</p></body></html>",
		"a.htm": "<html><head><title>A</title></head><body><p>first</p></body></html>",
		"c.htm": "<html><body><p>not in the contents</p></body></html>",
	}
	parts := []chmPart{
		{name: "/#SYSTEM", data: chmSystemFile("toc.hhc", "b.htm", "T", 0x409, "t")},
		{name: "/toc.hhc", data: chmTOC("b.htm", "a.htm", "b.htm#again"), compressed: true},
	}
	for _, p := range []string{"a.htm", "b.htm", "c.htm"} {
		parts = append(parts, chmPart{name: "/" + p, data: []byte(pages[p]), compressed: true})
	}
	s, _ := run(t, CHM{}, chmSpec{parts: parts}.build(), Options{Name: "t.chm"})
	if got, want := s.md.String(), "# T\n\n## b.htm\n\nsecond\n\n## a.htm\n\nfirst"; got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
	parts[0].data = chmSystemFile("", "b.htm", "T", 0x409, "t")
	parts = append(parts[:1], parts[2:]...)
	s, _ = run(t, CHM{}, chmSpec{parts: parts}.build(), Options{Name: "t.chm"})
	if got, want := s.md.String(), "# T\n\n## B\n\nsecond\n\n## A\n\nfirst\n\nnot in the contents"; got != want {
		t.Fatalf("no contents: got\n%q\nwant\n%q", got, want)
	}
}

func TestCHM_TooManyEntriesIsRefused(t *testing.T) {
	var parts []chmPart
	for i := 0; i <= maxCHMEntries; i++ {
		parts = append(parts, chmPart{name: fmt.Sprintf("/%d", i)})
	}
	_, err := extractBytes(CHM{}, "many.chm", chmSpec{parts: parts, chunkSize: 1 << 16}.build())
	if !IsRefused(err) || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("err = %v, want the directory refused", err)
	}
}

func TestCHM_LongEntryNameIsRefused(t *testing.T) {
	parts := []chmPart{{name: "/" + strings.Repeat("n", maxCHMName)}}
	_, err := extractBytes(CHM{}, "long.chm", chmSpec{parts: parts}.build())
	if !IsRefused(err) || !strings.Contains(err.Error(), "name is over") {
		t.Fatalf("err = %v, want the long name refused", err)
	}
}

func TestCHM_WindowOutsideLZXRangeIsRefused(t *testing.T) {
	data := testCHM(map[string]string{"a.htm": "<p>a</p>"}, []string{"a.htm"})
	f, err := openCHM(context.Background(), bytes.NewReader(data), int64(len(data)), maxCHMDecompressed)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := f.lookup(chmControlData)
	ctl := f.content0 + int64(e.offset)
	for _, frames := range []uint32{0, 3, 128} { // 0, 96 KiB, 4 MiB
		bad := append([]byte(nil), data...)
		binary.LittleEndian.PutUint32(bad[ctl+16:], frames)
		if _, err := extractBytes(CHM{}, "w.chm", bad); !IsRefused(err) || !strings.Contains(err.Error(), "compressed section's LZX window") {
			t.Errorf("window of %d frames: err = %v, want a refusal", frames, err)
		}
	}
}

func TestCHM_PageInAnUnlistedEncodingIsSkipped(t *testing.T) {
	pages := map[string]string{
		"a.htm": `<html><head><meta charset="shift_jis"></head><body><p>skipped</p></body></html>`,
		"b.htm": `<html><head><meta charset="koi8-r"></head><body><p>` + "\xf0\xd2\xc9\xd7\xc5\xd4" + `</p></body></html>`,
	}
	s, rep := run(t, CHM{}, testCHM(pages, []string{"a.htm", "b.htm"}), Options{Name: "e.chm"})
	md := s.md.String()
	if strings.Contains(md, "skipped") || !strings.Contains(md, "Привет") {
		t.Fatalf("got:\n%s", md)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "shift_jis") {
		t.Errorf("notes %q do not name the encoding", rep.Notes)
	}
}

func TestCHM_SniffedByNameOrType(t *testing.T) {
	if !(CHM{}).Match("application/vnd.ms-htmlhelp", "") || !(CHM{}).Match("", "Help.CHM") {
		t.Fatal("CHM does not match its type or extension")
	}
	if e, ok := Default().For("application/vnd.ms-htmlhelp", "x.chm"); !ok || e != (CHM{}) {
		t.Fatalf("Default() picks %T", e)
	}
}
