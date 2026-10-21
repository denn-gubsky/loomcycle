package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Store is a composition of domain interfaces so a consumer can depend on the
// slice it uses. That only holds while every method lives in exactly one
// domain: a method declared on Store directly is in no domain, and one
// declared in two domains compiles (identical signatures merge) but makes the
// split a lie. The domain list is read from Store's own declaration rather
// than restated here, so adding a domain needs no edit to this test.
func TestStore_IsDisjointUnionOfDomainInterfaces(t *testing.T) {
	ifaces := parsePackageInterfaces(t)

	storeIface, ok := ifaces["Store"]
	if !ok {
		t.Fatal("no `type Store interface` in package store")
	}

	// Interfaces from other packages that Store embeds, by selector.
	external := map[string]reflect.Type{
		"io.Closer": reflect.TypeOf((*io.Closer)(nil)).Elem(),
	}

	owner := map[string]string{} // method name -> domain that declares it
	claim := func(method, domain string) {
		if prev, dup := owner[method]; dup {
			t.Errorf("method %s is declared in both %s and %s; it must live in exactly one domain", method, prev, domain)
			return
		}
		owner[method] = domain
	}

	for _, f := range storeIface.Methods.List {
		if len(f.Names) > 0 {
			t.Errorf("Store declares method %s directly; put it in its domain interface", f.Names[0].Name)
			continue
		}
		switch e := f.Type.(type) {
		case *ast.Ident:
			dom, ok := ifaces[e.Name]
			if !ok {
				t.Errorf("Store embeds %s, which is not an interface declared in package store", e.Name)
				continue
			}
			for _, df := range dom.Methods.List {
				if len(df.Names) == 0 {
					t.Errorf("domain %s embeds another interface; keep domains flat so each method has one home", e.Name)
					continue
				}
				for _, n := range df.Names {
					claim(n.Name, e.Name)
				}
			}
		case *ast.SelectorExpr:
			name := exprString(e)
			rt, ok := external[name]
			if !ok {
				t.Errorf("Store embeds %s; add it to this test's external table", name)
				continue
			}
			for i := 0; i < rt.NumMethod(); i++ {
				claim(rt.Method(i).Name, name)
			}
		default:
			t.Errorf("Store embeds an unexpected element %s", exprString(f.Type))
		}
	}

	// The compiled method set must be exactly the union parsed above — this
	// ties the source reading to the type the backends actually implement.
	st := reflect.TypeOf((*Store)(nil)).Elem()
	compiled := map[string]bool{}
	for i := 0; i < st.NumMethod(); i++ {
		compiled[st.Method(i).Name] = true
	}
	var missing, extra []string
	for m := range compiled {
		if _, ok := owner[m]; !ok {
			missing = append(missing, m)
		}
	}
	for m := range owner {
		if !compiled[m] {
			extra = append(extra, m)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("Store methods in no domain interface: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("domain methods absent from Store's compiled method set: %v", extra)
	}
	if len(owner) == 0 {
		t.Fatal("parsed no domain methods; the test is not reading store.go")
	}
}

// parsePackageInterfaces returns every top-level interface type declared in
// the package's non-test source files, by name.
func parsePackageInterfaces(t *testing.T) map[string]*ast.InterfaceType {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.InterfaceType{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				if it, ok := ts.Type.(*ast.InterfaceType); ok {
					out[ts.Name.Name] = it
				}
			}
		}
	}
	return out
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	default:
		return reflect.TypeOf(e).String()
	}
}
