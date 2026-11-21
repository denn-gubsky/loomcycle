package builtin

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/files/filenames"
	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// Conversions of a workspace's file (RFC GW D34). The file is converted on
// its workspace's worker, in the conversion child that worker supervises;
// the core asks for the file's sha256 (its quarantine key), never for its
// bytes. A result written into a workspace on the same worker never leaves
// it; a result for the inbox, or for a workspace on another worker,
// streams back and is written here.

// wsConvertMaxImage bounds one image an extraction streams back, as the
// conversion dispatcher bounds a worker's.
const wsConvertMaxImage = 64 << 20

// wsConvertSource resolves a workspace source for a conversion: the
// workspace's read grant and hook, then a regular file within the per-file
// bound, with its sha256.
func (f *File) wsConvertSource(ctx context.Context, op string, r dataRef) (*workspaceLoc, workspace.FileInfo, tools.Result, bool) {
	l, res, ok := f.workspaceLoc(ctx, r, false, FileToolName)
	if !ok {
		return nil, workspace.FileInfo{}, prefixed(op, res), false
	}
	if res, ok := l.hook(ctx, wsHookRead); !ok {
		return nil, workspace.FileInfo{}, res, false
	}
	info, err := l.m.Stat(ctx, l.o, l.name, workspace.FStatArgs{Path: l.path, SHA256: true})
	if err != nil {
		return nil, workspace.FileInfo{}, prefixed(op, workspaceRefError(err, l.name)), false
	}
	switch {
	case info.Type == workspace.EntryLink:
		return nil, info, errValidation(fmt.Sprintf("%s: %s is a link, which is not followed", op, r.raw), ""), false
	case info.Type != workspace.EntryFile:
		return nil, info, errValidation(fmt.Sprintf("%s: %s is not a file", op, r.raw), ""), false
	case info.Size > inbox.MaxFileBytes(ctx):
		return nil, info, errValidation(fmt.Sprintf("%s: %s is %d bytes, over the %d a converted file may be", op, r.raw, info.Size, inbox.MaxFileBytes(ctx)), ""), false
	}
	return l, info, tools.Result{}, true
}

// wsConvertTarget resolves `to` as a folder in a workspace of the run the
// result is written into: the write grant, and the hook on the folder.
func (f *File) wsConvertTarget(ctx context.Context, op, to string) (*workspaceLoc, tools.Result, bool) {
	r, err := parseDataRef(strings.TrimSpace(to))
	if err != nil || r.kind != refWorkspace {
		return nil, errValidation(op+": to must be a folder in a workspace (workspace:<name>:<folder>), or left out to write to the inbox",
			"Copy the result to a volume afterwards with File op=copy."), false
	}
	l, res, ok := f.workspaceLoc(ctx, r, true, FileToolName)
	if !ok {
		return nil, prefixed(op, res), false
	}
	if l.path == "." {
		return nil, errValidation(op+": to names a workspace's root; name a folder in it", ""), false
	}
	if res, ok := l.hook(ctx, wsHookWrite); !ok {
		return nil, res, false
	}
	return l, tools.Result{}, true
}

// wsConvertError explains a conversion's failure on a workspace's worker.
func wsConvertError(op, shown string, err error, l *workspaceLoc) tools.Result {
	if errors.Is(err, lifecycle.ErrQuarantined) {
		return errBusiness(fmt.Sprintf("%s: %s crashed this workspace's file converter and will not be tried again in this workspace for now", op, shown),
			"The file may be damaged or built to exhaust memory. Try a different copy of it, or ask an operator.")
	}
	var oe *dispatch.OpError
	if errors.As(err, &oe) {
		msg := op + ": " + oe.Detail
		switch oe.Class {
		case workspace.ClassHelperMissing:
			return errBusiness(op+": "+workspace.ClassHelperMissing+": "+oe.Detail, "Ask an operator to install the helper where the workspace's worker runs.")
		case workspace.ClassConversionUnsupported:
			return errValidation(msg, "Extract reads DOCX, DOC, ODT, XLSX, PPTX, PDF, HTML, EPUB, FB2, MOBI, AZW3, PalmDOC, CHM, RTF, CSV and TSV, text, Markdown, captions and images; a recording's text is its speech (op=transcribe).")
		case workspace.ClassConversionFailed:
			return errValidation(msg, "Check the file is not damaged, or that its type is the one its name says.")
		case workspace.ClassConversionBusy, workspace.ClassSourceChanged:
			return errTransient(msg, "Try again.")
		}
	}
	return prefixed(op, workspaceRefError(err, l.name))
}

