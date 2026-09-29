package http

import (
	"context"
	"errors"

	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// teamDocumentReader implements teamrun.DocumentReader for a document-source
// Starter, over the Document tool's own read.
//
// It is called with the WALK's ctx, and that is its whole authority: the walk
// runs as the person who started it, so scope "user" is that person's tree —
// the same read a {{document:…}} binding in the Starter's prompt would make.
// It adds reach, not authority, and needs no grant of its own.
//
// The store and SQL Memory are read at call time rather than captured when the
// TeamDef tool is wired: SQL Memory is attached to the server separately, and a
// reader built before it would say "not available" for the life of the process.
type teamDocumentReader struct{ srv *Server }

func (r teamDocumentReader) Sections(ctx context.Context, scope, path string) ([]teamrun.DocumentSection, error) {
	if r.srv.store == nil || r.srv.sqlMem == nil {
		return nil, errors.New("documents are not available on this server (SQL Memory is not enabled)")
	}
	doc := &builtin.Document{Store: r.srv.store, SqlMem: r.srv.sqlMem}
	return doc.Sections(ctx, scope, path)
}
