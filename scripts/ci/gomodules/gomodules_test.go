package gomodules

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTree writes files (slash-separated paths relative to a new temp dir)
// and returns the dir.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestInTree_RootOnlyIsOneModule(t *testing.T) {
	root := writeTree(t, map[string]string{"go.mod": "module example.com/m\n\ngo 1.26\n"})
	got, err := InTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Module{{".", "example.com/m"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestInTree_ListsLocalReplacementsRootFirstAndSkipsTheRest(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod": `module example.com/m

go 1.26

require (
	example.com/m/internal/b v0.0.0
	example.com/m/internal/a v0.0.0
	example.com/other v1.0.0
)

replace example.com/m/internal/b => ./internal/b/

replace example.com/m/internal/a => ./internal/a

replace example.com/other => example.com/fork v1.2.3

replace example.com/outside => ../outside
`,
		"internal/a/go.mod": "module example.com/m/internal/a\n\ngo 1.26\n",
		"internal/b/go.mod": "module example.com/m/internal/b\n\ngo 1.26\n",
		// A module nobody replaces is not part of the build.
		"deploy/builder/go.mod": "module example.com/builder\n\ngo 1.26\n",
	})
	got, err := InTree(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []Module{
		{".", "example.com/m"},
		{"internal/a", "example.com/m/internal/a"},
		{"internal/b", "example.com/m/internal/b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestInTree_RefusesAReplacementGoCannotBuild(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"no go.mod in the directory": {
			"go.mod":           "module example.com/m\n\ngo 1.26\n\nreplace example.com/m/x => ./x\n",
			"x/placeholder.go": "package x\n",
		},
		"go.mod declares another path": {
			"go.mod":   "module example.com/m\n\ngo 1.26\n\nreplace example.com/m/x => ./x\n",
			"x/go.mod": "module example.com/m/y\n\ngo 1.26\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := InTree(writeTree(t, files))
			if err == nil || !strings.Contains(err.Error(), "replaces example.com/m/x with ./x") {
				t.Fatalf("got %v, want an error naming the directive", err)
			}
		})
	}
}

var mods = []Module{
	{".", "example.com/m"},
	{"internal/a", "example.com/m/internal/a"},
	{"internal/a/deep", "example.com/m/internal/a/deep"},
}

func TestOwner_LongestModulePathOnAPathBoundary(t *testing.T) {
	for ip, want := range map[string]string{
		"example.com/m":                     ".",
		"example.com/m/internal/x":          ".",
		"example.com/m/internal/a":          "internal/a",
		"example.com/m/internal/a/sub":      "internal/a",
		"example.com/m/internal/ab":         ".",
		"example.com/m/internal/a/deep/sub": "internal/a/deep",
	} {
		m, ok := Owner(mods, ip)
		if !ok || m.Dir != want {
			t.Errorf("Owner(%s) = %v, %v; want %s", ip, m, ok, want)
		}
	}
	if m, ok := Owner(mods, "example.com/other"); ok {
		t.Errorf("Owner(example.com/other) = %v; want none", m)
	}
}

func TestDirOwner_LongestDirectoryOnAPathBoundary(t *testing.T) {
	for p, want := range map[string]string{
		"go.mod":                    ".",
		"internal/x/x.go":           ".",
		"internal/a/go.mod":         "internal/a",
		"internal/a/sub/x.go":       "internal/a",
		"internal/ab/x.go":          ".",
		"internal/a/deep/x_test.go": "internal/a/deep",
		"internal/a":                "internal/a",
	} {
		m, ok := DirOwner(mods, p)
		if !ok || m.Dir != want {
			t.Errorf("DirOwner(%s) = %v, %v; want %s", p, m, ok, want)
		}
	}
}

// The repository's own go.mod must parse, and the root must come first: every
// caller relies on both. The root is named through its go.mod so that
// scripts/ci/affected sees this test read go.mod, not the whole repository.
func TestInTree_ReadsThisRepository(t *testing.T) {
	got, err := InTree(filepath.Dir(filepath.Join("..", "..", "..", "go.mod")))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Dir != "." || got[0].Path != "github.com/denn-gubsky/loomcycle" {
		t.Fatalf("got %v; want the root module first", got)
	}
}