// newImageMark is a random mark for an image's place in streamed Markdown.
func newImageMark() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "loomcycle-image-" + hex.EncodeToString(b[:]) + "-"
}

// wsStream runs a streaming conversion and hands each entry of the tar it
// streams back to entry, in order. The conversion runs in its own goroutine
// writing into a pipe, so the result passes through the core an entry at a
// time.
func wsStream(ctx context.Context, l *workspaceLoc, req lifecycle.ConvertRequest, entry func(h *tar.Header, r io.Reader) error) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	safego.Go(sctx, "file.workspace_convert", func() {
		// Failure until the conversion says otherwise: a recovered panic
		// must still end the reader.
		err := errors.New("the workspace conversion stopped")
		defer func() { pw.CloseWithError(err); done <- err }()
		err = l.m.Convert(sctx, l.o, req, pw, nil)
		if err == nil {
			err = io.EOF
		}
	})
	tr := tar.NewReader(pr)
	var rerr error
	for {
		h, err := tr.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				rerr = err
			}
			break
		}
		if err := entry(h, tr); err != nil {
			rerr = err
			break
		}
	}
	cancel()
	_ = pr.CloseWithError(errors.New("read ended"))
	cerr := <-done
	if cerr != nil && !errors.Is(cerr, io.EOF) && !errors.Is(cerr, context.Canceled) {
		return cerr
	}
	return rerr
}

// readEntry reads a tar entry of at most max bytes.
func readEntry(h *tar.Header, r io.Reader, max int64) ([]byte, error) {
	if h.Size < 0 || h.Size > max {
		return nil, fmt.Errorf("the worker sent %q of %d bytes, over its %d-byte bound", h.Name, h.Size, max)
	}
	return io.ReadAll(io.LimitReader(r, max))
}

// ---- extract ----

