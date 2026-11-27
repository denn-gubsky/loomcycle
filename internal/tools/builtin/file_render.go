package builtin

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/files/render"
	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// File op=render (RFC GR §4.3): a Document — or a Markdown file — written as
// a file of another format, into the inbox (rendered/<name>.<ext>) or a
// volume of the run's. The model gets a reference, never the bytes; keep,
// upload and the inbox download route take the file on from there.
//
// The document is read through Document's own dispatch (document_render.go),
// as the caller, with every gate an export_md passes. The file is written by
// the conversion workers when the server has them (RFC GO), else here.

// renderInboxDir is the inbox folder renders are written to.
const renderInboxDir = "rendered"

func (f *File) renderOp(ctx context.Context, in fileInput) (tools.Result, error) {
	format := strings.ToLower(strings.TrimSpace(in.Format))
	switch {
	case format == "":
		return errValidation("render: missing required field: format", "Pass format: pdf, docx, odt, epub, html or md."), nil
	case !render.Supported(format):
		return errValidation(fmt.Sprintf("render: unknown format %q", in.Format), "Pass format: pdf, docx, odt, epub, html or md."), nil
	}
	sources := 0
	for _, s := range []string{in.Document, in.DocPath, in.Path} {
		if strings.TrimSpace(s) != "" {
			sources++
		}
	}
	if sources != 1 {
		return errValidation("render: pass exactly one of document, doc_path or path",
			"document is a document id, doc_path its Path-tree name, path a Markdown file in the inbox or a volume."), nil
	}
	if in.Chunk != "" && in.Path != "" {
		return errValidation("render: chunk names a part of a document, and path is a file", "Drop chunk, or render a document."), nil
	}
	switch in.PageSize {
	case "", "a4", "letter":
	default:
		return errValidation("render: page_size must be a4 or letter", ""), nil
	}
	switch in.Metadata {
	case "", "none":
	case "embed":
		if in.Path != "" {
			return errValidation("render: metadata embed records a document, and path is a file", "Render a document (document or doc_path), or drop metadata."), nil
		}
	default:
		return errValidation("render: metadata must be none or embed", ""), nil
	}
	dest, res, ok := renderDestination(ctx, f, in)
	if !ok {
		return res, nil
	}
	if res, ok := checkPageText(in.PageText); !ok {
		return res, nil
	}
	job := &renderJob{f: f, in: in, format: format, dest: dest, date: time.Now().UTC().Format("2006-01-02")}
	if res, ok := job.resolveStyle(ctx, true); !ok {
		if res.Text == "" {
			return tools.Result{}, ctx.Err()
		}
		return res, nil
	}
	if in.Path != "" {
		return job.fromFile(ctx)
	}
	return job.fromDocument(ctx)
}

// renderDest is where a render goes: the inbox (to empty), a volume file,
// a workspace file (ws), or (download) back to an off-run caller (Export).
type renderDest struct {
	toVolume bool
	to       dataRef
	download bool
	// ws is the workspace place to names, the write grant checked; the
	// hook judges the final path when it is known (deliver).
	ws *workspaceLoc
}

func renderDestination(ctx context.Context, f *File, in fileInput) (renderDest, tools.Result, bool) {
	if strings.TrimSpace(in.To) == "" {
		if !inbox.Available(ctx) {
			return renderDest{}, errBusiness("render: this run cannot keep files, so there is nowhere to write the file",
				"Pass to as a file in one of your read-write volumes, or ask an operator to configure the run's inbox."), false
		}
		return renderDest{}, tools.Result{}, true
	}
	to, err := parseDataRef(strings.TrimSpace(in.To))
	if err == nil && to.kind == refWorkspace {
		l, res, ok := f.workspaceLoc(ctx, to, true, FileToolName)
		if !ok {
			return renderDest{}, prefixed("render", res), false
		}
		return renderDest{to: to, ws: l}, tools.Result{}, true
	}
	if err != nil || to.kind != refVolume {
		return renderDest{}, errValidation("render: to must be a volume or workspace reference", "Pass to, e.g. volume:reports:2026/minutes.html or workspace:work:in/minutes.md, or leave it out to write to the inbox."), false
	}
	if to.volume == inbox.Name {
		return renderDest{}, errValidation("render: to is in the inbox; leave to out to write there", ""), false
	}
	return renderDest{toVolume: true, to: to}, tools.Result{}, true
}

