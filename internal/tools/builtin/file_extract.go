package builtin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/fence"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
)

// File op=extract and op=import (RFC FF): a document kept in a volume becomes
// Markdown in the inbox, and that Markdown becomes a Document, with the
// runtime moving every byte. extract writes through the inbox writer (a
// runtime write, so a sub-agent whose inbox is read-only still extracts);
// import builds the Document through the Document tool's own import_md, so its
// scope and grant checks hold.

const (
	// FileContentKind is the untrusted-block kind an extract excerpt is
	// fenced in. The loop knows it (allowedUntrustedKinds).
	FileContentKind = "file_content"

	fileExtractTimeout = 60 * time.Second
	// fileExcerptChars is the default and the most an excerpt may hold.
	fileExcerptChars = 30000
	// fileExtractMaxMarkdown caps the Markdown one extraction writes.
	fileExtractMaxMarkdown = 32 << 20
	// fileImportMaxBytes caps the Markdown file op=import reads.
	fileImportMaxBytes = 32 << 20
)

// extractSink writes an extraction's images into the inbox as they come and
// gathers its Markdown.
type extractSink struct {
	ctx     context.Context
	dir     string
	origin  string
	md      strings.Builder
	images  []string
	notKept []string
	// starts is where each page's Markdown begins (extract.PageSink).
	starts map[int]int
}

func (s *extractSink) StartPage(n int) {
	if s.starts == nil {
		s.starts = map[int]int{}
	}
	s.starts[n] = s.md.Len()
}

func (s *extractSink) Text(md string) error {
	if s.md.Len()+len(md) > fileExtractMaxMarkdown {
		return extract.ErrSinkFull
	}
	s.md.WriteString(md)
	return nil
}

func (s *extractSink) Image(img extract.Image) (string, error) {
	res := inbox.Capture(s.ctx, inbox.Item{Name: img.Name, MediaType: img.MediaType, Origin: s.origin, Dir: s.dir,
		Page: img.Page, Slide: img.Slide}, img.Data)
	if !res.Kept {
		if len(s.notKept) < 10 {
			s.notKept = append(s.notKept, res.Stub)
		}
		return "", nil
	}
	s.images = append(s.images, res.Record.Ref)
	return res.Record.Ref, nil
}

// ---- extract ----

