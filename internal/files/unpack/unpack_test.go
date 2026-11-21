package unpack

import (
	"archive/zip"
	"bytes"
	"context"
	"io/fs"
	"strings"
	"testing"
)

type zipEnt struct {
	name string
	body []byte
	mode fs.FileMode
}

func zipArchive(t testing.TB, ents ...zipEnt) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range ents {
		fh := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			fh.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var text = []byte("A chapter of plain text.\n")

func TestRun_ZipDirectoryTooLargeIsRefusedBeforeLoading(t *testing.T) {
	arc := zipArchive(t, zipEnt{name: "a.txt", body: text})
	// Claim more records than the walk will load, in the end record.
	i := bytes.LastIndex(arc, []byte("PK\x05\x06"))
	arc[i+10], arc[i+11] = 0xfe, 0xff // 65534
	if n, ok := zipDirectoryRecords(bytes.NewReader(arc), int64(len(arc))); !ok || n != 65534 {
		t.Fatalf("records = %d %v", n, ok)
	}
	w := &walker{ctx: context.Background(), lim: DefaultLimits}
	if err := w.walkZip(bytes.NewReader(zip64Claim(t, arc)), int64(len(zip64Claim(t, arc)))); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("walk = %v", err)
	}
}

// zip64Claim appends a zip64 end record and locator claiming 200,000
// records before arc's end record, whose count is set to 0xFFFF.
func zip64Claim(t *testing.T, arc []byte) []byte {
	t.Helper()
	i := bytes.LastIndex(arc, []byte("PK\x05\x06"))
	out := append([]byte(nil), arc[:i]...)
	off := len(out)
	rec := append([]byte("PK\x06\x06"), le64(44)...)
	rec = append(rec, 45, 0, 45, 0)
	rec = append(rec, make([]byte, 8)...) // disk numbers
	rec = append(rec, le64(200000)...)
	rec = append(rec, le64(200000)...)
	rec = append(rec, make([]byte, 16)...) // directory size and offset
	out = append(out, rec...)
	loc := append([]byte("PK\x06\x07"), make([]byte, 4)...)
	loc = append(loc, le64(uint64(off))...)
	loc = append(loc, 1, 0, 0, 0)
	out = append(out, loc...)
	end := append([]byte(nil), arc[i:]...)
	end[10], end[11] = 0xff, 0xff
	return append(out, end...)
}

func le64(v uint64) []byte {
	b := make([]byte, 8)
	for i := range b {
		b[i] = byte(v >> (8 * i))
	}
	return b
}