// renderJob is one render call.
type renderJob struct {
	f      *File
	in     fileInput
	format string
	dest   renderDest
	date   string
	// actor overrides who the render log records (an off-run export names
	// the person); exported receives a download's file.
	actor    string
	exported *ExportedFile
	// renderErr is a download's render failure, for Export to classify (an
	// op answers it as a tool result instead).
	renderErr error
	// style is the house style the file is written in, and logo its
	// picture (render_style.go); styleNote says a named style was not used.
	style     *render.Style
	logo      []byte
	logoMT    string
	styleNote string
	// wsTarget is the workspace path a render to a workspace writes.
	wsTarget string
}

func (j *renderJob) options() render.Options {
	return render.Options{TOC: j.in.TOC, TitlePage: j.in.TitlePage, PageSize: j.in.PageSize}
}

func (j *renderJob) provenance() bool { return j.in.Provenance == nil || *j.in.Provenance }

// fromDocument renders a Document, read as Document op=export_md reads it.
func (j *renderJob) fromDocument(ctx context.Context) (tools.Result, error) {
	f, in := j.f, j.in
	if f.Docs == nil {
		return errBusiness("render: documents are not available here", "An operator must enable SQL Memory for the Document tool."), nil
	}
	scope := in.Scope
	if scope == "" {
		scope = "user"
	}
	switch scope {
	case "agent", "user", "tenant":
	default:
		return errValidation("render: scope must be agent, user or tenant", ""), nil
	}
	call := map[string]any{"op": "export_md", "scope": scope, "include_metadata": false}
	if in.Document != "" {
		call["document_id"] = in.Document
	} else {
		call["path"] = in.DocPath
	}
	if res, ok := fileDocumentReadGrant(ctx, call); !ok {
		return res, nil
	}
	raw, _ := json.Marshal(call)
	noMeta := false
	din := docInput{Op: "export_md", Scope: scope, DocumentID: in.Document, Path: in.DocPath, IncludeMetadata: &noMeta,
		render: &renderRequest{chunkID: in.Chunk, provenance: j.provenance(), language: in.Language, date: j.date,
			embed: in.Metadata == "embed", deliver: j.deliver}}
	res, err := f.Docs.executeInput(ctx, din, raw)
	if err == nil && res.IsError {
		res.Text = strings.Replace(res.Text, "export_md: ", "render: ", 1)
	}
	return res, err
}

// fileDocumentReadGrant refuses a render of a document the run could not
// export itself: without the Document tool, or when Document's pre-hooks
// deny the equivalent export_md.
func fileDocumentReadGrant(ctx context.Context, call map[string]any) (tools.Result, bool) {
	if !slices.Contains(tools.PolicyOf(ctx).Tools, "Document") {
		return errPermission("render needs the Document tool, which this run does not have", "Ask an operator to grant the tool."), false
	}
	call["via"] = FileToolName
	b, _ := json.Marshal(call)
	delete(call, "via")
	if res, ok := tools.HookPreCheck(ctx, "Document", b); !ok {
		return classified("render: refused by a Document hook: "+res.Text, tools.CategoryPermission, false, "Ask the operator about the hook."), false
	}
	return tools.Result{}, true
}

// fromFile renders a Markdown file: its first heading is the title and the
// rest its body. An HTML file is read into Markdown first, as op=extract
// reads it.
func (j *renderJob) fromFile(ctx context.Context) (tools.Result, error) {
	r, err := fileRef(j.in.Path)
	if err != nil {
		return errValidation("render: "+err.Error(), "Pass path, e.g. volume:inbox:extracted/report.md."), nil
	}
	if r.kind == refWorkspace {
		// A workspace's HTML file is read into Markdown on its worker (RFC
		// GW D34); its Markdown is read through a bounded stream.
		text, name, res, ok := j.f.workspaceMarkdown(ctx, r)
		if !ok {
			return res, nil
		}
		return j.fromMarkdown(ctx, text, name)
	}
	vfs, rel, res, ok := openRef(ctx, r, false)
	if !ok {
		return res, nil
	}
	var text string
	if ext := strings.ToLower(path.Ext(r.path)); ext == ".html" || ext == ".htm" {
		// The converter takes its own extraction slot for the parse.
		text, res, ok = j.htmlMarkdown(ctx, vfs, rel, r)
		vfs.Close()
		if !ok {
			return res, nil
		}
	} else {
		release, err := j.f.extractSlot(ctx)
		if err != nil {
			vfs.Close()
			return slotBusy(ctx, "render", j.f.slotWait)
		}
		defer release()
		b, res, ok := readMarkdown(vfs, rel, r)
		vfs.Close()
		if !ok {
			res.Text = strings.Replace(res.Text, "import: ", "render: ", 1)
			return res, nil
		}
		text = string(b)
	}
	return j.fromMarkdown(ctx, text, filepath.ToSlash(rel))
}

