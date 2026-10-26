package extract

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/files/extract/pdftest"
)

// pageSink is a memSink told where each page starts.
type pageSink struct {
	memSink
	starts map[int]int
}

func (s *pageSink) StartPage(n int) {
	if s.starts == nil {
		s.starts = map[int]int{}
	}
	s.starts[n] = s.md.Len()
}

// mixedPDF is a typed cover letter, a scanned annex page (a picture and no
// text), and a typed closing page.
func mixedPDF(t testing.TB) []byte {
	return pdftest.Doc{Pages: []pdftest.Page{
		{Lines: []pdftest.Line{
			{Text: "Супровідний лист до протоколу засідання правління", X: 72, Y: 770, Size: 12},
			{Text: "Додаток надіслано окремим скан-файлом.", X: 72, Y: 750, Size: 12},
		}},
		{Images: []pdftest.Image{{JPEG: tinyJPEG(t), W: 8, H: 8, X: 0, Y: 0, Width: 595, Height: 842}}},
		{Lines: []pdftest.Line{
			{Text: "Голова правління: Іван Петренко, підпис і печатка.", X: 72, Y: 770, Size: 12},
		}},
	}}.Bytes()
}

// The PDF reader reports each page's text yield, and a sink that asks is told
// where each page's Markdown begins: what OCR needs to replace one page.
func TestPDF_ReportsPageYieldsAndPageStarts(t *testing.T) {
	data := mixedPDF(t)
	s := &pageSink{}
	rep, err := PDF{}.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), s, Options{Name: "lyst.pdf", Images: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.PageYields) != 3 {
		t.Fatalf("page yields = %+v", rep.PageYields)
	}
	for i, y := range rep.PageYields {
		if y.Page != i+1 || y.Unread {
			t.Errorf("yield %d = %+v", i, y)
		}
	}
	if rep.PageYields[0].Chars < 40 || rep.PageYields[1].Chars != 0 || rep.PageYields[2].Chars < 40 {
		t.Errorf("yields = %+v, want text, none, text", rep.PageYields)
	}
	md := s.md.String()
	for n := 1; n <= 3; n++ {
		at, ok := s.starts[n]
		if !ok {
			t.Fatalf("no start for page %d", n)
		}
		if !strings.HasPrefix(strings.TrimLeft(md[at:], "\n"), "## Page ") {
			t.Errorf("page %d starts at %q", n, md[at:min(at+20, len(md))])
		}
	}
	if !(s.starts[1] < s.starts[2] && s.starts[2] < s.starts[3]) {
		t.Errorf("starts out of order: %v", s.starts)
	}
}

// SelectPages cuts a new PDF holding only the named pages, in order, which
// the reader reads as the pages they were.
func TestSelectPages_KeepsOnlyTheNamedPages(t *testing.T) {
	data := mixedPDF(t)
	out, err := SelectPages(context.Background(), bytes.NewReader(data), int64(len(data)), []int{1, 3})
	if err != nil {
		t.Fatal(err)
	}
	s := &memSink{}
	rep, err := PDF{}.Extract(context.Background(), bytes.NewReader(out), int64(len(out)), s, Options{Name: "cut.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pages != 2 {
		t.Fatalf("pages = %d, want 2", rep.Pages)
	}
	md := s.md.String()
	if !strings.Contains(md, "Супровідний лист") || !strings.Contains(md, "Голова правління") {
		t.Errorf("the cut lost a selected page:\n%s", md)
	}
	if _, err := SelectPages(context.Background(), bytes.NewReader(data), int64(len(data)), []int{4}); err == nil {
		t.Error("a page past the end was accepted")
	}
	if _, err := SelectPages(context.Background(), bytes.NewReader(data), int64(len(data)), nil); err == nil {
		t.Error("an empty selection was accepted")
	}
}

// A real producer's file (Chrome's print-to-PDF) cuts too: its fonts and
// resources come along, so the cut page reads as the page it was.
func TestSelectPages_RealProducerFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "chrome-board.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	whole := &memSink{}
	if _, err := (PDF{}).Extract(context.Background(), bytes.NewReader(data), int64(len(data)), whole, Options{Name: "b.pdf"}); err != nil {
		t.Fatal(err)
	}
	out, err := SelectPages(context.Background(), bytes.NewReader(data), int64(len(data)), []int{1})
	if err != nil {
		t.Fatal(err)
	}
	cut := &memSink{}
	rep, err := PDF{}.Extract(context.Background(), bytes.NewReader(out), int64(len(out)), cut, Options{Name: "b.pdf"})
	if err != nil || rep.Pages != 1 {
		t.Fatalf("cut: pages %d, %v", rep.Pages, err)
	}
	if len(rep.PageYields) != 1 || rep.PageYields[0].Chars < 40 {
		t.Fatalf("the cut page lost its text: %+v\n%s", rep.PageYields, cut.md.String())
	}
}