func (f *File) extract(ctx context.Context, in fileInput) (tools.Result, error) {
	r, err := fileRef(in.Path)
	if err != nil {
		return errValidation("extract: "+err.Error(), "Pass path, e.g. volume:inbox:documents/minutes.docx."), nil
	}
	if in.MaxRows < 0 || in.ExcerptChars < 0 {
		return errValidation("extract: max_rows and excerpt_chars may not be negative", ""), nil
	}
	excerptChars := in.ExcerptChars
	if excerptChars == 0 || excerptChars > fileExcerptChars {
		excerptChars = fileExcerptChars
	}
	oc, res, ok := f.ocrRequest(ctx, in)
	if !ok {
		return res, nil
	}
	if !inbox.Available(ctx) {
		return errBusiness("extract: this run cannot keep files, so there is nowhere to write the extracted text",
			"An operator must configure a dynamic_root volume for the run's inbox."), nil
	}
	vfs, rel, res, ok := openRef(ctx, r, false)
	if !ok {
		return res, nil
	}
	defer vfs.Close()
	fh, err := vfs.open(rel)
	if err != nil {
		return errNotFound(fmt.Sprintf("extract: no file at %s", r.raw), "List the folder with op=ls."), nil
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errValidation(fmt.Sprintf("extract: %s is not a file", r.raw), ""), nil
	}
	head := make([]byte, 512)
	n, _ := fh.ReadAt(head, 0)
	name := filepath.Base(rel)
	mt := inbox.Sniff("", head[:n], name)
	if isRecording(mt) {
		// A recording's text is its speech: extract hands it to transcribe,
		// so an agent need not know which op a file needs.
		call, res, ok := f.sttRequest(ctx, in)
		if !ok {
			return res, nil
		}
		return f.transcribeFile(ctx, call, r, rel, fh, info.Size(), mt)
	}
	ex, ok := extract.Default().For(mt, name)
	if !ok {
		return errValidation(fmt.Sprintf("extract: cannot extract text from %s (%s)", r.raw, mt),
			"Extract reads DOCX, XLSX, PPTX, PDF, HTML, CSV and TSV, text, Markdown, VTT and SRT captions, and images; op=transcribe reads audio and video."), nil
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, io.NewSectionReader(fh, 0, info.Size())); err != nil {
		return errTransient(fmt.Sprintf("extract: reading %s failed", r.raw), "Try again."), nil
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	origin := "extract:" + sourceOrigin(ctx, r, rel, digest)
	// The record of the file read, while its bytes still hash to it, rides on
	// the extracted text's record: an import of that text says what it came
	// from. Read here, before extraction, from the bytes that are extracted.
	var from *inbox.Record
	if rec, ok := recordFor(ctx, r, rel, digest); ok {
		from = capturedOriginal(rec)
	}

	slug := inbox.SanitizeName(strings.TrimSuffix(name, path.Ext(name)))
	if slug == "" {
		slug = "document"
	}
	xctx, cancel := context.WithTimeout(ctx, fileExtractTimeout)
	defer cancel()
	sink := &extractSink{ctx: xctx, dir: path.Join(inbox.DirExtracted, slug), origin: origin}
	// A dry run writes nothing, so it keeps no pictures either.
	images := (in.Images == nil || *in.Images) && !in.DryRun
	rep, err := ex.Extract(xctx, fh, info.Size(), sink, extract.Options{Name: name, Images: images, MaxRows: in.MaxRows})
	cut := false
	switch {
	case err == nil:
	case errors.Is(err, extract.ErrSinkFull):
		cut = true
		rep.Notes = append(rep.Notes, fmt.Sprintf("the text was cut at %d bytes", fileExtractMaxMarkdown))
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return errTransient(fmt.Sprintf("extract: %s took longer than %s", r.raw, fileExtractTimeout), "The file is too large or complex to extract here."), nil
	case ctx.Err() != nil:
		return tools.Result{}, ctx.Err()
	case extract.IsRefused(err):
		return errValidation(fmt.Sprintf("extract: %s was refused: %s", r.raw, strings.TrimPrefix(err.Error(), "extract: refused: ")), ""), nil
	default:
		return errValidation(fmt.Sprintf("extract: could not read %s as %s: %s", r.raw, rep.Format, strings.TrimPrefix(err.Error(), "extract: ")),
			"Check the file is not damaged, or that its type is the one its name says."), nil
	}
	md := sink.md.String()
	if cut {
		md += "\n\n[the rest of the document was not extracted]"
	}

	// OCR: after the text layer, which says which pages need it.
	var lowYield []int
	if rep.Format == "pdf" {
		lowYield = lowYieldPages(rep, oc.minChars)
	}
	var pass *ocrPass
	if pages := ocrPages(rep, oc); len(pages) > 0 || in.DryRun {
		var plan transcribe.Plan
		if len(pages) > 0 {
			if plan, err = f.OCR.Engine.Plan(ctx, mt); err != nil {
				return errBusiness("extract: OCR cannot read "+r.raw+": "+err.Error(), "An operator configures the OCR model (transcription.ocr)."), nil
			}
		}
		if in.DryRun {
			return f.ocrDryRun(ctx, oc, plan, r.raw, mt, pages, lowYield)
		}
		starts := sink.starts
		if rep.Format == "image" {
			// One page: everything after the title.
			if i := strings.Index(md, "\n\n"); i >= 0 {
				starts = map[int]int{1: i}
			}
		}
		pass = f.runOCR(ctx, oc, plan, fh, info.Size(), mt, pages)
		md = spliceOCR(md, starts, pass)
	}

	// ctx, not xctx: an OCR pass may well outlast the extraction's bound.
	kept := inbox.Capture(ctx, inbox.Item{Name: slug + ".md", MediaType: "text/markdown", Origin: origin, Dir: inbox.DirExtracted, ExtractedFrom: from}, []byte(md))
	if !kept.Kept {
		return errBusiness("extract: the extracted text could not be kept: "+kept.Stub, "Free inbox space with op=rm or op=clear, then try again."), nil
	}
	if pass != nil {
		tools.EventEmitter(ctx)(pass.event(ctx, r.raw, kept.Record.Ref))
		if pass.failure() != nil {
			return ocrFailure(pass, kept.Record.Ref), nil
		}
	}

	out := map[string]any{}
	if b, err := json.Marshal(rep); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	excerpt, truncated := cutRunes(md, excerptChars)
	out["ref"] = kept.Record.Ref
	out["source"] = r.raw
	out["media_type"] = mt
	// A picture of a page OCR replaced is no longer in the Markdown.
	out["image_refs"] = keptImageRefs(sink.images, md)
	if len(lowYield) > 0 {
		out["low_yield_pages"] = lowYield
	}
	if pass != nil {
		out["ocr"] = pass.summary(oc.mode)
	}
	if len(sink.notKept) > 0 {
		out["images_not_kept"] = sink.notKept
	}
	out["excerpt"] = fileContentBlock(kept.Record.Ref, excerpt, truncated)
	return okJSONRaw(out)
}

// keptImageRefs are the pictures the Markdown still links.
func keptImageRefs(refs []string, md string) []string {
	out := []string{}
	for _, r := range refs {
		if strings.Contains(md, "]("+r+")") {
			out = append(out, r)
		}
	}
	return out
}

// sourceOrigin is where a file came from: its capture record's origin while
// its bytes still hash to it, the volume it is in otherwise.
func sourceOrigin(ctx context.Context, r dataRef, rel, digest string) string {
	if rec, ok := recordFor(ctx, r, rel, digest); ok && rec.Origin != "" {
		return rec.Origin
	}
	if r.volume == "" {
		return "volume"
	}
	return "volume:" + r.volume
}

// capturedOriginal is the capture a trusted record stands for: the file an
// extraction read when the record is of extracted text, the record itself
// when it is of a file a tool returned. nil for text extracted from a file
// that was never captured (one of the agent's own volumes): nothing about
// that file was recorded.
func capturedOriginal(rec inbox.Record) *inbox.Record {
	if rec.ExtractedFrom != nil {
		return rec.ExtractedFrom
	}
	if strings.HasPrefix(rec.Origin, "extract:") {
		return nil
	}
	return &rec
}

// fileContentBlock fences a document's text for the model: everything in it
// is the document's, so it is escaped (fence.EscapeUntrusted) and nothing in
// it can close the block or open a tag of its own.
func fileContentBlock(ref, text string, truncated bool) string {
	return fmt.Sprintf("<%s path=%q truncated=\"%t\">\n%s\n</%s>",
		FileContentKind, fence.EscapeUntrusted(ref), truncated, fence.EscapeUntrusted(text), FileContentKind)
}

// cutRunes returns the first n characters of s.
func cutRunes(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	i := 0
	for range n {
		_, w := utf8.DecodeRuneInString(s[i:])
		i += w
	}
	return s[:i], true
}

// okJSONRaw is okJSON without HTML escaping, so a fenced block reads as the
// tags it is rather than < escapes.
func okJSONRaw(v any) (tools.Result, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return errFrom("encode result: "+err.Error(), err), nil
	}
	return tools.Result{Text: strings.TrimSuffix(b.String(), "\n")}, nil
}

