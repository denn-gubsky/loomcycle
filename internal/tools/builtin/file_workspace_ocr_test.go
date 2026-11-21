package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// wsOCRChild is a workspace worker's conversion child in a test: it
// extracts and cuts PDF pages in this process, "renders" a page as a fixed
// PNG (or dies rendering one), and "converts" a recording into wav.
type wsOCRChild struct {
	*countingConverter
	png         []byte
	wav         []byte
	crashRender bool
}

func (c *wsOCRChild) RenderPage(context.Context, []byte, string) ([]byte, error) {
	if c.crashRender {
		return nil, convert.ErrQuarantined
	}
	return c.png, nil
}

type bytesSpool struct{ *bytes.Reader }

func (s bytesSpool) Size() int64  { return int64(s.Len()) }
func (s bytesSpool) Close() error { return nil }

func (c *wsOCRChild) ConvertMedia(context.Context, io.ReaderAt, int64, string, string) (convert.Spool, error) {
	return bytesSpool{bytes.NewReader(c.wav)}, nil
}

// wsOCRFixture is a run with workspaces on a converting worker, OCR and
// speech-to-text engines in the core, and every op the worker is asked
// recorded with its arguments.
type wsOCRFixture struct {
	*fcFixture
	child *wsOCRChild
	ocr   *fakeOCR
	stt   *fakeSTT
	mu    sync.Mutex
	ops   []observedOp
}

type observedOp struct {
	op   string
	args string
}

func newWSOCRFixture(t *testing.T, labels []string, ocrOp config.OCRConfig, sttOp config.STTConfig) *wsOCRFixture {
	t.Helper()
	x := &wsOCRFixture{child: &wsOCRChild{countingConverter: &countingConverter{Client: &convert.InProcess{}}, png: testPNG(t), wav: testWAV(1000, 90*time.Second)}}
	opt := worker.Options{Capacity: 10, Convert: x.child, Labels: labels, Observe: func(op string, args []byte) {
		x.mu.Lock()
		x.ops = append(x.ops, observedOp{op, string(args)})
		x.mu.Unlock()
	}}
	x.fcFixture = newFCFixtureWith(t, inbox.Service{}, lifecycle.Limits{}, opt)
	ocrOp.Engine = config.OCREngineVision
	x.ocr = &fakeOCR{plan: transcribe.Plan{Engine: "vision", Provider: "anthropic", Model: "claude-x", MaxUnitBytes: 16 << 20}}
	sttOp.Engine = config.STTEngineOpenAIAudio
	sttOp.Provider, sttOp.Model = "whisper-local", "whisper"
	x.stt = &fakeSTT{plan: transcribe.STTPlan{Engine: "openai-audio", Provider: "whisper-local", Model: "large-v3", MaxBytes: 25 << 20}}
	x.file.OCR = &FileOCR{Config: config.TranscriptionConfig{OCR: &ocrOp}.Resolved(), Engine: x.ocr, Ledger: &fakeLedger{}}
	x.file.STT = &FileSTT{Config: config.TranscriptionConfig{STT: &sttOp}.Resolved(), Engine: x.stt, Ledger: &fakeLedger{}}
	x.ctx = tools.UpdateRunPolicy(x.ctx, func(p *tools.RunPolicy) { p.Transcription = &config.AgentTranscription{OCR: "auto"} })
	x.open(t, "w")
	return x
}

// The labels of a worker with the page renderer and ffmpeg.
var ocrLabels = append(slices.Clone(convLabels), wire.LabelRender, wire.LabelMedia)

func (x *wsOCRFixture) observed() []observedOp {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.ops)
}

func (x *wsOCRFixture) opsNamed(op string) int {
	n := 0
	for _, o := range x.observed() {
		if o.op == op {
			n++
		}
	}
	return n
}

func sentBytes(units []sentUnit) (n uint64) {
	for _, u := range units {
		n += uint64(len(u.data))
	}
	return n
}

