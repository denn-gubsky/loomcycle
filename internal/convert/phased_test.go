package convert

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
)

// marker answers every job with its own error, so a test sees where a job went.
type marker struct{ err error }

func (m marker) Extract(context.Context, string, io.ReaderAt, int64, extract.Sink, extract.Options) (extract.Report, error) {
	return extract.Report{}, m.err
}
func (m marker) SelectPages(context.Context, io.ReaderAt, int64, []int) ([]byte, error) {
	return nil, m.err
}
func (m marker) RenderPage(context.Context, []byte, string) ([]byte, error) { return nil, m.err }
func (m marker) ConvertMedia(context.Context, io.ReaderAt, int64, string, string) (Spool, error) {
	return nil, m.err
}
func (m marker) EncodeAudio(context.Context, []byte, string) ([]byte, error) { return nil, m.err }
func (m marker) ReencodeImage(context.Context, []byte) ([]byte, string, error) {
	return nil, "", m.err
}

// Extraction and page cuts go to the pool; rendering, media, encoding and
// images stay here until RFC GO P2 moves them.
func TestPhased_RoutesEachJobToItsSide(t *testing.T) {
	pool, here := errors.New("pool"), errors.New("here")
	p := Phased{Pool: marker{pool}, Here: marker{here}}
	ctx := context.Background()
	_, e1 := p.Extract(ctx, "", nil, 0, nil, extract.Options{})
	_, e2 := p.SelectPages(ctx, nil, 0, nil)
	_, e3 := p.RenderPage(ctx, nil, "")
	_, e4 := p.ConvertMedia(ctx, nil, 0, "", "")
	_, e5 := p.EncodeAudio(ctx, nil, "")
	_, _, e6 := p.ReencodeImage(ctx, nil)
	for i, c := range []struct{ got, want error }{{e1, pool}, {e2, pool}, {e3, here}, {e4, here}, {e5, here}, {e6, here}} {
		if c.got != c.want {
			t.Errorf("job %d went to %v, want %v", i+1, c.got, c.want)
		}
	}
}
