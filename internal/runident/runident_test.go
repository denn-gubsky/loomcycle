package runident_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

const modulePath = "github.com/denn-gubsky/loomcycle/"

// TestRunident_ImportsNothingFromThisModule guards the one property this
// package exists for. The memory backends and the remote document client read
// the run's tenant through it precisely so they need not import the tool layer
// above them; one intra-module import here and that dependency, or a cycle,
// comes back through this package instead.
func TestRunident_ImportsNothingFromThisModule(t *testing.T) {
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
				if strings.HasPrefix(path, modulePath) {
					t.Errorf("%s imports %s — runident must stay a leaf: packages "+
						"below the tool layer read the run identity through it.", name, path)
				}
			}
		}
	}
	if files == 0 {
		t.Fatal("parsed zero files — the scan is broken, so this guard proves nothing")
	}
}