// fromMarkdown renders a Markdown text read from the file at name.
func (j *renderJob) fromMarkdown(ctx context.Context, text, name string) (tools.Result, error) {
	title, body := splitMarkdownTitle(text, fileTitle(path.Base(name)))
	bld := render.NewBuilder(title)
	bld.Doc.Date, bld.Doc.Language = j.date, j.in.Language
	bld.Pictures = extractedPictures(ctx)
	bld.Doc.Blocks = bld.Body(body)
	doc, rep := bld.Finish()
	return j.deliver(ctx, doc, rep, renderedSource{}, nil)
}

// htmlMarkdown reads an HTML file into Markdown through the converter, as
// op=extract does, keeping nothing in the inbox: an <img> becomes a link to
// its source, which a render then leaves out, as it never fetches.
func (j *renderJob) htmlMarkdown(ctx context.Context, vfs *fsops.VolumeFS, rel string, r dataRef) (string, tools.Result, bool) {
	fh, err := vfs.Open(rel)
	if err != nil {
		return "", errNotFound(fmt.Sprintf("render: no file at %s", r.raw), "List the folder with op=ls."), false
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errValidation(fmt.Sprintf("render: %s is not a file", r.raw), ""), false
	}
	sink := &htmlRenderSink{}
	rep, err := j.f.converter().Extract(ctx, "text/html", fh, info.Size(), sink, extract.Options{Name: path.Base(filepath.ToSlash(rel))})
	switch {
	case err == nil:
	case errors.Is(err, extract.ErrSinkFull):
		// Cut where the extraction stopped; the body cap may cut it again.
	case errors.Is(err, convert.ErrBusy):
		res, _ := slotBusy(ctx, "render", j.f.slotWait)
		return "", res, false
	case errors.Is(err, convert.ErrQuarantined):
		return "", errBusiness(fmt.Sprintf("render: %s crashed the file converter and will not be tried again here for now", r.raw),
			"The file may be damaged or built to exhaust memory. Try a different copy of it, or ask an operator."), false
	case errors.Is(err, convert.ErrNoWorkers):
		return "", errTransient("render: no file conversion worker is running on this server", "Try again in a minute; an operator can check the conversion workers."), false
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return "", errTransient(fmt.Sprintf("render: reading %s took longer than %s", r.raw, fileExtractTimeout), "The file is too large or complex to render here."), false
	case extract.IsRefused(err):
		return "", errValidation(fmt.Sprintf("render: %s was refused: %s", r.raw, strings.TrimPrefix(err.Error(), "extract: refused: ")), ""), false
	default:
		return "", errValidation(fmt.Sprintf("render: could not read %s as %s: %s", r.raw, cmp.Or(rep.Format, "html"), strings.TrimPrefix(err.Error(), "extract: ")),
			"Check the file is not damaged, or that it is HTML."), false
	}
	return sink.md.String(), tools.Result{}, true
}

// htmlRenderSink collects an HTML file's Markdown for a render, within the
// extracted text's cap; it keeps no pictures.
type htmlRenderSink struct{ md strings.Builder }

func (s *htmlRenderSink) Text(md string) error {
	if s.md.Len()+len(md) > fileExtractMaxMarkdown {
		return extract.ErrSinkFull
	}
	s.md.WriteString(md)
	return nil
}

func (s *htmlRenderSink) Image(extract.Image) (string, error) { return "", nil }