// ---- import ----

func (f *File) importDoc(ctx context.Context, in fileInput) (tools.Result, error) {
	if f.Docs == nil {
		return errBusiness("import: documents are not available here", "An operator must enable SQL Memory for the Document tool."), nil
	}
	r, err := fileRef(in.Path)
	if err != nil {
		return errValidation("import: "+err.Error(), "Pass path, e.g. volume:inbox:extracted/minutes.md."), nil
	}
	scope := in.Scope
	if scope == "" {
		scope = "user"
	}
	switch scope {
	case "agent", "user", "tenant":
	default:
		return errValidation("import: scope must be agent, user or tenant", ""), nil
	}
	src, err := in.Source.normalize()
	if err != nil {
		return errValidation("import: "+err.Error(),
			"Pass source.url as the item's http(s) web link (a SharePoint webUrl, an email's webLink), and source.kind as one of "+strings.Join(ImportSourceKinds, ", ")+"."), nil
	}
	// Import writes a Document as import_md would, so the run must hold
	// Document and Document's hooks judge it. The scope's own grants are
	// checked by the Document tool itself, below.
	if res, ok := fileDocumentGrant(ctx, in, scope); !ok {
		return res, nil
	}
	vfs, rel, res, ok := openRef(ctx, r, false)
	if !ok {
		return res, nil
	}
	b, res, ok := readMarkdown(vfs, rel, r)
	vfs.Close()
	if !ok {
		return res, nil
	}
	// Headings are applied to the text read, never to the file: the bytes
	// still hash to their capture record, so the document keeps it.
	text := string(b)
	if len(in.Headings) > 0 {
		structured, err := applyImportHeadings(text, in.Headings)
		if err != nil {
			return errValidation("import: headings: "+err.Error()+". Nothing was imported.",
				"Give each entry a line of the file (counted from 1), in increasing order, a level from 2 to 6, and as heading or marker; a marker needs a one-line title. A drop names a line that is already a heading and takes nothing else."), nil
		}
		text = structured
	}
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	origin := strings.TrimPrefix(sourceOrigin(ctx, r, rel, digest), "extract:")
	prov, titleName, recorded := importFileProvenance(ctx, r, rel, digest)
	if len(in.Headings) > 0 {
		dropped := 0
		for _, h := range in.Headings {
			if h.As == importHeadingAsDrop {
				dropped++
			}
		}
		structured := map[string]any{"headings": len(in.Headings) - dropped, "by": tools.RunID(ctx)}
		// Omitted when nothing was dropped, so an import without drops
		// records exactly what it did before drops existed.
		if dropped > 0 {
			structured["dropped"] = dropped
		}
		prov.fields[importFieldStructured] = structured
	}
	// An inbox file whose bytes match no record changed after it was
	// captured: say so, and tag no source rather than source/volume/inbox,
	// which reads like one.
	unrecordedInbox := r.kind == refVolume && r.volume == inbox.Name && !recorded
	if src.URL == "" {
		// A downloaded file links back to its download, as the record keeps
		// it: no query.
		if u, _ := prov.fields[importFieldSourceURL].(string); u != "" {
			src.URL = u
		}
	}
	prov = prov.withSource(src)

	// The file is untrusted text: no raw HTML or loom metadata acts on the
	// import. Image references are left to the reader below, which follows
	// only the ones op=extract wrote.
	md := extract.Neutralize(text, false)
	md = withRootHeading(md, in.Title, fileTitle(titleName))
	md = hoistExtractedImages(md)

	call, _ := json.Marshal(importMDCall(in, scope, md))
	ictx := withImportProvenance(withVolumeImageFields(withVolumeAssetReader(ctx, readExtractedImage), extractedImageFields), prov)
	out, err := f.Docs.Execute(ictx, call)
	if err != nil {
		return tools.Result{}, err
	}
	if out.IsError {
		return out, nil
	}
	var imported map[string]any
	if err := json.Unmarshal([]byte(out.Text), &imported); err != nil {
		return errTransient("import: could not read the import's result", ""), nil
	}
	docID := asStr(imported["document_id"])
	tags := slices.Clone(in.Tags)
	if !unrecordedInbox {
		tags = append(tags, sourceTag(origin))
	}
	tags = append(tags, "untrusted")
	result := map[string]any{
		"document_id":   docID,
		"root_chunk_id": imported["root_chunk_id"],
		"chunks":        imported["chunks_created"],
		"image_chunks":  imported["image_chunks"],
		"skipped":       imported["skipped"],
		"source":        origin,
	}
	if s, ok := imported["skipped_images"]; ok {
		result["skipped_images"] = s
	}
	if p, ok := imported["provenance"]; ok {
		result["provenance"] = p
	}
	if unrecordedInbox {
		result["provenance_note"] = importNoRecordNote
	}
	if in.Reimport {
		// A new edition reports what it reconciled, not chunks made.
		for _, k := range []string{"chunks", "image_chunks", "skipped"} {
			delete(result, k)
		}
		for _, k := range []string{"reimport", "edition_date", "work_key", "changed_chunk_ids", "not_announced"} {
			if v, ok := imported[k]; ok {
				result[k] = v
			}
		}
	}
	tagCall, _ := json.Marshal(map[string]any{"op": "add_tags", "scope": scope, "document_id": docID, "tags": tags})
	tres, err := f.Docs.Execute(ctx, tagCall)
	switch {
	case err != nil:
		return tools.Result{}, err
	case tres.IsError:
		result["tags_error"] = tres.Text
	default:
		var t map[string]any
		_ = json.Unmarshal([]byte(tres.Text), &t)
		result["tags"] = t["tags"]
	}
	return okJSON(result)
}

