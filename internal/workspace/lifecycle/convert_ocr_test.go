package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/files/extract/pdftest"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// ocrChild is a conversion child that cuts PDF pages in this process,
// "renders" a page as a fixed PNG (or dies rendering one, crash), and
// "converts" a recording into a fixed WAV.
type ocrChild struct {
	convert.Client
	crash bool
}

var (
	fakePNG = []byte("\x89PNG\r\n\x1a\nfake page")
	fakeWAV = []byte("RIFF....WAVEfake audio")
)

func (c ocrChild) RenderPage(context.Context, []byte, string) ([]byte, error) {
	if c.crash {
		return nil, convert.ErrQuarantined
	}
	return fakePNG, nil
}

type memSpool struct{ *bytes.Reader }

func (s memSpool) Size() int64  { return int64(s.Len()) }
func (s memSpool) Close() error { return nil }

func (ocrChild) ConvertMedia(context.Context, io.ReaderAt, int64, string, string) (convert.Spool, error) {
	return memSpool{bytes.NewReader(fakeWAV)}, nil
}

func ocrFixture(t *testing.T, labels ...string) (*fixture, Owner) {
	t.Helper()
	return ocrFixtureOf(t, ocrChild{Client: &convert.InProcess{}}, labels...)
}

func ocrFixtureOf(t *testing.T, child ocrChild, labels ...string) (*fixture, Owner) {
	t.Helper()
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{Convert: child, Labels: labels})
	o := f.run(t, "alice")
	f.open(t, o, "w")
	return f, o
}

func twoPagePDF() string {
	return string(pdftest.Doc{Pages: []pdftest.Page{
		{Lines: []pdftest.Line{{Text: "one", X: 72, Y: 770, Size: 12}}},
		{Lines: []pdftest.Line{{Text: "two", X: 72, Y: 770, Size: 12}}},
	}}.Bytes())
}

func rasterize(f *fixture, o Owner, info workspace.FileInfo, args workspace.ConvertRasterizeArgs) ([]byte, error) {
	args.ConvertSource = workspace.ConvertSource{Path: info.Path, SHA256: info.SHA256}
	var out bytes.Buffer
	err := f.m.Convert(context.Background(), o, ConvertRequest{Op: workspace.OpConvertRasterize, Args: &args, Source: "w", SHA256: info.SHA256, MaxOutput: 1 << 20}, &out, nil)
	return out.Bytes(), err
}