// extractedPictures reads the pictures a rendered Markdown file names that
// op=extract wrote, as File op=import does (readExtractedImage): the file may
// be anyone's text, so a path it names to any other file, in the inbox or a
// volume, is not followed. Another reference is not a volume's, and is left
// out (a web URL: a render never fetches).
func extractedPictures(ctx context.Context) render.PictureReader {
	return func(ref string, max int) ([]byte, bool, error) {
		if !strings.HasPrefix(ref, "volume:") {
			return nil, false, nil
		}
		r, err := parseDataRef(ref)
		if err != nil || !isExtractedRef(r) {
			return nil, true, errors.New("only pictures op=extract wrote are read")
		}
		b, err := readVolumeBytes(ctx, r, int64(max))
		return b, true, err
	}
}

// splitMarkdownTitle takes a leading "# " heading off a Markdown text as its
// title; without one the title is fallback.
func splitMarkdownTitle(md, fallback string) (string, string) {
	trimmed := strings.TrimLeft(md, "\ufeff \t\r\n")
	line, rest, _ := strings.Cut(trimmed, "\n")
	if t, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "# "); ok && strings.TrimSpace(strings.Trim(t, "#")) != "" {
		return strings.TrimSpace(strings.TrimRight(strings.TrimSpace(t), "#")), rest
	}
	return fallback, md
}

// deliver writes the render tree as a file, logs it when it came from a
// document (log non-nil), and answers the op.
func (j *renderJob) deliver(ctx context.Context, doc *render.Doc, rep render.Report, src renderedSource, log *renderLog) (tools.Result, error) {
	rep.Format = j.format
	j.attachStyle(doc, &rep)
	// Diagrams are drawn before anything is hashed or bundled: the render
	// log's hash covers their pictures, and every writer sees a picture.
	if err := render.DrawDiagrams(ctx, doc, j.format, j.drawDiagram, &rep); err != nil {
		return tools.Result{}, err
	}
	opts := j.options()
	optsJSON := renderOptionsJSON(opts, j.provenance(), j.in.Language, j.in.Chunk)
	stem := inbox.SanitizeName(doc.Title)
	if stem == "" {
		stem = "document"
	}
	var wsFile *workspaceLoc
	if j.dest.ws != nil {
		var res tools.Result
		var ok bool
		if wsFile, stem, res, ok = j.workspaceFile(ctx, stem); !ok {
			return res, nil
		}
	}
	if j.dest.toVolume {
		if s := strings.TrimSuffix(path.Base(j.dest.to.path), path.Ext(j.dest.to.path)); !strings.HasSuffix(j.dest.to.path, "/") && s != "" {
			stem = s
		}
	}

	// What the file is made of, less the date and where its pictures land:
	// the same content and options always hash the same (D16).
	sourceSHA, err := renderSourceHash(doc, opts)
	if err != nil {
		return renderRefusal(err), nil
	}

	if log != nil && !j.dest.download && j.dest.ws == nil && (j.format != render.FormatMarkdown || len(doc.Images) == 0) {
		if out, ok := j.reuse(ctx, log, sourceSHA, optsJSON, src, rep); ok {
			return out, nil
		}
	}

	// Pictures beside a Markdown file are written first: where they land
	// decides the links the file holds.
	var imageRefs []string
	zipped := j.dest.download && j.format == render.FormatMarkdown && len(doc.Images) > 0
	if zipped {
		// A download carries its pictures in a zip beside the Markdown.
		opts.ImagePaths = map[string]string{}
		for _, im := range doc.Images {
			opts.ImagePaths[im.Name] = stem + "/" + im.Name
		}
	} else if j.format == render.FormatMarkdown && len(doc.Images) > 0 {
		paths, refs, res, ok := j.writeImages(ctx, doc, stem)
		if !ok {
			return res, nil
		}
		opts.ImagePaths, imageRefs = paths, refs
	}
	bundle, err := render.EncodeBundle(doc, opts)
	if err != nil {
		if j.dest.download {
			j.renderErr = err
		}
		return renderRefusal(err), nil
	}
	data, err := j.f.converter().Render(ctx, bundle, j.format)
	if err != nil && j.dest.download && ctx.Err() == nil {
		j.renderErr = err
	}
	if res, done, rerr := renderFailure(ctx, err, j.f.slotWait); done {
		return res, rerr
	}

	name, mediaType := stem+"."+j.format, render.MediaType(j.format)
	if zipped {
		if data, err = markdownBundle(stem, data, doc); err != nil {
			j.renderErr = err
			return renderRefusal(err), nil
		}
		name, mediaType = stem+".zip", "application/zip"
	}
	sum := sha256.Sum256(data)
	outSHA := hex.EncodeToString(sum[:])
	var ref, destination string
	if j.dest.download {
		destination = "download"
		// A person's download keeps the title as written; the stem inside a
		// Markdown zip stays the file-system-safe one its links use.
		*j.exported = ExportedFile{Name: downloadName(doc.Title, strings.TrimPrefix(path.Ext(name), ".")), MediaType: mediaType, Data: data}
	} else if j.dest.toVolume {
		var res tools.Result
		var ok bool
		ref, res, ok = writeVolumeBytes(ctx, "render", j.dest.to, stem+"."+j.format, data, j.in.Overwrite)
		if !ok {
			return res, nil
		}
		destination = ref
	} else if wsFile != nil {
		w, res, ok := wsFile.Create(ctx, int64(len(data)), bytes.NewReader(data), j.in.Overwrite)
		if !ok {
			return prefixed("render", res), nil
		}
		ref, destination = w.Ref, w.Ref
	} else {
		kept := inbox.Capture(ctx, inbox.Item{Name: stem + "." + j.format, MediaType: render.MediaType(j.format),
			Origin: renderOrigin(src), Dir: renderInboxDir}, data)
		if !kept.Kept {
			return errBusiness("render: the file could not be kept: "+kept.Stub, "Free inbox space with op=rm or op=clear, then try again."), nil
		}
		ref, destination = kept.Record.Ref, "inbox"
	}

	out := map[string]any{"ref": ref, "format": j.format, "media_type": render.MediaType(j.format),
		"size": len(data), "sha256": outSHA, "reused": false, "report": rep}
	if len(imageRefs) > 0 {
		out["image_refs"] = imageRefs
	}
	if log != nil {
		row := renderRow{DocumentID: src.DocumentID, RootChunkID: src.RootChunkID, SourceSHA256: sourceSHA, Format: j.format,
			Options: optsJSON, OutputSHA256: outSHA, Size: int64(len(data)), Destination: destination, Actor: j.actorOf(ctx)}
		if err := log.record(ctx, &row); err != nil {
			// The file is written; a log fault is said, not hidden.
			out["render_log_error"] = err.Error()
		} else {
			out["render_id"] = row.ID
			if j.exported != nil {
				j.exported.RenderID = row.ID
			}
		}
		out["document_id"] = src.DocumentID
	}
	return okJSON(out)
}

