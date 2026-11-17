package builtin

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/files/render"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Off-run export (RFC GR §4.4): a person downloads a Document as a file from
// the Web UI or an adapter, with no run. The render is the one File
// op=render makes — read through every gate Document op=export_md passes,
// written by the conversion workers or here — and is logged on the document
// with destination "download". The bytes go to the caller, never to a volume
// or an inbox.

// ExportRequest is one off-run render.
type ExportRequest struct {
	// Document is the document id; Scope its store (user, agent, tenant).
	Document string
	Scope    string
	// Chunk renders the subtree under that chunk; "" the whole document.
	Chunk  string
	Format string
	TOC    bool
	// TitlePage, PageSize, Language and Provenance are as File op=render
	// takes them.
	TitlePage  bool
	PageSize   string
	Language   string
	Provenance *bool
	// Metadata is none or embed, as File op=render takes it.
	Metadata string
	// Actor is who exported, as the render log records it (the HTTP route
	// passes the principal's subject).
	Actor string
}

// ExportedFile is a rendered file for the caller to send on.
type ExportedFile struct {
	// Name is the file's name: the document's title, made safe, with the
	// format's extension (.zip for Markdown with pictures).
	Name      string
	MediaType string
	Data      []byte
	// RenderID is the render log row, "" when the log could not be written.
	RenderID string
}

// ExportFailure is why an export gave no file, for the caller to answer with
// the matching status.
type ExportFailure int

const (
	// ExportInvalid: a parameter is wrong.
	ExportInvalid ExportFailure = iota + 1
	// ExportNotFound: the document or chunk is not there for this caller, or
	// it may not read it — never told apart (no oracle).
	ExportNotFound
	// ExportUnprocessable: the document is there and cannot be rendered as
	// asked (it has no content).
	ExportUnprocessable
	// ExportTooLarge: the file would pass the output caps.
	ExportTooLarge
	// ExportBusy: every converter stayed busy; try again.
	ExportBusy
	// ExportUnavailable: this server cannot write the format now (no
	// typesetter, no conversion worker, documents off).
	ExportUnavailable
	// ExportFailed: anything else.
	ExportFailed
)

// ExportError is a refused or failed export.
type ExportError struct {
	Kind    ExportFailure
	Message string
}

func (e *ExportError) Error() string { return e.Message }

// Export renders a document as a file for a person (RFC GR §4.4). A refusal
// is an *ExportError; a non-nil error is a fault of the server.
func (f *File) Export(ctx context.Context, req ExportRequest) (*ExportedFile, *ExportError, error) {
	invalid := func(msg string) (*ExportedFile, *ExportError, error) {
		return nil, &ExportError{Kind: ExportInvalid, Message: msg}, nil
	}
	format := strings.ToLower(strings.TrimSpace(req.Format))
	switch {
	case format == "":
		return invalid("export: missing required parameter: format (pdf, docx, html or md)")
	case !render.Supported(format):
		return invalid(fmt.Sprintf("export: unknown format %q (pdf, docx, html or md)", req.Format))
	case strings.TrimSpace(req.Document) == "":
		return invalid("export: missing document id")
	}
	switch req.PageSize {
	case "", "a4", "letter":
	default:
		return invalid("export: page_size must be a4 or letter")
	}
	if f.Docs == nil {
		return nil, &ExportError{Kind: ExportUnavailable, Message: "export: documents are not available on this server"}, nil
	}
	scope := req.Scope
	if scope == "" {
		scope = "user"
	}
	switch scope {
	case "agent", "user", "tenant":
	default:
		return invalid("export: scope must be agent, user or tenant")
	}
	switch req.Metadata {
	case "", "none", "embed":
	default:
		return invalid("export: metadata must be none or embed")
	}
	in := fileInput{Document: req.Document, Scope: scope, Chunk: req.Chunk, Format: format, TOC: req.TOC,
		TitlePage: req.TitlePage, Provenance: req.Provenance, PageSize: req.PageSize, Metadata: req.Metadata}
	in.Language = req.Language
	job := &renderJob{f: f, in: in, format: format, dest: renderDest{download: true},
		date: time.Now().UTC().Format("2006-01-02"), actor: req.Actor, exported: &ExportedFile{}}
	noMeta := false
	din := docInput{Op: "export_md", Scope: scope, DocumentID: req.Document, IncludeMetadata: &noMeta,
		render: &renderRequest{chunkID: req.Chunk, provenance: job.provenance(), language: req.Language, date: job.date,
			embed: req.Metadata == "embed", deliver: job.deliver}}
	raw := []byte(fmt.Sprintf(`{"op":"export_md","scope":%q,"document_id":%q,"include_metadata":false}`, scope, req.Document))
	res, err := f.Docs.executeInput(ctx, din, raw)
	if err != nil {
		return nil, nil, err
	}
	if job.renderErr != nil {
		return nil, exportFailureOf(ctx, job.renderErr), nil
	}
	if res.IsError {
		msg := strings.Replace(strings.Replace(res.Text, "export_md: ", "export: ", 1), "render: ", "export: ", 1)
		kind := ExportNotFound
		if res.Error != nil && res.Error.Category == tools.CategoryBusiness {
			kind = ExportUnprocessable
		}
		return nil, &ExportError{Kind: kind, Message: msg}, nil
	}
	if job.exported.Data == nil {
		return nil, nil, errors.New("export: the render wrote no file")
	}
	return job.exported, nil, nil
}