// The pages OCR reads are cut out of a PDF in the workspace on its worker,
// or one is rendered; a unit over its bound is refused with its own class,
// so the core can send fewer pages at a time.
func TestConvertRasterize_CutsOrRendersThePagesWithinTheUnitBound(t *testing.T) {
	f, o := ocrFixture(t, wire.LabelPDF, wire.LabelRender)
	info := f.put(t, o, "w", "scan.pdf", twoPagePDF())
	cut, err := rasterize(f, o, info, workspace.ConvertRasterizeArgs{Pages: []int{2}, MaxUnit: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := extract.PDF{}.Extract(context.Background(), bytes.NewReader(cut), int64(len(cut)), discard{}, extract.Options{})
	if err != nil || rep.Pages != 1 {
		t.Fatalf("the cut is not a one-page PDF: %v %+v", err, rep)
	}
	png, err := rasterize(f, o, info, workspace.ConvertRasterizeArgs{Pages: []int{1}, Render: true, Language: "uk", MaxUnit: 1 << 20})
	if err != nil || !bytes.Equal(png, fakePNG) {
		t.Fatalf("render: %q %v", png, err)
	}
	if _, err := rasterize(f, o, info, workspace.ConvertRasterizeArgs{Pages: []int{1, 2}, MaxUnit: 64}); classOf(err) != workspace.ClassUnitTooLarge {
		t.Fatalf("over the bound: %v", err)
	}
	if _, err := rasterize(f, o, info, workspace.ConvertRasterizeArgs{Pages: []int{1, 2}, Render: true, MaxUnit: 1 << 20}); classOf(err) != workspace.ClassInvalid {
		t.Fatalf("two pages rendered at once: %v", err)
	}
}

type discard struct{}

func (discard) Text(string) error                   { return nil }
func (discard) Image(extract.Image) (string, error) { return "", nil }

// A worker without the page renderer, or without ffmpeg, refuses the job
// and names the program to install.
func TestConvertRasterize_AMissingHelperIsRefusedByName(t *testing.T) {
	f, o := ocrFixture(t, wire.LabelPDF)
	info := f.put(t, o, "w", "scan.pdf", twoPagePDF())
	_, err := rasterize(f, o, info, workspace.ConvertRasterizeArgs{Pages: []int{1}, Render: true, MaxUnit: 1 << 20})
	if classOf(err) != workspace.ClassHelperMissing || !strings.Contains(err.Error(), "pdftoppm") {
		t.Fatalf("render with no renderer: %v", err)
	}
	rec := f.put(t, o, "w", "call.mp3", "ID3\x04\x00\x00\x00\x00\x00\x00fake mp3")
	var out bytes.Buffer
	err = f.m.Convert(context.Background(), o, ConvertRequest{Op: workspace.OpConvertMedia, Args: &workspace.ConvertMediaArgs{ConvertSource: workspace.ConvertSource{Path: rec.Path, SHA256: rec.SHA256}},
		Source: "w", SHA256: rec.SHA256, MaxOutput: 1 << 20}, &out, nil)
	if classOf(err) != workspace.ClassHelperMissing || !strings.Contains(err.Error(), "ffmpeg") || out.Len() != 0 {
		t.Fatalf("media with no ffmpeg: %v (%d bytes)", err, out.Len())
	}
}

// A recording is converted on the worker and only the WAV comes back; a
// document is not a recording.
func TestConvertMedia_SendsTheConvertedAudioOnly(t *testing.T) {
	f, o := ocrFixture(t, wire.LabelMedia)
	rec := f.put(t, o, "w", "call.mp3", "ID3\x04\x00\x00\x00\x00\x00\x00fake mp3")
	media := func(info workspace.FileInfo) ([]byte, error) {
		var out bytes.Buffer
		err := f.m.Convert(context.Background(), o, ConvertRequest{Op: workspace.OpConvertMedia, Args: &workspace.ConvertMediaArgs{ConvertSource: workspace.ConvertSource{Path: info.Path, SHA256: info.SHA256}},
			Source: "w", SHA256: info.SHA256, MaxOutput: 1 << 20}, &out, nil)
		return out.Bytes(), err
	}
	got, err := media(rec)
	if err != nil || !bytes.Equal(got, fakeWAV) {
		t.Fatalf("got %q, %v", got, err)
	}
	doc := f.put(t, o, "w", "a.txt", "just text")
	if _, err := media(doc); classOf(err) != workspace.ClassConversionUnsupported {
		t.Fatalf("a text file: %v", err)
	}
}

// A page that kills the conversion child while it is rendered quarantines
// the file for that workspace, as an extraction's crash does.
func TestConvertRasterize_ACrashQuarantinesTheFile(t *testing.T) {
	f, o := ocrFixtureOf(t, ocrChild{Client: &convert.InProcess{}, crash: true}, wire.LabelPDF, wire.LabelRender)
	info := f.put(t, o, "w", "bad.pdf", twoPagePDF())
	args := workspace.ConvertRasterizeArgs{Pages: []int{1}, Render: true, MaxUnit: 1 << 20}
	if _, err := rasterize(f, o, info, args); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("first: %v", err)
	}
	if _, err := rasterize(f, o, info, args); !errors.Is(err, ErrQuarantined) || classOf(err) == workspace.ClassConversionCrashed {
		t.Fatalf("second, refused before the worker: %v", err)
	}
}

// md_splice writes the OCR text in place of its pages in the workspace's
// file and keeps the rest; a file changed since its offsets were taken, or
// a header that is not one comment, is refused and the file is left as it
// was.
func TestSpliceMarkdown_ReplacesThePagesOnlyWhenTheFileIsUnchanged(t *testing.T) {
	f, o := ocrFixture(t)
	md := "# scan\n\n## Page 1\n\n\n\n## Page 2\n\ntyped"
	info := f.put(t, o, "w", "out/scan.md", md)
	starts := map[int]int{1: strings.Index(md, "\n\n## Page 1"), 2: strings.Index(md, "\n\n## Page 2")}
	in := workspace.MDSpliceInput{Header: "<!-- loom: ocr engine=vision pages=1 language=uk -->", PageStarts: starts, Pages: map[int]string{1: "read"}}
	ctx := context.Background()
	if _, err := f.m.SpliceMarkdown(ctx, o, "w", workspace.MDSpliceArgs{Path: "out/scan.md", SHA256: strings.Repeat("0", 64)}, in); classOf(err) != workspace.ClassSourceChanged {
		t.Fatalf("a stale digest: %v", err)
	}
	bad := in
	bad.Header = "<!-- x -->\n# injected"
	if _, err := f.m.SpliceMarkdown(ctx, o, "w", workspace.MDSpliceArgs{Path: "out/scan.md", SHA256: info.SHA256}, bad); classOf(err) != workspace.ClassInvalid {
		t.Fatalf("a two-line header: %v", err)
	}
	if got := f.get(t, o, "w", "out/scan.md"); got != md {
		t.Fatalf("a refused splice changed the file: %q", got)
	}
	ans, err := f.m.SpliceMarkdown(ctx, o, "w", workspace.MDSpliceArgs{Path: "out/scan.md", SHA256: info.SHA256}, in)
	if err != nil {
		t.Fatal(err)
	}
	want := "# scan\n\n<!-- loom: ocr engine=vision pages=1 language=uk -->\n\n## Page 1\n\nread\n\n## Page 2\n\ntyped"
	if got := f.get(t, o, "w", "out/scan.md"); got != want || ans.Size != int64(len(want)) {
		t.Fatalf("got %q (%d), want %q", got, ans.Size, want)
	}
}

func (f *fixture) get(t *testing.T, o Owner, name, p string) string {
	t.Helper()
	var b bytes.Buffer
	if _, err := f.m.GetFile(context.Background(), o, name, workspace.GetArgs{Path: p}, &b, 0); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