// reuse answers a render with an earlier one whose output, in a volume, still
// holds the same bytes: with to, copied there; without, its ref reported.
func (j *renderJob) reuse(ctx context.Context, log *renderLog, sourceSHA, optsJSON string, src renderedSource, rep render.Report) (tools.Result, bool) {
	rows, err := log.reusable(ctx, sourceSHA, j.format, optsJSON)
	if err != nil {
		return tools.Result{}, false
	}
	for _, row := range rows {
		earlier, err := parseDataRef(row.Destination)
		if err != nil || earlier.kind != refVolume {
			continue
		}
		data, ok := readVolumeIfDigest(ctx, earlier, row.OutputSHA256, row.Size)
		if !ok {
			continue
		}
		out := map[string]any{"format": j.format, "media_type": render.MediaType(j.format), "size": row.Size,
			"sha256": row.OutputSHA256, "reused": true, "report": rep, "document_id": src.DocumentID}
		if !j.dest.toVolume {
			out["ref"], out["render_id"] = row.Destination, row.ID
			res, _ := okJSON(out)
			return res, true
		}
		stem := strings.TrimSuffix(path.Base(earlier.path), path.Ext(earlier.path))
		ref, res, ok := writeVolumeBytes(ctx, "render", j.dest.to, stem+"."+j.format, data, j.in.Overwrite)
		if !ok {
			return res, true
		}
		out["ref"] = ref
		nr := renderRow{DocumentID: src.DocumentID, RootChunkID: src.RootChunkID, SourceSHA256: sourceSHA, Format: j.format,
			Options: optsJSON, OutputSHA256: row.OutputSHA256, Size: row.Size, Destination: ref, Actor: renderActor(ctx)}
		if err := log.record(ctx, &nr); err == nil {
			out["render_id"] = nr.ID
		}
		res, _ = okJSON(out)
		return res, true
	}
	return tools.Result{}, false
}

