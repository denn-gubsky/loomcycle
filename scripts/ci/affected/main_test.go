package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixture() []pkg {
	return []pkg{
		{importPath: "m/internal/store", dir: "internal/store", hasTests: true,
			testDeps: map[string]bool{"m/internal/store": true}},
		{importPath: "m/internal/api", dir: "internal/api", hasTests: true,
			testDeps: map[string]bool{"m/internal/api": true, "m/internal/store": true},
			reads:    []string{"adapters/ts/src/types.ts", "proto"}},
		{importPath: "m/internal/walker", dir: "internal/walker", hasTests: true,
			reads: []string{""}},
		{importPath: "m/internal/notests", dir: "internal/notests"},
		{importPath: "m/internal/leaf", dir: "internal/leaf", hasTests: true,
			testDeps: map[string]bool{"m/internal/leaf": true, "m/internal/notests": true}},
	}
}

func paths(sel []selection) []string {
	var out []string
	for _, s := range sel {
		out = append(out, s.importPath)
	}
	return out
}

func TestSelectPackages_ChangeReachesImportersThroughTestDeps(t *testing.T) {
	got := paths(selectPackages(fixture(), []string{"internal/store/store.go"}))
	want := []string{"m/internal/api", "m/internal/store", "m/internal/walker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_EmbeddedFileBelongsToTheEnclosingPackage(t *testing.T) {
	got := paths(selectPackages(fixture(), []string{"internal/store/migrations/0100_x.up.sql"}))
	want := []string{"m/internal/api", "m/internal/store", "m/internal/walker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_PackageWithoutTestsSelectsItsImporters(t *testing.T) {
	got := paths(selectPackages(fixture(), []string{"internal/notests/x.go"}))
	want := []string{"m/internal/leaf", "m/internal/walker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_TestFileChangeSelectsOnlyItsOwnPackage(t *testing.T) {
	got := paths(selectPackages(fixture(), []string{"internal/store/store_test.go"}))
	want := []string{"m/internal/store", "m/internal/walker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_TestAndSourceChangeInOnePackageReachesImporters(t *testing.T) {
	got := paths(selectPackages(fixture(), []string{"internal/store/store_test.go", "internal/store/store.go"}))
	want := []string{"m/internal/api", "m/internal/store", "m/internal/walker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_NonGoFileSelectsTheTestsThatReadIt(t *testing.T) {
	for _, f := range []string{"adapters/ts/src/types.ts", "proto/loomcycle.proto"} {
		got := paths(selectPackages(fixture(), []string{f}))
		want := []string{"m/internal/api", "m/internal/walker"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %v, want %v", f, got, want)
		}
	}
}

func TestSelectPackages_ReadPrefixDoesNotMatchASiblingWithTheSamePrefix(t *testing.T) {
	got := paths(selectPackages(fixture(), []string{"protocol/notes.md"}))
	want := []string{"m/internal/walker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_DocsChangeSelectsOnlyRepoWalkers(t *testing.T) {
	got := paths(selectPackages(fixture(), []string{"docs/CONFIGURATION.md"}))
	want := []string{"m/internal/walker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestWithoutWalkers_DocsChangeSelectsNothing(t *testing.T) {
	if got := selectPackages(withoutWalkers(fixture()), []string{"docs/CONFIGURATION.md"}); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
	got := paths(selectPackages(withoutWalkers(fixture()), []string{"proto/loomcycle.proto"}))
	if want := []string{"m/internal/api"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_NoChangeSelectsNothing(t *testing.T) {
	if got := selectPackages(fixture(), nil); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

func TestSelectPackages_FullRunFilesSelectEveryPackageWithTests(t *testing.T) {
	for _, f := range []string{"go.mod", "go.sum", ".github/workflows/ci.yml", "scripts/ci/affected/main.go"} {
		got := paths(selectPackages(fixture(), []string{"docs/x.md", f}))
		want := []string{"m/internal/api", "m/internal/leaf", "m/internal/store", "m/internal/walker"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %v, want %v", f, got, want)
		}
	}
}

func TestRelativeReads_ResolvesLiteralsAndJoinsAgainstThePackageDir(t *testing.T) {
	src := `package x

import (
	"os"
	"path/filepath"
)

var driver = "anthropic"

func f() {
	_, _ = os.ReadFile("../../adapters/ts/src/types.ts")
	_, _ = os.ReadFile(filepath.Join("..", "..", "bundles", "chat", "loomcycle.yaml"))
	_, _ = os.ReadFile(filepath.Join("..", "..", "providers", driver, "driver.go"))
	_ = filepath.Join("..", "..")
	_ = filepath.Join("testdata", "..", "x")
	_ = "../../../../etc/passwd"
	_ = "not/relative"
}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "x_test.go")
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got := relativeReads(file, "internal/api")
	want := []string{
		"adapters/ts/src/types.ts",
		"bundles/chat/loomcycle.yaml",
		"providers",
		"",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The selector is only as good as its view of this repository: a drift test
// that reads the TS adapter must be found by it, or a PR that changes the
// adapter would skip the test that compares it with Go.
func TestLoadPackages_FindsTheReadsOfThisRepository(t *testing.T) {
	root, err := gitOutput("rev-parse", "--show-toplevel")
	if err != nil {
		t.Skip("not in a git checkout:", err)
	}
	pkgs, err := loadPackages(strings.TrimSpace(root), "./internal/api/grpc/")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pkgs {
		if p.dir != "internal/api/grpc" {
			continue
		}
		if !p.hasTests || len(p.testDeps) == 0 {
			t.Fatalf("internal/api/grpc: hasTests=%v deps=%d", p.hasTests, len(p.testDeps))
		}
		for _, r := range p.reads {
			if r == "proto/loomcycle.proto" {
				return
			}
		}
		t.Fatalf("internal/api/grpc reads %q; want proto/loomcycle.proto among them", p.reads)
	}
	t.Fatal("internal/api/grpc not listed")
}

func TestCheckArgs_RefusesWhatGitOrGoListWouldReadAsAnOption(t *testing.T) {
	if err := checkArgs("HEAD^1", "HEAD", "./... ./internal/store/...", "./internal/api/http/"); err != nil {
		t.Fatalf("ordinary arguments refused: %v", err)
	}
	for _, c := range []struct{ base, head, within, exclude string }{
		{"--output=/tmp/x", "HEAD", "./...", ""},
		{"HEAD^1", "-p", "./...", ""},
		{"", "HEAD", "./...", ""},
		{"HEAD^1", "HEAD", "./... -toolexec=x", ""},
		{"HEAD^1", "HEAD", "./...", "-exec=x"},
	} {
		if err := checkArgs(c.base, c.head, c.within, c.exclude); err == nil {
			t.Errorf("checkArgs(%q, %q, %q, %q) = nil; want an error", c.base, c.head, c.within, c.exclude)
		}
	}
}
