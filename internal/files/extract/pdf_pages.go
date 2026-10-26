package extract

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
)

// SelectPages writes a new PDF holding only the given pages of the one in r,
// in the order given: the cut an OCR engine is sent when it reads a page
// range whole. The file is parsed under the same limits as extraction, and an
// encrypted one is refused the same way.
//
// pdfcpu copies each page's resources into the new file; a page that cannot
// be copied fails the whole cut rather than producing a file with a page
// missing, which would shift every page number the transcript is filed under.
func SelectPages(ctx context.Context, r io.ReaderAt, size int64, pages []int) (out []byte, err error) {
	if len(pages) == 0 {
		return nil, errors.New("extract: no pages selected")
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("extract: the PDF could not be cut: %s", cutPanic(p))
		}
	}()
	pc, err := openPDFContext(ctx, r, size, maxPDFDecode)
	if err != nil {
		return nil, err
	}
	for _, n := range pages {
		if n < 1 || n > pc.PageCount {
			return nil, fmt.Errorf("extract: page %d is not in the PDF (it has %d)", n, pc.PageCount)
		}
	}
	dest, err := pdfcpu.ExtractPages(ctx, pc, pages, false)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, fmt.Errorf("extract: the pages could not be cut out: %s", cutPanic(err))
	}
	var b bytes.Buffer
	bw := bufio.NewWriter(&b)
	dest.Write.Writer = bw
	if err := pdfcpu.WriteContext(ctx, dest); err != nil {
		return nil, fmt.Errorf("extract: the cut PDF could not be written: %s", cutPanic(err))
	}
	if err := bw.Flush(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
