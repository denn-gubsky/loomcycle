package builtin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/files/render"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// RFC GR P4c (D20, §12.1): a render's house style is a Document of the
// tenant scope, named by Path — the call's style, else the tenant default
// renderDefaultStyle when it exists. It is read with the caller's own access
// through Document's export, so every gate an export passes applies to it.

// renderDefaultStyle is the tenant's default style document.
const renderDefaultStyle = "/render/styles/default"

// renderStyleArg is a render's style argument: a Path (or none), or the
// style JSON itself passed on the call (RFC HB D5), which needs no Document
// and so no SQL Memory.
type renderStyleArg struct {
	Name   string
	Inline json.RawMessage
}

// errStyleArg is a style argument that is neither a string nor an object.
var errStyleArg = errors.New("style must be a Path, none, or a style object")

func (a *renderStyleArg) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case bytes.Equal(b, []byte("null")):
		*a = renderStyleArg{}
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return errStyleArg
		}
		*a = renderStyleArg{Name: s}
	case len(b) > 0 && b[0] == '{':
		*a = renderStyleArg{Inline: append(json.RawMessage(nil), b...)}
	default:
		return errStyleArg
	}
	return nil
}

// renderPageText is page_text: a header and footer for this render only,
// and the text {date} prints (RFC HB D3).
type renderPageText struct {
	Header *render.PageLine `json:"header"`
	Footer *render.PageLine `json:"footer"`
	Date   string           `json:"date"`
}

// styleRequest asks export_md for a style document's parts instead of
// Markdown. Set only in process (docInput.styleRead is not decoded from
// JSON), by a render resolving its style.
type styleRequest struct {
	documentID string
	revision   int
	body       string
	logo       []byte
	logoMT     string
}

// readStyleDoc fills out from the style document docID: its root's body and
// revision, and its first picture in reading order.
func (d *Document) readStyleDoc(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, docID string, out *styleRequest) (tools.Result, error) {
	roots, byParent, err := d.loadChunkTree(ctx, key, docID)
	if err != nil {
		return errFrom("style: "+err.Error(), err), nil
	}
	if len(roots) == 0 {
		return errBusiness("style: the document is empty", ""), nil
	}
	out.documentID, out.revision = docID, roots[0].Revision
	werr := d.walkChunkTree(ctx, key, mscope, roots, byParent, 0, false, func(c treeChunk) error {
		if c.row.ID == roots[0].ID {
			out.body = c.body.Body
			return nil
		}
		if out.logo == nil && c.row.Type == "image" && c.hasAsset {
			out.logo, out.logoMT = c.asset, c.assetMT
		}
		return nil
	})
	if werr != nil {
		return errFrom("style: "+werr.Error(), werr), nil
	}
	return jsonResult(map[string]any{"document_id": docID})
}

// resolveStyle reads the render's style, before the render: none when the
// call says none or the format is Markdown (which a style does not change);
// otherwise the named style, or the tenant default when it exists. checkGrant
// asks the run's Document grant and hooks about the read, as a run's own
// export_md of the style would be asked; an off-run caller has neither.
//
// A named style that is missing, or one the caller may not read, is refused
// with the same answer. A style that does not parse is refused naming the
// field, the default included: a style never falls back.
func (j *renderJob) resolveStyle(ctx context.Context, checkGrant bool) (tools.Result, bool) {
	name := strings.TrimSpace(j.in.Style.Name)
	if j.format == render.FormatMarkdown {
		if name != "" && name != "none" || j.in.Style.Inline != nil || j.in.PageText != nil {
			j.styleNote = "the style does not apply to Markdown"
		}
		return tools.Result{}, true
	}
	if j.in.Style.Inline != nil {
		return j.inlineStyle()
	}
	if name == "none" {
		return tools.Result{}, true
	}
	explicit := name != ""
	path := name
	if !explicit {
		path = renderDefaultStyle
	}
	missing := func() (tools.Result, bool) {
		if !explicit {
			return tools.Result{}, true
		}
		return errNotFound("render: no style at "+path, "Name a style document of the tenant's Path tree, or pass style: none for the built-in look."), false
	}
	if j.f.Docs == nil {
		return missing()
	}
	call := map[string]any{"op": "export_md", "scope": "tenant", "path": path, "include_metadata": false}
	if checkGrant {
		if res, ok := fileDocumentReadGrant(ctx, call); !ok {
			if !explicit {
				return tools.Result{}, true
			}
			return res, false
		}
	}
	raw, _ := json.Marshal(call)
	noMeta := false
	sr := &styleRequest{}
	res, err := j.f.Docs.executeInput(ctx, docInput{Op: "export_md", Scope: "tenant", Path: path, IncludeMetadata: &noMeta, styleRead: sr}, raw)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return tools.Result{}, false
		}
		return missing()
	}
	if res.IsError || sr.documentID == "" {
		return missing()
	}
	st, perr := render.ParseStyle(sr.body)
	if perr != nil {
		return errValidation(fmt.Sprintf("render: the style at %s is not one: %s", path, strings.TrimPrefix(perr.Error(), "render: style: ")),
			"Correct the style document (help topic File/render lists its keys), or pass style: none."), false
	}
	st.Source = fmt.Sprintf("%s@%d", sr.documentID, sr.revision)
	j.style, j.logo, j.logoMT = st, sr.logo, sr.logoMT
	return tools.Result{}, true
}

