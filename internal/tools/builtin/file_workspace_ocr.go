package builtin

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/files/ocrsplice"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/sttpass"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// OCR and speech-to-text of a workspace's file (RFC GW D34). The model call
// needs a provider key, and a key never reaches a worker, so the work is
// split: the worker cuts the pages OCR reads, or converts the recording, in
// its conversion child, and sends back only those bytes; the core calls the
// model exactly as for a file in the inbox (the same plan, the agent's
// ocr_model, the run's page and minute caps, the ledger). Markdown the
// worker wrote into a workspace stays there: the core sends only the OCR
// text (md_splice). No op's arguments name the provider or the model, and
// no provider key or credential ever reaches a worker. The provenance line
// written into the workspace does name the provider and the model, as a
// result in the inbox does (a model's name is not a secret).

// ---- OCR ----

// wsOCRUnits is the unit source of a file in a workspace: each unit is cut
// (or rendered, or for an image sent as it is) on the workspace's worker,
// within the bound a unit of the plan may be.
func (f *File) wsOCRUnits(l *workspaceLoc, info workspace.FileInfo, mt string, plan transcribe.Plan, language string) ocrUnitSource {
	return func(ctx context.Context, chunk []int) (ocrUnit, [][]int, error) {
		if extract.IsDjVu(mt, "") {
			return ocrUnit{}, nil, errDjVuOCR
		}
		image := mt != "application/pdf"
		render := !image && !plan.WholePDF
		limit := int64(wsConvertMaxImage)
		if !render && plan.MaxUnitBytes > 0 && plan.MaxUnitBytes < limit {
			// As for a file here: a rendered page is not held to the
			// request size, a cut PDF and an image are.
			limit = plan.MaxUnitBytes
		}
		args := &workspace.ConvertRasterizeArgs{ConvertSource: workspace.ConvertSource{Path: l.path, SHA256: info.SHA256, MaxBytes: info.Size},
			Pages: chunk, Render: render, Language: language, MaxUnit: limit}
		var buf bytes.Buffer
		err := l.m.Convert(ctx, l.o, lifecycle.ConvertRequest{Op: workspace.OpConvertRasterize, Args: args, Source: l.name, SHA256: info.SHA256, MaxOutput: limit}, &buf, nil)
		switch {
		case err == nil:
		case dispatch.ClassOf(err) == workspace.ClassUnitTooLarge && image:
			return ocrUnit{}, nil, fmt.Errorf("the image is over the %d bytes one OCR request takes", limit)
		case dispatch.ClassOf(err) == workspace.ClassUnitTooLarge && len(chunk) > 1:
			half := len(chunk) / 2
			return ocrUnit{}, [][]int{chunk[:half:half], chunk[half:]}, nil
		case dispatch.ClassOf(err) == workspace.ClassUnitTooLarge:
			return ocrUnit{}, nil, fmt.Errorf("page %d alone is over the %d bytes one OCR request takes", chunk[0], limit)
		case render:
			return ocrUnit{}, nil, fmt.Errorf("rendering page %d on the workspace's worker: %w", chunk[0], err)
		default:
			return ocrUnit{}, nil, fmt.Errorf("cutting page %s out of the PDF on the workspace's worker: %w", pageListText(chunk), err)
		}
		umt := "application/pdf"
		switch {
		case image:
			umt = mt
		case render:
			umt = "image/png"
		}
		return ocrUnit{data: buf.Bytes(), mediaType: umt, pages: chunk}, nil, nil
	}
}

// wsOCRPass runs OCR over the pages of a workspace's file that need it, as
// the extraction's answer reports them. nil with ok is no pass: no page
// needs one, OCR is off, or the file is a DjVu book (its pages are rendered
// inside an extraction, which the worker is not asked for). A plan the
// engine refuses is the call's refusal.
func (f *File) wsOCRPass(ctx context.Context, oc ocrCall, l *workspaceLoc, info workspace.FileInfo, ans workspace.ConvertExtractAnswer, src string) (*ocrPass, tools.Result, bool) {
	if !oc.active() || !f.ocrAvailable() {
		return nil, tools.Result{}, true
	}
	rep := wsReport(ans)
	if rep.Format == "djvu" {
		return nil, tools.Result{}, true
	}
	pages := ocrPages(rep, oc)
	if ans.Sampled && rep.Format == "pdf" {
		// The page whose text max_chars refused has a yield, but none of
		// its text was written: OCR has no place for it.
		var written []int
		for _, n := range pages {
			if _, ok := ans.PageStarts[n]; ok {
				written = append(written, n)
			}
		}
		pages = written
	}
	if len(pages) == 0 {
		return nil, tools.Result{}, true
	}
	plan, err := f.OCR.Engine.Plan(ctx, ans.MediaType, oc.model)
	if err != nil {
		return nil, errBusiness("extract: OCR cannot read "+src+": "+err.Error(), "An operator configures the OCR model (transcription.ocr, or the agent's transcription.ocr_model)."), false
	}
	return f.runOCR(ctx, oc, plan, ans.MediaType, pages, f.wsOCRUnits(l, info, ans.MediaType, plan, oc.language)), tools.Result{}, true
}

