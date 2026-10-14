package http

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/lanegate"
)

// TestSweepRequest_MarksTheRequestAsBackgroundWork — the mark is on the
// request's own ctx, so everything a handler derives from it carries it.
func TestSweepRequest_MarksTheRequestAsBackgroundWork(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/_memory/backfill_embeddings", nil)
	if lanegate.IsBackground(r.Context()) {
		t.Fatal("a plain request is already marked")
	}
	if !lanegate.IsBackground(sweepRequest(r).Context()) {
		t.Fatal("sweepRequest left the request unmarked")
	}
}

// sweepHandlers are the operator-run maintenance sweeps that make model calls:
// each re-indexes, backfills, re-embeds or generates over a whole scope, a
// page at a time, and none of it is anybody's live request.
var sweepHandlers = map[string]string{
	"handleDocumentReindex":          "document_reindex.go",
	"handleMemoryBackfillEmbeddings": "memory_backfill.go",
	"handleMemoryBackfillTraces":     "trace_backfill.go",
	"handleMemoryReembed":            "memory_admin.go",
	"handleDeriveUnits":              "document_derive_units.go",
	"handleDescribeImages":           "document_describe.go",
}

// TestSweepHandlers_MarkTheirRequestBeforeAnythingElse — every sweep handler's
// FIRST statement is `r = sweepRequest(r)`. First, because these handlers read
// r.Context() in a dozen places each: marking it once, before any of them, is
// what makes it impossible for a later one to make an unmarked model call that
// queues ahead of live runs.
//
// It is checked on the source because the calls a sweep makes cannot be
// observed on the test store: it has no vector index, so a sweep stops before
// its first embedding.
func TestSweepHandlers_MarkTheirRequestBeforeAnythingElse(t *testing.T) {
	for name, file := range sweepHandlers {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		var fn *ast.FuncDecl
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name && fd.Recv != nil {
				fn = fd
			}
		}
		if fn == nil {
			t.Errorf("%s: no handler %s — the list of sweeps is stale", file, name)
			continue
		}
		if len(fn.Body.List) == 0 || !isSweepMark(fn.Body.List[0]) {
			t.Errorf("%s: %s does not begin with `r = sweepRequest(r)`: its model calls would queue as live work", file, name)
		}
	}
}

// isSweepMark reports whether stmt is exactly `r = sweepRequest(r)`.
func isSweepMark(stmt ast.Stmt) bool {
	as, ok := stmt.(*ast.AssignStmt)
	if !ok || as.Tok != token.ASSIGN || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
		return false
	}
	lhs, ok := as.Lhs[0].(*ast.Ident)
	if !ok || lhs.Name != "r" {
		return false
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	arg, ok2 := call.Args[0].(*ast.Ident)
	return ok && ok2 && fn.Name == "sweepRequest" && arg.Name == "r"
}
