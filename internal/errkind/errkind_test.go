package errkind_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/errkind"
)

const modulePath = "github.com/denn-gubsky/loomcycle/"

// TestErrkind_ImportsNothingFromThisModule is the invariant that makes this
// package worth existing, so it is guarded rather than trusted.
//
// internal/providers sits near the floor of the import graph and has to carry
// a category on a run event. It can only do that while errkind depends on
// nothing of ours. Add one intra-module import here — a classifier, a
// renderer, a convenience that reaches for internal/store — and providers can
// no longer import it, the cycle comes back, and the reason the package was
// split out is gone.
//
// The failure would not be a compile error at this line. It would surface as a
// cycle somewhere else entirely, in whatever unlucky package next tried to use
// the type, which is exactly the kind of thing worth failing loudly and close
// to the cause.
func TestErrkind_ImportsNothingFromThisModule(t *testing.T) {
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
					t.Errorf("%s imports %s — errkind must stay a leaf. "+
						"internal/providers can only carry a category while this "+
						"package depends on nothing of ours; a classifier belongs in "+
						"internal/errclassify and a renderer belongs with its transport.",
						name, path)
				}
			}
		}
	}
	if files == 0 {
		t.Fatal("parsed zero files — the scan is broken, so this guard proves nothing")
	}
}

// The vocabulary itself: four values, distinct, and spelled the way every
// transport already renders them. These strings are a wire contract — they
// appear in MCP structuredContent and in gRPC ErrorInfo metadata — so a change
// has to be deliberate rather than incidental.
func TestErrkind_CategoryStringsAreTheWireContract(t *testing.T) {
	got := map[errkind.Category]string{
		errkind.CategoryTransient:  "transient",
		errkind.CategoryValidation: "validation",
		errkind.CategoryBusiness:   "business",
		errkind.CategoryPermission: "permission",
	}
	if len(got) != 4 {
		t.Fatalf("expected four distinct categories, got %d — two constants share a value", len(got))
	}
	for c, want := range got {
		if string(c) != want {
			t.Errorf("category renders %q, want %q", string(c), want)
		}
	}
}

// A zero Info is "not classified", and must stay distinguishable from a
// classified one. There is deliberately no "unknown" category to fall back to.
func TestErrkind_ZeroInfoIsUnclassified(t *testing.T) {
	var zero errkind.Info
	if zero.Category != "" {
		t.Errorf("zero Info has category %q — there is no default bucket by design", zero.Category)
	}
	if zero.RetryAfter != nil {
		t.Error("zero Info carries a backoff hint")
	}
}