// importMDCall is the Document import_md call an import makes. A reimport's
// fields pass through unchanged: import_md checks them.
func importMDCall(in fileInput, scope, md string) map[string]any {
	call := map[string]any{"op": "import_md", "scope": scope, "markdown": md, "path": in.DocPath}
	if in.Reimport {
		call["reimport"] = true
	}
	if in.WorkKey != "" {
		call["work_key"] = in.WorkKey
	}
	if in.EditionDate != nil {
		call["edition_date"] = *in.EditionDate
	}
	if in.AmendingDocumentID != "" {
		call["amending_document_id"] = in.AmendingDocumentID
	}
	return call
}

// importFileProvenance is what an import of the file at rel records on the
// document, from the capture record while the bytes still hash to it
// (digest): the captured file's name, type, sha256, where it was captured
// from, when and by which run, and, for a download, its URL as the record
// keeps it (in source_url, for the caller to keep unless a source is given).
// For text op=extract wrote, the captured file is the one it was extracted
// from, whose bytes extract hashed when it read them. titleName is the name
// a document without a heading is titled after: the captured file's, else
// the imported file's, never its inbox folder. recorded reports whether the
// bytes matched a capture record at all.
func importFileProvenance(ctx context.Context, r dataRef, rel, digest string) (prov importProvenance, titleName string, recorded bool) {
	prov = newImportProvenance()
	titleName = path.Base(filepath.ToSlash(rel))
	rec, ok := recordFor(ctx, r, rel, digest)
	if !ok {
		return prov, titleName, false
	}
	orig := capturedOriginal(rec)
	if orig == nil {
		return prov, titleName, true
	}
	f := prov.fields
	f[importFieldFile] = path.Base(orig.Path)
	f[importFieldMediaType] = orig.MediaType
	f[importFieldSHA256] = orig.SHA256
	f[importFieldCapturedFrom] = orig.Origin
	if !orig.CapturedAt.IsZero() {
		f[importFieldCapturedAt] = orig.CapturedAt.UTC().Format(time.RFC3339)
	}
	if orig.RunID != "" {
		f[importFieldRunID] = orig.RunID
	}
	if strings.HasPrefix(orig.Origin, "download:") && orig.URL != "" {
		f[importFieldSourceURL] = orig.URL
	}
	return prov, path.Base(orig.Path), true
}

