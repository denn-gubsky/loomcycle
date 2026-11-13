package convert

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
)

type mdSink struct{ b strings.Builder }

func (s *mdSink) Text(md string) error                { s.b.WriteString(md); return nil }
func (s *mdSink) Image(extract.Image) (string, error) { return "", nil }
func scratchIn(dir string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return dir, nil }
}

func TestInProcess_ExtractWritesTheDocument(t *testing.T) {
	csv := []byte("name,age\nada,36\n")
	var sink mdSink
	rep, err := (&InProcess{}).Extract(context.Background(), "text/csv", bytes.NewReader(csv), int64(len(csv)), &sink, extract.Options{Name: "people.csv"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Format != "csv" || !strings.Contains(sink.b.String(), "ada") {
		t.Fatalf("report %+v, markdown %q", rep, sink.b.String())
	}
}

func TestInProcess_ExtractUnknownTypeIsUnsupported(t *testing.T) {
	var sink mdSink
	_, err := (&InProcess{}).Extract(context.Background(), "application/x-nothing", bytes.NewReader(nil), 0, &sink, extract.Options{Name: "x.nothing"})
	if !errors.Is(err, extract.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// With every slot taken, a job waits SlotWait and then reports ErrBusy rather
// than running past the bound.
func TestInProcess_FullSlotsReportBusy(t *testing.T) {
	lim := extract.NewLimiter(1)
	release, err := lim.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	p := &InProcess{Extractions: lim, SlotWait: 20 * time.Millisecond}
	var sink mdSink
	csv := []byte("a\n1\n")
	if _, err := p.Extract(context.Background(), "text/csv", bytes.NewReader(csv), int64(len(csv)), &sink, extract.Options{Name: "a.csv"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("Extract: err = %v, want ErrBusy", err)
	}
	if _, err := p.SelectPages(context.Background(), bytes.NewReader(nil), 0, []int{1}); !errors.Is(err, ErrBusy) {
		t.Fatalf("SelectPages: err = %v, want ErrBusy", err)
	}
}

func TestInProcess_UnsetHelpersAreNotConfigured(t *testing.T) {
	p := &InProcess{Scratch: scratchIn(t.TempDir())}
	ctx := context.Background()
	if _, err := p.RenderPage(ctx, []byte("%PDF"), ""); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("RenderPage: %v", err)
	}
	if _, err := p.ConvertMedia(ctx, bytes.NewReader(nil), 0, "audio/mpeg", ""); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("ConvertMedia: %v", err)
	}
	if _, err := p.EncodeAudio(ctx, []byte("RIFF"), "mp3"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("EncodeAudio: %v", err)
	}
	if _, _, err := p.ReencodeImage(ctx, []byte("\x89PNG")); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("ReencodeImage: %v", err)
	}
}

// A helper job runs in a scratch folder of its own and leaves nothing behind;
// a converted file's folder lasts until the spool is closed.
func TestInProcess_HelperScratchIsRemoved(t *testing.T) {
	root := t.TempDir()
	cp := &Helper{Key: "test.copy", Command: "cp {in} {out}", Timeout: 10 * time.Second}
	p := &InProcess{Scratch: scratchIn(root), Encode: cp, Media: cp}
	ctx := context.Background()

	out, err := p.EncodeAudio(ctx, []byte("RIFFwav"), "mp3")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "RIFFwav" {
		t.Fatalf("encoded %q", out)
	}
	assertEmpty(t, root)

	src := []byte("recording bytes")
	sp, err := p.ConvertMedia(ctx, bytes.NewReader(src), int64(len(src)), "audio/mpeg", "")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, sp.Size())
	if _, err := sp.ReadAt(got, 0); err != nil || !bytes.Equal(got, src) {
		t.Fatalf("spool holds %q (%v)", got, err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Fatalf("the open spool's folder is gone: %d entries", len(entries))
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	assertEmpty(t, root)
}

func TestInProcess_EncodedAudioOverTheCapIsRefused(t *testing.T) {
	root := t.TempDir()
	p := &InProcess{Scratch: scratchIn(root), MaxEncodedBytes: 4,
		Encode: &Helper{Key: "speech.encode", Command: "cp {in} {out}", Timeout: 10 * time.Second}}
	_, err := p.EncodeAudio(context.Background(), []byte("longer than four"), "mp3")
	if err == nil || !strings.Contains(err.Error(), "over the 4 limit") {
		t.Fatalf("err = %v", err)
	}
	assertEmpty(t, root)
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%d entries left in the scratch root, want none", len(entries))
	}
}