// readVolumeIfDigest reads a volume file the run may read when it still is
// size bytes hashing to digest. Anything else — gone, changed, not readable
// by this run — is no.
func readVolumeIfDigest(ctx context.Context, r dataRef, digest string, size int64) ([]byte, bool) {
	if size <= 0 || size > render.MaxOutput {
		return nil, false
	}
	vfs, rel, _, ok := openRef(ctx, r, false)
	if !ok {
		return nil, false
	}
	defer vfs.Close()
	fh, err := vfs.Open(rel)
	if err != nil {
		return nil, false
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, size+1))
	if err != nil || int64(len(data)) != size {
		return nil, false
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]) == digest
}

// writeImages writes a Markdown render's pictures beside it, in a folder
// named after the file, and returns each picture's path relative to the
// Markdown file and its reference.
func (j *renderJob) writeImages(ctx context.Context, doc *render.Doc, stem string) (map[string]string, []string, tools.Result, bool) {
	paths := map[string]string{}
	var refs []string
	if j.dest.ws != nil {
		// Beside the file in the workspace: <its folder>/<stem>/<name>.
		dir := *j.dest.ws
		dir.path = wsClean(path.Join(path.Dir(j.wsTarget), stem))
		for _, im := range doc.Images {
			img := dir.child(im.Name)
			if res, ok := img.hook(ctx, wsHookWrite); !ok {
				return nil, nil, res, false
			}
			w, res, ok := img.Create(ctx, int64(len(im.Data)), bytes.NewReader(im.Data), j.in.Overwrite)
			if !ok {
				return nil, nil, prefixed("render", res), false
			}
			paths[im.Name] = stem + "/" + im.Name
			refs = append(refs, w.Ref)
		}
		return paths, refs, tools.Result{}, true
	}
	if !j.dest.toVolume {
		dir := path.Join(renderInboxDir, stem)
		for _, im := range doc.Images {
			kept := inbox.Capture(ctx, inbox.Item{Name: im.Name, MediaType: im.MediaType, Origin: "render", Dir: dir}, im.Data)
			if !kept.Kept {
				return nil, nil, errBusiness("render: a picture could not be kept: "+kept.Stub, "Free inbox space with op=rm or op=clear, then try again."), false
			}
			rel := strings.TrimPrefix(kept.Record.Path, renderInboxDir+"/")
			paths[im.Name] = rel
			refs = append(refs, kept.Record.Ref)
		}
		return paths, refs, tools.Result{}, true
	}
	// A volume: <folder of to>/<stem>/img-N.<ext>.
	folder := path.Dir(j.dest.to.path)
	if strings.HasSuffix(j.dest.to.path, "/") {
		folder = strings.TrimSuffix(j.dest.to.path, "/")
	}
	sub := dataRef{raw: refPrefix(j.dest.to) + path.Join(folder, stem), kind: refVolume, volume: j.dest.to.volume, path: path.Join(folder, stem)}
	if res, ok := ensureVolumeFolder(ctx, "render", sub); !ok {
		return nil, nil, res, false
	}
	for _, im := range doc.Images {
		target := dataRef{raw: sub.raw + "/" + im.Name, kind: refVolume, volume: sub.volume, path: sub.path + "/" + im.Name}
		ref, res, ok := writeVolumeBytes(ctx, "render", target, im.Name, im.Data, j.in.Overwrite)
		if !ok {
			return nil, nil, res, false
		}
		paths[im.Name] = stem + "/" + im.Name
		refs = append(refs, ref)
	}
	return paths, refs, tools.Result{}, true
}

// workspaceFile is the workspace file a render writes: to itself, or
// <stem>.<format> inside it when it ends in "/" or names a folder. The
// workspace's write hook judges the final path; its stem names the
// pictures' folder beside it.
func (j *renderJob) workspaceFile(ctx context.Context, stem string) (*workspaceLoc, string, tools.Result, bool) {
	l := j.dest.ws
	target := l
	if strings.HasSuffix(j.dest.to.path, "/") || l.path == "." {
		target = l.child(stem + "." + j.format)
	} else {
		e, res, ok := l.Stat(ctx)
		if !ok {
			return nil, "", prefixed("render", res), false
		}
		if e.Type == workspace.EntryDir {
			target = l.child(stem + "." + j.format)
		} else if s := strings.TrimSuffix(path.Base(l.path), path.Ext(l.path)); s != "" {
			stem = s
		}
	}
	if res, ok := target.hook(ctx, wsHookWrite); !ok {
		return nil, "", res, false
	}
	j.wsTarget = target.path
	return target, stem, tools.Result{}, true
}

