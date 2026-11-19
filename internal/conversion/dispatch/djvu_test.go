//go:build unix

package dispatch_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/conversion/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/conversion/worker"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/convert/djvutest"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"golang.org/x/image/tiff"
)

func djvuBook(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "files", "extract", "testdata", "djvu", "book.djvu"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func djvuHelpers() (text, inspect *convert.Helper) {
	return &convert.Helper{Key: convert.DjVuTextHelperKey, Command: convert.DefaultDjVuTextCommand},
		&convert.Helper{Key: convert.DjVuInspectHelperKey, Command: convert.DefaultDjVuInspectCommand}
}

// A worker whose DjVu text helper does not resolve does not carry djvu, and
// a DjVu extraction with no such worker attached is refused at once as a
// program to install, naming DjVuLibre.
func TestPool_AWorkerWithoutDjvutxtDoesNotOfferDjVu(t *testing.T) {
	d, target := pool(t, dispatch.Options{QueueWait: time.Minute, Configured: map[string]bool{wire.LabelDjVu: true}})
	helperWorker(t, target, "w1", worker.Options{
		DjVuText: &convert.Helper{Key: convert.DjVuTextHelperKey, Command: "loomcycle-no-such-djvutxt --page={pages} {in}"},
	})
	waitIdle(t, d, 1)
	book := djvuBook(t)
	start := time.Now()
	_, err := d.Client().Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(book), int64(len(book)), &recSink{}, extract.Options{Name: "book.djvu"})
	if !errors.Is(err, convert.ErrNoHelper) || !strings.Contains(err.Error(), "djvulibre-bin") || !strings.Contains(err.Error(), "djvutxt") {
		t.Errorf("err = %v, want ErrNoHelper naming djvulibre-bin and djvutxt", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("the refusal waited %v; it must not queue", time.Since(start))
	}
	// Other extractions still run there.
	csv := []byte("a,b\n1,2\n")
	if _, err := d.Client().Extract(context.Background(), "text/csv", bytes.NewReader(csv), int64(len(csv)), &recSink{}, extract.Options{Name: "t.csv"}); err != nil {
		t.Errorf("csv on the same worker: %v", err)
	}
}

// A DjVu goes to the worker that has DjVuLibre and reads as it reads in
// this process, pages and all: only the pages asked for reach the helper.
func TestPool_DjVuOnAWorkerMatchesInProcess(t *testing.T) {
	fake := djvutest.Install(t)
	text, inspect := djvuHelpers()
	d, target := pool(t, dispatch.Options{Configured: map[string]bool{wire.LabelDjVu: true}})
	helperWorker(t, target, "plain", worker.Options{})
	helperWorker(t, target, "djvu", worker.Options{DjVuText: text, DjVuInspect: inspect})
	waitIdle(t, d, 2)
	book := djvuBook(t)
	here := &convert.InProcess{DjVuText: text, DjVuInspect: inspect, Scratch: func(context.Context) (string, error) { return t.TempDir(), nil }}
	for _, pages := range [][]int{nil, {3}} {
		opts := extract.Options{Name: "book.djvu", Pages: pages}
		var want, got recSink
		wantRep, err := here.Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(book), int64(len(book)), &want, opts)
		if err != nil {
			t.Fatal(err)
		}
		// Each extraction lands on the djvu worker, whichever slot is idle first.
		for range 3 {
			got = recSink{}
			gotRep, err := d.Client().Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(book), int64(len(book)), &got, opts)
			if err != nil {
				t.Fatalf("pages %v: %v", pages, err)
			}
			if got.md.String() != want.md.String() || !reflect.DeepEqual(got.starts, want.starts) || !reflect.DeepEqual(gotRep, wantRep) {
				t.Fatalf("pages %v: pool\n%s\n%+v\nin process\n%s\n%+v", pages, got.md.String(), gotRep, want.md.String(), wantRep)
			}
		}
	}
	var asked []string
	for _, c := range fake.Calls(t) {
		if strings.HasPrefix(c, "djvutxt ") {
			asked = append(asked, strings.Fields(c)[2])
		}
	}
	if strings.Join(asked, " ") != strings.Repeat("--page=1,3 ", 4)+strings.TrimSpace(strings.Repeat("--page=3 ", 4)) {
		t.Errorf("text helper asked %q", asked)
	}
}

func TestExtractLabels_DjVuNeedsTheDjVuWorker(t *testing.T) {
	for _, c := range []struct {
		mt, name          string
		pages, pageImages bool
		want              []string
	}{
		{"application/pdf", "a.pdf", false, false, []string{wire.LabelExtract}},
		{"application/pdf", "a.pdf", true, false, []string{wire.LabelExtract, wire.LabelExtractPages}},
		{extract.DjVuMediaType, "", false, false, []string{wire.LabelDjVu}},
		{"image/x-djvu", "", true, false, []string{wire.LabelDjVu, wire.LabelExtractPages}},
		{"application/octet-stream", "scan.djvu", false, false, []string{wire.LabelDjVu}},
		{"application/pdf", "misnamed.djvu", false, false, []string{wire.LabelExtract}},
		{extract.DjVuMediaType, "", true, true, []string{wire.LabelDjVu, wire.LabelDjVuRender, wire.LabelExtractPages}},
		{"application/pdf", "a.pdf", false, true, []string{wire.LabelExtract}},
	} {
		if got := wire.ExtractLabels(c.mt, c.name, c.pages, c.pageImages); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ExtractLabels(%q, %q, %v, %v) = %v, want %v", c.mt, c.name, c.pages, c.pageImages, got, c.want)
		}
	}
	if !wire.HelperLabel(wire.LabelDjVu) || !wire.HelperLabel(wire.LabelDjVuRender) {
		t.Error("djvu or djvu_render is not a helper label")
	}
}

