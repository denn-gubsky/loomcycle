package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/files/render"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC GR: a Document rendered back into a file. The Document side is here:
// reading the document into a render tree (render.Doc), as the caller, and
// the render log. The File side (file_render.go) turns the tree into a file.
//
// File op=render reaches this code through Document's own dispatch, as an
// export_md call carrying a renderRequest, so a render passes every gate an
// export does — the read mode, the scope grants, the access-control set — and
// reads exactly what export_md would show the caller.

// documentRendersDDL is the render log (RFC GR §4.8, D16): one row per
// completed render of a document, holding hashes, refs and who, never
// content. It is additive: a scope provisioned before it gets it on its next
// provisioning, and records nothing until then.
const documentRendersDDL = `CREATE TABLE IF NOT EXISTS document_renders (
	id TEXT PRIMARY KEY, document_id TEXT NOT NULL, root_chunk_id TEXT NOT NULL,
	source_sha256 TEXT NOT NULL, format TEXT NOT NULL, options TEXT NOT NULL,
	output_sha256 TEXT NOT NULL, size BIGINT NOT NULL, destination TEXT NOT NULL,
	actor TEXT NOT NULL, created_at BIGINT NOT NULL)`

const (
	documentRendersDocIndexDDL    = `CREATE INDEX IF NOT EXISTS document_renders_doc ON document_renders(document_id, created_at)`
	documentRendersSourceIndexDDL = `CREATE INDEX IF NOT EXISTS document_renders_source ON document_renders(source_sha256, format)`
	documentRendersOutIndexDDL    = `CREATE INDEX IF NOT EXISTS document_renders_output ON document_renders(output_sha256)`
)

// renderRequest asks export_md for a render tree instead of Markdown. It is
// set only in process (docInput.render is not decoded from JSON), by File
// op=render.
type renderRequest struct {
	// chunkID renders the subtree under that chunk of the document; "" the
	// whole document.
	chunkID string
	// provenance adds the imported document's provenance line under the
	// title (whole-document renders only).
	provenance bool
	language   string
	// date is the render date, YYYY-MM-DD UTC.
	date string
	// deliver turns the tree into a file and returns the op's result. It
	// runs inside the export, with the scope's render log at hand.
	deliver func(ctx context.Context, doc *render.Doc, rep render.Report, src renderedSource, log *renderLog) (tools.Result, error)
}

// renderedSource names what a render read.
type renderedSource struct {
	DocumentID  string
	RootChunkID string
	Scope       string
}

// renderDocument builds the render tree of docID (or of one of its chunks)
// and hands it to the request's deliver.
func (d *Document) renderDocument(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput, docID, title string) (tools.Result, error) {
	req := in.render
	roots, byParent, err := d.loadChunkTree(ctx, key, docID)
	if err != nil {
		return errFrom("render: "+err.Error(), err), nil
	}
	rows := roots
	if req.chunkID != "" {
		row, ok, err := d.getChunkRow(ctx, key, req.chunkID)
		if err != nil {
			return errFrom("render: "+err.Error(), err), nil
		}
		if !ok || row.DocumentID != docID {
			return errNotFound("render: no chunk "+req.chunkID+" in document "+docID, "Pass chunk as the id of a chunk of that document (get_document lists its sections)."), nil
		}
		rows = []chunkRow{row}
	}
	if len(rows) == 0 {
		return errBusiness("render: the document has no content to render", ""), nil
	}

	// Which chunks the file holds, so a link to one of them becomes a link
	// inside the file. Known before any body is parsed: a link may point
	// forward.
	inFile := map[string]bool{}
	var collect func(rs []chunkRow)
	collect = func(rs []chunkRow) {
		for _, r := range rs {
			inFile[r.ID] = true
			collect(byParent[r.ID])
		}
	}
	collect(rows)

	scope := in.Scope
	if scope == "" {
		scope = "user"
	}
	b := render.NewBuilder(title)
	b.Links = d.renderLinks(ctx, key, scope, inFile)
	b.Doc.Language = req.language
	b.Doc.Date = req.date

	// stack[i] is the open section at walk depth i+1 (depth 0 is a root).
	var stack []*render.Section
	stopped := false
	werr := d.walkChunkTree(ctx, key, mscope, rows, byParent, 0, true, func(c treeChunk) error {
		if stopped {
			return nil
		}
		blocks := renderBlocks(b, c)
		if c.depth == 0 && b.Doc.Anchor == "" {
			// The walk's first root is the file's title.
			b.Doc.Title = strings.Join(strings.Fields(c.row.Title), " ")
			if req.chunkID == "" && title != "" {
				b.Doc.Title = strings.Join(strings.Fields(title), " ")
			}
			b.Doc.Anchor = chunkAnchor(c.row.ID)
			b.Doc.Status = c.row.Status
			b.Doc.Blocks = blocks
			stack = stack[:0]
			return nil
		}
		if !b.AddSection() {
			stopped = true
			return nil
		}
		depth := c.depth
		if depth < 1 {
			depth = 1 // a second root sits beside the first root's sections
		}
		level := depth + 1
		if level > 6 {
			level = 6
		}
		sec := &render.Section{ID: c.row.ID, Anchor: chunkAnchor(c.row.ID), Title: strings.Join(strings.Fields(c.row.Title), " "), Level: level, Blocks: blocks}
		if len(stack) > depth-1 {
			stack = stack[:depth-1]
		}
		if len(stack) == 0 {
			b.Doc.Sections = append(b.Doc.Sections, sec)
		} else {
			p := stack[len(stack)-1]
			p.Children = append(p.Children, sec)
		}
		stack = append(stack, sec)
		return nil
	})
	if werr != nil {
		return errFrom("render: body: "+werr.Error(), werr), nil
	}
	if req.provenance && req.chunkID == "" && len(roots) > 0 {
		rootBody, rerr := d.readBody(ctx, mscope, key, roots[0].ID)
		if rerr != nil {
			return errFrom("render: body: "+rerr.Error(), rerr), nil
		}
		b.Doc.Provenance = provenanceLine(d.importedProvenance(ctx, mscope, key, roots[0].ID, rootBody))
	}
	doc, rep := b.Finish()
	rootChunk := rows[0].ID
	return req.deliver(ctx, doc, rep, renderedSource{DocumentID: docID, RootChunkID: rootChunk, Scope: scope}, &renderLog{d: d, key: key})
}

