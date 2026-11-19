package convert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/helper"
)

// DjVu is read through DjVuLibre's programs (RFC GV P3, D6): djvutxt for the
// text layer and djvused, with one of two fixed scripts, for the outline and
// the metadata. The document is copied into a scratch folder once per
// extraction and every call names only that copy, a page list the runtime
// wrote and a script from extract's fixed pair.
const (
	// DefaultDjVuTextCommand reads the text layer of the pages asked for,
	// one S-expression per page. Replaced on a worker by
	// LOOMCYCLE_WORKER_DJVU_TEXT_COMMAND.
	DefaultDjVuTextCommand = "djvutxt --detail=page --page={pages} {in}"
	// DefaultDjVuInspectCommand runs one fixed djvused script. Replaced on
	// a worker by LOOMCYCLE_WORKER_DJVU_INSPECT_COMMAND.
	DefaultDjVuInspectCommand = "djvused -e {script} {in}"
	// DefaultDjVuRasterizeCommand renders one page for OCR (RFC GV P4) as a
	// TIFF fitted in the {size} box (WxH; ddjvu keeps the page's aspect).
	// Replaced on a worker by LOOMCYCLE_WORKER_DJVU_RASTERIZE_COMMAND.
	DefaultDjVuRasterizeCommand = "ddjvu -format=tiff -page={page} -size={size} {in} {out}"
	// DjVuTextProgram, DjVuInspectProgram and DjVuRasterizeProgram are the
	// defaults' programs.
	DjVuTextProgram      = "djvutxt"
	DjVuInspectProgram   = "djvused"
	DjVuRasterizeProgram = "ddjvu"
	// DjVuTextHelperKey, DjVuInspectHelperKey and DjVuRasterizeHelperKey
	// name the helpers in errors and logs, as the configuration does.
	DjVuTextHelperKey      = "transcription.ocr.djvu.text"
	DjVuInspectHelperKey   = "transcription.ocr.djvu.inspect"
	DjVuRasterizeHelperKey = "transcription.ocr.djvu.rasterize"
	// DjVuCallTimeout bounds one helper call (a batch of pages, or one
	// script) when its Helper sets no bound. The extraction's own bound
	// covers the whole book.
	DjVuCallTimeout = 30 * time.Second
)

// djvuPageList is what the text helper's {pages} may be: the list extract
// writes ("1-3,7"), never anything else, and at most maxDjVuPageList bytes
// (a batch of 16 pages is under 100).
var djvuPageList = regexp.MustCompile(`^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$`)

const maxDjVuPageList = 512

// djvuHelpers runs p's DjVu helpers for the DjVu extractor.
type djvuHelpers struct{ p *InProcess }

func (h djvuHelpers) Renders() bool { return h.p.DjVuRasterize != nil }

func (h djvuHelpers) Open(ctx context.Context, r io.ReaderAt, size int64) (extract.DjVuSession, error) {
	s, err := h.p.openScratch(ctx, ".djvu-")
	if err != nil {
		return nil, err
	}
	// A copy, named here: the helpers never see where the file came from.
	if err := copyNew(s.dir, "book.djvu", io.NewSectionReader(r, 0, size)); err != nil {
		s.remove()
		return nil, fmt.Errorf("copying the DjVu file for its helpers: %w", err)
	}
	return &djvuSession{p: h.p, s: s, in: filepath.Join(s.path, "book.djvu")}, nil
}

type djvuSession struct {
	p  *InProcess
	s  *scratch
	in string
}

func (d *djvuSession) Close() error {
	d.s.remove()
	return nil
}

func (d *djvuSession) Text(ctx context.Context, pages string, maxBytes int) ([]byte, bool, error) {
	if len(pages) > maxDjVuPageList || !djvuPageList.MatchString(pages) {
		return nil, false, fmt.Errorf("%s: %q is not a page list", DjVuTextHelperKey, pages)
	}
	return d.run(ctx, d.p.DjVuText, map[string]string{"in": d.in, "pages": pages}, maxBytes)
}

func (d *djvuSession) Inspect(ctx context.Context, script extract.DjVuScript, maxBytes int) ([]byte, bool, error) {
	if !script.Valid() {
		return nil, false, fmt.Errorf("%s: %q is not one of the two DjVu scripts", DjVuInspectHelperKey, script)
	}
	if d.p.DjVuInspect == nil {
		return nil, false, extract.ErrHelperUnavailable
	}
	return d.run(ctx, d.p.DjVuInspect, map[string]string{"in": d.in, "script": string(script)}, maxBytes)
}

// maxDjVuRenderSide bounds the box a page is rendered in, whatever the
// caller asks: the extractor asks for the vision edge.
const maxDjVuRenderSide = 4096

// Render has the rasterize helper write page as a TIFF in the session's
// folder and reads it back through the folder's own handle, bounded. Its
// command line holds only the copy's path, the page's number, the box and
// the output path, all made here.
func (d *djvuSession) Render(ctx context.Context, page, maxSide, maxBytes int) ([]byte, error) {
	h := d.p.DjVuRasterize
	if h == nil {
		return nil, extract.ErrHelperUnavailable
	}
	if page < 1 || page > extract.MaxDjVuPages || maxSide < 1 || maxSide > maxDjVuRenderSide || maxBytes < 1 {
		return nil, fmt.Errorf("%s: page %d at %d pixels is not a render the runtime asks for", DjVuRasterizeHelperKey, page, maxSide)
	}
	name := "page-" + strconv.Itoa(page) + ".tiff"
	// A page is rendered once a session; a file of the name would be
	// another's, and the read below must see only the helper's.
	if err := d.s.dir.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	defer func() { _ = d.s.dir.Remove(name) }()
	side := strconv.Itoa(maxSide)
	// The helper writes a file; what it prints is only a diagnostic.
	if _, _, err := d.run(ctx, h, map[string]string{"in": d.in, "page": strconv.Itoa(page), "size": side + "x" + side, "out": filepath.Join(d.s.path, name)}, 64<<10); err != nil {
		return nil, err
	}
	fi, err := d.s.dir.Lstat(name)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return nil, fmt.Errorf("%s: the helper wrote no page image at {out}", DjVuRasterizeHelperKey)
	}
	if fi.Size() > int64(maxBytes) {
		return nil, fmt.Errorf("%s: the page image is %d bytes, over %d", DjVuRasterizeHelperKey, fi.Size(), maxBytes)
	}
	fh, err := d.s.dir.Open(name)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("%s: the page image grew past %d bytes while it was read", DjVuRasterizeHelperKey, maxBytes)
	}
	return data, nil
}

func (d *djvuSession) run(ctx context.Context, h *Helper, vars map[string]string, maxBytes int) ([]byte, bool, error) {
	if h == nil {
		return nil, false, extract.ErrHelperUnavailable
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = DjVuCallTimeout
	}
	res, err := helper.Run(ctx, helper.Spec{
		Key: h.Key, Template: h.Command, Wrapper: h.Wrapper, Vars: vars,
		Root: d.s.rootPath, Dir: d.s.path, Timeout: timeout, MaxOutputBytes: maxBytes,
	})
	var exit *helper.ExitError
	switch {
	case errors.Is(err, helper.ErrNotFound):
		return nil, false, fmt.Errorf("%w (%w): %v", ErrNoHelper, extract.ErrHelperUnavailable, err)
	case errors.As(err, &exit):
		return nil, false, fmt.Errorf("%s: %s", h.Key, helperMessage(res.Stderr))
	case err != nil:
		return nil, false, err
	}
	return res.Stdout, res.StdoutTruncated, nil
}