// extractWorkspace extracts a file in a workspace on its worker: into a
// folder of a workspace (to), or into the inbox as op=extract writes. OCR
// runs as for any file, its pages cut on the worker and its model called
// here (file_workspace_ocr.go).
func (f *File) extractWorkspace(ctx context.Context, in fileInput, r dataRef, pages []int, excerptChars int, oc ocrCall) (tools.Result, error) {
	if in.DryRun {
		return errValidation("extract: dry_run is not available for a file in a workspace", "Extract it without dry_run, or copy it into the inbox with File op=copy to estimate an OCR pass first."), nil
	}
	l, info, res, ok := f.wsConvertSource(ctx, "extract", r)
	if !ok {
		return res, nil
	}
	args := &workspace.ConvertExtractArgs{
		ConvertSource: workspace.ConvertSource{Path: l.path, SHA256: info.SHA256, MaxBytes: inbox.MaxFileBytes(ctx)},
		Images:        in.Images == nil || *in.Images, MaxRows: in.MaxRows, InferHeadings: in.Structure != nil && in.Structure.InferHeadings,
		Pages: pages, MaxChars: in.MaxChars, MaxMarkdown: fileExtractMaxMarkdown,
	}
	var ans workspace.ConvertExtractAnswer
	var md string
	var pass *ocrPass
	out := map[string]any{"source": r.raw}
	if strings.TrimSpace(in.To) != "" {
		to, res, ok := f.wsConvertTarget(ctx, "extract", in.To)
		if !ok {
			return res, nil
		}
		// The files the extraction writes are judged too: the Markdown,
		// named as the worker names it, and the pictures' folder.
		for _, p := range []string{extractSlug(l.path) + ".md", "images"} {
			if res, ok := to.child(p).hook(ctx, wsHookWrite); !ok {
				return res, nil
			}
		}
		same, err := l.m.SameWorker(ctx, l.o, l.name, to.name)
		if err != nil {
			return prefixed("extract", workspaceRefError(err, to.name)), nil
		}
		if same {
			args.To = workspace.ConvertTarget{Path: to.path, Overwrite: in.Overwrite}
			err := l.m.Convert(ctx, l.o, lifecycle.ConvertRequest{Op: workspace.OpConvertExtract, Args: args, Source: l.name, SHA256: info.SHA256, To: to.name}, nil, &ans)
			if err != nil {
				return wsConvertError("extract", r.raw, err, l), nil
			}
			out["ref"] = wsRef(to.name, ans.Markdown)
			refs := make([]string, 0, len(ans.Images))
			for _, p := range ans.Images {
				refs = append(refs, wsRef(to.name, p))
			}
			out["image_refs"] = refs
			if len(ans.NotKept) > 0 {
				out["images_not_kept"] = ans.NotKept
			}
			out["note"] = "the text was written in the workspace and not read here; read it with Workspace op=read, or import it with File op=import"
			pass, res, ok := f.wsOCRInto(ctx, oc, l, info, to, ans, r.raw)
			if !ok {
				return res, nil
			}
			return f.extractWorkspaceAnswer(ctx, in, ans, out, oc, pass, r.raw)
		}
		w := &wsFolderOut{l: to, overwrite: in.Overwrite}
		md, ans, res, ok = f.extractStreamed(ctx, l, info, args, r, w, f.wsOCRSplice(ctx, oc, l, info, r.raw, false, &pass))
		if !ok {
			return res, nil
		}
		out["ref"], out["image_refs"] = w.mdRef, w.refs
		if len(w.notKept) > 0 {
			out["images_not_kept"] = w.notKept
		}
	} else {
		if !inbox.Available(ctx) {
			return errBusiness("extract: this run cannot keep files, so there is nowhere to write the extracted text",
				"Pass to as a folder in a workspace, or ask an operator to configure the run's inbox."), nil
		}
		w := &wsInboxOut{ctx: ctx, origin: "extract:workspace:" + l.name}
		md, ans, res, ok = f.extractStreamed(ctx, l, info, args, r, w, f.wsOCRSplice(ctx, oc, l, info, r.raw, true, &pass))
		if !ok {
			return res, nil
		}
		out["ref"], out["image_refs"] = w.mdRef, keptImageRefs(w.sink.images, md)
		if len(w.sink.notKept) > 0 {
			out["images_not_kept"] = w.sink.notKept
		}
	}
	excerpt, truncated := cutRunes(md, excerptChars)
	out["excerpt"] = fileContentBlock(out["ref"].(string), excerpt, truncated)
	return f.extractWorkspaceAnswer(ctx, in, ans, out, oc, pass, r.raw)
}

// extractWorkspaceAnswer finishes extract's answer from the worker's: the
// report's fields, and the OCR pass (reported as an event, and as the
// call's failure when it stopped), or the pages OCR would read.
func (f *File) extractWorkspaceAnswer(ctx context.Context, in fileInput, ans workspace.ConvertExtractAnswer, out map[string]any, oc ocrCall, pass *ocrPass, src string) (tools.Result, error) {
	if pass != nil {
		ref, _ := out["ref"].(string)
		tools.EventEmitter(ctx)(pass.event(ctx, src, ref))
		if pass.failure() != nil {
			return ocrFailure(pass, ref), nil
		}
		out["ocr"] = pass.summary(oc.mode)
	}
	rep := wsReport(ans)
	if ans.Cut && !ans.Sampled {
		rep.Notes = append(rep.Notes, fmt.Sprintf("the text was cut at %d bytes", fileExtractMaxMarkdown))
	}
	if ans.Sampled {
		rep.Notes = append(rep.Notes, fmt.Sprintf("the text was cut at %d characters, as max_chars asked; the rest of the file was not read", in.MaxChars))
		out["truncated_at"] = in.MaxChars
	}
	base := map[string]any{}
	if b, err := json.Marshal(rep); err == nil {
		_ = json.Unmarshal(b, &base)
	}
	for k, v := range base {
		if _, set := out[k]; !set {
			out[k] = v
		}
	}
	out["media_type"] = ans.MediaType
	if rep.Format == "pdf" || rep.Format == "djvu" {
		if low := lowYieldPages(rep, oc.minChars); len(low) > 0 {
			out["low_yield_pages"] = low
		}
	}
	if pages := ocrPages(rep, oc); pass == nil && len(pages) > 0 && oc.mode != transcribe.OCRNever {
		// Only a DjVu book's pages are not read here: they are rendered
		// inside its extraction, which a workspace's worker does not ask
		// for yet.
		out["ocr"] = map[string]any{"pages_needing_ocr": pages,
			"note": "OCR of a DjVu book in a workspace is not available yet; copy it into the inbox with File op=copy and extract it there to read these pages"}
	}
	return okJSONRaw(out)
}