// inlineStyle parses a style passed on the call (RFC HB D5) with the same
// strict keys and ranges as a style document. It has no logo, and its
// source in the render log is the hash of its canonical JSON.
func (j *renderJob) inlineStyle() (tools.Result, bool) {
	st, err := render.ParseStyle(string(j.in.Style.Inline))
	if err != nil {
		return errValidation("render: the style passed on the call is not one: "+strings.TrimPrefix(err.Error(), "render: style: "),
			"Correct the style (help topic File/render lists its keys), or pass style: none."), false
	}
	canon, err := json.Marshal(st)
	if err != nil {
		return errFrom("render: style: "+err.Error(), err), false
	}
	sum := sha256.Sum256(canon)
	st.Source = "inline:" + hex.EncodeToString(sum[:])
	j.style = st
	return tools.Result{}, true
}

// checkPageText holds page_text to what a style's header and footer are
// held to, before any work is done.
func checkPageText(pt *renderPageText) (tools.Result, bool) {
	if pt == nil {
		return tools.Result{}, true
	}
	probe := render.Style{Version: render.StyleVersion, Date: pt.Date}
	if pt.Header != nil {
		probe.Header = *pt.Header
	}
	if pt.Footer != nil {
		probe.Footer = *pt.Footer
	}
	if err := probe.Check(); err != nil {
		return errValidation("render: page_text: "+strings.TrimPrefix(err.Error(), "render: style: "),
			"header and footer are each one line or {left, center, right}, using only {page}, {pages}, {title} and {date} ({{ for a brace); date is one line of at most 40 characters."), false
	}
	return tools.Result{}, true
}

// attachStyle gives the built Doc the resolved style and its logo, with the
// call's page text over the style's header and footer.
func (j *renderJob) attachStyle(doc *render.Doc, rep *render.Report) {
	if j.styleNote != "" {
		rep.Notes = append(rep.Notes, j.styleNote)
	}
	if j.style == nil && j.in.PageText == nil && j.photo == nil {
		return
	}
	st := render.Style{Version: render.StyleVersion}
	if j.style != nil {
		st = *j.style
	}
	if pt := j.in.PageText; pt != nil {
		if pt.Header != nil {
			st.Header = *pt.Header
		}
		if pt.Footer != nil {
			st.Footer = *pt.Footer
		}
	}
	// {date} prints the call's date, else the render day; set only when a
	// line shows it, so the render log reuses a render across days when
	// its page text does not.
	if st.UsesDate() {
		st.Date = j.date
		if j.in.PageText != nil && j.in.PageText.Date != "" {
			st.Date = j.in.PageText.Date
		}
	}
	doc.Style = &st
	// The photo (RFC HB D4): the call's picture, else the document's first
	// picture before its first heading when the style places one.
	switch {
	case j.photo != nil:
		if !doc.AddPhoto(j.photoMT, j.photo) {
			rep.Notes = append(rep.Notes, "the photo was left out: the document already holds as many pictures as a file may")
		}
	case st.Photo != nil:
		if !doc.TakePhoto() {
			rep.Notes = append(rep.Notes, "the style places a photo, and the document has no picture before its first heading")
		}
	}
	if j.logo != nil && !doc.AddLogo(j.logoMT, j.logo) {
		rep.Notes = append(rep.Notes, "the style's logo was left out: it is not a picture a file can show")
	}
}

// maxPhotoBytes is the most a call's photo may be.
const maxPhotoBytes = 5 << 20

// readPhoto reads the call's photo: a PNG or JPEG in the run's own inbox,
// at most 5 MiB (RFC HB D4). The picture is the caller's choice, so it is
// read from wherever in the inbox it is, not only from what extract wrote;
// a picture named in a rendered Markdown file still is.
func (j *renderJob) readPhoto(ctx context.Context) (tools.Result, bool) {
	ref := strings.TrimSpace(j.in.Photo)
	if ref == "" {
		return tools.Result{}, true
	}
	if j.format == render.FormatMarkdown {
		j.styleNote = "the style does not apply to Markdown"
		return tools.Result{}, true
	}
	r, err := parseDataRef(ref)
	if err != nil || r.kind != refVolume || r.volume != inbox.Name {
		return errValidation("render: photo must be a picture in the run's inbox", "Pass photo as volume:inbox:<path>, e.g. a picture an MCP tool returned."), false
	}
	b, err := readVolumeBytes(ctx, r, maxPhotoBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return tools.Result{}, false
		}
		return errValidation("render: photo: "+err.Error(), "Pass a PNG or JPEG of at most 5 MiB from the run's inbox."), false
	}
	mt := http.DetectContentType(b)
	if mt != "image/png" && mt != "image/jpeg" {
		return errValidation("render: photo is not a PNG or JPEG picture", ""), false
	}
	j.photo, j.photoMT = b, mt
	return tools.Result{}, true
}
