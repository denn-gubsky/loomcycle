package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/scripts/ci/gomodules"
)

var rootOnly = []gomodules.Module{{Dir: ".", Path: "m"}}

func fixture() []pkg {
	return []pkg{
		{importPath: "m/internal/store", module: ".", dir: "internal/store", hasTests: true,
			testDeps: map[string]bool{"m/internal/store": true}},
		{importPath: "m/internal/api", module: ".", dir: "internal/api", hasTests: true,
			testDeps: map[string]bool{"m/internal/api": true, "m/internal/store": true},
			reads:    []string{"adapters/ts/src/types.ts", "proto"}},
		{importPath: "m/internal/walker", module: ".", dir: "internal/walker", hasTests: true,
			reads: []string{""}},
		{importPath: "m/internal/notests", module: ".", dir: "internal/notests"},
		{importPath: "m/internal/leaf", module: ".", dir: "internal/leaf", hasTests: true,
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
	got := paths(selectPackages(rootOnly, fixture(), []string{"internal/store/store.go"}))
	want := []string{"m/internal/api", "m/internal/store", "m/internal/walker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_EmbeddedFileBelongsToTheEnclosingPackage(t *testing.T) {
	got := paths(selectPackages(rootOnly, fixture(), []string{"internal/store/migrations/0100_x.up.sql"}))
	want := []string{"m/internal/api", "m/internal/store", "m/internal/walker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_PackageWithoutTestsSelectsItsImporters(t *testing.T) {
	got := paths(selectPackages(rootOnly, fixture(), []string{"internal/notests/x.go"}))
	want := []string{"m/internal/leaf", "m/internal/walker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_TestFileChangeSelectsOnlyItsOwnPackage(t *testing.T) {
	got := paths(selectPackages(rootOnly, fixture(), []string{"internal/store/store_test.go"}))
	want := []string{"m/internal/store", "m/internal/walker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_TestAndSourceChangeInOnePackageReachesImporters(t *testing.T) {
	got := paths(selectPackages(rootOnly, fixture(), []string{"internal/store/store_test.go", "internal/store/store.go"}))
	want := []string{"m/internal/api", "m/internal/store", "m/internal/walker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_NonGoFileSelectsTheTestsThatReadIt(t *testing.T) {
	for _, f := range []string{"adapters/ts/src/types.ts", "proto/loomcycle.proto"} {
		got := paths(selectPackages(rootOnly, fixture(), []string{f}))
		want := []string{"m/internal/api", "m/internal/walker"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %v, want %v", f, got, want)
		}
	}
}

func TestSelectPackages_ReadPrefixDoesNotMatchASiblingWithTheSamePrefix(t *testing.T) {
	got := paths(selectPackages(rootOnly, fixture(), []string{"protocol/notes.md"}))
	want := []string{"m/internal/walker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_DocsChangeSelectsOnlyRepoWalkers(t *testing.T) {
	got := paths(selectPackages(rootOnly, fixture(), []string{"docs/CONFIGURATION.md"}))
	want := []string{"m/internal/walker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestWithoutWalkers_DocsChangeSelectsNothing(t *testing.T) {
	if got := selectPackages(rootOnly, withoutWalkers(fixture()), []string{"docs/CONFIGURATION.md"}); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
	got := paths(selectPackages(rootOnly, withoutWalkers(fixture()), []string{"proto/loomcycle.proto"}))
	if want := []string{"m/internal/api"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_NoChangeSelectsNothing(t *testing.T) {
	if got := selectPackages(rootOnly, fixture(), nil); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

func TestSelectPackages_FullRunFilesSelectEveryPackageWithTests(t *testing.T) {
	for _, f := range []string{"go.mod", "go.sum", ".github/workflows/ci.yml", "scripts/ci/affected/main.go"} {
		got := paths(selectPackages(rootOnly, fixture(), []string{"docs/x.md", f}))
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
	pkgs, err := loadPackages(strings.TrimSpace(root), gomodules.Module{Dir: ".", Path: "github.com/denn-gubsky/loomcycle"}, "./internal/api/grpc/")
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

// twoModules is the fixture with a nested module at internal/nest (module
// "m/internal/nest") that a root package imports. The root also has a package
// at internal/, the directory around the nested module, to show that a file
// of the nested module is never charged to it.
var twoModules = []gomodules.Module{
	{Dir: ".", Path: "m"},
	{Dir: "internal/nest", Path: "m/internal/nest"},
}

func nestedFixture() []pkg {
	return []pkg{
		{importPath: "m/internal", module: ".", dir: "internal", hasTests: true,
			testDeps: map[string]bool{"m/internal": true}},
		{importPath: "m/internal/user", module: ".", dir: "internal/user", hasTests: true,
			testDeps: map[string]bool{"m/internal/user": true, "m/internal/nest/kind": true}},
		{importPath: "m/internal/nest/kind", module: "internal/nest", dir: "internal/nest/kind", hasTests: true,
			testDeps: map[string]bool{"m/internal/nest/kind": true}},
		{importPath: "m/internal/other", module: ".", dir: "internal/other", hasTests: true,
			testDeps: map[string]bool{"m/internal/other": true}},
	}
}

func TestSelectPackages_NestedModuleChangeReachesItsRootImporters(t *testing.T) {
	got := paths(selectPackages(twoModules, nestedFixture(), []string{"internal/nest/kind/kind.go"}))
	want := []string{"m/internal/nest/kind", "m/internal/user"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_NestedModuleFileIsNotChargedToTheEnclosingRootPackage(t *testing.T) {
	// internal/nest/README.md sits in the nested module, outside any of its
	// packages. Walking up past the module's directory would charge it to the
	// root's internal package; it belongs to no package instead.
	if got := selectPackages(twoModules, nestedFixture(), []string{"internal/nest/README.md"}); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
	// The same walk inside the root module still finds the package above.
	got := paths(selectPackages(twoModules, nestedFixture(), []string{"internal/testdata/x.json"}))
	if want := []string{"m/internal"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectPackages_NestedModuleFilesSelectEveryPackage(t *testing.T) {
	for _, f := range []string{"internal/nest/go.mod", "internal/nest/go.sum", "scripts/ci/gomodules/gomodules.go"} {
		got := paths(selectPackages(twoModules, nestedFixture(), []string{f}))
		want := []string{"m/internal", "m/internal/nest/kind", "m/internal/other", "m/internal/user"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %v, want %v", f, got, want)
		}
	}
	// A go.mod the root does not replace (a standalone module) is no module of
	// the build, so it selects nothing.
	if got := selectPackages(twoModules, nestedFixture(), []string{"deploy/builder/go.mod"}); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

func TestMatchAll_PatternsSpanModulesInGoListOrder(t *testing.T) {
	pkgs := nestedFixture()
	cases := []struct {
		within, exclude string
		want            []string
	}{
		{"./...", "", []string{"m/internal", "m/internal/nest/kind", "m/internal/other", "m/internal/user"}},
		{"./internal/nest/...", "", []string{"m/internal/nest/kind"}},
		{"./internal/nest/kind/", "", []string{"m/internal/nest/kind"}},
		{"m/internal/nest/...", "", []string{"m/internal/nest/kind"}},
		// Pattern order, each pattern sorted, a repeat listed once.
		{"./internal/user/ ./internal/... ./internal/other", "", []string{
			"m/internal/user", "m/internal", "m/internal/nest/kind", "m/internal/other"}},
		// With an exclusion, sorted.
		{"./internal/user/ ./internal/other ./internal/nest/...", "./internal/other",
			[]string{"m/internal/nest/kind", "m/internal/user"}},
		{"./...", "./internal/nest/...", []string{"m/internal", "m/internal/other", "m/internal/user"}},
		{"./internal/nes", "", nil},
		{"./internal/nes...", "", []string{"m/internal/nest/kind"}},
	}
	for _, c := range cases {
		got, err := matchAll(pkgs, c.within, c.exclude)
		if err != nil {
			t.Fatalf("%q - %q: %v", c.within, c.exclude, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q - %q: got %v, want %v", c.within, c.exclude, got, c.want)
		}
	}
	for _, bad := range []string{"all", "std", "../...", "./internal/../../x"} {
		if _, err := matchAll(pkgs, bad, ""); err == nil {
			t.Errorf("matchAll(%q) = nil error; want a refusal", bad)
		}
	}
}

// The graph merge on a real tree: two modules, the root importing the nested
// one through a replace directive, each listed from its own directory.
func TestLoadPackages_MergesTheGraphsOfEveryInTreeModule(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/m\n\ngo 1.21\n\nrequire example.com/m/nest v0.0.0\n\nreplace example.com/m/nest => ./nest\n")
	write("user/user.go", "package user\n\nimport \"example.com/m/nest\"\n\nvar K = nest.K\n")
	write("user/user_test.go", "package user\n\nimport \"testing\"\n\nfunc TestK(t *testing.T) { _ = K }\n")
	write("nest/go.mod", "module example.com/m/nest\n\ngo 1.21\n")
	write("nest/nest.go", "package nest\n\nconst K = 1\n")
	write("nest/nest_test.go", "package nest\n\nimport \"testing\"\n\nfunc TestK(t *testing.T) { _ = K }\n")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")

	mods, err := gomodules.InTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(mods) != 2 || mods[1].Dir != "nest" {
		t.Fatalf("modules %v; want the root and nest", mods)
	}
	var pkgs []pkg
	for _, m := range mods {
		ps, err := loadPackages(root, m, "./...")
		if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, ps...)
	}
	byPath := map[string]pkg{}
	for _, p := range pkgs {
		byPath[p.importPath] = p
	}
	nest, user := byPath["example.com/m/nest"], byPath["example.com/m/user"]
	if nest.module != "nest" || nest.dir != "nest" || !nest.hasTests {
		t.Fatalf("nest = %+v; want module nest, dir nest, with tests", nest)
	}
	if user.module != "." || !user.testDeps["example.com/m/nest"] {
		t.Fatalf("user = %+v; want module . and a test dependency on example.com/m/nest", user)
	}
	got := paths(selectPackages(mods, pkgs, []string{"nest/nest.go"}))
	if want := []string{"example.com/m/nest", "example.com/m/user"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