// wsReport is the extraction's report as the worker sent it, its page
// yields (which the report's JSON leaves out) included. What a worker
// sent is not trusted: a yield list past the pages a PDF read is cut.
func wsReport(ans workspace.ConvertExtractAnswer) extract.Report {
	var rep extract.Report
	_ = json.Unmarshal(ans.Report, &rep)
	var yields []extract.PageYield
	if json.Unmarshal(ans.PageYields, &yields) == nil {
		rep.PageYields = yields[:min(len(yields), extract.MaxPDFPages)]
	}
	return rep
}

// extractOut is where a streamed extraction's images and Markdown go.
type extractOut interface {
	// image keeps one image and returns the link the Markdown gets ("" when
	// it was not kept).
	image(ctx context.Context, img extract.Image) (string, error)
	// text keeps the Markdown, named after slug.
	text(ctx context.Context, slug, md string) (tools.Result, bool)
	abort(ctx context.Context)
}

// extractSplice writes OCR text into a streamed extraction's Markdown
// before it is kept (wsOCRSplice); it sees the Markdown as the worker sent
// it, so the page offsets hold.
type extractSplice func(md string, ans workspace.ConvertExtractAnswer) (string, tools.Result, bool)

// extractStreamed runs the extraction with its result streamed back, and
// writes it through w, after splice.
func (f *File) extractStreamed(ctx context.Context, l *workspaceLoc, info workspace.FileInfo, args *workspace.ConvertExtractArgs, r dataRef, w extractOut, splice extractSplice) (string, workspace.ConvertExtractAnswer, tools.Result, bool) {
	var ans workspace.ConvertExtractAnswer
	args.ImageMark = newImageMark()
	links := map[string]string{}
	var md string
	var gotAnswer bool
	// The images' share is the inbox's budget, at most 1 GiB (min is not
	// used: a test file of this package declares its own).
	images := inbox.Remaining(ctx)
	if images > 1<<30 || images < 0 {
		images = 1 << 30
	}
	maxOut := int64(fileExtractMaxMarkdown) + 2<<20 + images
	err := wsStream(ctx, l, lifecycle.ConvertRequest{Op: workspace.OpConvertExtract, Args: args, Source: l.name, SHA256: info.SHA256, MaxOutput: maxOut},
		func(h *tar.Header, er io.Reader) error {
			switch {
			case strings.HasPrefix(h.Name, workspace.ConvertTarImage):
				data, err := readEntry(h, er, wsConvertMaxImage)
				if err != nil {
					return err
				}
				p := h.PAXRecords
				page, _ := strconv.Atoi(p[workspace.ConvertPAXPage])
				slide, _ := strconv.Atoi(p[workspace.ConvertPAXSlide])
				link, err := w.image(ctx, extract.Image{Name: p[workspace.ConvertPAXName], MediaType: p[workspace.ConvertPAXType], Alt: p[workspace.ConvertPAXAlt],
					Page: page, Slide: slide, Data: data})
				if err != nil {
					return err
				}
				links[args.ImageMark+strings.TrimPrefix(h.Name, workspace.ConvertTarImage)+workspace.ImageMarkEnd] = link
			case h.Name == workspace.ConvertTarText:
				data, err := readEntry(h, er, int64(fileExtractMaxMarkdown)+1<<20)
				if err != nil {
					return err
				}
				md = string(data)
			case h.Name == workspace.ConvertTarAnswer:
				data, err := readEntry(h, er, 1<<20)
				if err != nil {
					return err
				}
				if err := json.Unmarshal(data, &ans); err != nil {
					return errors.New("the worker's answer is not JSON")
				}
				gotAnswer = true
			default:
				return fmt.Errorf("the worker sent an unexpected entry %q", h.Name)
			}
			return nil
		})
	if err == nil && !gotAnswer {
		err = errors.New("the worker's stream ended before its answer")
	}
	if err != nil {
		w.abort(ctx)
		var oe *dispatch.OpError
		if errors.As(err, &oe) || errors.Is(err, lifecycle.ErrQuarantined) || errors.Is(err, lifecycle.ErrNotFound) || errors.Is(err, lifecycle.ErrNotReady) {
			return "", ans, wsConvertError("extract", r.raw, err, l), false
		}
		return "", ans, errTransient("extract: "+err.Error(), "Try again."), false
	}
	md, res, ok := splice(md, ans)
	if !ok {
		w.abort(ctx)
		return "", ans, res, false
	}
	// Every image's mark becomes the reference it was kept at; a mark of
	// an image not kept leaves an empty link, as a sink that keeps nothing
	// does.
	for mark, link := range links {
		md = strings.ReplaceAll(md, mark, link)
	}
	if ans.Cut || ans.Sampled {
		md += "\n\n[the rest of the document was not extracted]"
	}
	if res, ok := w.text(ctx, extractSlug(l.path), md); !ok {
		w.abort(ctx)
		return "", ans, res, false
	}
	return md, ans, tools.Result{}, true
}