// renderBlocks is one chunk's content as blocks: an image chunk's picture, a
// mermaid chunk's source, any other chunk's body parsed.
func renderBlocks(b *render.Builder, c treeChunk) []render.Node {
	switch c.row.Type {
	case "image":
		if c.hasAsset {
			return []render.Node{b.Image(c.assetMT, c.asset, c.body.Body)}
		}
	case "mermaid":
		if c.body.Body != "" {
			return []render.Node{b.Diagram("mermaid", c.body.Body)}
		}
		return nil
	}
	return b.Body(c.body.Body)
}

// chunkAnchor is a chunk's anchor in a rendered file: stable, so a link to a
// part of a file sent last month still lands.
func chunkAnchor(id string) string {
	if a := "chunk-" + id; render.ValidAnchor(a) {
		return a
	}
	sum := sha256.Sum256([]byte(id))
	return "chunk-" + hex.EncodeToString(sum[:8])
}

// loomLinkRe is a loom: reference as a body cites it (help topic
// Document/citing): loom:doc/<id> or loom:chunk/<id>, optionally ?scope=.
var loomLinkRe = regexp.MustCompile(`^loom:(doc|chunk)/([A-Za-z0-9._:-]{1,128})(?:\?scope=(agent|user|tenant))?$`)

// renderLinks resolves a body's links for a render of scope (RFC GR §4.1,
// D6). A loom: link to a chunk in the file becomes a link inside it. One to a
// document the caller may read becomes its Web UI address when the server
// has a public URL; anything else becomes its text, with nothing said about
// whether the target exists (no oracle). Other links keep only http, https
// and mailto.
func (d *Document) renderLinks(ctx context.Context, key sqlmem.ScopeKey, scope string, inFile map[string]bool) render.LinkResolver {
	public := ""
	if d.Cfg != nil {
		public = strings.TrimRight(d.Cfg.Env.PublicURL, "/")
	}
	if u, err := url.Parse(public); public != "" && (err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "") {
		public = ""
	}
	acl := d.newDocACL(ctx, key)
	if acl != nil {
		acl.useIndex()
	}
	return func(dest string) (string, string, bool) {
		dest = strings.TrimSpace(dest)
		if !strings.HasPrefix(strings.ToLower(dest), "loom:") {
			return render.ExternalOnly(dest)
		}
		m := loomLinkRe.FindStringSubmatch(dest)
		if m == nil {
			return "", "", false
		}
		kind, id, linkScope := m[1], m[2], m[3]
		if linkScope == "" {
			linkScope = "user"
		}
		if linkScope != scope {
			// Another scope's document is not checked from here.
			return "", "", false
		}
		if kind == "chunk" && inFile[id] {
			return "", chunkAnchor(id), true
		}
		if public == "" {
			return "", "", false
		}
		docID := id
		if kind == "chunk" {
			row, ok, err := d.getChunkRow(ctx, key, id)
			if err != nil || !ok {
				return "", "", false
			}
			docID = row.DocumentID
		} else if res, err := d.query(ctx, key, `SELECT id FROM documents WHERE id = ? LIMIT 1`, id); err != nil || len(res.Rows) == 0 {
			return "", "", false
		}
		if acl != nil {
			if ok, err := acl.permits(ctx, "read", docID); err != nil || !ok {
				return "", "", false
			}
		}
		return public + "/ui/documents/" + url.PathEscape(docID) + "?scope=" + scope, "", true
	}
}

