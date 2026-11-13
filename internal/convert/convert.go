// Package convert is the one seam every file conversion the runtime does
// passes through (RFC GO): extracting a document's text, cutting pages out of
// a PDF, rendering a page for OCR, turning a recording into a WAV, encoding
// synthesized speech, and re-encoding an image a model refused.
//
// These are the jobs whose memory a hostile or merely large file decides, so
// they are the ones a deployment may want out of the core's process. A Client
// is handed bytes and returns bytes, never a path: whatever runs the job needs
// no access to a run's volumes, which is what lets it run elsewhere.
//
// InProcess runs every job in this process, as the runtime always has. A
// pool of worker processes behind a dispatcher implements the same Client
// (RFC GO P1).
package convert

import (
	"context"
	"errors"
	"io"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
)

// Client runs conversion jobs.
type Client interface {
	// Extract reads a document of mediaType and writes its Markdown and
	// images to sink, as extract.Registry.Extract does. ErrUnsupported when
	// no extractor reads the type.
	Extract(ctx context.Context, mediaType string, r io.ReaderAt, size int64, sink extract.Sink, opts extract.Options) (extract.Report, error)
	// SelectPages cuts the given pages (counted from 1) out of a PDF into a
	// PDF of their own.
	SelectPages(ctx context.Context, r io.ReaderAt, size int64, pages []int) ([]byte, error)
	// RenderPage renders a one-page PDF to a PNG for OCR. language is the
	// OCR language hint the operator's rasteriser may take.
	RenderPage(ctx context.Context, onePagePDF []byte, language string) ([]byte, error)
	// ConvertMedia turns a recording into a PCM WAV the speech-to-text pass
	// cuts. The returned Spool is the caller's to close.
	ConvertMedia(ctx context.Context, r io.ReaderAt, size int64, mediaType, language string) (Spool, error)
	// EncodeAudio encodes a WAV into format (mp3, opus, ...).
	EncodeAudio(ctx context.Context, wav []byte, format string) ([]byte, error)
	// ReencodeImage decodes an image and encodes it again, scaled to fit the
	// vision models' edge, returning the new bytes and their media type.
	ReencodeImage(ctx context.Context, data []byte) ([]byte, string, error)
}

// Spool is a converted file the caller reads at random and then closes; its
// storage is gone once it is closed.
type Spool interface {
	io.ReaderAt
	Size() int64
	Close() error
}

var (
	// ErrBusy is wrapped by the error of a job that got no conversion slot
	// within the wait, beside the context error that ended the wait. A caller
	// whose own context is still live tells the model the server is busy and
	// may be tried again.
	ErrBusy = extract.ErrBusy
	// ErrNotConfigured is returned by a job whose operator command (the
	// rasteriser, the media converter, the encoder) is not set. The caller
	// names the setting.
	ErrNotConfigured = errors.New("convert: the helper command for this job is not configured")
)
