package providers_test

import (
	"fmt"
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

// The embedder census.
//
// The server's embedder is not the driver: it is the driver inside its
// provider's embedder lane (lanegate.WrapEmbedder), inside the health observer
// (providers.ObserveEmbedder). A wrapper hides whatever the value beneath it
// implements beyond providers.Embedder, so a type assertion on an embedder
// finds only what the OUTERMOST wrapper is — and is false, with no error
// anywhere, for anything a layer below implements.
//
// Embedders have no providers.As, because each optional interface they have
// is implemented by the outermost wrapper: the health report (the observer is
// outermost), and query mode (every wrapper implements it — the wrapper census
// below). This test holds that count. A new assertion on an embedder value fails here until
// its author has decided where in the wrapper chain the asserted type lives
// (and, if it is not the outermost, given embedders an As like providers').

// embedderAssertions lists every assertion on an embedder value the tree may
// contain: file → the asserted type as written.
var embedderAssertions = map[string]string{
	// Implemented by *providers.ObservedEmbedder, the outermost wrapper.
	"internal/api/http/memory_changes.go": "embedderHealthReporter",
	// The query-mode helper. Every wrapper implements QueryEmbedder and hands
	// on through the helper, so asking the outermost value is asking the chain.
	"internal/providers/embedder.go": "QueryEmbedder",
}

// embedderOperand reports whether x reads like an embedder value: a variable or
// field named embedder / Embedder / emb.
func embedderOperand(x ast.Expr) bool {
	name := ""
	switch v := x.(type) {
	case *ast.Ident:
		name = v.Name
	case *ast.SelectorExpr:
		name = v.Sel.Name
	}
	lower := strings.ToLower(name)
	return lower == "emb" || strings.HasSuffix(lower, "embedder")
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(v.X)
	}
	return fmt.Sprintf("%T", e)
}

// embedderAssertionsIn returns the asserted types of every assertion (and type
// switch) on an embedder-named value in f.
func embedderAssertionsIn(f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.TypeAssertExpr:
			if !embedderOperand(x.X) {
				return true
			}
			if x.Type == nil { // the x.(type) of a type switch: its cases are the types
				out = append(out, "type switch")
				return true
			}
			out = append(out, exprString(x.Type))
		}
		return true
	})
	return out
}

func TestEmbedderAssertions_AllClassified(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	found := map[string]string{}
	var violations []string
	files := 0
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if censusSkipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			files++
			for _, typ := range embedderAssertionsIn(f) {
				if embedderAssertions[rel] == typ {
					found[rel] = typ
					continue
				}
				violations = append(violations, fmt.Sprintf("%s asserts an embedder to %s. The server's embedder is wrapped twice (its provider's lane, then the health observer): an assertion sees only the outermost wrapper. Decide which layer implements %s, then list the assertion in embedderAssertions.", rel, typ, typ))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 100 {
		t.Fatalf("the census parsed only %d files — it is not walking the module", files)
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}
	// The list must not outlive what it lists: a stale entry would let the same
	// assertion come back somewhere else in that file unexamined.
	for rel, typ := range embedderAssertions {
		if found[rel] != typ {
			t.Errorf("embedderAssertions lists %s → %s, which is no longer there", rel, typ)
		}
	}
}

// TestEmbedderAssertions_CensusCatchesEachShape — the walk is only a guard if
// it can fail.
func TestEmbedderAssertions_CensusCatchesEachShape(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"a field", `func f(s *S) { _, _ = s.embedder.(interface{ Health() int }) }`, 1},
		{"an exported field", `func f(d *D) { _, _ = d.Embedder.(*Concrete) }`, 1},
		{"a local", `func f(emb any) { _, _ = emb.(Reporter) }`, 1},
		{"a type switch", `func f(embedder any) { switch embedder.(type) { case Reporter: } }`, 1},
		{"another value", `func f(p any) { _, _ = p.(Reporter) }`, 0},
		{"no assertion", `func f(s *S) { _ = s.embedder }`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package x\n"+tc.body, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := len(embedderAssertionsIn(f)); got != tc.want {
				t.Fatalf("found %d assertions, want %d", got, tc.want)
			}
		})
	}
}

// The query-mode wrapper census.
//
// providers.EmbedQuery asks only the value it is given whether it embeds
// queries differently (it does not walk Unwrap: walking to the driver would skip
// the lane and the health counters for every query). So every wrapper must
// implement EmbedQuery itself, taking its lane or recording its outcome as Embed
// does, and handing on through providers.EmbedQuery. A wrapper that forgets it
// silently turns every query below it back into a stored-text embedding, which
// for a model with an asymmetric query mode is a retrieval regression nobody
// sees fail.