// renderLog is one scope's render log.
type renderLog struct {
	d   *Document
	key sqlmem.ScopeKey
}

// renderRow is one row of the render log.
type renderRow struct {
	ID           string `json:"render_id"`
	DocumentID   string `json:"document_id"`
	RootChunkID  string `json:"root_chunk_id"`
	SourceSHA256 string `json:"source_sha256"`
	Format       string `json:"format"`
	Options      string `json:"options"`
	OutputSHA256 string `json:"output_sha256"`
	Size         int64  `json:"size"`
	Destination  string `json:"destination"`
	Actor        string `json:"actor"`
	CreatedAt    int64  `json:"created_at"`
}

const renderRowCols = `id, document_id, root_chunk_id, source_sha256, format, options, output_sha256, size, destination, actor, created_at`

func scanRenderRow(r []any) renderRow {
	size, _ := asInt64(r[7])
	at, _ := asInt64(r[10])
	return renderRow{ID: asStr(r[0]), DocumentID: asStr(r[1]), RootChunkID: asStr(r[2]), SourceSHA256: asStr(r[3]),
		Format: asStr(r[4]), Options: asStr(r[5]), OutputSHA256: asStr(r[6]), Size: size, Destination: asStr(r[8]),
		Actor: asStr(r[9]), CreatedAt: at}
}

// record writes a completed render, giving it an id when it has none.
func (l *renderLog) record(ctx context.Context, r *renderRow) error {
	if r.ID == "" {
		r.ID = newDocID()
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().UnixNano()
	}
	return l.d.exec(ctx, l.key, `INSERT INTO document_renders (`+renderRowCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.DocumentID, r.RootChunkID, r.SourceSHA256, r.Format, r.Options, r.OutputSHA256, r.Size, r.Destination, r.Actor, r.CreatedAt)
}

// reusable is the earlier renders of the same content, format and options
// whose output went to a volume, newest first: the candidates for reuse. The
// caller checks each file still holds its output.
func (l *renderLog) reusable(ctx context.Context, sourceSHA, format, options string) ([]renderRow, error) {
	res, err := l.d.query(ctx, l.key, `SELECT `+renderRowCols+` FROM document_renders
		WHERE source_sha256 = ? AND format = ? AND options = ? AND destination LIKE 'volume:%'
		ORDER BY created_at DESC LIMIT 5`, sourceSHA, format, options)
	if err != nil {
		return nil, err
	}
	out := make([]renderRow, 0, len(res.Rows))
	for _, r := range res.Rows {
		out = append(out, scanRenderRow(r))
	}
	return out, nil
}

// rendersRootedAt lists the renders of the subtree rooted at chunkID (a
// whole document's renders are rooted at its root chunk), newest first.
func (d *Document) rendersRootedAt(ctx context.Context, key sqlmem.ScopeKey, chunkID string, limit int) ([]renderRow, error) {
	res, err := d.query(ctx, key, `SELECT `+renderRowCols+` FROM document_renders WHERE root_chunk_id = ? ORDER BY created_at DESC LIMIT ?`, chunkID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]renderRow, 0, len(res.Rows))
	for _, r := range res.Rows {
		out = append(out, scanRenderRow(r))
	}
	return out, nil
}

// latestRenders is the newest render of the whole document per format, for
// get_document. A store fault leaves them out rather than fail the read.
func (d *Document) latestRenders(ctx context.Context, key sqlmem.ScopeKey, docID, rootChunkID string) map[string]renderRow {
	res, err := d.query(ctx, key, `SELECT `+renderRowCols+` FROM document_renders WHERE document_id = ? AND root_chunk_id = ? ORDER BY created_at DESC LIMIT 200`, docID, rootChunkID)
	if err != nil || len(res.Rows) == 0 {
		return nil
	}
	out := map[string]renderRow{}
	for _, r := range res.Rows {
		row := scanRenderRow(r)
		if _, seen := out[row.Format]; !seen {
			out[row.Format] = row
		}
	}
	return out
}

// renderOptionsJSON is the options a render log row records: the ones that
// change the output, in a fixed order so equal options compare equal.
func renderOptionsJSON(o render.Options, provenance bool, language, chunkID string) string {
	b, _ := json.Marshal(struct {
		TOC        bool   `json:"toc,omitempty"`
		TitlePage  bool   `json:"title_page,omitempty"`
		PageSize   string `json:"page_size,omitempty"`
		Provenance bool   `json:"provenance"`
		Language   string `json:"language,omitempty"`
		Chunk      string `json:"chunk,omitempty"`
	}{o.TOC, o.TitlePage, o.PageSize, provenance, language, chunkID})
	return string(b)
}
