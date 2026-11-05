package builtin

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// caseLiterals returns the string literals of every `case` in the named function.
func caseLiterals(t *testing.T, file, fn string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn {
			continue
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					s, _ := strconv.Unquote(lit.Value)
					out = append(out, s)
				}
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Fatalf("no case literals in %s.%s", file, fn)
	}
	return out
}

// TestDocumentOps_EveryOpIsClassifiedForTheOntologyGuard is the census: every op the
// dispatch serves is either asked by the ontology guard, read-only, or exempt with a
// stated reason — exactly one. set_remote and sync shipped writing the ontology from a
// peer because the guarded list was hand-kept beside a dispatch that grew.
func TestDocumentOps_EveryOpIsClassifiedForTheOntologyGuard(t *testing.T) {
	ops := caseLiterals(t, "document.go", "dispatch")
	var bad []string
	for _, op := range ops {
		if op == "" {
			continue
		}
		n := 0
		if ontologyWriteOp(op) {
			n++
		}
		if documentReadOp(op) {
			n++
		}
		if ontologyGuardExemption(op) != "" {
			n++
		}
		if n != 1 {
			bad = append(bad, fmt.Sprintf("%s (in %d classes)", op, n))
		}
	}
	served := map[string]bool{}
	for _, op := range ops {
		served[op] = true
	}
	for _, fn := range []string{"ontologyWriteOp", "documentReadOp", "ontologyGuardExemption"} {
		for _, op := range caseLiterals(t, "document_ontology_guard.go", fn) {
			if !served[op] {
				bad = append(bad, fmt.Sprintf("%s classifies %q, which dispatch does not serve", fn, op))
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("Document ops not classified exactly once for the ontology guard:\n%s", strings.Join(bad, "\n"))
	}
}

// TestOntologyGuard_PeerAndMetadataWritesAreRefused: the ops that reach the ontology
// document without editing a chunk body — binding it to a peer, pulling a peer over
// it, tagging it, declaring its types, giving a chunk an asset.
func TestOntologyGuard_PeerAndMetadataWritesAreRefused(t *testing.T) {
	e := newReservedEnv(t)
	read, _ := e.d.OntologyTermsFromTree(e.opCtx, "tenant", memrank.OntologyPath)
	for _, req := range []string{
		`{"op":"set_remote","scope":"tenant","document_id":"` + read.DocumentID + `","source":"peer","remote_ref":"/x"}`,
		`{"op":"sync","scope":"tenant","document_id":"` + read.DocumentID + `"}`,
		`{"op":"add_tags","scope":"tenant","document_id":"` + read.DocumentID + `","tags":["x"]}`,
		`{"op":"add_tags","scope":"tenant","id":"` + read.RootChunkID + `","tags":["x"]}`,
		`{"op":"remove_tags","scope":"tenant","document_id":"` + read.DocumentID + `","tags":["x"]}`,
		`{"op":"define_type","scope":"tenant","document_id":"` + read.DocumentID + `","name":"event","fields":["a"]}`,
	} {
		r := e.doc(t, e.agentCtx, req)
		if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryPermission ||
			!strings.Contains(r.Text, "may not modify the ontology") {
			t.Errorf("not refused by the ontology guard: %s\n  -> %+v", req, r)
		}
	}
}
