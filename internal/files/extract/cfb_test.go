package extract

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// cfbStreamSpec is one stream of a test compound file.
type cfbStreamSpec struct {
	name string
	data []byte
}

// testCFB is a compound file built for a test, with where its parts are so
// a test can damage them.
type testCFB struct {
	data  []byte
	fatAt int               // the byte offset of the FAT's first sector
	start map[string]uint32 // each stream's first sector
	dirAt int               // the byte offset of the directory
}

// fatEntry overwrites FAT entry i.
func (c *testCFB) fatEntry(i, v uint32) {
	binary.LittleEndian.PutUint32(c.data[c.fatAt+4*int(i):], v)
}

// dirField overwrites a uint32 of directory entry i at offset off.
func (c *testCFB) dirField(i, off int, v uint32) {
	binary.LittleEndian.PutUint32(c.data[c.dirAt+128*i+off:], v)
}

// buildCFB writes a version 3 compound file holding the streams in the
// root storage, each in regular sectors: a stream under 4,096 bytes is
// padded to it, which every reader here ignores past what it parses. The
// directory lists the streams as a chain of right siblings.
func buildCFB(t testing.TB, streams ...cfbStreamSpec) *testCFB {
	t.Helper()
	const ss = 512
	sectorsOf := func(n int) int { return (n + ss - 1) / ss }
	for i := range streams {
		if len(streams[i].data) < 4096 {
			streams[i].data = append(append([]byte(nil), streams[i].data...), make([]byte, 4096-len(streams[i].data))...)
		}
	}
	dirSectors := sectorsOf(128 * (len(streams) + 1))
	other := dirSectors
	for _, s := range streams {
		other += sectorsOf(len(s.data))
	}
	fatSectors := 1
	for (other+fatSectors)*4 > fatSectors*ss {
		fatSectors++
	}
	if fatSectors > 109 {
		t.Fatal("test compound file too large")
	}
	total := fatSectors + other
	fat := make([]uint32, fatSectors*ss/4)
	for i := range fat {
		fat[i] = cfbFreeSect
	}
	for i := 0; i < fatSectors; i++ {
		fat[i] = 0xFFFFFFFD
	}
	next := uint32(fatSectors)
	chain := func(n int) uint32 {
		first := next
		for k := 0; k < n; k++ {
			if k == n-1 {
				fat[next] = cfbEndOfChain
			} else {
				fat[next] = next + 1
			}
			next++
		}
		return first
	}
	dirStart := chain(dirSectors)
	out := &testCFB{start: map[string]uint32{}}
	for _, s := range streams {
		out.start[s.name] = chain(sectorsOf(len(s.data)))
	}
	buf := make([]byte, ss*(1+total))
	h := buf[:ss]
	copy(h, cfbSignature)
	binary.LittleEndian.PutUint16(h[24:], 0x3E)
	binary.LittleEndian.PutUint16(h[26:], 3)
	binary.LittleEndian.PutUint16(h[28:], 0xFFFE)
	binary.LittleEndian.PutUint16(h[30:], 9)
	binary.LittleEndian.PutUint16(h[32:], 6)
	binary.LittleEndian.PutUint32(h[44:], uint32(fatSectors))
	binary.LittleEndian.PutUint32(h[48:], dirStart)
	binary.LittleEndian.PutUint32(h[56:], 4096)
	binary.LittleEndian.PutUint32(h[60:], cfbEndOfChain)
	binary.LittleEndian.PutUint32(h[68:], cfbEndOfChain)
	for i := 0; i < 109; i++ {
		v := uint32(cfbFreeSect)
		if i < fatSectors {
			v = uint32(i)
		}
		binary.LittleEndian.PutUint32(h[76+4*i:], v)
	}
	at := func(sec uint32) int { return int(sec+1) * ss }
	out.fatAt = at(0)
	for i, v := range fat {
		binary.LittleEndian.PutUint32(buf[out.fatAt+4*i:], v)
	}
	out.dirAt = at(dirStart)
	entry := func(i int, name string, typ byte, right, child, start uint32, size int) {
		e := buf[out.dirAt+128*i:]
		u := utf16.Encode([]rune(name))
		for k, c := range u {
			binary.LittleEndian.PutUint16(e[2*k:], c)
		}
		binary.LittleEndian.PutUint16(e[64:], uint16(2*len(u)+2))
		e[66], e[67] = typ, 1
		binary.LittleEndian.PutUint32(e[68:], cfbNoStream)
		binary.LittleEndian.PutUint32(e[72:], right)
		binary.LittleEndian.PutUint32(e[76:], child)
		binary.LittleEndian.PutUint32(e[116:], start)
		binary.LittleEndian.PutUint64(e[120:], uint64(size))
	}
	rootChild := uint32(cfbNoStream)
	if len(streams) > 0 {
		rootChild = 1
	}
	entry(0, "Root Entry", cfbTypeRoot, cfbNoStream, rootChild, cfbEndOfChain, 0)
	for i, s := range streams {
		right := uint32(cfbNoStream)
		if i+1 < len(streams) {
			right = uint32(i + 2)
		}
		entry(i+1, s.name, cfbTypeStream, right, cfbNoStream, out.start[s.name], len(s.data))
		copy(buf[at(out.start[s.name]):], s.data)
	}
	// The unused entries of the last directory sector are empty.
	for i := len(streams) + 1; i < dirSectors*ss/128; i++ {
		e := buf[out.dirAt+128*i:]
		binary.LittleEndian.PutUint32(e[68:], cfbNoStream)
		binary.LittleEndian.PutUint32(e[72:], cfbNoStream)
		binary.LittleEndian.PutUint32(e[76:], cfbNoStream)
	}
	out.data = buf
	return out
}