// wsOCRSplice is the splice of a streamed extraction: the pass runs once
// the text layer is in, and its text goes in place of its pages. The pass
// is left in *pass.
func (f *File) wsOCRSplice(ctx context.Context, oc ocrCall, l *workspaceLoc, info workspace.FileInfo, src string, pass **ocrPass) extractSplice {
	return func(md string, ans workspace.ConvertExtractAnswer) (string, tools.Result, bool) {
		p, res, ok := f.wsOCRPass(ctx, oc, l, info, ans, src)
		if !ok || p == nil {
			return md, res, ok
		}
		*pass = p
		if len(p.done) == 0 {
			return md, tools.Result{}, true
		}
		starts := ans.PageStarts
		if isImageReport(ans) {
			starts = ocrsplice.ImageStarts(md)
		}
		return ocrsplice.Splice(md, starts, p.text, p.header()), tools.Result{}, true
	}
}

func isImageReport(ans workspace.ConvertExtractAnswer) bool {
	var rep struct{ Format string }
	_ = json.Unmarshal(ans.Report, &rep)
	return rep.Format == "image"
}

// wsOCRInto runs the pass for an extraction the worker wrote into a
// workspace (to) and has the worker splice the text into that Markdown:
// only the text crosses. A splice that fails after the model read the
// pages leaves the pass partial-at-best, reported as its failure.
func (f *File) wsOCRInto(ctx context.Context, oc ocrCall, l *workspaceLoc, info workspace.FileInfo, to *workspaceLoc, ans workspace.ConvertExtractAnswer, src string) (*ocrPass, tools.Result, bool) {
	p, res, ok := f.wsOCRPass(ctx, oc, l, info, ans, src)
	if !ok {
		hint := ""
		if res.Error != nil {
			hint = res.Error.Description
		}
		return nil, errBusiness(fmt.Sprintf("%s; the text layer was written to %s", res.Text, wsRef(to.name, ans.Markdown)), hint), false
	}
	if p == nil || len(p.done) == 0 {
		return p, tools.Result{}, true
	}
	in := workspace.MDSpliceInput{Header: p.header(), PageStarts: ans.PageStarts, Image: isImageReport(ans), Pages: p.text}
	if _, err := l.m.SpliceMarkdown(ctx, l.o, to.name, workspace.MDSpliceArgs{Path: ans.Markdown, SHA256: ans.MarkdownSHA256}, in); err != nil {
		// The pages were read and paid for; the Markdown keeps its text
		// layer. done stays, so the event counts what was spent.
		if p.err == nil && p.callErr == nil && p.capErr == nil {
			p.err = fmt.Errorf("the OCR text could not be written into the workspace's Markdown: %v", workspaceRefError(err, to.name).Text)
		}
	}
	return p, tools.Result{}, true
}

// ---- speech-to-text ----

// wsTranscribeTarget resolves transcribe's `to` in a workspace: a file, or
// a folder (a path ending in /, the root, or a folder there) where the
// transcript takes its recording's name. The write grant and the hook on
// the final path are checked before anything is converted.
func (f *File) wsTranscribeTarget(ctx context.Context, to, slug string) (*workspaceLoc, tools.Result, bool) {
	r, err := parseDataRef(to)
	if err != nil || r.kind != refWorkspace {
		return nil, errValidation("transcribe: to is a workspace file (workspace:<name>:<path>) or a name in the inbox", ""), false
	}
	l, res, ok := f.workspaceLoc(ctx, r, true, FileToolName)
	if !ok {
		return nil, prefixed("transcribe", res), false
	}
	e, res, ok := l.Stat(ctx)
	if !ok {
		return nil, prefixed("transcribe", res), false
	}
	if strings.HasSuffix(r.path, "/") || l.path == "." || e.Type == workspace.EntryDir {
		l = l.child(slug + ".md")
	} else if path.Ext(l.path) != ".md" {
		l.path += ".md"
	}
	if res, ok := l.hook(ctx, wsHookWrite); !ok {
		return nil, res, false
	}
	return l, tools.Result{}, true
}