// renderSourceHash is the hash of what a render is made of: the tree and the
// options that change the output, less the date and where pictures land.
func renderSourceHash(doc *render.Doc, opts render.Options) (string, error) {
	d := *doc
	d.Date = ""
	o := opts
	o.ImagePaths, o.AssetDir = nil, ""
	b, err := render.EncodeBundle(&d, o)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func renderOrigin(src renderedSource) string {
	if src.DocumentID == "" {
		return "render"
	}
	return "render:" + src.Scope + "/" + src.DocumentID
}

// actorOf is who rendered, as the render log records it.
func (j *renderJob) actorOf(ctx context.Context) string {
	if j.actor != "" {
		return j.actor
	}
	return renderActor(ctx)
}

// renderActor is who rendered: the run.
func renderActor(ctx context.Context) string {
	if id := tools.RunID(ctx); id != "" {
		return id
	}
	return tools.RunIdentity(ctx).UserID
}

func renderRefusal(err error) tools.Result {
	if errors.Is(err, render.ErrTooLarge) {
		return errValidation("render: "+strings.TrimPrefix(err.Error(), "render: "), "Render a part of the document with chunk.")
	}
	return errFrom("render: "+err.Error(), err)
}

// renderFailure maps a conversion error to the op's answer; done false means
// the render succeeded.
func renderFailure(ctx context.Context, err error, wait time.Duration) (tools.Result, bool, error) {
	switch {
	case err == nil:
		return tools.Result{}, false, nil
	case errors.Is(err, convert.ErrBusy):
		res, rerr := slotBusy(ctx, "render", wait)
		return res, true, rerr
	case errors.Is(err, convert.ErrQuarantined):
		return errBusiness("render: this document crashed the file converter and will not be tried again here for now",
			"Render a part of it with chunk, or ask an operator."), true, nil
	case errors.Is(err, convert.ErrNotConfigured), errors.Is(err, convert.ErrNoHelper):
		// The only render with a helper is PDF: Typst is not installed
		// where this server converts files.
		return errBusiness("render: this server cannot write PDF: the typesetter (Typst) is not installed where it converts files",
			"Render as docx for now; an operator can run the loomcycle-worker image, which has Typst."), true, nil
	case errors.Is(err, convert.ErrNoWorkers):
		return errTransient("render: no file conversion worker is running on this server", "Try again in a minute; an operator can check the conversion workers."), true, nil
	case errors.Is(err, render.ErrTooLarge):
		return errValidation("render: the file would be larger than "+fmt.Sprint(render.MaxOutput>>20)+" MiB", "Render a part of the document with chunk."), true, nil
	case errors.Is(err, render.ErrUnsupported):
		return errBusiness("render: the file converter cannot write this format", "Ask an operator to update the conversion workers."), true, nil
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return errTransient("render: the render took too long", "Render a part of the document with chunk."), true, nil
	case ctx.Err() != nil:
		return tools.Result{}, true, ctx.Err()
	}
	return errFrom("render: "+err.Error(), err), true, nil
}

// writeVolumeBytes writes data to the volume file to names, as keep writes a
// file: to ending in / (or naming a folder) takes name inside it, the folder
// must exist, the run's Write grant and hooks judge the final path, and an
// existing file is replaced only with overwrite.
func writeVolumeBytes(ctx context.Context, op string, to dataRef, name string, data []byte, overwrite bool) (string, tools.Result, bool) {
	root, fence, err := effectiveRoot(ctx, to.volume, true)
	if err != nil {
		return "", errPermission(op+": "+err.Error(), "Name a read-write volume you are bound to."), false
	}
	toPath := to.path
	if strings.HasSuffix(toPath, "/") {
		toPath += name
	}
	target, err := fsops.ResolveParentInsideRoot(root, toPath)
	if err == nil {
		err = fsops.OutsideFence(fence, target)
	}
	if err != nil {
		return "", errPermission(op+": "+err.Error(), "Create the folder first, or name one that exists."), false
	}
	dst, err := fsops.OpenVolumeFS(root, fence)
	if err != nil {
		return "", errPermission(op+": "+err.Error(), ""), false
	}
	defer dst.Close()
	rel, err := dst.Rel(target)
	if err != nil {
		return "", errPermission(op+": "+err.Error(), ""), false
	}
	dir, err := dst.OpenDir(filepath.Dir(rel))
	if err != nil {
		return "", errNotFound(op+": the folder for "+to.raw+" does not exist", "Create it first, or name one that exists."), false
	}
	defer dir.Close()
	base := filepath.Base(rel)
	if fi, err := dir.Lstat(base); err == nil && fi.IsDir() {
		sub, err := dst.Step(dir, base)
		if err != nil {
			return "", errPermission(op+": "+err.Error(), ""), false
		}
		defer sub.Close()
		dir, base, rel = sub, name, filepath.Join(rel, name)
	}
	final := dataRef{raw: refPrefix(to) + filepath.ToSlash(rel), kind: refVolume, volume: to.volume, path: filepath.ToSlash(rel)}
	if res, ok := checkRefGrant(ctx, final, true, FileToolName, "Leave to out to write to the inbox"); !ok {
		return "", res, false
	}
	if fi, err := dir.Lstat(base); err == nil {
		if !fi.Mode().IsRegular() || !overwrite {
			return "", errBusiness(fmt.Sprintf("%s: %s already exists", op, final.raw), "Pass overwrite: true to replace it, or choose another name."), false
		}
	}
	tmp, tmpName, err := fsops.CreateTemp(dir, ".loomcycle-"+op+"-")
	if err != nil {
		return "", errTransient(op+": "+err.Error(), "Try again."), false
	}
	defer dir.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", errTransient(op+": writing failed: "+err.Error(), "Try again."), false
	}
	if err := tmp.Close(); err != nil {
		return "", errTransient(op+": writing failed: "+err.Error(), "Try again."), false
	}
	if err := dir.Rename(tmpName, base); err != nil {
		return "", errTransient(op+": "+err.Error(), "Try again."), false
	}
	return final.raw, tools.Result{}, true
}

