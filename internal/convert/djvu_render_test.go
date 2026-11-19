//go:build unix

package convert

import (
	"bytes"
	"context"
	"image/png"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/convert/djvutest"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
)

// imagedSink is a pagedSink that keeps the pages rendered for OCR.
type imagedSink struct {
	pagedSink
	pages  []int
	images [][]byte
}

func (s *imagedSink) PageImage(n int, b []byte) error {
	s.pages = append(s.pages, n)
	s.images = append(s.images, b)
	return nil
}

func djvuRenderer(root string) *InProcess {
	p := djvuInProcess(root, false)
	p.DjVuRasterize = &Helper{Key: DjVuRasterizeHelperKey, Command: DefaultDjVuRasterizeCommand}
	return p
}

// TestInProcess_DjVuPagesAreRenderedWithTheRasterizeHelper: an extraction
// that asks for page images has the rasterize helper draw each thin page,
// with only runtime values on its command line (the scratch copy, the page
// number, the vision edge, the output path in the same folder), and leaves
// the scratch folder empty. Without the helper it says rendering is not
// available, and reads the text all the same.
func TestInProcess_DjVuPagesAreRenderedWithTheRasterizeHelper(t *testing.T) {
	fake := djvutest.Install(t)
	data := djvuFixture(t, "book.djvu")
	root := t.TempDir()
	var sink imagedSink
	rep, err := djvuRenderer(root).Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(data), int64(len(data)), &sink,
		extract.Options{Name: "b.djvu", PageImages: &extract.PageImages{MinChars: 40, MaxPages: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sink.pages, []int{2}) || rep.PageImagesUnavailable {
		t.Fatalf("rendered %v, unavailable %v; want page 2 (the one without text)", sink.pages, rep.PageImagesUnavailable)
	}
	img, err := png.Decode(bytes.NewReader(sink.images[0]))
	if err != nil || img.Bounds().Dx() != djvutest.PageWidth || img.Bounds().Dy() != djvutest.PageHeight {
		t.Fatalf("page 2: %v %v", img, err)
	}
	var render []string
	for _, c := range fake.Calls(t) {
		if strings.HasPrefix(c, "ddjvu ") {
			render = append(render, c)
		}
	}
	scratch := regexp.QuoteMeta(root) + `/\.djvu-[0-9a-f]+`
	want := `^ddjvu -format=tiff -page=2 -size=1568x1568 ` + scratch + `/book\.djvu ` + scratch + `/page-2\.tiff$`
	if len(render) != 1 || !regexp.MustCompile(want).MatchString(render[0]) {
		t.Fatalf("ddjvu calls = %q, want one matching %s", render, want)
	}
	assertEmpty(t, root)

	sink = imagedSink{}
	rep, err = djvuInProcess(t.TempDir(), false).Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(data), int64(len(data)), &sink,
		extract.Options{Name: "b.djvu", PageImages: &extract.PageImages{MinChars: 40, MaxPages: 10}})
	if err != nil || !rep.PageImagesUnavailable || len(sink.pages) != 0 || !strings.Contains(sink.b.String(), "Text of page 3") {
		t.Fatalf("no rasterize helper: err %v, unavailable %v, rendered %v", err, rep.PageImagesUnavailable, sink.pages)
	}
}

// A helper that writes no image, or one past the bound, fails that page
// only; the bound is checked before the file is read.
func TestDjVuSession_RenderIsBounded(t *testing.T) {
	fake := djvutest.Install(t)
	root := t.TempDir()
	p := djvuRenderer(root)
	data := djvuFixture(t, "scan.djvu")
	s, err := djvuHelpers{p}.Open(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, c := range []struct{ page, side, max int }{{0, 10, 10}, {1, 0, 10}, {1, maxDjVuRenderSide + 1, 10}, {extract.MaxDjVuPages + 1, 10, 10}, {1, 10, 0}} {
		if _, err := s.Render(context.Background(), c.page, c.side, c.max); err == nil {
			t.Errorf("render %+v ran", c)
		}
	}
	if len(fake.Calls(t)) != 0 {
		t.Fatalf("refused renders reached the helper: %q", fake.Calls(t))
	}
	if _, err := s.Render(context.Background(), 1, 100, 64); err == nil || !strings.Contains(err.Error(), "over 64") {
		t.Errorf("an image over the bound: %v", err)
	}
	p.DjVuRasterize = &Helper{Key: DjVuRasterizeHelperKey, Command: "true -page={page} -size={size} {in} {out}"}
	if _, err := s.Render(context.Background(), 1, 100, 1<<20); err == nil || !strings.Contains(err.Error(), "wrote no page image") {
		t.Errorf("no image written: %v", err)
	}
}

// TestInProcess_DjVuLibreRendersAScannedPage runs ddjvu itself with the
// default command over the scan fixture: every page is rendered for an
// always pass, each a PNG whose long side fits the vision edge.
func TestInProcess_DjVuLibreRendersAScannedPage(t *testing.T) {
	requireDjVuLibre(t)
	if _, err := exec.LookPath(DjVuRasterizeProgram); err != nil {
		if os.Getenv("LOOMCYCLE_REQUIRE_DJVULIBRE") != "" {
			t.Fatalf("%s is required here and not on PATH", DjVuRasterizeProgram)
		}
		t.Skipf("%s not installed", DjVuRasterizeProgram)
	}
	data := djvuFixture(t, "scan.djvu")
	root := t.TempDir()
	var sink imagedSink
	rep, err := djvuRenderer(root).Extract(context.Background(), extract.DjVuMediaType, bytes.NewReader(data), int64(len(data)), &sink,
		extract.Options{Name: "scan.djvu", PageImages: &extract.PageImages{Always: true, MinChars: 40, MaxPages: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sink.pages, []int{1, 2, 3}) {
		t.Fatalf("rendered %v; notes %v", sink.pages, rep.Notes)
	}
	for i, b := range sink.images {
		cfg, err := png.DecodeConfig(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("page %d: %v", i+1, err)
		}
		if long := max(cfg.Width, cfg.Height); long < 100 || long > extract.DjVuRenderEdge {
			t.Errorf("page %d is %d×%d", i+1, cfg.Width, cfg.Height)
		}
	}
	assertEmpty(t, root)
}
