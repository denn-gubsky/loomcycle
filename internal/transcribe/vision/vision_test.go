package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
	"golang.org/x/image/bmp"
)

// fakeModel is a provider that records each request and answers with reply.
type fakeModel struct {
	caps  providers.Capabilities
	reply func(req providers.Request) string
	stop  string
	usage providers.Usage

	mu   sync.Mutex
	reqs []providers.Request
}

func (f *fakeModel) ID() string                                   { return "fake" }
func (f *fakeModel) Capabilities() providers.Capabilities         { return f.caps }
func (f *fakeModel) Probe(context.Context) error                  { return nil }
func (f *fakeModel) ListModels(context.Context) ([]string, error) { return []string{"m"}, nil }
func (f *fakeModel) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	ch := make(chan providers.Event, 3)
	text := ""
	if f.reply != nil {
		text = f.reply(req)
	}
	if text != "" {
		ch <- providers.Event{Type: providers.EventText, Text: text}
	}
	u := f.usage
	ch <- providers.Event{Type: providers.EventDone, StopReason: f.stop, Usage: &u}
	close(ch)
	return ch, nil
}

func (f *fakeModel) calls() []providers.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]providers.Request(nil), f.reqs...)
}

func engineWith(t *testing.T, m *fakeModel, ocr config.OCRConfig) *Engine {
	t.Helper()
	ocr.Engine = config.OCREngineVision
	e, err := New(Options{
		Config: *config.TranscriptionConfig{OCR: &ocr}.Resolved().OCR,
		Resolve: func(_ context.Context, tier, alias string) (string, string, error) {
			return "fake-provider", "fake-model", nil
		},
		Provider: func(id string) (providers.Provider, error) {
			if id != "fake-provider" {
				return nil, errors.New("no such provider")
			}
			return m, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func pdfUnit(t *testing.T, first, last int) transcribe.Unit {
	t.Helper()
	data := []byte("%PDF-1.7 a cut page range")
	u, err := transcribe.NewUnit(bytes.NewReader(data), 0, int64(len(data)), "application/pdf")
	if err != nil {
		t.Fatal(err)
	}
	u.Pages = transcribe.PageRange{First: first, Last: last}
	return u
}

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func imageUnit(t *testing.T, data []byte, mt string) transcribe.Unit {
	t.Helper()
	u, err := transcribe.NewUnit(bytes.NewReader(data), 0, int64(len(data)), mt)
	if err != nil {
		t.Fatal(err)
	}
	u.Pages = transcribe.PageRange{First: 1, Last: 1}
	return u
}

var documentModel = providers.Capabilities{SupportsVision: true, SupportsDocumentInput: true}
var imageOnlyModel = providers.Capabilities{SupportsVision: true}

// A model that reads images only cannot take a PDF without a rasteriser: the
// plan refuses it, naming the setting, before any call or reservation.
func TestOCR_ImageOnlyEngineRefusesPDFWithoutRasterize(t *testing.T) {
	m := &fakeModel{caps: imageOnlyModel}
	e := engineWith(t, m, config.OCRConfig{})
	_, err := e.Plan(context.Background(), "application/pdf")
	if err == nil || !strings.Contains(err.Error(), "transcription.ocr.rasterize") {
		t.Fatalf("plan for a PDF = %v, want a refusal naming transcription.ocr.rasterize", err)
	}
	if p, err := e.Plan(context.Background(), "image/png"); err != nil || p.Provider != "fake-provider" || p.Model != "fake-model" {
		t.Fatalf("an image is refused too: %+v %v", p, err)
	}
	// With the helper configured the PDF is planned page image by page image.
	e = engineWith(t, m, config.OCRConfig{Rasterize: &config.HelperCommand{Command: "pdftoppm -r 150 -png {in} {out_prefix}"}})
	p, err := e.Plan(context.Background(), "application/pdf")
	if err != nil || p.WholePDF {
		t.Fatalf("plan with rasterize = %+v %v, want page images", p, err)
	}
	// And a PDF unit sent anyway is refused at the call, not passed on.
	if _, err := e.OCR(context.Background(), p, pdfUnit(t, 1, 1), transcribe.OCROptions{Prompt: "x", MaxTokens: 10}); err == nil {
		t.Error("an image-only model was sent a PDF")
	}
	if n := len(m.calls()); n != 0 {
		t.Fatalf("%d model calls, want none", n)
	}
}

// A tier that resolves to a model that cannot see is refused before a call.
func TestVision_BlindModelRefusedBeforeAnyCall(t *testing.T) {
	m := &fakeModel{caps: providers.Capabilities{}}
	e := engineWith(t, m, config.OCRConfig{})
	if _, err := e.Plan(context.Background(), "image/png"); err == nil || !strings.Contains(err.Error(), "cannot accept images") {
		t.Fatalf("plan = %v", err)
	}
	if len(m.calls()) != 0 {
		t.Fatal("a blind model was called")
	}
}

// A page range goes to a document-reading model as one PDF; the reply is
// split back into its pages by a marker the document could not have known.
func TestVision_PDFRangeGoesAsOneDocumentAndSplitsIntoPages(t *testing.T) {
	m := &fakeModel{caps: documentModel, usage: providers.Usage{InputTokens: 4000, OutputTokens: 900, CredentialSource: "tenant"}}
	m.reply = func(req providers.Request) string {
		marker := markerIn(t, req)
		return "preamble\n" + marker + "\nПерша сторінка\n" + marker + "\n\n" + marker + "\n## Третя\n"
	}
	e := engineWith(t, m, config.OCRConfig{})
	plan, err := e.Plan(context.Background(), "application/pdf")
	if err != nil || !plan.WholePDF {
		t.Fatalf("plan = %+v %v", plan, err)
	}
	res, err := e.OCR(context.Background(), plan, pdfUnit(t, 3, 5), transcribe.OCROptions{Prompt: "Transcribe.", MaxTokens: 6000})
	if err != nil {
		t.Fatal(err)
	}
	want := []transcribe.Page{{Number: 3, Text: "Перша сторінка"}, {Number: 4, Text: "[no text]"}, {Number: 5, Text: "## Третя"}}
	if len(res.Pages) != 3 {
		t.Fatalf("pages = %+v", res.Pages)
	}
	for i, p := range want {
		if res.Pages[i] != p {
			t.Errorf("page %d = %+v, want %+v", i, res.Pages[i], p)
		}
	}
	if res.Provider != "fake-provider" || res.Model != "fake-model" || res.InputTokens != 4000 || res.OutputTokens != 900 || res.CredentialSource != "tenant" {
		t.Errorf("usage = %+v", res)
	}
	calls := m.calls()
	if len(calls) != 1 {
		t.Fatalf("%d calls", len(calls))
	}
	req := calls[0]
	if len(req.Documents) != 1 || req.Documents[0].MediaType != "application/pdf" {
		t.Fatalf("documents = %+v", req.Documents)
	}
	if got, _ := base64.StdEncoding.DecodeString(req.Documents[0].Data); string(got) != "%PDF-1.7 a cut page range" {
		t.Errorf("document bytes = %q", got)
	}
	if req.Model != "fake-model" || req.MaxTokens != 6000 {
		t.Errorf("model %q max_tokens %d", req.Model, req.MaxTokens)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" || len(req.Messages[0].Content) != 1 ||
		!strings.HasPrefix(req.Messages[0].Content[0].Text, "Transcribe.") {
		t.Errorf("messages = %+v", req.Messages)
	}
}

// A reply whose markers do not match the page count is kept whole on the
// range's first page rather than spread over pages by guesswork.
func TestVision_MarkerMismatchKeepsTheTextOnTheFirstPage(t *testing.T) {
	m := &fakeModel{caps: documentModel, reply: func(providers.Request) string { return "all the text, no markers" }}
	e := engineWith(t, m, config.OCRConfig{})
	plan, _ := e.Plan(context.Background(), "application/pdf")
	res, err := e.OCR(context.Background(), plan, pdfUnit(t, 1, 2), transcribe.OCROptions{Prompt: "x", MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pages) != 2 || res.Pages[0].Text != "all the text, no markers" || !strings.Contains(res.Pages[1].Text, "page 1") {
		t.Fatalf("pages = %+v", res.Pages)
	}
}

// An empty answer is "[no text]"; an empty answer cut by the token ceiling is
// an error, so a misconfigured budget is not recorded as a blank page.
func TestVision_EmptyAndCutReplies(t *testing.T) {
	m := &fakeModel{caps: imageOnlyModel}
	e := engineWith(t, m, config.OCRConfig{})
	plan, _ := e.Plan(context.Background(), "image/png")
	res, err := e.OCR(context.Background(), plan, imageUnit(t, pngOf(t, 4, 4), "image/png"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10})
	if err != nil || len(res.Pages) != 1 || res.Pages[0].Text != "[no text]" {
		t.Fatalf("empty reply = %+v %v", res, err)
	}
	m.stop = "max_tokens"
	if _, err := e.OCR(context.Background(), plan, imageUnit(t, pngOf(t, 4, 4), "image/png"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10}); err == nil {
		t.Fatal("an empty reply cut at the token ceiling was taken as a blank page")
	}
	m.reply = func(providers.Request) string { return "half a page" }
	res, err = e.OCR(context.Background(), plan, imageUnit(t, pngOf(t, 4, 4), "image/png"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10})
	if err != nil || !strings.HasPrefix(res.Pages[0].Text, "half a page") || !strings.Contains(res.Pages[0].Text, "token") {
		t.Fatalf("cut reply = %+v %v", res, err)
	}
}

// An image goes whole, its bytes untouched, unless it is above the engine's
// pixel cap; then it is scaled down to fit. A BMP, which no model takes, is
// sent as a PNG.
func TestVision_ImagesWholeUnlessAboveThePixelCap(t *testing.T) {
	m := &fakeModel{caps: imageOnlyModel, reply: func(providers.Request) string { return "text" }}
	e := engineWith(t, m, config.OCRConfig{})
	plan, _ := e.Plan(context.Background(), "image/png")
	small := pngOf(t, 40, 30)
	if _, err := e.OCR(context.Background(), plan, imageUnit(t, small, "image/png"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10}); err != nil {
		t.Fatal(err)
	}
	big := pngOf(t, MaxImageEdge*2, 100)
	if _, err := e.OCR(context.Background(), plan, imageUnit(t, big, "image/png"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10}); err != nil {
		t.Fatal(err)
	}
	src := image.NewRGBA(image.Rect(0, 0, 20, 10))
	var bm bytes.Buffer
	if err := bmp.Encode(&bm, src); err != nil {
		t.Fatal(err)
	}
	if _, err := e.OCR(context.Background(), plan, imageUnit(t, bm.Bytes(), "image/bmp"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10}); err != nil {
		t.Fatal(err)
	}
	calls := m.calls()
	if len(calls) != 3 {
		t.Fatalf("%d calls", len(calls))
	}
	sent := func(i int) (providers.ContentBlock, image.Config) {
		b := calls[i].Messages[0].Content[0]
		raw, _ := base64.StdEncoding.DecodeString(b.Data)
		cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("call %d: not an image: %v", i, err)
		}
		if i == 0 && !bytes.Equal(raw, small) {
			t.Error("a small image was re-encoded")
		}
		return b, cfg
	}
	if b, c := sent(0); b.Type != "image" || b.MediaType != "image/png" || c.Width != 40 {
		t.Errorf("small: %s %s %dx%d", b.Type, b.MediaType, c.Width, c.Height)
	}
	if _, c := sent(1); c.Width != MaxImageEdge || c.Height != 50 {
		t.Errorf("big image sent at %dx%d, want %dx50", c.Width, c.Height, MaxImageEdge)
	}
	if b, c := sent(2); b.MediaType != "image/png" || c.Width != 20 {
		t.Errorf("bmp sent as %s %dx%d", b.MediaType, c.Width, c.Height)
	}
}

// markerIn finds the page marker the prompt asks for.
func markerIn(t *testing.T, req providers.Request) string {
	t.Helper()
	p := req.Messages[0].Content[len(req.Messages[0].Content)-1].Text
	i := strings.Index(p, "<<<PAGE-")
	if i < 0 {
		t.Fatalf("no marker in prompt: %s", p)
	}
	j := strings.Index(p[i:], ">>>")
	return p[i : i+j+3]
}

// The result says whether the call reached the model: a refusal before any
// model call did not (its pages can be given back), a call that was made did,
// even when it then failed.
func TestVision_ResultSaysWhetherTheModelWasReached(t *testing.T) {
	m := &fakeModel{caps: imageOnlyModel}
	e := engineWith(t, m, config.OCRConfig{})
	plan, _ := e.Plan(context.Background(), "image/png")
	res, err := e.OCR(context.Background(), plan, pdfUnit(t, 1, 1), transcribe.OCROptions{Prompt: "x", MaxTokens: 10})
	if err == nil || res.Reached || len(m.calls()) != 0 {
		t.Fatalf("a PDF to an image-only model: reached %v, err %v, calls %d", res.Reached, err, len(m.calls()))
	}
	m.stop = "max_tokens" // empty and cut: an error after the call
	res, err = e.OCR(context.Background(), plan, imageUnit(t, pngOf(t, 4, 4), "image/png"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10})
	if err == nil || !res.Reached {
		t.Fatalf("a failed call that was made: reached %v, err %v", res.Reached, err)
	}
	m.stop = ""
	res, err = e.OCR(context.Background(), plan, imageUnit(t, pngOf(t, 4, 4), "image/png"), transcribe.OCROptions{Prompt: "x", MaxTokens: 10})
	if err != nil || !res.Reached {
		t.Fatalf("a successful call: reached %v, err %v", res.Reached, err)
	}
}
