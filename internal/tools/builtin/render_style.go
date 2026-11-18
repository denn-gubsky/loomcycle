package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/files/render"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC GR P4c (D20, §12.1): a render's house style is a Document of the
// tenant scope, named by Path — the call's style, else the tenant default
// renderDefaultStyle when it exists. It is read with the caller's own access
// through Document's export, so every gate an export passes applies to it.

// renderDefaultStyle is the tenant's default style document.
const renderDefaultStyle = "/render/styles/default"

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
	name := strings.TrimSpace(j.in.Style)
	if name == "none" {
		return tools.Result{}, true
	}
	if j.format == render.FormatMarkdown {
		if name != "" {
			j.styleNote = "the style does not apply to Markdown"
		}
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

// attachStyle gives the built Doc the resolved style and its logo.
func (j *renderJob) attachStyle(doc *render.Doc, rep *render.Report) {
	if j.styleNote != "" {
		rep.Notes = append(rep.Notes, j.styleNote)
	}
	if j.style == nil {
		return
	}
	st := *j.style
	doc.Style = &st
	if j.logo != nil && !doc.AddLogo(j.logoMT, j.logo) {
		rep.Notes = append(rep.Notes, "the style's logo was left out: it is not a picture a file can show")
	}
}
