package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/files/ocrsplice"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// The byte halves of OCR and speech-to-text on a workspace's file (RFC GW
// D34). The core calls the model, with a key this process never holds; this
// process has its conversion child cut and render the pages, or convert the
// recording, and sends back only what the model reads. md_splice then
// writes the core's OCR text into Markdown that never left the workspace.

// maxUnit bounds what a caller may ask one OCR unit to be: the largest page
// image the core takes back from a conversion.
const maxUnit = 64 << 20

// maxRasterizePages bounds the pages one unit cuts.
const maxRasterizePages = extract.MaxPDFPages

// rasterizeOp sends the unit OCR reads for in.Pages: those pages cut out as
// one PDF, the one page rendered to a PNG (Render), or an image itself.
func (w *worker) rasterizeOp(ctx context.Context, real string, in workspace.ConvertRasterizeArgs, out *chunkWriter) answer {
	if in.MaxUnit <= 0 || in.MaxUnit > maxUnit {
		return refuse(workspace.ClassInvalid, "max_unit must be from 1 to %d", maxUnit)
	}
	if len(in.Pages) == 0 || len(in.Pages) > maxRasterizePages || (in.Render && len(in.Pages) != 1) {
		return refuse(workspace.ClassInvalid, "name 1 to %d pages, and exactly one to render", maxRasterizePages)
	}
	for _, p := range in.Pages {
		if p < 1 || p > maxRasterizePages {
			return refuse(workspace.ClassInvalid, "page %d is not from 1 to %d", p, maxRasterizePages)
		}
	}
	src, done, a := openSource(real, in.ConvertSource)
	if a != nil {
		return *a
	}
	defer done()
	what := fmt.Sprintf("%q", in.Path)
	switch {
	case strings.HasPrefix(src.mt, "image/"):
		// An image is the page: what the model reads is the file itself.
		if in.Render || len(in.Pages) != 1 || in.Pages[0] != 1 {
			return refuse(workspace.ClassInvalid, "an image has one page, sent as it is")
		}
		if src.size > in.MaxUnit {
			return refuse(workspace.ClassUnitTooLarge, "the image is %d bytes; one OCR request takes at most %d", src.size, in.MaxUnit)
		}
		if _, err := io.Copy(out, io.NewSectionReader(src.f, 0, src.size)); err != nil {
			return refuse(workspace.ClassFailed, "sending the image: %v", err)
		}
		return answer{}
	case src.mt != "application/pdf":
		return refuse(workspace.ClassConversionUnsupported, "OCR here reads the pages of a PDF or an image, not %s", src.mt)
	}
	need := []string{wire.LabelPDF}
	if in.Render {
		need = append(need, wire.LabelRender)
	}
	if a := w.needLabels(need); a != nil {
		return *a
	}
	cut, err := w.opt.Convert.SelectPages(ctx, src.f, src.size, in.Pages)
	if err != nil {
		return convertError(err, what)
	}
	unit := cut
	if in.Render {
		if unit, err = w.opt.Convert.RenderPage(ctx, cut, in.Language); err != nil {
			return convertError(err, fmt.Sprintf("page %d of %s", in.Pages[0], what))
		}
	}
	if int64(len(unit)) > in.MaxUnit {
		return refuse(workspace.ClassUnitTooLarge, "the pages are %d bytes; one OCR request takes at most %d", len(unit), in.MaxUnit)
	}
	if _, err := out.Write(unit); err != nil {
		return refuse(workspace.ClassFailed, "sending the pages: %v", err)
	}
	return answer{}
}

// isRecording is a type speech-to-text reads, as sttpass.IsRecording
// says; the worker does not import the transcription packages.
func isRecording(mt string) bool {
	return strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/") || mt == "application/ogg"
}

// mediaOp converts a recording into the speech-to-text WAV in the
// conversion child and streams the WAV back. The core bounds it (MaxOutput).
func (w *worker) mediaOp(ctx context.Context, real string, in workspace.ConvertMediaArgs, out *chunkWriter) answer {
	src, done, a := openSource(real, in.ConvertSource)
	if a != nil {
		return *a
	}
	defer done()
	if !isRecording(src.mt) {
		return refuse(workspace.ClassConversionUnsupported, "%q is not an audio or video file (%s)", in.Path, src.mt)
	}
	if a := w.needLabels([]string{wire.LabelMedia}); a != nil {
		return *a
	}
	wav, err := w.opt.Convert.ConvertMedia(ctx, src.f, src.size, src.mt, in.Language)
	if err != nil {
		return convertError(err, fmt.Sprintf("%q", in.Path))
	}
	defer wav.Close()
	if _, err := io.Copy(out, io.NewSectionReader(wav, 0, wav.Size())); err != nil {
		return refuse(workspace.ClassFailed, "sending the audio: %v", err)
	}
	return answer{}
}

// mdSpliceOp writes the OCR text of its input into the Markdown file at
// in.Path, which must still hash to in.SHA256 (the page offsets are its):
// the new file goes into a stage, counted against the quota, and replaces
// the old one in one rename. On any failure the file is as it was.
func (w *worker) mdSpliceOp(ctx context.Context, id, real string, raw, input []byte) answer {
	var in workspace.MDSpliceArgs
	if a := decodeArgs(raw, &in); a.class != "" {
		return a
	}
	var body workspace.MDSpliceInput
	if err := json.Unmarshal(input, &body); err != nil {
		return refuse(workspace.ClassInvalid, "md_splice takes its pages as JSON input")
	}
	if len(body.Pages) == 0 || !oneLineComment(body.Header) {
		return refuse(workspace.ClassInvalid, "md_splice takes at least one page and a one-line comment as its header")
	}
	rel, bad := opPath(in.Path, false)
	if bad != nil {
		return *bad
	}
	dir := filepath.ToSlash(filepath.Dir(rel))
	if dir == "." {
		return refuse(workspace.ClassInvalid, "md_splice writes a Markdown file in a folder of the workspace")
	}
	src, done, a := openSource(real, workspace.ConvertSource{Path: in.Path, SHA256: in.SHA256, MaxBytes: maxMarkdown})
	if a != nil {
		return *a
	}
	data, err := io.ReadAll(io.NewSectionReader(src.f, 0, src.size))
	done()
	if err != nil {
		return refuse(workspace.ClassFailed, "reading %q: %v", in.Path, err)
	}
	md := string(data)
	starts := body.PageStarts
	if body.Image {
		starts = ocrsplice.ImageStarts(md)
	}
	spliced := []byte(ocrsplice.Splice(md, starts, body.Pages, body.Header))
	t, a := w.openTarget(workspace.ConvertTarget{Workspace: id, Path: dir, Overwrite: true})
	if a != nil {
		return *a
	}
	defer t.close()
	if a := w.stageWrite(ctx, t, filepath.Base(rel), spliced); a != nil {
		return *a
	}
	usage, a := w.commitTarget(ctx, t)
	if a != nil {
		return *a
	}
	sum := sha256.Sum256(spliced)
	return jsonAnswer(workspace.MDSpliceAnswer{Size: int64(len(spliced)), SHA256: hex.EncodeToString(sum[:]), Usage: usage})
}

// oneLineComment reports whether h is one HTML comment on one line, so a
// header cannot add text to the Markdown or end a comment early.
func oneLineComment(h string) bool {
	return len(h) >= 7 && !strings.ContainsAny(h, "\r\n") && strings.HasPrefix(h, "<!--") && strings.HasSuffix(h, "-->") &&
		!strings.Contains(h[4:len(h)-3], "--")
}