// fileDocumentGrant refuses an import the run could not make with the
// Document tool: outside a run, without Document in its tools, or when
// Document's pre-hooks deny the equivalent import_md.
func fileDocumentGrant(ctx context.Context, in fileInput, scope string) (tools.Result, bool) {
	if tools.IsSubstrateOperator(ctx) && tools.RunID(ctx) == "" {
		return errPermission("import is not available outside a run", "Use the Document tool's import_md."), false
	}
	if !slices.Contains(tools.PolicyOf(ctx).Tools, "Document") {
		return errPermission("import needs the Document tool, which this run does not have", "Ask an operator to grant the tool."), false
	}
	// The hooks see the import_md call the import makes, its reimport fields
	// included, less the file's text: a hook refusing a work key must not be
	// stepped around by importing through File.
	call := importMDCall(in, scope, "")
	delete(call, "markdown")
	if in.DocPath == "" {
		delete(call, "path")
	}
	call["via"] = FileToolName
	b, _ := json.Marshal(call)
	if res, ok := tools.HookPreCheck(ctx, "Document", b); !ok {
		return classified("import: refused by a Document hook: "+res.Text, tools.CategoryPermission, false,
			"Ask the operator about the hook."), false
	}
	return tools.Result{}, true
}

// readMarkdown reads the file to import: UTF-8 text, at most
// fileImportMaxBytes.
func readMarkdown(vfs *volumeFS, rel string, r dataRef) ([]byte, tools.Result, bool) {
	fh, err := vfs.open(rel)
	if err != nil {
		return nil, errNotFound(fmt.Sprintf("import: no file at %s", r.raw), "List the folder with op=ls."), false
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errValidation(fmt.Sprintf("import: %s is not a file", r.raw), ""), false
	}
	if info.Size() > fileImportMaxBytes {
		return nil, errValidation(fmt.Sprintf("import: %s is over %d bytes", r.raw, fileImportMaxBytes), "Split the document."), false
	}
	b, err := io.ReadAll(io.LimitReader(fh, fileImportMaxBytes+1))
	if err != nil {
		return nil, errTransient(fmt.Sprintf("import: reading %s failed", r.raw), "Try again."), false
	}
	if len(b) > fileImportMaxBytes {
		return nil, errValidation(fmt.Sprintf("import: %s is over %d bytes", r.raw, fileImportMaxBytes), "Split the document."), false
	}
	if !utf8.Valid(b) {
		return nil, errValidation(fmt.Sprintf("import: %s is not UTF-8 text", r.raw), "Import a Markdown file, e.g. one op=extract wrote."), false
	}
	return b, tools.Result{}, true
}