// imgSink is a recSink that keeps the pages rendered for OCR.
type imgSink struct {
	recSink
	pages  []int
	images map[int][]byte
}

func (s *imgSink) PageImage(n int, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.images == nil {
		s.images = map[int][]byte{}
	}
	s.pages = append(s.pages, n)
	s.images[n] = b
	return nil
}

func djvuRasterize() *convert.Helper {
	return &convert.Helper{Key: convert.DjVuRasterizeHelperKey, Command: convert.DefaultDjVuRasterizeCommand}
}

// TestPool_DjVuPageImagesOnAWorkerMatchInProcess: an extraction that asks
// for page images goes to the worker that renders, and its pages come back
// as they come in this process: the same Markdown, the same pages, the same
// PNGs, the same report. A page image larger than a gRPC message arrives
// whole.
func TestPool_DjVuPageImagesOnAWorkerMatchInProcess(t *testing.T) {
	fake := djvutest.Install(t)
	// A page of noise is a PNG over a chunk, so it crosses in several frames.
	fake.SetPage(t, 2, noisyTIFF(t, 1200, 1200))
	text, inspect := djvuHelpers()
	d, target := pool(t, dispatch.Options{Configured: map[string]bool{wire.LabelDjVu: true, wire.LabelDjVuRender: true}})
	helperWorker(t, target, "text-only", worker.Options{DjVuText: text, DjVuInspect: inspect})
	helperWorker(t, target, "renders", worker.Options{DjVuText: text, DjVuInspect: inspect, DjVuRasterize: djvuRasterize()})
	waitIdle(t, d, 2)
	book := djvuBook(t)
	here := &convert.InProcess{DjVuText: text, DjVuInspect: inspect, DjVuRasterize: djvuRasterize(), Scratch: func(context.Context) (string, error) { return t.TempDir(), nil }}
	for _, pi := range []*extract.PageImages{{MinChars: 40, MaxPages: 5}, {Always: true, MinChars: 40, MaxPages: 2}} {
		opts := extract.Options{Name: "book.djvu", PageImages: pi}
		var want imgSink
		wantRep, err := here.Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(book), int64(len(book)), &want, opts)
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			var got imgSink
			gotRep, err := d.Client().Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(book), int64(len(book)), &got, opts)
			if err != nil {
				t.Fatal(err)
			}
			if got.md.String() != want.md.String() || !reflect.DeepEqual(got.pages, want.pages) || !reflect.DeepEqual(got.images, want.images) || !reflect.DeepEqual(gotRep, wantRep) {
				t.Fatalf("%+v: pool pages %v %+v, in process pages %v %+v", *pi, got.pages, gotRep, want.pages, wantRep)
			}
		}
		if len(want.pages) == 0 {
			t.Fatalf("%+v: nothing was rendered", *pi)
		}
		if b := want.images[2]; len(b) <= wire.ChunkSize {
			t.Fatalf("page 2's image is %d bytes, not over a frame", len(b))
		}
	}
	if n := len(fake.Calls(t)); n == 0 {
		t.Fatal("no helper ran")
	}
}

// With DjVu workers and none that renders, the book is read for its text
// and the report says page images are not available, as in this process
// without a rasterize helper.
func TestPool_DjVuWithoutARenderingWorkerSaysPageImagesAreUnavailable(t *testing.T) {
	djvutest.Install(t)
	text, inspect := djvuHelpers()
	d, target := pool(t, dispatch.Options{Configured: map[string]bool{wire.LabelDjVu: true, wire.LabelDjVuRender: true}})
	helperWorker(t, target, "text-only", worker.Options{DjVuText: text, DjVuInspect: inspect})
	waitIdle(t, d, 1)
	book := djvuBook(t)
	var got imgSink
	rep, err := d.Client().Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(book), int64(len(book)), &got,
		extract.Options{Name: "book.djvu", PageImages: &extract.PageImages{MinChars: 40, MaxPages: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.PageImagesUnavailable || len(got.pages) != 0 || !strings.Contains(got.md.String(), "Text of page 3") {
		t.Fatalf("unavailable %v, pages %v\n%s", rep.PageImagesUnavailable, got.pages, got.md.String())
	}
}

// noisyTIFF is a w×h page of noise: it does not compress, so its PNG is
// larger than one frame.
func noisyTIFF(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	x := uint32(2463534242)
	for i := range img.Pix {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		img.Pix[i] = uint8(x)
	}
	var b bytes.Buffer
	if err := tiff.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
