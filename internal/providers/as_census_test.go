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
	"strconv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// The optional-interface census.
//
// A provider handed out by the registry is a wrapper around the driver
// (internal/lanegate), and a wrapper hides everything the driver implements
// beyond providers.Provider. `p.(providers.KeyedProvider)` on such a value is
// false for EVERY provider, with no error anywhere: a restricted run is routed
// to a provider it cannot key, a thinking model is not downgraded on fallback.
// providers.As looks through the wrapper, so the rule is: nothing asserts a
// provider value to one of these types directly.
//
// Two ways to break that rule, two checks:
//   - a new direct assertion: the walk below fails on it;
//   - a new optional interface that nobody thought about: every interface
//     declared in this package must be classified in ifaceKinds, so adding one
//     fails here until its author has read this comment.

const modulePath = "github.com/denn-gubsky/loomcycle"

type ifaceKind int

const (
	// optional: a driver may implement it; reached only through providers.As.
	optional ifaceKind = iota
	// plumbing: asserted only by providers.As itself.
	plumbing
	// notOnProvider: not discovered by asserting a Provider value at all.
	notOnProvider
)

// ifaceKinds classifies every interface declared in internal/providers.
var ifaceKinds = map[string]ifaceKind{
	"Provider": notOnProvider,
	"Embedder": notOnProvider,
	// QueryEmbedder is asserted on an EMBEDDER, only by providers.EmbedQuery;
	// every embedder wrapper implements it (embedder_census_test.go).
	"QueryEmbedder":                 notOnProvider,
	"Unwrapper":                     plumbing,
	"KeyedProvider":                 optional,
	"ThinkingDowngrader":            optional,
	"ModelToolChoiceEnforcer":       optional,
	"ModelStructuredOutputEnforcer": optional,
	"ModelIdentifier":               optional,
	"RunEnder":                      optional,
	// RequestAuth rides in DriverOptions to the driver that uses it.
	"RequestAuth": notOnProvider,
}

// asFile is the one file allowed to assert on a provider value.
const asFile = "internal/providers/as.go"

var censusSkipDirs = map[string]bool{".git": true, "web": true, "node_modules": true, "adapters": true, "packages": true, "vendor": true, "testdata": true, "bin": true}

// repoRoot is the repository root, two levels above this package. The census
// walks the root module's callers, so it cannot use the nearest go.mod: once
// internal/providers is a module of its own (RFC FE), that is this package's.
// A literal path also lets scripts/ci/affected see these tests read the whole
// tree, so every change selects them.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !strings.HasPrefix(string(mod), "module "+modulePath+"\n") {
		t.Fatalf("%s is not the root of %s (go.mod: %v)", root, modulePath, err)
	}
	return root
}

// declaredInterfaces returns the interface types declared in one parsed file.
func declaredInterfaces(f *ast.File) []string {
	var out []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, s := range gd.Specs {
			ts := s.(*ast.TypeSpec)
			if _, ok := ts.Type.(*ast.InterfaceType); ok {
				out = append(out, ts.Name.Name)
			}
		}
	}
	return out
}

// importsOf maps each local import name of f to its import path.
func importsOf(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, im := range f.Imports {
		path, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if im.Name != nil {
			name = im.Name.Name
		}
		out[name] = path
	}
	return out
}

// assertedTypes returns the type expressions of every type assertion and
// type-switch case in f.
func assertedTypes(f *ast.File) []ast.Expr {
	var out []ast.Expr
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.TypeAssertExpr:
			if x.Type != nil { // nil is the x.(type) of a type switch
				out = append(out, x.Type)
			}
		case *ast.TypeSwitchStmt:
			for _, c := range x.Body.List {
				out = append(out, c.(*ast.CaseClause).List...)
			}
		}
		return true
	})
	return out
}

// censusViolation says why asserting to typ in the file rel (whose package is
// inProviders or not, with the given imports) breaks the rule; "" when it does
// not.
func censusViolation(typ ast.Expr, rel string, inProviders bool, imports map[string]string) string {
	name := ""
	switch x := typ.(type) {
	case *ast.Ident:
		if inProviders {
			name = x.Name
		}
	case *ast.SelectorExpr:
		if pkg, ok := x.X.(*ast.Ident); ok && imports[pkg.Name] == modulePath+"/internal/providers" {
			name = x.Sel.Name
		}
	case *ast.StarExpr:
		// A concrete driver type: `.(*codejs.Provider)`.
		sel, ok := x.X.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || !strings.HasPrefix(imports[pkg.Name], modulePath+"/internal/providers/") {
			return ""
		}
		// A driver package may assert on a value of its own making (a driver
		// that delegates to another holds that driver, not a registry provider).
		if strings.HasPrefix(rel, "internal/providers/") {
			return ""
		}
		return fmt.Sprintf("asserts a provider to the concrete driver type *%s.%s — use providers.As[*%s.%s]", pkg.Name, sel.Sel.Name, pkg.Name, sel.Sel.Name)
	}
	kind, known := ifaceKinds[name]
	if name == "" || !known {
		return ""
	}
	switch kind {
	case optional:
		return fmt.Sprintf("asserts a provider to providers.%s directly — a registry provider is wrapped, so this is false for every provider; use providers.As[providers.%s]", name, name)
	case plumbing:
		if rel != asFile {
			return fmt.Sprintf("asserts providers.%s outside %s — use providers.As", name, asFile)
		}
	}
	return ""
}