// extractSlug is the name an extraction's Markdown takes after its
// source, as the worker names it.
func extractSlug(p string) string {
	slug := inbox.SanitizeName(strings.TrimSuffix(path.Base(p), path.Ext(p)))
	if slug == "" {
		return "document"
	}
	return slug
}

// wsInboxOut keeps a streamed extraction in the inbox, as op=extract keeps
// one of an inbox file: images under extracted/<slug>/, the text as
// extracted/<slug>.md.
type wsInboxOut struct {
	ctx    context.Context
	origin string
	sink   *extractSink
	mdRef  string
}

func (o *wsInboxOut) image(ctx context.Context, img extract.Image) (string, error) {
	if o.sink == nil {
		o.sink = &extractSink{ctx: ctx, origin: o.origin}
	}
	return o.sink.Image(img)
}

func (o *wsInboxOut) text(ctx context.Context, slug, md string) (tools.Result, bool) {
	if o.sink == nil {
		o.sink = &extractSink{ctx: ctx, origin: o.origin}
	}
	kept := inbox.Capture(ctx, inbox.Item{Name: slug + ".md", MediaType: "text/markdown", Origin: o.origin, Dir: inbox.DirExtracted}, []byte(md))
	if !kept.Kept {
		return errBusiness("extract: the extracted text could not be kept: "+kept.Stub, "Free inbox space with op=rm or op=clear, then try again."), false
	}
	o.mdRef = kept.Record.Ref
	return tools.Result{}, true
}

// abort leaves the images kept: the inbox keeps a capture as it lands, as
// op=extract's own sink does.
func (o *wsInboxOut) abort(context.Context) {}

// wsFolderOut writes a streamed extraction into a folder of a workspace on
// another worker: a stage there, committed at the folder when the text is
// in, discarded on any failure.
type wsFolderOut struct {
	l         *workspaceLoc
	overwrite bool
	stage     string
	taken     map[string]bool
	refs      []string
	notKept   []string
	mdRef     string
}

func (o *wsFolderOut) ensureStage(ctx context.Context) error {
	if o.stage != "" {
		return nil
	}
	st, err := o.l.m.Stage(ctx, o.l.o, o.l.name)
	if err != nil {
		return err
	}
	o.stage, o.taken = st, map[string]bool{}
	return nil
}

func (o *wsFolderOut) put(ctx context.Context, rel string, data []byte) error {
	_, err := o.l.m.PutFile(ctx, o.l.o, o.l.name, workspace.PutArgs{Path: rel, Size: int64(len(data)), Stage: o.stage}, bytes.NewReader(data))
	return err
}

