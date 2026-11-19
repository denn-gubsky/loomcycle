package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"time"

	"golang.org/x/image/tiff"
)

// Rendering DjVu pages for OCR (RFC GV P4, D7). The pages an OCR pass will
// read are rendered here, inside the extraction, so a 100 MB book is sent
// to a worker once: ddjvu writes each page as a TIFF fitted in a box no
// larger than a vision model reads whole, Go checks its dimensions from the
// header before decoding a pixel, and the page goes to the sink as a PNG.
// The decoder behind ddjvu is C++ with a history (CVE-2025-53367), so
// everything it hands back is bounded: the file, the pixels, the PNG, the
// pages and the bytes of the whole pass, and each call's time.
const (
	// DjVuRenderEdge is the long side a page is rendered at: the edge the
	// vision engine sends an image at whole (vision.MaxImageEdge, which a
	// test holds equal), so a rendered page is never scaled again.
	DjVuRenderEdge = 1568
	// maxDjVuRenderFile bounds the TIFF the helper may write for one page:
	// an uncompressed 16-bit RGBA page at the edge is under 20 MiB.
	maxDjVuRenderFile = 32 << 20
	// MaxDjVuPagePNG bounds one rendered page as PNG: the vision engine's
	// request bound (vision.MaxUnitBytes).
	MaxDjVuPagePNG = 16 << 20
	// MaxDjVuRenderedBytes bounds the PNGs of one extraction, which the
	// caller holds until its OCR pass sends them. A text page at the edge
	// is a few hundred KiB, so this is some hundreds of pages; the pass
	// says where it stopped.
	MaxDjVuRenderedBytes = 128 << 20
	// MaxDjVuRenderedPages bounds the pages one extraction renders, whatever
	// budget the caller reserved.
	MaxDjVuRenderedPages = 1000
	// djvuRenderReserve is the time an extraction keeps in hand: rendering
	// stops when its deadline is nearer, so the text already read is
	// returned rather than lost to the deadline.
	djvuRenderReserve = 3 * time.Second
)

// render renders the pages pi asks for, in page order, and hands each to
// the sink. A page that cannot be rendered is noted and left to its text
// layer; only the context ending is an error.
func (x *djvuExtract) render(pi *PageImages) error {
	if pi == nil {
		return nil
	}
	if !x.e.Helpers.Renders() {
		x.rep.PageImagesUnavailable = true
		return nil
	}
	sink, ok := x.w.sink.(PageImageSink)
	if !ok {
		return nil
	}
	want := x.renderPages(pi)
	var failed []int
	var reason string
	fail := func(n int, err error) {
		failed = append(failed, n)
		if reason == "" {
			reason = err.Error()
		}
	}
	total := 0
	for i, n := range want {
		ctx := x.w.ctx
		if err := ctx.Err(); err != nil {
			return err
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < djvuRenderReserve {
			x.rep.note("rendering pages for OCR stopped before the extraction's time bound; %s not rendered", pagesPhrase(want[i:]))
			break
		}
		s, err := x.session()
		if err != nil {
			return err
		}
		rctx, cancel := renderContext(ctx)
		data, err := s.Render(rctx, n, DjVuRenderEdge, maxDjVuRenderFile)
		cancel()
		switch {
		case errors.Is(err, ErrHelperUnavailable):
			x.rep.PageImagesUnavailable = true
			return nil
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			fail(n, err)
			continue
		}
		img, err := djvuPagePNG(data)
		if err != nil {
			fail(n, err)
			continue
		}
		if total+len(img) > MaxDjVuRenderedBytes {
			x.rep.note("rendering pages for OCR stopped at %d MiB of page images; %s not rendered", MaxDjVuRenderedBytes>>20, pagesPhrase(want[i:]))
			break
		}
		total += len(img)
		if err := sink.PageImage(n, img); err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			x.rep.note("the page images were refused after %s: %v", pagesPhrase(want[:i]), err)
			break
		}
	}
	if len(failed) > 0 {
		x.rep.note("%s could not be rendered for OCR (%s)", pagesPhrase(failed), reason)
	}
	return nil
}

// renderPages are the pages pi asks to render, among those written: every
// one, or those under its threshold, at most MaxPages.
func (x *djvuExtract) renderPages(pi *PageImages) []int {
	yields := map[int]PageYield{}
	for _, y := range x.rep.PageYields {
		yields[y.Page] = y
	}
	limit := min(pi.MaxPages, MaxDjVuRenderedPages)
	var want []int
	for _, n := range x.written {
		if len(want) >= limit {
			break
		}
		y := yields[n]
		if pi.Always || (!y.Unread && y.Chars < pi.MinChars) {
			want = append(want, n)
		}
	}
	return want
}

// renderContext bounds one render to what the extraction has left, less
// its reserve, so a slow page ends as that page's failure, not the
// extraction's.
func renderContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if dl, ok := ctx.Deadline(); ok {
		return context.WithDeadline(ctx, dl.Add(-djvuRenderReserve))
	}
	return context.WithCancel(ctx)
}

// errDjVuRender is a rendered page that is not one the runtime asked for.
var errDjVuRender = errors.New("extract: the rendered page is not a page image the runtime takes")

// djvuPagePNG checks a rendered page's dimensions from its header, decodes
// it only when they fit the edge, and encodes it as PNG.
func djvuPagePNG(data []byte) ([]byte, error) {
	cfg, err := tiff.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errDjVuRender, err)
	}
	// ddjvu fits the page in the box it is given; a page over it was not
	// rendered as asked, and is refused before its pixels are allocated.
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > DjVuRenderEdge || cfg.Height > DjVuRenderEdge {
		return nil, fmt.Errorf("%w: it is %d×%d pixels, over the %d-pixel edge", errDjVuRender, cfg.Width, cfg.Height, DjVuRenderEdge)
	}
	img, err := tiff.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errDjVuRender, err)
	}
	if b := img.Bounds(); b.Dx() != cfg.Width || b.Dy() != cfg.Height {
		return nil, fmt.Errorf("%w: its header and its pixels disagree", errDjVuRender)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, fmt.Errorf("%w: %v", errDjVuRender, err)
	}
	if out.Len() > MaxDjVuPagePNG {
		return nil, fmt.Errorf("%w: it is %d bytes as PNG, over %d", errDjVuRender, out.Len(), MaxDjVuPagePNG)
	}
	return out.Bytes(), nil
}
