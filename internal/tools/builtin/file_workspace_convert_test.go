package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// convLabels are the labels of a conversion child with no helper programs.
var convLabels = []string{wire.LabelExtract, wire.LabelExtractPages, wire.LabelPDF, wire.LabelWrite, wire.LabelWriteBook}

// countingConverter is the worker's conversion child in a test: it converts
// in this process, counts the extractions it was asked for, and crashes (as
// the child's death reads to the worker) on a file holding crashMark.
type countingConverter struct {
	convert.Client
	extracts atomic.Int32
}

const crashMark = "CRASH-THE-CONVERTER"

func (c *countingConverter) Extract(ctx context.Context, mt string, r io.ReaderAt, size int64, sink extract.Sink, opts extract.Options) (extract.Report, error) {
	c.extracts.Add(1)
	b, _ := io.ReadAll(io.NewSectionReader(r, 0, size))
	if bytes.Contains(b, []byte(crashMark)) {
		return extract.Report{}, convert.ErrQuarantined
	}
	return c.Client.Extract(ctx, mt, r, size, sink, opts)
}

// convFixture is fcFixture with a worker that converts.
func convFixture(t *testing.T, labels []string) (*fcFixture, *countingConverter) {
	t.Helper()
	return convFixtureCap(t, labels, 10)
}

func convFixtureCap(t *testing.T, labels []string, capacity int) (*fcFixture, *countingConverter) {
	t.Helper()
	cv := &countingConverter{Client: &convert.InProcess{}}
	f := newFCFixtureWith(t, inbox.Service{}, lifecycle.Limits{}, worker.Options{Capacity: capacity, Convert: cv, Labels: labels})
	return f, cv
}

// crossed is the bytes the workers have sent the core, by op.
func (f *fcFixture) crossed() map[string]uint64 { return f.ws.h.D.Stats().OutputBytes }

func crossedSince(before, after map[string]uint64) (total uint64, byOp map[string]uint64) {
	byOp = map[string]uint64{}
	for k, v := range after {
		if d := v - before[k]; d > 0 {
			byOp[k] = d
			total += d
		}
	}
	return total, byOp
}

func (f *fcFixture) wsPutBytes(t *testing.T, name, p string, data []byte) {
	t.Helper()
	f.wsPut(t, name, p, string(data))
}

// A PDF, a DOCX and an EPUB in a workspace are extracted into a folder of
// the same workspace on its worker: the text and the pictures land there,
// and none of the source's bytes cross to the core (no get ran, and all
// that came back is far smaller than the file).
func TestFileExtract_AWorkspaceFileIsExtractedIntoItsWorkspaceWithNoSourceByteCrossing(t *testing.T) {
	f, _ := convFixture(t, convLabels)
	f.open(t, "w")
	for _, c := range []struct {
		name string
		data []byte
		want string
		pics bool
	}{
		{"board.pdf", boardMinutesPDF(t, testJPEG(t)), "затвердити бюджет", true},
		{"minutes.docx", minutesDOCX(t, testPNG(t)), "затвердити бюджет", true},
		{"book.epub", tinyEPUB(t), "", false},
	} {
		f.wsPutBytes(t, "w", "in/"+c.name, c.data)
		before := f.crossed()
		out, res := f.exec(t, f.ctx, map[string]any{"op": "extract", "path": "workspace:w:in/" + c.name, "to": "workspace:w:out/" + c.name})
		if res.IsError {
			t.Fatalf("%s: %s", c.name, res.Text)
		}
		total, byOp := crossedSince(before, f.crossed())
		if byOp["get"] != 0 || total >= uint64(len(c.data)) {
			t.Errorf("%s: %d bytes crossed (%v) for a %d-byte file", c.name, total, byOp, len(c.data))
		}
		ref := asStr(out["ref"])
		if !strings.HasPrefix(ref, "workspace:w:out/"+c.name+"/") || !strings.HasSuffix(ref, ".md") {
			t.Fatalf("%s: ref %q", c.name, ref)
		}
		md, ok := f.wsGet(t, "w", strings.TrimPrefix(ref, "workspace:w:"))
		if !ok || (c.want != "" && !strings.Contains(md, c.want)) || strings.TrimSpace(md) == "" {
			t.Errorf("%s: the Markdown in the workspace: %q", c.name, md)
		}
		if out["excerpt"] != nil {
			t.Errorf("%s: an excerpt crossed: %v", c.name, out["excerpt"])
		}
		refs, _ := out["image_refs"].([]any)
		if c.pics && len(refs) == 0 {
			t.Errorf("%s: no picture was kept: %v", c.name, out)
		}
		for _, r := range refs {
			p := strings.TrimPrefix(asStr(r), "workspace:w:")
			if !f.wsHas(t, "w", p) || !strings.Contains(md, strings.TrimPrefix(p, "out/"+c.name+"/")) {
				t.Errorf("%s: picture %s not in the workspace or not linked:\n%s", c.name, p, md)
			}
		}
	}
}