// embedderWrappersIn returns the receiver types in f that unwrap to an
// embedder (`Unwrap() providers.Embedder`, or `Unwrap() Embedder` inside
// package providers), and the receiver types that declare EmbedQuery.
func embedderWrappersIn(f *ast.File) (wrappers, queryers []string) {
	inProviders := f.Name.Name == "providers"
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
			continue
		}
		recv := exprString(fd.Recv.List[0].Type)
		recv = strings.TrimPrefix(recv, "*")
		switch fd.Name.Name {
		case "EmbedQuery":
			queryers = append(queryers, recv)
		case "Unwrap":
			res := fd.Type.Results
			if res == nil || len(res.List) != 1 {
				continue
			}
			got := exprString(res.List[0].Type)
			if got == "providers.Embedder" || (inProviders && got == "Embedder") {
				wrappers = append(wrappers, recv)
			}
		}
	}
	return wrappers, queryers
}

// embedderWrappersMissingEmbedQuery returns "dir.Type" for every embedder
// wrapper under root's internal/ and cmd/ trees whose package declares no
// EmbedQuery on it, plus the number of wrappers found.
func embedderWrappersMissingEmbedQuery(t *testing.T, root string) (missing []string, wrappers int) {
	t.Helper()
	fset := token.NewFileSet()
	type pkgTypes struct{ wrappers, queryers map[string]bool }
	byDir := map[string]*pkgTypes{}
	for _, top := range []string{"internal", "cmd"} {
		base := filepath.Join(root, top)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if censusSkipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			p := byDir[rel]
			if p == nil {
				p = &pkgTypes{wrappers: map[string]bool{}, queryers: map[string]bool{}}
				byDir[rel] = p
			}
			w, q := embedderWrappersIn(f)
			for _, n := range w {
				p.wrappers[n] = true
			}
			for _, n := range q {
				p.queryers[n] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for dir, p := range byDir {
		for n := range p.wrappers {
			wrappers++
			if !p.queryers[n] {
				missing = append(missing, dir+"."+n)
			}
		}
	}
	sort.Strings(missing)
	return missing, wrappers
}

// TestEmbedderWrappers_AllImplementEmbedQuery — every type that unwraps to an
// embedder forwards query mode itself.
func TestEmbedderWrappers_AllImplementEmbedQuery(t *testing.T) {
	missing, wrappers := embedderWrappersMissingEmbedQuery(t, moduleRoot(t))
	// Today: lanegate.Embedder and providers.ObservedEmbedder. A count below that
	// means the walk stopped finding them, not that they are gone.
	if wrappers < 2 {
		t.Fatalf("the census found %d embedder wrappers, want at least 2 — it is not seeing them", wrappers)
	}
	for _, m := range missing {
		t.Errorf("%s unwraps to an embedder but declares no EmbedQuery: every query through it falls back to Embed. Implement EmbedQuery the way its Embed works (same lane / same bookkeeping), handing on via providers.EmbedQuery.", m)
	}
}

// TestEmbedderWrappers_CensusCatchesAWrapperWithoutEmbedQuery — the walk is
// only a guard if it can fail: a module whose one wrapper lacks EmbedQuery is
// reported, and adding the method clears it.
func TestEmbedderWrappers_CensusCatchesAWrapperWithoutEmbedQuery(t *testing.T) {
	const wrapper = `package wrap

import "github.com/denn-gubsky/loomcycle/internal/providers"

type W struct{ inner providers.Embedder }

func (w *W) Unwrap() providers.Embedder { return w.inner }
`
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "wrap")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("w.go", wrapper)
	missing, n := embedderWrappersMissingEmbedQuery(t, root)
	if n != 1 || len(missing) != 1 || missing[0] != "internal/wrap.W" {
		t.Fatalf("wrapper without EmbedQuery: found %d wrappers, missing %v; want 1 and [internal/wrap.W]", n, missing)
	}
	// The method may live in another file of the same package.
	write("q.go", "package wrap\n\nimport \"context\"\n\nfunc (w *W) EmbedQuery(ctx context.Context, texts []string) ([][]float32, error) { return nil, nil }\n")
	if missing, _ := embedderWrappersMissingEmbedQuery(t, root); len(missing) != 0 {
		t.Fatalf("wrapper with EmbedQuery still reported missing: %v", missing)
	}
}