// A scanned PDF in a workspace becomes Markdown in the same workspace with
// the OCR text in place of its pages: the PDF's bytes never cross to the
// core, only the rendered pages the model reads do, and the Markdown does
// not cross at all (the worker splices the text into it). The provenance
// line written there names no provider and no model.
func TestFileExtract_AScannedPDFInAWorkspaceGetsItsOCRTextInTheWorkspace(t *testing.T) {
	x := newWSOCRFixture(t, ocrLabels, config.OCRConfig{Language: "uk"}, config.STTConfig{})
	pdf := scannedPDF(t, 2)
	x.wsPutBytes(t, "w", "in/skan.pdf", pdf)
	before := x.crossed()
	out, res := x.exec(t, x.ctx, map[string]any{"op": "extract", "path": "workspace:w:in/skan.pdf", "to": "workspace:w:out"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	_, byOp := crossedSince(before, x.crossed())
	sent := x.ocr.sent()
	if len(sent) != 2 || sent[0].mediaType != "image/png" || !bytes.Equal(sent[0].data, x.child.png) {
		t.Fatalf("the engine got %d units: %+v", len(sent), sent)
	}
	if byOp["get"] != 0 || byOp["convert_rasterize"] != sentBytes(sent) {
		t.Errorf("crossed %v; want no get and exactly the %d bytes of page images", byOp, sentBytes(sent))
	}
	for op, n := range byOp {
		if op != "convert_rasterize" && n > 4096 {
			t.Errorf("%s brought back %d bytes: more than an answer", op, n)
		}
	}
	md, ok := x.wsGet(t, "w", strings.TrimPrefix(asStr(out["ref"]), "workspace:w:"))
	for _, want := range []string{"<!-- loom: ocr engine=vision pages=2 language=uk -->", "## Page 1\n\nocr call 1 page 1", "## Page 2\n\nocr call 2 page 1"} {
		if !ok || !strings.Contains(md, want) {
			t.Errorf("missing %q in the workspace's Markdown:\n%s", want, md)
		}
	}
	if strings.Contains(md, "claude-x") || strings.Contains(md, "anthropic") {
		t.Errorf("the workspace's Markdown names the model:\n%s", md)
	}
	ocr, _ := out["ocr"].(map[string]any)
	if ocr["status"] != "ok" || ocr["model"] != "claude-x" || out["excerpt"] != nil {
		t.Errorf("answer %v", out)
	}
}

// The same scan extracted into the inbox: the Markdown is spliced here and
// kept with today's provenance line; only the extraction's text and the
// page images cross.
func TestFileExtract_AScannedPDFInAWorkspaceIsOCRedIntoTheInbox(t *testing.T) {
	x := newWSOCRFixture(t, ocrLabels, config.OCRConfig{}, config.STTConfig{})
	x.ocr.plan.WholePDF = true
	pdf := scannedPDF(t, 2)
	x.wsPutBytes(t, "w", "skan.pdf", pdf)
	before := x.crossed()
	out, res := x.exec(t, x.ctx, map[string]any{"op": "extract", "path": "workspace:w:skan.pdf"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	_, byOp := crossedSince(before, x.crossed())
	sent := x.ocr.sent()
	if len(sent) != 1 || sent[0].mediaType != "application/pdf" || sent[0].pages != 2 || bytes.Equal(sent[0].data, pdf) {
		t.Fatalf("the engine got %+v; want the two pages cut out, not the file", sent)
	}
	if byOp["get"] != 0 || byOp["convert_rasterize"] != sentBytes(sent) {
		t.Errorf("crossed %v", byOp)
	}
	md := readFile(t, filepath.Join(x.inboxRoot(t), strings.TrimPrefix(asStr(out["ref"]), "volume:inbox:")))
	for _, want := range []string{"<!-- loom: ocr engine=vision provider=anthropic model=claude-x pages=2", "## Page 2\n\nocr call 1 page 2"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	if !strings.Contains(asStr(out["excerpt"]), "ocr call 1 page 1") {
		t.Errorf("excerpt %v", out["excerpt"])
	}
}

// A recording in a workspace is transcribed into the workspace and into the
// inbox: the worker converts it and only the converted audio crosses.
func TestFileTranscribe_ARecordingInAWorkspaceIsTranscribedFromItsConvertedAudio(t *testing.T) {
	x := newWSOCRFixture(t, ocrLabels, config.OCRConfig{}, config.STTConfig{ChunkMinutes: 1})
	rec := testM4A(30*time.Second, 200)
	x.wsPutBytes(t, "w", "calls/board.m4a", rec)
	for _, c := range []struct {
		to   string
		want string
	}{
		{"workspace:w:notes/", "workspace:w:notes/board.md"},
		{"", "volume:inbox:extracted/"},
	} {
		before := x.crossed()
		in := map[string]any{"op": "transcribe", "path": "workspace:w:calls/board.m4a"}
		if c.to != "" {
			in["to"] = c.to
		}
		out, res := x.exec(t, x.ctx, in)
		if res.IsError {
			t.Fatalf("to %q: %s", c.to, res.Text)
		}
		_, byOp := crossedSince(before, x.crossed())
		if byOp["get"] != 0 || byOp["convert_media"] != uint64(len(x.child.wav)) {
			t.Errorf("to %q: crossed %v; want only the %d converted bytes", c.to, byOp, len(x.child.wav))
		}
		ref := asStr(out["ref"])
		if !strings.HasPrefix(ref, c.want) || out["route"] != "convert" {
			t.Fatalf("to %q: answer %v", c.to, out)
		}
		var md string
		if strings.HasPrefix(ref, "workspace:") {
			md, _ = x.wsGet(t, "w", strings.TrimPrefix(ref, "workspace:w:"))
			if strings.Contains(md, "large-v3") || strings.Contains(md, "whisper-local") {
				t.Errorf("the workspace's transcript names the model:\n%s", md)
			}
		} else {
			md = readFile(t, filepath.Join(x.inboxRoot(t), strings.TrimPrefix(ref, "volume:inbox:")))
		}
		if !strings.Contains(md, "call ") {
			t.Errorf("to %q: transcript:\n%s", c.to, md)
		}
	}
	for _, u := range x.stt.sent() {
		if u.mediaType != "audio/wav" {
			t.Errorf("the engine got %s, not the converted WAV", u.mediaType)
		}
	}
}

// A worker without the page renderer or ffmpeg refuses the job and names
// the program; nothing reaches a model.
func TestFileWorkspaceOCR_AMissingHelperIsRefusedByName(t *testing.T) {
	x := newWSOCRFixture(t, convLabels, config.OCRConfig{}, config.STTConfig{})
	x.wsPutBytes(t, "w", "skan.pdf", scannedPDF(t, 1))
	_, res := x.exec(t, x.ctx, map[string]any{"op": "extract", "path": "workspace:w:skan.pdf", "to": "workspace:w:out"})
	wantFileRefusal(t, "OCR with no renderer", res, "pdftoppm")
	x.wsPutBytes(t, "w", "call.m4a", testM4A(30*time.Second, 200))
	_, res = x.exec(t, x.ctx, map[string]any{"op": "transcribe", "path": "workspace:w:call.m4a"})
	wantFileRefusal(t, "transcribe with no ffmpeg", res, "ffmpeg")
	if len(x.ocr.sent()) != 0 || len(x.stt.sent()) != 0 {
		t.Error("a model was called")
	}
}

// The run's page cap and minute cap hold for a workspace's file as for any:
// the second page is not sent past a one-page cap, and a recording past the
// minute cap reaches no model. A page over the request size is refused.
func TestFileWorkspaceOCR_ThePageAndMinuteBoundsHold(t *testing.T) {
	x := newWSOCRFixture(t, ocrLabels, config.OCRConfig{MaxPagesPerRun: 1}, config.STTConfig{MaxMinutesPerRun: 1})
	x.wsPutBytes(t, "w", "skan.pdf", scannedPDF(t, 2))
	_, res := x.exec(t, x.ctx, map[string]any{"op": "extract", "path": "workspace:w:skan.pdf", "to": "workspace:w:out"})
	wantFileRefusal(t, "past the page cap", res, "max_pages_per_run")
	if n := len(x.ocr.sent()); n != 1 {
		t.Errorf("%d units sent past a one-page cap", n)
	}
	x.wsPutBytes(t, "w", "call.m4a", testM4A(30*time.Second, 200))
	_, res = x.exec(t, x.ctx, map[string]any{"op": "transcribe", "path": "workspace:w:call.m4a"})
	wantFileRefusal(t, "past the minute cap", res, "max_minutes_per_run")
	if len(x.stt.sent()) != 0 {
		t.Error("audio past the minute cap reached the model")
	}

	y := newWSOCRFixture(t, ocrLabels, config.OCRConfig{}, config.STTConfig{})
	y.ocr.plan.WholePDF, y.ocr.plan.MaxUnitBytes = true, 64
	y.wsPutBytes(t, "w", "skan.pdf", scannedPDF(t, 2))
	_, res = y.exec(t, y.ctx, map[string]any{"op": "extract", "path": "workspace:w:skan.pdf"})
	wantFileRefusal(t, "a page over the request size", res, "alone is over")
	if len(y.ocr.sent()) != 0 {
		t.Error("a unit over the request size was sent")
	}
}

// No provider key, provider name, model name or credential reaches the
// worker: every op it was asked, with its arguments, is checked, and so is
// every file the run wrote into the workspace.
func TestFileWorkspaceOCR_NoProviderOrModelReachesTheWorker(t *testing.T) {
	x := newWSOCRFixture(t, ocrLabels, config.OCRConfig{Language: "uk"}, config.STTConfig{ChunkMinutes: 1})
	x.ctx = tools.UpdateRunPolicy(x.ctx, func(p *tools.RunPolicy) {
		p.Transcription = &config.AgentTranscription{OCR: "auto", OCRModel: "secret-ocr-alias"}
	})
	x.wsPutBytes(t, "w", "skan.pdf", scannedPDF(t, 2))
	x.wsPutBytes(t, "w", "call.m4a", testM4A(30*time.Second, 200))
	for _, in := range []map[string]any{
		{"op": "extract", "path": "workspace:w:skan.pdf", "to": "workspace:w:out"},
		{"op": "extract", "path": "workspace:w:skan.pdf"},
		{"op": "transcribe", "path": "workspace:w:call.m4a", "to": "workspace:w:notes/call.md"},
	} {
		if _, res := x.exec(t, x.ctx, in); res.IsError {
			t.Fatalf("%v: %s", in, res.Text)
		}
	}
	secrets := []string{"anthropic", "claude-x", "secret-ocr-alias", "ollama-local", "whisper", "large-v3", "api_key", "credential", "bearer"}
	ops := x.observed()
	for _, want := range []string{"convert_extract", "convert_rasterize", "md_splice", "convert_media", "put"} {
		if x.opsNamed(want) == 0 {
			t.Fatalf("no %s was observed: %v", want, ops)
		}
	}
	for _, o := range ops {
		for _, s := range secrets {
			if strings.Contains(strings.ToLower(o.args), s) {
				t.Errorf("%s's arguments hold %q: %s", o.op, s, o.args)
			}
		}
	}
	for _, p := range []string{"notes/call.md"} {
		md, _ := x.wsGet(t, "w", p)
		for _, s := range secrets[:6] {
			if strings.Contains(md, s) {
				t.Errorf("%s holds %q", p, s)
			}
		}
	}
	ls, _ := x.ws.tool.Manager.ListFiles(context.Background(), x.owner(), "w", workspace.FListArgs{Path: "out", Recursive: true})
	for _, e := range ls.Entries {
		if strings.HasSuffix(e.Path, ".md") {
			md, _ := x.wsGet(t, "w", e.Path)
			for _, s := range secrets[:6] {
				if strings.Contains(md, s) {
					t.Errorf("%s holds %q", e.Path, s)
				}
			}
		}
	}
}

// A workspace reference is judged as any is (PR 2): a run whose grant does
// not let it write refuses a transcript into a workspace, and a Workspace
// hook refuses the transcript's final path, both before the recording is
// converted.
func TestFileTranscribe_AWorkspaceTargetIsJudgedBeforeTheRecordingIsConverted(t *testing.T) {
	x := newWSOCRFixture(t, ocrLabels, config.OCRConfig{}, config.STTConfig{})
	x.wsPutBytes(t, "w", "call.m4a", testM4A(30*time.Second, 200))
	readOnly := tools.UpdateRunPolicy(x.ctx, func(p *tools.RunPolicy) { p.Workspace = tools.WorkspacePolicyValue{Read: true} })
	_, res := x.exec(t, readOnly, map[string]any{"op": "transcribe", "path": "workspace:w:call.m4a", "to": "workspace:w:notes/"})
	wantFileRefusal(t, "read-only grant", res, "does not allow write")
	hooked := tools.WithHookPreCheck(x.ctx, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
		var in struct{ Op, Path string }
		_ = json.Unmarshal(input, &in)
		if name == "Workspace" && in.Op == "write" && in.Path == "notes/call.md" {
			return tools.Result{Text: "no notes here", IsError: true}, false
		}
		return tools.Result{}, true
	})
	_, res = x.exec(t, hooked, map[string]any{"op": "transcribe", "path": "workspace:w:call.m4a", "to": "workspace:w:notes/"})
	wantFileRefusal(t, "hooked target", res, "no notes here")
	if n := x.opsNamed("convert_media"); n != 0 {
		t.Errorf("the recording was converted %d times for refused calls", n)
	}
	// A recording that is not in a workspace takes no workspace target.
	_, res = x.exec(t, x.ctx, map[string]any{"op": "transcribe", "path": "volume:inbox:call.m4a", "to": "workspace:w:notes/"})
	wantFileRefusal(t, "inbox source", res, "takes a recording in a workspace")
}

// A page that kills the worker's conversion child while it is rendered
// fails the OCR pass with the text layer kept; the worker and the
// workspace go on, and the file is then refused in that workspace.
func TestFileWorkspaceOCR_AChildCrashQuarantinesTheFile(t *testing.T) {
	x := newWSOCRFixture(t, ocrLabels, config.OCRConfig{}, config.STTConfig{})
	x.child.crashRender = true
	x.wsPutBytes(t, "w", "skan.pdf", scannedPDF(t, 1))
	_, res := x.exec(t, x.ctx, map[string]any{"op": "extract", "path": "workspace:w:skan.pdf", "to": "workspace:w:out"})
	wantFileRefusal(t, "a crash rendering", res, "OCR stopped")
	if !x.wsHas(t, "w", "out/skan.md") {
		t.Error("the text layer was not kept")
	}
	_, res = x.exec(t, x.ctx, map[string]any{"op": "extract", "path": "workspace:w:skan.pdf", "to": "workspace:w:out2"})
	wantFileRefusal(t, "the same file again", res, "crashed this workspace's file converter")
	x.wsPut(t, "w", "ok.txt", "still here")
	if got, ok := x.wsGet(t, "w", "ok.txt"); !ok || got != "still here" {
		t.Error("the workspace did not survive the crash")
	}
}

// A picture in a workspace is its own one page: the image is the unit the
// model reads, and its text goes after the title of the Markdown the
// worker wrote.
func TestFileExtract_AnImageInAWorkspaceGetsItsOCRTextAfterItsTitle(t *testing.T) {
	x := newWSOCRFixture(t, ocrLabels, config.OCRConfig{}, config.STTConfig{})
	img := testPNG(t)
	x.wsPutBytes(t, "w", "photo.png", img)
	out, res := x.exec(t, x.ctx, map[string]any{"op": "extract", "path": "workspace:w:photo.png", "to": "workspace:w:out"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	sent := x.ocr.sent()
	if len(sent) != 1 || sent[0].mediaType != "image/png" || !bytes.Equal(sent[0].data, img) {
		t.Fatalf("the engine got %+v", sent)
	}
	md, _ := x.wsGet(t, "w", strings.TrimPrefix(asStr(out["ref"]), "workspace:w:"))
	if !strings.Contains(md, "<!-- loom: ocr engine=vision pages=1") || !strings.Contains(md, "## Page 1\n\nocr call 1 page 1") {
		t.Errorf("the Markdown:\n%s", md)
	}
}