// exportFailureOf words a render's failure for a person downloading a file.
func exportFailureOf(ctx context.Context, err error) *ExportError {
	switch {
	case errors.Is(err, render.ErrTooLarge):
		return &ExportError{Kind: ExportTooLarge, Message: "export: " + strings.TrimPrefix(err.Error(), "render: ") + "; download a part of the document"}
	case errors.Is(err, convert.ErrNotConfigured), errors.Is(err, convert.ErrNoHelper):
		return &ExportError{Kind: ExportUnavailable, Message: "export: this server cannot write PDF: the typesetter (Typst) is not installed where it converts files"}
	case errors.Is(err, convert.ErrNoWorkers):
		return &ExportError{Kind: ExportUnavailable, Message: "export: no file conversion worker is running on this server; try again in a minute"}
	case errors.Is(err, convert.ErrQuarantined):
		return &ExportError{Kind: ExportUnavailable, Message: "export: this document crashed the file converter and will not be tried again here for now"}
	case errors.Is(err, convert.ErrBusy):
		return &ExportError{Kind: ExportBusy, Message: "export: every file converter is busy; try again shortly"}
	case errors.Is(err, render.ErrUnsupported):
		return &ExportError{Kind: ExportUnavailable, Message: "export: the file converter cannot write this format; the conversion workers may need updating"}
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return &ExportError{Kind: ExportBusy, Message: "export: the render took too long; download a part of the document"}
	}
	return &ExportError{Kind: ExportFailed, Message: "export: " + strings.TrimPrefix(err.Error(), "render: ")}
}

// markdownBundle is a Markdown render whose pictures go with it: one zip
// holding <stem>.md and <stem>/img-N.<ext>, the layout a render into a
// volume writes. Entries carry a fixed time so the same render zips the
// same.
func markdownBundle(stem string, md []byte, doc *render.Doc) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, method uint16, data []byte) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	if err := add(stem+".md", zip.Deflate, md); err != nil {
		return nil, err
	}
	for _, im := range doc.Images {
		if err := add(stem+"/"+im.Name, zip.Store, im.Data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > render.MaxOutput {
		return nil, fmt.Errorf("%w: the Markdown and its pictures are %d MiB, more than %d MiB", render.ErrTooLarge, buf.Len()>>20, render.MaxOutput>>20)
	}
	return buf.Bytes(), nil
}

// downloadName is a download's file name: the title as written, Unicode
// kept, with what a file system or a header would read as structure (path
// separators, control characters, quotes) replaced, cut to 120 characters.
func downloadName(title, ext string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.Join(strings.Fields(title), " ") {
		if n >= 120 {
			break
		}
		switch {
		case r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
		n++
	}
	stem := strings.Trim(b.String(), " .")
	if stem == "" {
		stem = "document"
	}
	return stem + "." + ext
}