// The same file extracted with no `to` goes to the inbox, as an inbox
// file's extraction does: only the result crosses, its pictures are kept
// and linked, and the excerpt is fenced.
func TestFileExtract_AWorkspaceFileIsExtractedIntoTheInboxAndOnlyTheResultCrosses(t *testing.T) {
	f, _ := convFixture(t, convLabels)
	f.open(t, "w")
	f.wsPutBytes(t, "w", "minutes.docx", minutesDOCX(t, testPNG(t)))
	before := f.crossed()
	out, res := f.exec(t, f.ctx, map[string]any{"op": "extract", "path": "workspace:w:minutes.docx"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	_, byOp := crossedSince(before, f.crossed())
	if byOp["get"] != 0 || byOp["convert_extract"] == 0 {
		t.Errorf("crossed %v: want the conversion's result and no get", byOp)
	}
	ref := asStr(out["ref"])
	if !strings.HasPrefix(ref, "volume:inbox:extracted/") {
		t.Fatalf("ref %q", ref)
	}
	md := readFile(t, filepath.Join(f.inboxRoot(t), strings.TrimPrefix(ref, "volume:inbox:")))
	refs, _ := out["image_refs"].([]any)
	if !strings.Contains(md, "затвердити бюджет") || len(refs) != 1 || !strings.Contains(md, strings.TrimPrefix(asStr(refs[0]), "volume:inbox:extracted/")) {
		t.Errorf("the Markdown does not hold the text or link the picture %v:\n%s", refs, md)
	}
	if strings.Contains(md, "loomcycle-image-") {
		t.Errorf("an image mark was left in the Markdown:\n%s", md)
	}
	if !strings.Contains(asStr(out["excerpt"]), "затвердити бюджет") || asStr(out["source"]) != "workspace:w:minutes.docx" {
		t.Errorf("answer %v", out)
	}
}

// Into a workspace on another worker the result streams through the core
// and is committed there; the source workspace is untouched.
func TestFileExtract_IntoAWorkspaceOnAnotherWorkerStreamsTheResult(t *testing.T) {
	f, _ := convFixtureCap(t, convLabels, 1)
	f.ws.h.AddWorker("w2", 1)
	f.open(t, "a")
	f.open(t, "b")
	ra, _ := f.ws.tool.Manager.Get(context.Background(), f.owner(), "a")
	rb, _ := f.ws.tool.Manager.Get(context.Background(), f.owner(), "b")
	if ra.WorkerID == rb.WorkerID {
		t.Fatalf("both workspaces on %s", ra.WorkerID)
	}
	f.wsPutBytes(t, "a", "minutes.docx", minutesDOCX(t, testPNG(t)))
	out, res := f.exec(t, f.ctx, map[string]any{"op": "extract", "path": "workspace:a:minutes.docx", "to": "workspace:b:out"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	md, ok := f.wsGet(t, "b", strings.TrimPrefix(asStr(out["ref"]), "workspace:b:"))
	refs, _ := out["image_refs"].([]any)
	if !ok || !strings.Contains(md, "затвердити бюджет") || len(refs) != 1 || !strings.Contains(md, "](images/") {
		t.Errorf("b holds %q, refs %v", md, refs)
	}
}

// A conversion the worker's child has no helper for is refused, and the
// refusal names the helper to install.
func TestFileExtract_AHelperTheWorkerLacksIsRefusedByName(t *testing.T) {
	f, cv := convFixture(t, convLabels) // no djvu label
	f.open(t, "w")
	f.wsPut(t, "w", "book.djvu", "AT&TFORM\x00\x00\x00\x10DJVUINFO")
	_, res := f.exec(t, f.ctx, map[string]any{"op": "extract", "path": "workspace:w:book.djvu", "to": "workspace:w:out"})
	wantFileRefusal(t, "djvu", res, "conversion_helper_missing")
	if !strings.Contains(res.Text, "djvutxt") {
		t.Errorf("the refusal does not name the helper: %s", res.Text)
	}
	if cv.extracts.Load() != 0 {
		t.Error("the conversion ran")
	}
}

// A file that crashes the conversion child fails the call; the worker and
// the workspace go on; the file is then refused in that workspace without
// reaching the worker, while the same file in another workspace is still
// converted.
func TestFileExtract_AFileThatCrashesTheChildIsQuarantinedForItsWorkspaceOnly(t *testing.T) {
	f, cv := convFixture(t, convLabels)
	f.open(t, "w")
	f.open(t, "v")
	bad := "<html><body>" + crashMark + "</body></html>"
	f.wsPut(t, "w", "bad.html", bad)
	f.wsPut(t, "v", "bad.html", bad)
	f.wsPutBytes(t, "w", "good.docx", minutesDOCX(t, testPNG(t)))

	_, res := f.exec(t, f.ctx, map[string]any{"op": "extract", "path": "workspace:w:bad.html", "to": "workspace:w:out"})
	wantFileRefusal(t, "first", res, "crashed")
	if cv.extracts.Load() != 1 {
		t.Fatalf("extractions = %d", cv.extracts.Load())
	}
	_, res = f.exec(t, f.ctx, map[string]any{"op": "extract", "path": "workspace:w:bad.html"})
	wantFileRefusal(t, "again in w", res, "will not be tried again")
	if cv.extracts.Load() != 1 {
		t.Error("the quarantined file reached the worker again")
	}
	_, res = f.exec(t, f.ctx, map[string]any{"op": "extract", "path": "workspace:v:bad.html"})
	wantFileRefusal(t, "in v", res, "crashed")
	if cv.extracts.Load() != 2 {
		t.Error("the file was refused in another workspace too")
	}
	if _, res := f.exec(t, f.ctx, map[string]any{"op": "extract", "path": "workspace:w:good.docx", "to": "workspace:w:good"}); res.IsError {
		t.Errorf("the worker or the workspace did not survive: %s", res.Text)
	}
}

// The grant and the Workspace hooks refuse before any conversion starts:
// read on the source, write on the target, the hook on each.
func TestFileExtract_GrantsAndHooksRefuseBeforeAnyConversion(t *testing.T) {
	f, cv := convFixture(t, convLabels)
	f.open(t, "w")
	f.wsPutBytes(t, "w", "m.docx", minutesDOCX(t, testPNG(t)))
	none := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace = tools.WorkspacePolicyValue{} })
	readOnly := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace = tools.WorkspacePolicyValue{Read: true} })
	_, res := f.exec(t, none, map[string]any{"op": "extract", "path": "workspace:w:m.docx"})
	wantFileRefusal(t, "no grant", res, "does not allow read")
	_, res = f.exec(t, readOnly, map[string]any{"op": "extract", "path": "workspace:w:m.docx", "to": "workspace:w:out"})
	wantFileRefusal(t, "read only", res, "does not allow write")
	hooked := func(path string) context.Context {
		return tools.WithHookPreCheck(f.ctx, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
			var in struct{ Path string }
			_ = json.Unmarshal(input, &in)
			if name == "Workspace" && in.Path == path {
				return tools.Result{Text: "not this one", IsError: true}, false
			}
			return tools.Result{}, true
		})
	}
	_, res = f.exec(t, hooked("m.docx"), map[string]any{"op": "extract", "path": "workspace:w:m.docx"})
	wantFileRefusal(t, "source hook", res, "not this one")
	_, res = f.exec(t, hooked("out"), map[string]any{"op": "extract", "path": "workspace:w:m.docx", "to": "workspace:w:out"})
	wantFileRefusal(t, "target hook", res, "not this one")
	_, res = f.exec(t, hooked("out/m.md"), map[string]any{"op": "extract", "path": "workspace:w:m.docx", "to": "workspace:w:out"})
	wantFileRefusal(t, "target file hook", res, "not this one")
	_, res = f.exec(t, hooked("m.docx"), map[string]any{"op": "render", "path": "workspace:w:m.docx", "format": "md"})
	wantFileRefusal(t, "render source hook", res, "not this one")
	if n := cv.extracts.Load(); n != 0 || f.wsHas(t, "w", "out") {
		t.Errorf("a refused call converted (%d) or wrote", n)
	}
}

