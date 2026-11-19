package builtin

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
)

// OCR of a DjVu book (RFC GV P4). Its pages are rendered inside the
// extraction (D7), so the pages it may render are reserved before the
// extraction starts; what it renders comes back as one PNG per page through
// the sink, and each is sent to the engine as a one-page image. The book
// itself is never a unit: it is not an image a vision model reads.

// djvuNoPageOCR is the note of a DjVu extraction that asked for OCR where
// no page can be rendered: its text layer is all it reads, which is an
// answer, not a failure.
const djvuNoPageOCR = "page OCR is not available here: nothing on this server renders DjVu pages (an operator runs conversion workers from the loomcycle-worker image, which has DjVuLibre's ddjvu, or sets transcription.ocr.djvu.rasterize); the pages with little or no text layer keep it"

// ocrHold is OCR pages reserved on a run's and its tree's budgets before
// the work that spends them. held is what is still reserved and not handed
// to an engine call; give returns pages to both budgets.
type ocrHold struct {
	run, tree *transcribe.Budget
	reserved  int
	held      int
}

func (h *ocrHold) give(n int) {
	n = min(n, h.held)
	if n <= 0 {
		return
	}
	h.run.ReleasePages(n)
	h.tree.ReleasePages(n)
	h.held -= n
}

// spend hands n held pages to an engine call, which gives them back itself
// when it provably consumed nothing (callOCR).
func (h *ocrHold) spend(n int) { h.held -= min(n, h.held) }

// holdDjVuPages reserves, on both budgets, as many pages as the extraction
// may render: every page it reads, the most it could find thin. Unspent
// pages go back when the extract call ends. nil when the run has no inbox
// to count against.
//
// Assumption: holding the upper bound for the length of one extraction
// (fileExtractTimeout) is acceptable; a sibling run's OCR in that window
// sees less of the tree's budget, and its cap error says so.
func (f *File) holdDjVuPages(ctx context.Context, c ocrCall, r io.ReaderAt, size int64, pages []int) *ocrHold {
	run, tree, ok := f.ocrBudgetsFor(ctx, c.caps)
	if !ok {
		return nil
	}
	h := &ocrHold{run: run, tree: tree}
	n, err := extract.DjVuPageCount(r, size)
	if err != nil || n < 1 {
		// The extraction refuses the same file on the same walk.
		return h
	}
	n = min(min(n, extract.MaxDjVuPages), extract.MaxDjVuRenderedPages)
	if len(pages) > 0 {
		n = min(n, len(pages))
	}
	got := run.ReserveUpToPages(n)
	if got > 0 {
		if t := tree.ReserveUpToPages(got); t < got {
			run.ReleasePages(got - t)
			got = t
		}
	}
	h.reserved, h.held = got, got
	return h
}

// PageImage keeps a page rendered for OCR (extract.PageImageSink). A
// worker's answer is held to what the core asked: a page it wrote, once,
// at most the pages reserved and the bytes the extractor bounds itself to.
func (s *extractSink) PageImage(n int, png []byte) error {
	switch _, written := s.starts[n]; {
	case !written:
		return fmt.Errorf("page %d was not written", n)
	case s.pageImages[n] != nil:
		return fmt.Errorf("page %d was rendered twice", n)
	case len(s.pageImages) >= s.pageImageCap:
		return errors.New("more pages were rendered than were reserved")
	case len(png) == 0 || len(png) > extract.MaxDjVuPagePNG || s.pageImageBytes+len(png) > extract.MaxDjVuRenderedBytes:
		return fmt.Errorf("page %d's image is %d bytes, past the bounds", n, len(png))
	}
	if s.pageImages == nil {
		s.pageImages = map[int][]byte{}
	}
	s.pageImages[n] = png
	s.pageImageBytes += len(png)
	return nil
}

var _ extract.PageImageSink = (*extractSink)(nil)

// runDjVuOCR sends each rendered page of pages (in order) to the engine as
// a one-page PNG, the pages reserved in hold. A page past what was
// reserved is the run's cap, reported as a PDF's is; a page within it that
// was not rendered (its notes say why) leaves the pass partial.
func (f *File) runDjVuOCR(ctx context.Context, c ocrCall, plan transcribe.Plan, hold *ocrHold, images map[int][]byte, pages []int) *ocrPass {
	p := &ocrPass{plan: plan, language: c.language, text: map[int]string{}}
	if hold == nil {
		p.err = errors.New("this run has no inbox to count its pages against")
		return p
	}
	op := f.ocrConfig()
	opts := transcribe.OCROptions{Language: c.language, Prompt: transcribe.OCRPrompt(op.PromptTemplate, c.language)}
	var unrendered []int
	for i, n := range pages {
		if i >= hold.reserved {
			// Rendering stopped at the pages reserved before the book was
			// read. Asking for the rest names the cap as a PDF's pass does;
			// when they fit now (a sibling gave pages back), they are not
			// rendered, so they go back at once.
			rest := len(pages) - i
			err := reserveOCRPages(hold.run, hold.tree, rest)
			var capErr *transcribe.CapError
			switch {
			case errors.As(err, &capErr):
				p.capErr = capErr
			case err != nil:
				p.err = err
			default:
				hold.run.ReleasePages(rest)
				hold.tree.ReleasePages(rest)
				p.err = fmt.Errorf("page %s was not rendered: the run's OCR pages were in use while the book was read; extract it again", pageListText(pages[i:]))
			}
			return p
		}
		img, ok := images[n]
		if !ok || (plan.MaxUnitBytes > 0 && int64(len(img)) > plan.MaxUnitBytes) {
			unrendered = append(unrendered, n)
			continue
		}
		hold.spend(1)
		if !f.callOCR(ctx, p, plan, opts, ocrUnit{data: img, mediaType: "image/png", pages: []int{n}}, hold.run, hold.tree) {
			return p
		}
	}
	if len(unrendered) > 0 {
		p.err = fmt.Errorf("page %s could not be rendered for OCR (the extraction's notes say why)", pageListText(unrendered))
	}
	return p
}