// ensureVolumeFolder makes the folder r names, inside a folder that exists,
// unless it is there already.
func ensureVolumeFolder(ctx context.Context, op string, r dataRef) (tools.Result, bool) {
	if res, ok := checkRefGrant(ctx, r, true, FileToolName, "Leave to out to write to the inbox"); !ok {
		return res, false
	}
	root, fence, err := effectiveRoot(ctx, r.volume, true)
	if err != nil {
		return errPermission(op+": "+err.Error(), "Name a read-write volume you are bound to."), false
	}
	target, err := fsops.ResolveParentInsideRoot(root, r.path)
	if err == nil {
		err = fsops.OutsideFence(fence, target)
	}
	if err != nil {
		return errPermission(op+": "+err.Error(), "Create the folder first, or name one that exists."), false
	}
	dst, err := fsops.OpenVolumeFS(root, fence)
	if err != nil {
		return errPermission(op+": "+err.Error(), ""), false
	}
	defer dst.Close()
	rel, err := dst.Rel(target)
	if err != nil {
		return errPermission(op+": "+err.Error(), ""), false
	}
	dir, err := dst.OpenDir(filepath.Dir(rel))
	if err != nil {
		return errNotFound(op+": the folder for "+r.raw+" does not exist", "Create it first, or name one that exists."), false
	}
	defer dir.Close()
	base := filepath.Base(rel)
	if err := dir.Mkdir(base, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return errTransient(op+": "+err.Error(), "Try again."), false
	}
	if fi, err := dir.Lstat(base); err != nil || !fi.IsDir() {
		return errBusiness(fmt.Sprintf("%s: %s is not a folder", op, r.raw), "Choose another name for the file."), false
	}
	return tools.Result{}, true
}

// drawDiagram draws one Mermaid diagram on this tool's converter (RFC GR
// P4d). A server with nothing to draw it, or none free to, answers
// render.ErrNoDrawer: the render's diagrams then stay code rather than
// each waiting its turn.
func (j *renderJob) drawDiagram(ctx context.Context, src string) ([]byte, error) {
	b, err := j.f.converter().DrawDiagram(ctx, []byte(src))
	if errors.Is(err, convert.ErrNotConfigured) || errors.Is(err, convert.ErrNoHelper) ||
		errors.Is(err, convert.ErrNoWorkers) || errors.Is(err, convert.ErrBusy) {
		return nil, render.ErrNoDrawer
	}
	return b, err
}