// readExtractedImage is the only volume read an import through this tool
// makes: an image op=extract wrote, in the inbox's extracted folder. A file
// being imported is someone else's text, so a reference it names to any
// other file, in the inbox or in the agent's own volumes, is not followed:
// the importer would otherwise copy a file the text chose into a document.
func readExtractedImage(ctx context.Context, r dataRef, max int64) ([]byte, error) {
	if !isExtractedRef(r) {
		return nil, errors.New("only images op=extract wrote are attached by File op=import")
	}
	return readVolumeBytes(ctx, r, max)
}

// extractedImageFields gives an extracted picture's image chunk the page or
// slide it was on, from its capture record, while the bytes read are still
// the ones captured.
func extractedImageFields(ctx context.Context, r dataRef, data []byte) map[string]any {
	sum := sha256.Sum256(data)
	rec, ok := recordFor(ctx, r, path.Clean(r.path), hex.EncodeToString(sum[:]))
	if !ok {
		return nil
	}
	out := map[string]any{}
	if rec.Page > 0 {
		out["page"] = rec.Page
	}
	if rec.Slide > 0 {
		out["slide"] = rec.Slide
	}
	return out
}

// isExtractedRef reports a reference into the inbox's extracted folder,
// written relative to the volume root.
func isExtractedRef(r dataRef) bool {
	if r.kind != refVolume || r.volume != inbox.Name || strings.HasPrefix(r.path, "/") || strings.Contains(r.path, "\\") {
		return false
	}
	p := path.Clean(r.path)
	return p == r.path && strings.HasPrefix(p, inbox.DirExtracted+"/")
}

// fileTitle is a file name without its extension.
func fileTitle(name string) string {
	t := strings.TrimSpace(strings.TrimSuffix(name, path.Ext(name)))
	if t == "" {
		return "Document"
	}
	return t
}

// withRootHeading makes the Markdown open with the heading import_md roots
// the document at: the given title replaces the first heading's text, and a
// file that does not open with a heading gets one, so text above the first
// heading is not lost.
func withRootHeading(md, titleArg, fallback string) string {
	titleArg = strings.Join(strings.Fields(titleArg), " ")
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if m := mdHeadingRe.FindStringSubmatch(l); m != nil {
			if titleArg != "" {
				lines[i] = m[1] + " " + titleArg
			}
			return strings.Join(lines, "\n")
		}
		break
	}
	t := titleArg
	if t == "" {
		t = fallback
	}
	return "# " + t + "\n\n" + strings.Join(lines, "\n")
}