// transcribeWorkspace transcribes a recording in a workspace: the worker
// converts it to the WAV the pass cuts, the WAV is spooled here (on disk,
// never whole in memory), and the pass runs as for any recording. The
// transcript goes where call.wsTo says, or into the inbox.
func (f *File) transcribeWorkspace(ctx context.Context, call sttCall, r dataRef) (tools.Result, error) {
	if call.dryRun {
		return errValidation("transcribe: dry_run is not available for a file in a workspace", "Transcribe it without dry_run, or copy it into the inbox with File op=copy to estimate first."), nil
	}
	l, info, res, ok := f.wsConvertSource(ctx, "transcribe", r)
	if !ok {
		return res, nil
	}
	name := path.Base(l.path)
	slug := cmp.Or(call.out, inbox.SanitizeName(strings.TrimSuffix(name, path.Ext(name))), "recording")
	var to *workspaceLoc
	if call.wsTo != "" {
		if to, res, ok = f.wsTranscribeTarget(ctx, call.wsTo, slug); !ok {
			return res, nil
		}
	} else if !inbox.Available(ctx) {
		return errBusiness("transcribe: this run cannot keep files, so there is nowhere to write the transcript",
			"Pass to as a file in a workspace, or ask an operator to configure the run's inbox."), nil
	}
	spool, err := os.CreateTemp(cmp.Or(f.ScratchDir, os.TempDir()), "loomcycle-ws-wav-*")
	if err != nil {
		return errTransient("transcribe: no scratch space for the converted audio", "Try again."), nil
	}
	defer func() { spool.Close(); os.Remove(spool.Name()) }()
	args := &workspace.ConvertMediaArgs{ConvertSource: workspace.ConvertSource{Path: l.path, SHA256: info.SHA256, MaxBytes: info.Size}, Language: call.opts.Language}
	err = l.m.Convert(ctx, l.o, lifecycle.ConvertRequest{Op: workspace.OpConvertMedia, Args: args, Source: l.name, SHA256: info.SHA256, MaxOutput: sttpass.MaxSource}, spool, nil)
	if err != nil {
		return wsConvertError("transcribe", r.raw, err, l), nil
	}
	st, err := spool.Stat()
	if err != nil {
		return errTransient("transcribe: reading the converted audio failed", "Try again."), nil
	}
	deps := f.sttDeps()
	plan, err := sttpass.Prepare(ctx, deps, call.caps, spool, st.Size(), "audio/wav")
	if err != nil {
		return sttPrepareRefusal(r.raw, err), nil
	}
	if plan.Route != sttpass.RouteCut {
		return errBusiness("transcribe: the workspace's worker did not convert "+r.raw+" to a PCM WAV", "Ask an operator to check the worker's media helper (ffmpeg)."), nil
	}
	if len(plan.Pieces) == 0 {
		return errValidation(fmt.Sprintf("transcribe: %s holds no audio", r.raw), ""), nil
	}
	var pass *sttpass.Pass
	if run, tree, ok := f.sttBudgetsFor(ctx, call.caps); ok {
		pass = sttpass.Run(ctx, deps, plan, spool, call.opts, call.caps, []*transcribe.Budget{run, tree})
	} else {
		pass = sttpass.Failed(plan, errors.New("this run has no inbox to count its minutes against"))
	}
	// The recording reached the engine converted, on the worker.
	pass.Route = sttpass.RouteConvert
	keep := func(md string) (string, tools.Result, bool) {
		// A transcript into the inbox is captured; into a workspace it is
		// put through the core, as a copy's file is.
		if to == nil {
			return f.keepTranscript(ctx, slug, md, "extract:workspace:"+l.name)
		}
		w, res, ok := to.Create(ctx, int64(len(md)), strings.NewReader(md), call.overwrite)
		if !ok {
			return "", prefixed("transcribe", res), false
		}
		return w.Ref, tools.Result{}, true
	}
	// The recording's type as its name says: the worker typed it from its
	// bytes, which this process never reads.
	return f.finishSTT(ctx, call, pass, r, name, inbox.Sniff("", nil, name), keep)
}
