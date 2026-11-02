package cdc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The RFC FV writer census. A change row carries the document a chunk-body
// write belongs to only when the write's ctx says so (store.WithChangeDocument);
// a writer that forgets produces a row the reader feed can never place, so a
// member's view silently stops updating for that write. Every call in the module
// to a store method this decorator turns into a change row is listed here, by
// file and enclosing function, either as STAMPED (its ctx argument is built by
// one of changeDocumentStampers, right at the call) or as UNSTAMPED with the
// reason it needs no stamp. A new call fails the census until it is placed.
//
// Counts are exact both ways: a site that gains a call fails, and a site that
// loses one fails until its entry shrinks, so the list stays the real set.

// changeRowMethods are the decorator's keyed writes (cdc.go emitKey).
// MemoryDeleteScope is not one: it emits memory.scope_deleted, never a chunk.
var changeRowMethods = map[string]bool{
	"MemorySet": true, "MemorySetProvenance": true, "MemorySetTimed": true,
	"MemoryIncrement": true, "MemoryDelete": true, "MemoryAtomicUpdate": true,
}

// changeDocumentStampers are the calls that put the document on a write's ctx:
// the store helper itself, and the Document tool's helper that resolves a chunk
// body's document for a raw k/v delete.
var changeDocumentStampers = map[string]bool{
	"WithChangeDocument": true,
	"ChunkBodyChangeCtx": true,
}

// stampedWriters: the chunk-body writers, each with its stamp at the call.
var stampedWriters = map[string]int{
	// Every body write: create_document, create_chunk, update_chunk, upsert_chunk,
	// set_asset, import_md (new and reimport), facts, subject merge and adoption,
	// ontology instances. The callers that know the document stamp it; putBody
	// reads the chunk row's for the rest.
	"internal/tools/builtin/document.go:(*Document).putBody": 2,
	// Every body delete: delete_chunk, delete_document, create_document's unwind,
	// the retention prune (cascadeDeleteChunks) and the fact re-home's dropped
	// placeholder root.
	"internal/tools/builtin/document_change_feed.go:(*Document).deleteBody": 1,
	// copy_documents: each copy's bodies, filed under the NEW document.
	"internal/tools/builtin/document_copy.go:(*Document).copyDocumentBodies": 2,
	// DELETE /v1/_memory/scopes/…/keys/doc.chunk:<id> — a consolidated fact's
	// body row, the one chunk body removable by key (MemoryDeleteRefusal).
	"internal/api/http/memory.go:(*Server).handleDeleteMemoryEntry": 1,
}

// unstampedWriters: calls that write no chunk body, or a chunk body whose
// document cannot be known — with why.
var unstampedWriters = map[string]struct {
	n   int
	why string
}{
	"cmd/loomcycle/main.go:bootstrapMemoryEntries":                               {1, "operator yaml memory.entries; not a document write"},
	"internal/api/http/memory.go:(*Server).handlePutMemoryEntry":                 {1, "MemorySetRefusal refuses the doc.chunk: namespace on this route"},
	"internal/api/http/memory_collapse.go:(*Server).handleMemoryCollapseFacts":   {1, "deletes memory/ fact rows only; the prefix is asserted per row before the delete"},
	"internal/api/http/run_tree_delete.go:(*Server).deleteRunTreeTraces":         {1, "trace.turn: rows only"},
	"internal/api/http/trace_backfill.go:(*Server).handleMemoryBackfillTraces":   {1, "trace.turn: rows only"},
	"internal/api/http/trace_index.go:(*Server).indexAssistantTurns":             {1, "trace.turn: rows only"},
	"internal/api/http/trace_index.go:(*Server).indexUserTurn":                   {1, "trace.turn: rows only"},
	"internal/api/webhook/oncomplete.go:(*Receiver).dispatchOnCompleteMemorySet": {1, "an operator-declared on_complete memory.set key; not a document write"},
	"internal/help/index.go:ReconcileIndex":                                      {3, "the help namespace's own keys"},
	"internal/memory/backends/inprocess/inprocess.go:(*Backend).Add":             {1, "a verbatim mem_<hex> row"},
	"internal/memory/backends/inprocess/inprocess.go:(*Backend).Set":             {1, "the Memory tool's set; MemorySetRefusal refuses the doc.chunk: namespace before it"},
	"internal/memory/backends/inprocess/inprocess.go:(*Backend).Delete":          {1, "generic k/v delete: carries the stamp its caller put on ctx (the Memory tool's execDelete stamps a fact body's document)"},
	"internal/scheduler/dispatch.go:(*Scheduler).dispatchMemorySet":              {1, "an operator-declared on_complete memory.set key; not a document write"},
	"internal/tools/builtin/document_copy.go:(*Document).copyDocumentBodies":     {1, "a derived unit (doc.unit:), which is not a chunk body"},
	"internal/tools/builtin/document_deadlink.go:(*Document).ReconcileDeadLinks": {2, "derived units, and ORPHAN bodies whose chunk row is gone — no document can be named, and its deletion already sent the frames"},
	"internal/tools/builtin/document_units.go:(*Document).ReplaceUnits":          {2, "derived units (doc.unit:), not chunk bodies"},
	"internal/tools/builtin/document_units.go:(*Document).deleteUnitsOf":         {1, "derived units (doc.unit:), not chunk bodies"},
	"internal/tools/builtin/memory.go:(*Memory).execAppendDedupe":                {1, "MemorySetRefusal refuses the doc.chunk: namespace before it"},
	"internal/tools/builtin/memory.go:(*Memory).execBoundedList":                 {1, "MemorySetRefusal refuses the doc.chunk: namespace before it"},
	"internal/tools/builtin/memory.go:(*Memory).execIncr":                        {1, "MemorySetRefusal refuses the doc.chunk: namespace before it"},
	"internal/tools/builtin/memory.go:(*Memory).execMerge":                       {1, "MemorySetRefusal refuses the doc.chunk: namespace before it"},
}