func (o *wsFolderOut) image(ctx context.Context, img extract.Image) (string, error) {
	if err := o.ensureStage(ctx); err != nil {
		return "", err
	}
	name := filenames.WithExtension(filenames.SanitizeName(img.Name), img.MediaType)
	base, ext := name, filenames.FileExt(name)
	for i := 2; o.taken[name]; i++ {
		name = strings.TrimSuffix(base, ext) + "-" + strconv.Itoa(i) + ext
	}
	o.taken[name] = true
	rel := path.Join("images", name)
	if err := o.put(ctx, rel, img.Data); err != nil {
		if dispatch.ClassOf(err) == workspace.ClassOverQuota {
			return "", err
		}
		if len(o.notKept) < 10 {
			o.notKept = append(o.notKept, img.Name+": "+err.Error())
		}
		return "", nil
	}
	o.refs = append(o.refs, wsRef(o.l.name, path.Join(o.l.path, rel)))
	return rel, nil
}

func (o *wsFolderOut) text(ctx context.Context, slug, md string) (tools.Result, bool) {
	if err := o.ensureStage(ctx); err != nil {
		return prefixed("extract", workspaceRefError(err, o.l.name)), false
	}
	if err := o.put(ctx, slug+".md", []byte(md)); err != nil {
		return prefixed("extract", workspaceRefError(err, o.l.name)), false
	}
	if _, err := o.l.m.Commit(ctx, o.l.o, o.l.name, workspace.CommitArgs{Stage: o.stage, Path: o.l.path, Overwrite: o.overwrite}); err != nil {
		return prefixed("extract", workspaceRefError(err, o.l.name)), false
	}
	o.stage = ""
	o.mdRef = wsRef(o.l.name, path.Join(o.l.path, slug+".md"))
	return tools.Result{}, true
}

func (o *wsFolderOut) abort(ctx context.Context) {
	if o.stage == "" {
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = o.l.m.Discard(dctx, o.l.o, o.l.name, o.stage)
	o.stage = ""
}

// ---- render ----

// workspaceMarkdown reads the Markdown a render of a workspace file
// typesets: an HTML file is read into Markdown on its worker, a Markdown
// file is read through a bounded stream (no conversion).
func (f *File) workspaceMarkdown(ctx context.Context, r dataRef) (string, string, tools.Result, bool) {
	l, info, res, ok := f.wsConvertSource(ctx, "render", r)
	if !ok {
		return "", "", res, false
	}
	var buf bytes.Buffer
	if ext := strings.ToLower(path.Ext(l.path)); ext == ".html" || ext == ".htm" {
		args := &workspace.ConvertRenderHTMLArgs{ConvertSource: workspace.ConvertSource{Path: l.path, SHA256: info.SHA256, MaxBytes: inbox.MaxFileBytes(ctx)},
			MaxMarkdown: fileExtractMaxMarkdown}
		err := l.m.Convert(ctx, l.o, lifecycle.ConvertRequest{Op: workspace.OpConvertRenderHTML, Args: args, Source: l.name, SHA256: info.SHA256,
			MaxOutput: fileExtractMaxMarkdown}, &buf, nil)
		if err != nil {
			return "", "", wsConvertError("render", r.raw, err, l), false
		}
		return buf.String(), l.path, tools.Result{}, true
	}
	if info.Size > fileImportMaxBytes {
		return "", "", errValidation(fmt.Sprintf("render: %s is %d bytes, over the %d MiB a Markdown file may be", r.raw, info.Size, fileImportMaxBytes>>20), ""), false
	}
	if _, err := l.m.GetFile(ctx, l.o, l.name, workspace.GetArgs{Path: l.path}, &buf, fileImportMaxBytes); err != nil {
		return "", "", prefixed("render", workspaceRefError(err, l.name)), false
	}
	if !utf8.Valid(buf.Bytes()) {
		return "", "", errValidation(fmt.Sprintf("render: %s is not UTF-8 text", r.raw), "Render a Markdown or HTML file."), false
	}
	return buf.String(), l.path, tools.Result{}, true
}
