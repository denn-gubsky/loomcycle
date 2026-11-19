package extract

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/image/tiff"
)

// ---- the rasterize helper's stand-in ----

func (f *fakeDjVu) Renders() bool { return f.render != nil }

func (f *fakeDjVu) Render(ctx context.Context, page, maxSide, maxBytes int) ([]byte, error) {
	f.record(fmt.Sprintf("render %d %d", page, maxSide))
	if f.render == nil {
		return nil, ErrHelperUnavailable
	}
	b, err := f.render(page, maxSide)
	if err == nil && len(b) > maxBytes {
		return nil, errors.New("ddjvu: the page is over its bound")
	}
	return b, err
}

func (f *fakeDjVu) renderCalls() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for _, c := range f.calls {
		var n, side int
		if _, err := fmt.Sscanf(c, "render %d %d", &n, &side); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// tiffPage is a w×h grey page whose first pixel row says which page it is.
func tiffPage(t testing.TB, page, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for x := range w {
		img.SetGray(x, 0, color.Gray{Y: uint8(page)})
	}
	var b bytes.Buffer
	if err := tiff.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func renderAt(t testing.TB, w, h int) func(page, maxSide int) ([]byte, error) {
	return func(page, _ int) ([]byte, error) { return tiffPage(t, page, w, h), nil }
}

// imageSink is a djvuSink that keeps the pages rendered for OCR.
type imageSink struct {
	djvuSink
	pages  []int
	images map[int][]byte
}

func (s *imageSink) PageImage(n int, b []byte) error {
	if s.images == nil {
		s.images = map[int][]byte{}
	}
	s.pages = append(s.pages, n)
	s.images[n] = b
	return nil
}

// thinBook is n pages, the ones in text holding a text layer.
func thinBook(n int, text ...int) ([]byte, *fakeDjVu) {
	has := make([]bool, n)
	f := &fakeDjVu{text: map[int]string{}}
	for _, p := range text {
		has[p-1] = true
		f.text[p] = fmt.Sprintf("The text of page %d, long enough to count as a text layer of its own.", p)
	}
	return djvuDoc(has, false, false), f
}

// ---- tests ----

// TestDjVu_AutoRendersOnlyThinPages: in auto mode only the pages whose text
// layer is under the threshold are rendered, each as a PNG of the size the
// helper drew, fitted in the vision edge it was asked for.
func TestDjVu_AutoRendersOnlyThinPages(t *testing.T) {
	doc, f := thinBook(3, 1, 3)
	f.render = renderAt(t, 300, 400)
	sink := &imageSink{}
	rep, err := extractDjVu(t, doc, f, sink, Options{PageImages: &PageImages{MinChars: 40, MaxPages: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sink.pages, []int{2}) || !reflect.DeepEqual(f.renderCalls(), []int{2}) {
		t.Fatalf("rendered %v (helper asked for %v), want page 2 only", sink.pages, f.renderCalls())
	}
	img, err := png.Decode(bytes.NewReader(sink.images[2]))
	if err != nil {
		t.Fatalf("page 2 is not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 300 || b.Dy() != 400 {
		t.Errorf("page 2 is %v", b)
	}
	if g := color.GrayModel.Convert(img.At(0, 0)).(color.Gray); g.Y != 2 {
		t.Errorf("page 2's image is page %d's", g.Y)
	}
	if rep.PageImagesUnavailable {
		t.Error("rendering reported unavailable")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "render ") && c != fmt.Sprintf("render 2 %d", DjVuRenderEdge) {
			t.Errorf("helper asked %q, want the vision edge %d", c, DjVuRenderEdge)
		}
	}
}

// TestDjVu_AlwaysRendersEverySelectedPage: always renders every page read,
// text layer or not, and pages limits what is read and so what is rendered.
func TestDjVu_AlwaysRendersEverySelectedPage(t *testing.T) {
	doc, f := thinBook(4, 1, 3)
	f.render = renderAt(t, 10, 10)
	sink := &imageSink{}
	if _, err := extractDjVu(t, doc, f, sink, Options{PageImages: &PageImages{Always: true, MinChars: 40, MaxPages: 10}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sink.pages, []int{1, 2, 3, 4}) {
		t.Fatalf("always rendered %v", sink.pages)
	}

	doc, f = thinBook(4, 1, 3)
	f.render = renderAt(t, 10, 10)
	sink = &imageSink{}
	if _, err := extractDjVu(t, doc, f, sink, Options{Pages: []int{2, 3}, PageImages: &PageImages{Always: true, MinChars: 40, MaxPages: 10}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sink.pages, []int{2, 3}) || !reflect.DeepEqual(f.renderCalls(), []int{2, 3}) {
		t.Fatalf("pages 2-3: rendered %v, helper asked %v", sink.pages, f.renderCalls())
	}
}

// TestDjVu_RenderingStopsAtTheReservedPages: the caller reserved MaxPages
// of its OCR budget before the job; the helper is never asked for more.
func TestDjVu_RenderingStopsAtTheReservedPages(t *testing.T) {
	doc, f := thinBook(5)
	f.render = renderAt(t, 10, 10)
	sink := &imageSink{}
	if _, err := extractDjVu(t, doc, f, sink, Options{PageImages: &PageImages{MinChars: 40, MaxPages: 2}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.renderCalls(), []int{1, 2}) || !reflect.DeepEqual(sink.pages, []int{1, 2}) {
		t.Fatalf("helper asked %v, rendered %v; want pages 1 and 2", f.renderCalls(), sink.pages)
	}

	// Zero renders nothing, and still says rendering is available.
	doc, f = thinBook(2)
	f.render = renderAt(t, 10, 10)
	rep, err := extractDjVu(t, doc, f, &imageSink{}, Options{PageImages: &PageImages{MinChars: 40}})
	if err != nil || len(f.renderCalls()) != 0 || rep.PageImagesUnavailable {
		t.Fatalf("a zero budget: err %v, helper asked %v, unavailable %v", err, f.renderCalls(), rep.PageImagesUnavailable)
	}
}

// TestDjVu_RenderedPageOverThePixelCapIsRefused: ddjvu is asked to fit the
// page in the vision edge; an image over it is refused from its header,
// before its pixels are decoded, and the page keeps its text layer. The
// other pages are still rendered.
func TestDjVu_RenderedPageOverThePixelCapIsRefused(t *testing.T) {
	doc, f := thinBook(2)
	f.render = func(page, _ int) ([]byte, error) {
		if page == 1 {
			return tiffPage(t, page, DjVuRenderEdge+1, 4), nil
		}
		return tiffPage(t, page, 10, 10), nil
	}
	sink := &imageSink{}
	rep, err := extractDjVu(t, doc, f, sink, Options{PageImages: &PageImages{MinChars: 40, MaxPages: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sink.pages, []int{2}) {
		t.Fatalf("rendered %v, want page 2 only", sink.pages)
	}
	if notes := strings.Join(rep.Notes, "\n"); !strings.Contains(notes, "page 1 could not be rendered for OCR") || !strings.Contains(notes, "pixel edge") {
		t.Errorf("notes:\n%s", notes)
	}

	// A header claiming 60 000 × 60 000 pixels over a few bytes of data is
	// refused for its size, not for failing to decode.
	huge := tiffPage(t, 1, 1, 1)
	setTIFFSize(t, huge, 60000, 60000)
	if _, err := djvuPagePNG(huge); err == nil || !strings.Contains(err.Error(), "over the 1568-pixel edge") {
		t.Fatalf("a huge header: %v", err)
	}
}

// setTIFFSize rewrites the width and height tags of a little-endian TIFF
// tiff.Encode wrote, leaving its pixels as they are.
func setTIFFSize(t *testing.T, b []byte, w, h uint32) {
	t.Helper()
	le := binary.LittleEndian
	ifd := le.Uint32(b[4:8])
	n := int(le.Uint16(b[ifd:]))
	set := 0
	for i := range n {
		e := b[int(ifd)+2+12*i:]
		var v uint32
		switch le.Uint16(e) {
		case 256:
			v = w
		case 257:
			v = h
		default:
			continue
		}
		switch le.Uint16(e[2:]) {
		case 3: // SHORT
			if v > 0xffff {
				le.PutUint16(e[2:], 4) // LONG
				le.PutUint32(e[8:], v)
			} else {
				le.PutUint16(e[8:], uint16(v))
			}
		case 4:
			le.PutUint32(e[8:], v)
		}
		set++
	}
	if set != 2 {
		t.Fatalf("the TIFF has %d size tags", set)
	}
}

// Without a rasterize helper the text layer is read as before and the
// report says rendering is not available here: not a failure.
func TestDjVu_WithoutARasterizeHelperRenderingIsUnavailable(t *testing.T) {
	doc, f := thinBook(2, 1)
	sink := &imageSink{}
	rep, err := extractDjVu(t, doc, f, sink, Options{PageImages: &PageImages{MinChars: 40, MaxPages: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.PageImagesUnavailable || len(sink.pages) != 0 || len(f.renderCalls()) != 0 {
		t.Fatalf("unavailable %v, rendered %v", rep.PageImagesUnavailable, sink.pages)
	}
	if !strings.Contains(sink.b.String(), "The text of page 1") {
		t.Errorf("the text layer was not read:\n%s", sink.b.String())
	}
}

// A page the helper fails on is noted and left to its text layer; the
// pages after it are rendered.
func TestDjVu_APageTheHelperFailsOnIsLeftToItsTextLayer(t *testing.T) {
	doc, f := thinBook(3)
	f.render = func(page, _ int) ([]byte, error) {
		if page == 2 {
			return nil, errors.New("ddjvu: corrupted page")
		}
		return tiffPage(t, page, 10, 10), nil
	}
	sink := &imageSink{}
	rep, err := extractDjVu(t, doc, f, sink, Options{PageImages: &PageImages{MinChars: 40, MaxPages: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sink.pages, []int{1, 3}) || !strings.Contains(strings.Join(rep.Notes, "\n"), "page 2 could not be rendered for OCR (ddjvu: corrupted page)") {
		t.Fatalf("rendered %v, notes %v", sink.pages, rep.Notes)
	}
}

// Without PageImages nothing is rendered, and a sink that cannot take
// page images is never handed one.
func TestDjVu_NothingIsRenderedUnlessAsked(t *testing.T) {
	doc, f := thinBook(2)
	f.render = renderAt(t, 10, 10)
	if _, err := extractDjVu(t, doc, f, &imageSink{}, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := extractDjVu(t, doc, f, &djvuSink{}, Options{PageImages: &PageImages{Always: true, MaxPages: 2}}); err != nil {
		t.Fatal(err)
	}
	if c := f.renderCalls(); len(c) != 0 {
		t.Fatalf("the helper was asked to render %v", c)
	}
}

// A page whose text max_chars refused is not rendered: OCR has no place
// in the Markdown for it.
func TestDjVu_APageTheSinkRefusedIsNotRendered(t *testing.T) {
	doc, f := thinBook(3)
	f.render = renderAt(t, 10, 10)
	sink := &imageSink{}
	sink.max = 20 // the title and page 1's heading, not page 2's
	_, err := extractDjVu(t, doc, f, sink, Options{PageImages: &PageImages{MinChars: 40, MaxPages: 10}})
	if !errors.Is(err, ErrSinkFull) {
		t.Fatalf("err = %v, want ErrSinkFull", err)
	}
	if !reflect.DeepEqual(sink.pages, []int{1}) {
		t.Fatalf("rendered %v, want page 1 only (written %v)", sink.pages, sink.starts)
	}
}