// A link as a source is refused: it is reported as a link and never read
// through, by the core's check and by the worker's own.
func TestFileExtract_ALinkAsASourceIsRefused(t *testing.T) {
	f, cv := convFixture(t, convLabels)
	f.open(t, "w")
	secret := filepath.Join(t.TempDir(), "secret.html")
	if err := os.WriteFile(secret, []byte("<p>secret</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(f.wsDir(t, "w"), "link.html")); err != nil {
		t.Fatal(err)
	}
	for _, in := range []map[string]any{
		{"op": "extract", "path": "workspace:w:link.html"},
		{"op": "render", "path": "workspace:w:link.html", "format": "md"},
	} {
		_, res := f.exec(t, f.ctx, in)
		wantFileRefusal(t, in["op"].(string), res, "link")
	}
	if cv.extracts.Load() != 0 {
		t.Error("a link was converted")
	}
}

// An HTML file in a workspace is read into Markdown on the worker and
// rendered as File op=render renders HTML; only the Markdown crosses.
func TestFileRender_AnHTMLFileInAWorkspaceIsReadOnItsWorker(t *testing.T) {
	f, cv := convFixture(t, convLabels)
	f.open(t, "w")
	f.wsPut(t, "w", "page.html", "<html><head><title>T</title></head><body><h1>Звіт</h1><p>Бюджет затверджено.</p></body></html>")
	before := f.crossed()
	out, res := f.exec(t, f.ctx, map[string]any{"op": "render", "path": "workspace:w:page.html", "format": "md", "to": "workspace:w:out/page.md"})
	if res.IsError {
		t.Fatal(res.Text)
	}
	if cv.extracts.Load() != 1 {
		t.Errorf("extractions on the worker = %d", cv.extracts.Load())
	}
	if _, byOp := crossedSince(before, f.crossed()); byOp["get"] != 0 || byOp["convert_render_html"] == 0 {
		t.Errorf("crossed %v", byOp)
	}
	md, ok := f.wsGet(t, "w", "out/page.md")
	if !ok || !strings.Contains(md, "Бюджет затверджено") || asStr(out["ref"]) != "workspace:w:out/page.md" {
		t.Errorf("rendered %q, answer %v", md, out)
	}
}

// A Document is rendered as DOCX, and as PDF where the typesetter is
// installed, into a workspace, its pictures' folder beside a Markdown
// render.
func TestFileRender_ADocumentIsRenderedIntoAWorkspace(t *testing.T) {
	f, _ := convFixture(t, convLabels)
	d, _, _ := documentFixture(t)
	f.file.Docs = d
	ctx := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Tools = append(p.Tools, "Document") })
	f.open(t, "w")
	// An in-process PDF render works in the inbox's folder, which a first
	// capture makes.
	inbox.Capture(ctx, inbox.Item{Name: "x.txt"}, []byte("x"))
	docID, _ := renderFixtureDoc(t, d, ctx)
	formats := []string{"docx", "md"}
	if _, err := exec.LookPath("typst"); err == nil {
		formats = append(formats, "pdf")
	}
	for _, format := range formats {
		out, res := f.exec(t, ctx, map[string]any{"op": "render", "document": docID, "format": format, "to": "workspace:w:out/"})
		if res.IsError {
			t.Fatalf("%s: %s", format, res.Text)
		}
		ref := asStr(out["ref"])
		if !strings.HasPrefix(ref, "workspace:w:out/") || !strings.HasSuffix(ref, "."+format) {
			t.Fatalf("%s: ref %q", format, ref)
		}
		data, ok := f.wsGet(t, "w", strings.TrimPrefix(ref, "workspace:w:"))
		if !ok || int64(len(data)) != int64(out["size"].(float64)) {
			t.Fatalf("%s: the file in the workspace: %d bytes, answer %v", format, len(data), out)
		}
		if format == "md" {
			refs, _ := out["image_refs"].([]any)
			if len(refs) != 1 || !f.wsHas(t, "w", strings.TrimPrefix(asStr(refs[0]), "workspace:w:")) {
				t.Errorf("md pictures: %v", out["image_refs"])
			}
		}
	}
}
