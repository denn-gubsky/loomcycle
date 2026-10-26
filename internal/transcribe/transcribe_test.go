package transcribe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestUnit_IsBoundedToItsCut — a Unit reads only its own window of the
// underlying file: a read past its end is io.EOF, never the next cut's bytes.
func TestUnit_IsBoundedToItsCut(t *testing.T) {
	src := bytes.NewReader([]byte("0123456789"))
	u, err := NewUnit(src, 2, 4, "application/pdf")
	if err != nil {
		t.Fatalf("NewUnit: %v", err)
	}
	if u.Size() != 4 || u.MediaType != "application/pdf" {
		t.Errorf("unit = size %d type %q, want 4 application/pdf", u.Size(), u.MediaType)
	}
	got, err := io.ReadAll(io.NewSectionReader(u, 0, 100))
	if err != nil || string(got) != "2345" {
		t.Errorf("read = %q, %v; want 2345", got, err)
	}
	buf := make([]byte, 2)
	if n, err := u.ReadAt(buf, 4); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("read past the end = %d, %v; want 0, EOF", n, err)
	}
}

func TestNewUnit_RefusesABadWindow(t *testing.T) {
	src := bytes.NewReader([]byte("x"))
	for _, c := range []struct {
		r        io.ReaderAt
		off, n   int64
		mt, want string
	}{
		{nil, 0, 1, "image/png", "reader"},
		{src, -1, 1, "image/png", "offset"},
		{src, 0, -1, "image/png", "size"},
		{src, 0, 1, "", "media type"},
	} {
		if _, err := NewUnit(c.r, c.off, c.n, c.mt); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("NewUnit(off=%d n=%d mt=%q) err = %v, want it to mention %q", c.off, c.n, c.mt, err, c.want)
		}
	}
}

func TestPageRange_Count(t *testing.T) {
	for _, c := range []struct {
		r    PageRange
		want int
	}{{PageRange{}, 0}, {PageRange{First: 1, Last: 1}, 1}, {PageRange{First: 3, Last: 7}, 5}, {PageRange{First: 5, Last: 2}, 0}} {
		if got := c.r.Count(); got != c.want {
			t.Errorf("%+v.Count() = %d, want %d", c.r, got, c.want)
		}
	}
}

// fakeEngines prove the interfaces are implementable as declared and hold the
// shape later phases build on; a change that breaks them breaks this file.
type fakeOCR struct{}

func (fakeOCR) Name() string                  { return "fake-ocr" }
func (fakeOCR) Accepts(mediaType string) bool { return strings.HasPrefix(mediaType, "image/") }
func (fakeOCR) Plan(context.Context, string) (Plan, error) {
	return Plan{Engine: "fake-ocr", WholePDF: true}, nil
}
func (fakeOCR) OCR(_ context.Context, _ Plan, u Unit, o OCROptions) (Result, error) {
	return Result{Pages: []Page{{Number: u.Pages.First, Text: "text in " + o.Language}}}, nil
}

type fakeSTT struct{}

func (fakeSTT) Name() string        { return "fake-stt" }
func (fakeSTT) Accepts(string) bool { return true }
func (fakeSTT) MaxBytes() int64     { return 25 << 20 }
func (fakeSTT) Transcribe(_ context.Context, u Unit, o STTOptions) (Segments, error) {
	return Segments{{Start: u.Span.Start, End: u.Span.End, Speaker: "Speaker 1", Text: o.Language}}, nil
}

var (
	_ OCREngine = fakeOCR{}
	_ STTEngine = fakeSTT{}
)

func TestEngines_ImplementTheInterfaces(t *testing.T) {
	u, _ := NewUnit(bytes.NewReader([]byte("png")), 0, 3, "image/png")
	u.Pages = PageRange{First: 2, Last: 2}
	var o OCREngine = fakeOCR{}
	plan, err := o.Plan(context.Background(), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	r, err := o.OCR(context.Background(), plan, u, OCROptions{Language: "uk"})
	if err != nil || len(r.Pages) != 1 || r.Pages[0].Number != 2 || r.Pages[0].Text != "text in uk" {
		t.Errorf("OCR = %+v, %v", r, err)
	}
	u.Span = TimeRange{Start: time.Minute, End: 2 * time.Minute}
	var s STTEngine = fakeSTT{}
	segs, err := s.Transcribe(context.Background(), u, STTOptions{Language: "uk", Diarize: DiarizeAuto})
	if err != nil || len(segs) != 1 || segs[0].Start != time.Minute || segs[0].Text != "uk" {
		t.Errorf("Transcribe = %+v, %v", segs, err)
	}
}