func TestChangeDocumentCensus_EveryChunkBodyWriterIsStamped(t *testing.T) {
	stamped, unstamped, scanned := scanChangeRowWriters(t, filepath.Join("..", "..", ".."))
	if scanned < 20 {
		t.Fatalf("scanned only %d change-row writes — the detector no longer recognises them", scanned)
	}
	check := func(kind string, got map[string]int, want func(string) int, known func(string) bool) {
		var sites []string
		for s := range got {
			sites = append(sites, s)
		}
		sort.Strings(sites)
		for _, s := range sites {
			if !known(s) {
				t.Errorf("%s: %d %s change-row write(s) not in the census. A chunk-body write must stamp its document "+
					"at the call (store.WithChangeDocument); anything else goes in unstampedWriters with why", s, got[s], kind)
				continue
			}
			if got[s] != want(s) {
				t.Errorf("%s: %d %s write(s), census says %d — update the entry", s, got[s], kind, want(s))
			}
		}
	}
	check("stamped", stamped, func(s string) int { return stampedWriters[s] }, func(s string) bool { _, ok := stampedWriters[s]; return ok })
	check("unstamped", unstamped, func(s string) int { return unstampedWriters[s].n }, func(s string) bool { _, ok := unstampedWriters[s]; return ok })
	for s, n := range stampedWriters {
		if stamped[s] != n {
			t.Errorf("census lists %s with %d stamped write(s), the code has %d", s, n, stamped[s])
		}
	}
	for s, e := range unstampedWriters {
		if unstamped[s] != e.n {
			t.Errorf("census lists %s with %d unstamped write(s), the code has %d", s, e.n, unstamped[s])
		}
		if strings.TrimSpace(e.why) == "" {
			t.Errorf("census entry %s says nothing about why it needs no stamp", s)
		}
	}
}

// TestChangeDocumentCensus_DetectorSeesBothKinds proves the detector on a
// planted file, so a green census cannot mean "it matches nothing".
func TestChangeDocumentCensus_DetectorSeesBothKinds(t *testing.T) {
	src := `package x
func (d *Document) planted() {
	_ = d.Store.MemorySet(store.WithChangeDocument(ctx, doc), t, s, id, k, v, 0)
	_, _ = d.Store.MemoryDelete(ctx, t, s, id, k)
	_, _ = d.Store.MemoryGet(ctx, t, s, id, k)
	_, _ = d.Store.MemoryDeleteScope(ctx, t, s, id)
}`
	f, err := parser.ParseFile(token.NewFileSet(), "planted.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	stamped, unstamped := map[string]int{}, map[string]int{}
	if n := scanChangeRowFile(f, "planted.go", stamped, unstamped); n != 2 {
		t.Errorf("scanned %d change-row writes, want 2 (the set and the delete)", n)
	}
	if stamped["planted.go:(*Document).planted"] != 1 || unstamped["planted.go:(*Document).planted"] != 1 {
		t.Errorf("stamped %v unstamped %v, want one of each", stamped, unstamped)
	}
}

func scanChangeRowWriters(t *testing.T, root string) (stamped, unstamped map[string]int, scanned int) {
	t.Helper()
	stamped, unstamped = map[string]int{}, map[string]int{}
	fset := token.NewFileSet()
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if e.IsDir() {
				// The store implements these methods (and this decorator calls them);
				// the census is about who calls them.
				if rel == "internal/store" || e.Name() == "testdata" || e.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(fset, p, src, 0)
			if err != nil {
				return err
			}
			scanned += scanChangeRowFile(f, rel, stamped, unstamped)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return stamped, unstamped, scanned
}

func scanChangeRowFile(f *ast.File, file string, stamped, unstamped map[string]int) int {
	scanned := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		site := file + ":" + censusFuncName(fn)
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !changeRowMethods[calleeName(call.Fun)] || len(call.Args) == 0 {
				return true
			}
			scanned++
			if inner, ok := call.Args[0].(*ast.CallExpr); ok && changeDocumentStampers[calleeName(inner.Fun)] {
				stamped[site]++
			} else {
				unstamped[site]++
			}
			return true
		})
	}
	return scanned
}

func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel.Name
	case *ast.Ident:
		return f.Name
	}
	return ""
}

func censusFuncName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return "(*" + id.Name + ")." + fn.Name.Name
		}
	case *ast.Ident:
		return t.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}