// TestOptionalInterfaces_AllClassified — every interface declared in this
// package is in ifaceKinds, and ifaceKinds names nothing that is gone.
func TestOptionalInterfaces_AllClassified(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "internal", "providers")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	declared := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range declaredInterfaces(f) {
			declared[name] = true
		}
	}
	for name := range declared {
		if _, ok := ifaceKinds[name]; !ok {
			t.Errorf("interface providers.%s is not classified in ifaceKinds. If a driver may implement it and callers discover that on a Provider value, it is `optional`: callers must reach it with providers.As (a registry provider is a wrapper), never a type assertion.", name)
		}
	}
	for name := range ifaceKinds {
		if !declared[name] {
			t.Errorf("ifaceKinds names providers.%s, which this package no longer declares", name)
		}
	}
}

// TestOptionalInterfaces_ReachedOnlyThroughAs walks the module's non-test Go
// sources and fails on a type assertion (or type-switch case) to an optional
// provider interface or to a concrete driver type.
func TestOptionalInterfaces_ReachedOnlyThroughAs(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
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
			inProviders := filepath.ToSlash(filepath.Dir(rel)) == "internal/providers"
			imports := importsOf(f)
			for _, typ := range assertedTypes(f) {
				if msg := censusViolation(typ, rel, inProviders, imports); msg != "" {
					violations = append(violations, fmt.Sprintf("%s: %s", fset.Position(typ.Pos()), msg))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// A walk that found nothing to look at proves nothing.
	if files < 100 {
		t.Fatalf("the census parsed only %d files — it is not walking the module", files)
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}
}

// TestOptionalInterfaces_CensusCatchesEachShape — the walk is only a guard if
// it can fail. Each shape it must catch is fed to it here, beside the shapes it
// must leave alone.
func TestOptionalInterfaces_CensusCatchesEachShape(t *testing.T) {
	const header = `package x
import (
	"github.com/denn-gubsky/loomcycle/internal/providers"
	prov "github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/store"
)
var _ = providers.As[providers.KeyedProvider]
var _ prov.Provider
var _ *codejs.Provider
var _ store.Store
`
	for _, tc := range []struct {
		name, rel, body string
		inProviders     bool
		wantViolation   bool
	}{
		{"direct assertion", "internal/api/http/x.go", `func f(p providers.Provider) { _, _ = p.(providers.KeyedProvider) }`, false, true},
		{"renamed import", "internal/api/http/x.go", `func f(p prov.Provider) { _, _ = p.(prov.ThinkingDowngrader) }`, false, true},
		{"type switch case", "internal/loop/x.go", `func f(p providers.Provider) { switch p.(type) { case providers.ModelIdentifier: } }`, false, true},
		{"concrete driver", "cmd/loomcycle/x.go", `func f(p providers.Provider) { _, _ = p.(*codejs.Provider) }`, false, true},
		{"unwrapper outside As", "internal/resolve/x.go", `func f(p providers.Provider) { _, _ = p.(providers.Unwrapper) }`, false, true},
		{"bare name inside the package", "internal/providers/x.go", `func f(p Provider) { _, _ = p.(RunEnder) }`, true, true},
		{"through As", "internal/api/http/x.go", `func f(p providers.Provider) { _, _ = providers.As[providers.KeyedProvider](p) }`, false, false},
		{"another package's type", "internal/api/http/x.go", `func f(s any) { _, _ = s.(store.Store) }`, false, false},
		{"a driver on its own inner", "internal/providers/deepseek/x.go", `func f(p providers.Provider) { _, _ = p.(*codejs.Provider) }`, false, false},
		{"a same-named local type elsewhere", "internal/api/http/x.go", `type KeyedProvider interface{}; func f(p any) { _, _ = p.(KeyedProvider) }`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := header + tc.body
			if tc.inProviders {
				src = "package providers\n" + tc.body
			}
			f, err := parser.ParseFile(token.NewFileSet(), tc.rel, src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := false
			for _, typ := range assertedTypes(f) {
				if censusViolation(typ, tc.rel, tc.inProviders, importsOf(f)) != "" {
					got = true
				}
			}
			if got != tc.wantViolation {
				t.Fatalf("violation = %v, want %v", got, tc.wantViolation)
			}
		})
	}
}

// As is generic over what it looks for; this pins that it walks a chain and
// stops at a provider that does not unwrap.
type chainLink struct {
	providers.Provider
	inner providers.Provider
}

func (c chainLink) Unwrap() providers.Provider { return c.inner }

type keyed struct{ providers.Provider }

func (keyed) KeyEnvName() string { return "K" }

func TestAs_WalksTheWrapperChain(t *testing.T) {
	var p providers.Provider = chainLink{inner: chainLink{inner: keyed{}}}
	if kp, ok := providers.As[providers.KeyedProvider](p); !ok || kp.KeyEnvName() != "K" {
		t.Fatal("As did not find an interface two wrappers down")
	}
	if _, ok := providers.As[providers.ThinkingDowngrader](p); ok {
		t.Fatal("As found an interface nothing in the chain implements")
	}
	if _, ok := providers.As[providers.KeyedProvider](nil); ok {
		t.Fatal("As on a nil provider must find nothing")
	}
	// The outermost match wins: a wrapper that answers for itself is not
	// looked through.
	if got, ok := providers.As[chainLink](p); !ok || got.inner == nil {
		t.Fatal("As did not return the outermost match")
	}
}
