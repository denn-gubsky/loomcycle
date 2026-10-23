package otel_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

const loomcyclePath = "github.com/denn-gubsky/loomcycle/"

// TestOtel_ImportsNothingFromThisModule keeps this package a leaf.
//
// internal/otel is its own Go module so that internal/providers, which
// records its calls as spans, can become one too. A module only builds
// against what its go.mod requires, so an import of a root package fails
// `go build` run inside this directory. It does NOT fail a build from the
// repository root, where the root module supplies the package: this guard is
// the in-package backstop that fails wherever the tests run. A loomcycle type
// a span needs is mirrored here as a plain field (see Config), never imported.
func TestOtel_ImportsNothingFromThisModule(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	files := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			files++
			for _, imp := range file.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if strings.HasPrefix(path, loomcyclePath) {
					t.Errorf("%s imports %s: internal/otel must stay a leaf, its own "+
						"module requires nothing of the root. Take the value as a "+
						"parameter or a field instead.", name, path)
				}
			}
		}
	}
	if files == 0 {
		t.Fatal("parsed zero files — the scan is broken, so this guard proves nothing")
	}
}
