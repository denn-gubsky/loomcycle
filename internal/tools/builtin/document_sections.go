package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

var _ teamrun.DocumentReader = (*Document)(nil)

// Sections reads the document at path and returns its top-level sections —
// the direct children of its root, in document order — each with its whole
// subtree rendered as export_md renders it without metadata. It is the read a
// document-source Starter dispatches its wave from, and it satisfies
// teamrun.DocumentReader.
//
// It runs under ctx's identity through the same scope resolution the Document
// tool's own ops use, so it can read nothing a {{document:…}} binding in the
// same walk could not: a path in another user's tree is simply absent from
// this scope's dirents and reads as not found, and scope "tenant" needs the
// same memory + SQL Memory grants a tenant document read needs.
func (d *Document) Sections(ctx context.Context, scope, path string) ([]teamrun.DocumentSection, error) {
	if d.Store == nil || d.SqlMem == nil {
		return nil, errors.New("documents are not available on this server (SQL Memory is not enabled)")
	}
	if scope != "user" && scope != "tenant" {
		// No agent scope: a walk runs as a person, and has no agent tree.
		return nil, fmt.Errorf("unsupported scope %q (want user|tenant)", scope)
	}
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	if err := d.ensureSchema(ctx, key); err != nil {
		return nil, fmt.Errorf("schema init: %w", err)
	}
	docID, err := d.docIDFromInput(ctx, key, docInput{Path: path})
	if err != nil {
		return nil, err
	}
	dres, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil {
		return nil, err
	}
	if len(dres.Rows) == 0 {
		// A dirent that outlived its document.
		return nil, fmt.Errorf("no such document: %s", path)
	}
	rootID := asStr(dres.Rows[0][0])
	_, byParent, err := d.loadChunkTree(ctx, key, docID)
	if err != nil {
		return nil, err
	}
	children := byParent[rootID]
	out := make([]teamrun.DocumentSection, 0, len(children))
	for i, row := range children {
		var b strings.Builder
		// Depth 1: a section keeps the heading level it has in the whole
		// document, so its markdown reads as a slice of the export.
		if err := d.renderChunkTree(ctx, key, mscope, &b, []chunkRow{row}, byParent, 1, false); err != nil {
			return nil, fmt.Errorf("section %q: %w", row.Title, err)
		}
		out = append(out, teamrun.DocumentSection{
			DocumentID: docID, ChunkID: row.ID, Index: i, Title: row.Title,
			Markdown: strings.TrimRight(b.String(), "\n"),
		})
	}
	return out, nil
}
