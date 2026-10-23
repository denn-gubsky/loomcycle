package providers_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The in-repo modules internal/providers may import: itself and the two leaf
// modules its go.mod requires. Everything else under loomcyclePath belongs to
// the root module.
const loomcyclePath = "github.com/denn-gubsky/loomcycle/"

var allowedInRepo = []string{
	loomcyclePath + "internal/providers",
	loomcyclePath + "internal/errkind",
	loomcyclePath + "internal/otel",
}

func allowedImport(path string) bool {
	for _, a := range allowedInRepo {
		if path == a || strings.HasPrefix(path, a+"/") {
			return true
		}
	}
	return false
}

// TestProviders_ImportsNothingFromTheRootModule keeps internal/providers a
// module that sits below the root.
//
// internal/providers is its own Go module (RFC FE), requiring only errkind and
// otel, so an import of a root package fails `go build`, `go vet` and
// `go test` run inside this directory. It does NOT fail a build from the
// repository root, where the root module supplies the package: this guard is
// the backstop that fails wherever the tests run. It reads every Go file
// under this directory, tests and every build tag included. A test that needs
// the loop, a tool or the store with a real driver belongs to that root
// package's tests, not here.
func TestProviders_ImportsNothingFromTheRootModule(t *testing.T) {
	fset := token.NewFileSet()
	files := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		files++
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(p, loomcyclePath) && !allowedImport(p) {
				t.Errorf("%s imports %s: internal/providers is its own module and "+
					"requires only errkind and otel of this repository. Take the "+
					"value as a parameter or an interface, or move the test to the "+
					"root package it exercises.", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// 16 packages: far fewer files means the walk is not reading the module.
	if files < 100 {
		t.Fatalf("parsed only %d files — the walk is broken, so this guard proves nothing", files)
	}
}