func openTestCFB(t *testing.T, data []byte) (*cfbFile, error) {
	t.Helper()
	return openCFB(context.Background(), bytes.NewReader(data), int64(len(data)))
}

// A stream reads back through its chain, a short one from the mini
// stream too (the LibreOffice fixtures keep their property sets there).
func TestCFB_StreamsReadBackThroughTheirChains(t *testing.T) {
	a := bytes.Repeat([]byte("abcdefgh"), 1000)
	c, err := openTestCFB(t, buildCFB(t, cfbStreamSpec{"One", a}, cfbStreamSpec{"Two", []byte("two")}).data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.readStream("one", 1<<20) // names compare ignoring case
	if err != nil || !bytes.Equal(got, a) {
		t.Fatalf("One: %d bytes, %v", len(got), err)
	}
	if s, err := c.stream("Three"); s != nil || err != nil {
		t.Fatalf("a missing stream: %v %v", s, err)
	}
	f, err := openTestCFB(t, mustRead(t, "testdata/doc/report.doc"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := f.stream("\x05SummaryInformation")
	if err != nil || s == nil || s.backing == nil {
		t.Fatalf("the summary stream is not read from the mini stream: %v %v", s, err)
	}
}

// A FAT chain that loops back on itself is refused as a loop.
func TestCFB_FATChainLoopIsRefused(t *testing.T) {
	c := buildCFB(t, cfbStreamSpec{"WordDocument", make([]byte, 8192)})
	s := c.start["WordDocument"]
	c.fatEntry(s+3, s) // the fourth sector leads back to the first
	f, err := openTestCFB(t, c.data)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.stream("WordDocument")
	if !IsRefused(err) || !strings.Contains(err.Error(), "loops") {
		t.Fatalf("err = %v, want a refusal of the loop", err)
	}
	// The directory's own chain looping is refused when the file opens.
	d := buildCFB(t, cfbStreamSpec{"WordDocument", make([]byte, 8192)})
	dirSec := uint32((d.dirAt - 512) / 512)
	d.fatEntry(dirSec, dirSec)
	if _, err := openTestCFB(t, d.data); !IsRefused(err) || !strings.Contains(err.Error(), "loops") {
		t.Fatalf("directory loop: err = %v, want a refusal of the loop", err)
	}
}

// A chain that names a sector past the FAT is refused, not read.
func TestCFB_ChainPastTheFileIsRefused(t *testing.T) {
	c := buildCFB(t, cfbStreamSpec{"WordDocument", make([]byte, 8192)})
	c.fatEntry(c.start["WordDocument"], 0x00FFFFFF)
	f, err := openTestCFB(t, c.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.stream("WordDocument"); !IsRefused(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

// A directory whose sibling links form a cycle is refused: the walk visits
// each entry once, rather than going round for ever.
func TestCFB_DirectoryTreeCycleIsRefused(t *testing.T) {
	c := buildCFB(t, cfbStreamSpec{"A", nil}, cfbStreamSpec{"B", nil})
	c.dirField(2, 72, 1) // B's right sibling is A, whose right sibling is B
	f, err := openTestCFB(t, c.data)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.stream("Missing")
		done <- err
	}()
	select {
	case err := <-done:
		if !IsRefused(err) || !strings.Contains(err.Error(), "loops") {
			t.Fatalf("err = %v, want a refusal of the cycle", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the directory walk did not end")
	}
}

// A stream longer than its chain, or over the stream cap, is refused.
func TestCFB_StreamLongerThanItsChainIsRefused(t *testing.T) {
	c := buildCFB(t, cfbStreamSpec{"WordDocument", make([]byte, 8192)})
	c.dirField(1, 120, 8192*4)
	f, err := openTestCFB(t, c.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.stream("WordDocument"); !IsRefused(err) || !strings.Contains(err.Error(), "longer than its chain") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	c.dirField(1, 120, maxCFBStream+1)
	if f, err = openTestCFB(t, c.data); err != nil {
		t.Fatal(err)
	}
	if _, err := f.stream("WordDocument"); !IsRefused(err) || !strings.Contains(err.Error(), "over") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// FuzzCFB fuzzes the compound file reader on its own: opening, a lookup and
// reading the stream it finds, under the caps.
func FuzzCFB(f *testing.F) {
	f.Add(buildCFB(f, cfbStreamSpec{"WordDocument", []byte("x")}, cfbStreamSpec{"1Table", []byte("y")}).data)
	if b, err := os.ReadFile("testdata/doc/cyrillic.doc"); err == nil {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), fuzzContext)
		defer cancel()
		c, err := openCFB(ctx, bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		for _, name := range []string{"WordDocument", "\x05SummaryInformation", "Missing"} {
			if b, err := c.readStream(name, 1<<20); err == nil && len(b) > len(data) {
				t.Fatalf("%q read %d bytes out of a %d-byte file", name, len(b), len(data))
			}
		}
	})
}