// hoistExtractedImages gives each extracted image a chunk of its own.
// import_md attaches an image only to a chunk whose whole body is the image,
// and op=extract writes a picture in place, inside a section's text. A line
// that is exactly one extracted image, in a section that holds more than it,
// moves to a sub-heading after the section's text. Fenced code is left alone.
func hoistExtractedImages(md string) string {
	lines := strings.Split(md, "\n")
	var out, raw, body, images []string
	level := 0
	n := 0
	flush := func() {
		nonBlank := 0
		for _, l := range body {
			if strings.TrimSpace(l) != "" {
				nonBlank++
			}
		}
		if len(images) == 0 || (nonBlank == 0 && len(images) == 1) || level == 0 {
			// Nothing to move, or the image already is the whole body.
			out = append(out, raw...)
		} else {
			out = append(out, body...)
			for _, img := range images {
				n++
				alt, _, _ := classifyVolumeImage(img)
				h := strings.Join(strings.Fields(alt), " ")
				if h == "" {
					h = fmt.Sprintf("Image %d", n)
				}
				if t, cut := cutRunes(h, 80); cut {
					h = t + "…"
				}
				out = append(out, "", strings.Repeat("#", min(level+1, 6))+" "+h, "", img)
			}
		}
		raw, body, images = nil, nil, nil
	}
	var fenceChar byte
	fenceRun := 0
	for _, l := range lines {
		if c, run := mdFenceDelim(l); run > 0 {
			switch {
			case fenceRun == 0:
				fenceChar, fenceRun = c, run
			case c == fenceChar && run >= fenceRun:
				fenceRun = 0
			}
			raw, body = append(raw, l), append(body, l)
			continue
		}
		if fenceRun > 0 {
			raw, body = append(raw, l), append(body, l)
			continue
		}
		if m := mdHeadingRe.FindStringSubmatch(l); m != nil {
			flush()
			out = append(out, l)
			level = len(m[1])
			continue
		}
		raw = append(raw, l)
		if _, ref, ok := classifyVolumeImage(l); ok && strings.TrimSpace(l) == l {
			if r, err := parseDataRef(ref); err == nil && isExtractedRef(r) {
				images = append(images, l)
				continue
			}
		}
		body = append(body, l)
	}
	flush()
	return strings.Join(out, "\n")
}

// sourceTag is the document tag naming where a file came from:
// source/mcp/<server>, source/download/<host>, source/http, source/webfetch,
// source/volume/<name>, or source/file when unknown.
func sourceTag(origin string) string {
	kind, rest, _ := strings.Cut(origin, ":")
	seg := func(s string) string {
		var b strings.Builder
		for _, c := range strings.ToLower(s) {
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_' {
				b.WriteRune(c)
			} else {
				b.WriteByte('-')
			}
		}
		return strings.Trim(b.String(), "-")
	}
	switch kind {
	case "mcp":
		server, _, _ := strings.Cut(rest, "/")
		if s := seg(server); s != "" {
			return "source/mcp/" + s
		}
		return "source/mcp"
	case "download":
		if s := seg(rest); s != "" {
			return "source/download/" + s
		}
		return "source/download"
	case "http", "webfetch":
		return "source/" + kind
	case "volume":
		if s := seg(rest); s != "" {
			return "source/volume/" + s
		}
		return "source/volume"
	}
	return "source/file"
}

// ScopeGrants implements tools.ScopedTool: op=import writes a document, so
// its scope is the Document tool's, judged by the Document tool's own check.
func (f *File) ScopeGrants(ctx context.Context) []tools.ScopeField {
	return []tools.ScopeField{{Applies: "import", Grants: scopeGrants(f.InputSchema(), func(s string) error {
		if f.Docs == nil {
			return errors.New("documents are not available here")
		}
		_, _, err := f.Docs.resolveScope(ctx, s)
		return err
	})}}
}
