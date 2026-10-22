// Command affected prints the Go packages whose tests a change can reach, so a
// pull request runs those tests instead of the whole suite.
//
// A package is selected when its test binary can see the change:
//
//   - a changed file belongs to the package itself (a .go file, or an embedded
//     file, testdata, a migration — anything under its directory that no
//     deeper package owns);
//   - a changed non-test file belongs to a package the test binary imports,
//     directly or transitively, test-only imports included (a _test.go file is
//     compiled into its own package's tests only, so it reaches no importer);
//   - a changed file sits under a path the package's tests open by a relative
//     literal ("../../adapters/ts/src/types.ts", filepath.Join("..", "..",
//     "bundles")). Drift tests compare Go against the TS adapter, the proto,
//     the docs; none of that is an import, so the import graph alone would
//     miss it. These paths are read from the test files' syntax, so the rule
//     cannot fall behind the tests.
//
// A test that builds its path at run time from something other than literals
// is not seen. That is the residual the full suite on main exists for: a pull
// request may miss such a failure, main does not.
//
// Some changes reach everything and select every package: go.mod, go.sum, the
// CI workflow, and this command itself.
//
// Usage:
//
//	affected [-base REV] [-head REV] [-within PATTERNS] [-exclude PATTERNS] [-explain] [-no-walkers]
//
// -no-walkers ignores tests that walk the whole repository (a root read). Those
// census tests look at every file, so every change selects them; CI uses this
// flag only to decide whether a change concerns Go at all, and then runs the
// selection without it, walkers included.
//
// -within restricts the output to the packages that space-separated go list
// patterns name, for the CI steps that test a fixed subset (the Postgres
// tiers, one race shard); -exclude removes the packages its patterns name.
// -explain writes to stderr why each package was selected. The output is one
// import path per line, sorted; no output means no test can see the change.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// fullRunFiles select every package when they change. The module files change
// what every package compiles against; the workflow and this command change
// how the selection itself is made, so they must be proven on everything.
var fullRunFiles = []string{
	"go.mod",
	"go.sum",
	".github/workflows/ci.yml",
}

const selfDir = "scripts/ci/affected"

type pkg struct {
	importPath string
	dir        string // repo-relative, slash-separated
	hasTests   bool
	testDeps   map[string]bool // import paths the test binary links
	reads      []string        // repo-relative paths the tests open; "" is the repo root
}

func main() {
	base := flag.String("base", "HEAD^1", "git revision the change is measured from")
	head := flag.String("head", "HEAD", "git revision of the change")
	within := flag.String("within", "./...", "space-separated go list patterns the output is restricted to")
	exclude := flag.String("exclude", "", "space-separated go list patterns removed from the output")
	explain := flag.Bool("explain", false, "write to stderr why each package was selected")
	noWalkers := flag.Bool("no-walkers", false, "ignore tests that walk the whole repository")
	flag.Parse()

	var why io.Writer
	if *explain {
		why = os.Stderr
	}
	if err := run(*base, *head, *within, *exclude, *noWalkers, os.Stdout, why); err != nil {
		fmt.Fprintln(os.Stderr, "affected:", err)
		os.Exit(1)
	}
}

func run(base, head, within, exclude string, noWalkers bool, out, why io.Writer) error {
	root, err := gitOutput("rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	root = strings.TrimSpace(root)

	diff, err := gitOutput("diff", "--name-only", "--no-renames", base, head)
	if err != nil {
		return err
	}
	changed := nonEmptyLines(diff)

	pkgs, err := loadPackages(root, "./...")
	if err != nil {
		return err
	}
	if noWalkers {
		pkgs = withoutWalkers(pkgs)
	}
	scope, err := listImportPaths(root, within)
	if err != nil {
		return err
	}
	if exclude != "" {
		drop, err := listImportPaths(root, exclude)
		if err != nil {
			return err
		}
		for ip := range drop {
			delete(scope, ip)
		}
	}

	for _, s := range selectPackages(pkgs, changed) {
		if !scope[s.importPath] {
			continue
		}
		fmt.Fprintln(out, s.importPath)
		if why != nil {
			fmt.Fprintf(why, "%s: %s\n", s.importPath, s.reason)
		}
	}
	return nil
}

type selection struct {
	importPath string
	reason     string
}

// selectPackages returns, sorted by import path, the packages with tests that
// the changed files (repo-relative, slash-separated) can reach.
func selectPackages(pkgs []pkg, changed []string) []selection {
	fullBy := ""
	for _, f := range changed {
		if isFullRunFile(f) {
			fullBy = f
			break
		}
	}

	byDir := make(map[string]string, len(pkgs))
	for _, p := range pkgs {
		byDir[p.dir] = p.importPath
	}
	// owners maps a package with a changed file to whether the change can reach
	// its importers. A _test.go file is compiled only into its own package's
	// test binary, so a change confined to test files selects the package and
	// nothing that imports it.
	owners := map[string]bool{}
	for _, f := range changed {
		if ip, ok := owningPackage(byDir, f); ok {
			owners[ip] = owners[ip] || !strings.HasSuffix(f, "_test.go")
		}
	}

	var sel []selection
	for _, p := range pkgs {
		if !p.hasTests {
			continue
		}
		reason := ""
		if fullBy != "" {
			reason = fullBy + " changes every package"
		} else {
			reason = reaches(p, owners, changed)
		}
		if reason != "" {
			sel = append(sel, selection{p.importPath, reason})
		}
	}
	sort.Slice(sel, func(i, j int) bool { return sel[i].importPath < sel[j].importPath })
	return sel
}

func isFullRunFile(f string) bool {
	for _, full := range fullRunFiles {
		if f == full {
			return true
		}
	}
	return strings.HasPrefix(f, selfDir+"/")
}

// reaches says why p's tests can see the change, or "" when they cannot.
func reaches(p pkg, owners map[string]bool, changed []string) string {
	if _, ok := owners[p.importPath]; ok {
		return "changed"
	}
	deps := make([]string, 0, len(p.testDeps))
	for dep := range p.testDeps {
		if owners[dep] {
			deps = append(deps, dep)
		}
	}
	if len(deps) > 0 {
		sort.Strings(deps)
		return "imports " + deps[0]
	}
	for _, r := range p.reads {
		for _, f := range changed {
			if r == "" || f == r || strings.HasPrefix(f, r+"/") {
				return "tests read " + f
			}
		}
	}
	return ""
}

// withoutWalkers drops every package's read of the repository root, so a
// census test is selected only by its own package and imports.
func withoutWalkers(pkgs []pkg) []pkg {
	out := make([]pkg, len(pkgs))
	for i, p := range pkgs {
		var reads []string
		for _, r := range p.reads {
			if r != "" {
				reads = append(reads, r)
			}
		}
		p.reads = reads
		out[i] = p
	}
	return out
}

// owningPackage returns the package whose directory is the nearest ancestor of
// f: a migration or testdata file belongs to the package above it.
func owningPackage(byDir map[string]string, f string) (string, bool) {
	for d := filepath.ToSlash(filepath.Dir(f)); ; d = filepath.ToSlash(filepath.Dir(d)) {
		if ip, ok := byDir[d]; ok {
			return ip, true
		}
		if d == "." || d == "/" {
			return "", false
		}
	}
}

// listEntry is the subset of `go list -test -json` this command reads.
type listEntry struct {
	ImportPath   string
	Dir          string
	ForTest      string
	Deps         []string
	TestGoFiles  []string
	XTestGoFiles []string
}

func loadPackages(root, pattern string) ([]pkg, error) {
	cmd := exec.Command("go", "list", "-test", "-e",
		"-json=ImportPath,Dir,ForTest,Deps,TestGoFiles,XTestGoFiles", pattern)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %v: %s", err, stderr.String())
	}

	var bases []listEntry
	testDeps := map[string]map[string]bool{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	for dec.More() {
		var e listEntry
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("decode go list output: %v", err)
		}
		switch {
		case strings.HasSuffix(e.ImportPath, ".test"):
			deps := map[string]bool{}
			for _, d := range e.Deps {
				deps[stripTestVariant(d)] = true
			}
			testDeps[strings.TrimSuffix(e.ImportPath, ".test")] = deps
		case e.ForTest == "" && !strings.Contains(e.ImportPath, " ["):
			bases = append(bases, e)
		}
	}

	pkgs := make([]pkg, 0, len(bases))
	for _, e := range bases {
		rel, err := filepath.Rel(root, e.Dir)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		p := pkg{importPath: e.ImportPath, dir: rel, testDeps: testDeps[e.ImportPath]}
		files := append(append([]string{}, e.TestGoFiles...), e.XTestGoFiles...)
		p.hasTests = len(files) > 0
		for _, f := range files {
			p.reads = append(p.reads, relativeReads(filepath.Join(e.Dir, f), rel)...)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// stripTestVariant maps "p [q.test]" to "p".
func stripTestVariant(ip string) string {
	if i := strings.Index(ip, " ["); i >= 0 {
		return ip[:i]
	}
	return ip
}

// relativeReads returns the repo-relative paths a test file names by climbing
// out of its package with "..": string literals that start with "../", and
// filepath.Join / path.Join calls whose leading literal arguments start with
// "..". A Join stops at its first non-literal argument, so a computed tail
// widens the path to its literal prefix — over-selection, never under. Paths
// that leave the repository (sandbox-escape fixtures) are dropped.
func relativeReads(file, pkgDir string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		// go list already parsed it; a file it accepts and we cannot read is a
		// toolchain mismatch, and missing reads only narrows a PR run.
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(parts ...string) {
		rel := filepath.ToSlash(filepath.Join(append([]string{pkgDir}, parts...)...))
		if rel == ".." || strings.HasPrefix(rel, "../") {
			return
		}
		if rel == "." {
			rel = ""
		}
		if !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if !isJoin(n.Fun) {
				return true
			}
			var lits []string
			for _, a := range n.Args {
				s, ok := stringLit(a)
				if !ok {
					break
				}
				lits = append(lits, s)
			}
			if len(lits) > 0 && lits[0] == ".." {
				add(lits...)
			}
		case *ast.BasicLit:
			if s, ok := stringLit(n); ok && strings.HasPrefix(s, "../") {
				add(s)
			}
		}
		return true
	})
	return out
}

func isJoin(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Join" {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && (x.Name == "filepath" || x.Name == "path")
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func listImportPaths(root, patterns string) (map[string]bool, error) {
	cmd := exec.Command("go", append([]string{"list", "-e"}, strings.Fields(patterns)...)...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s: %v: %s", patterns, err, stderr.String())
	}
	set := map[string]bool{}
	for _, l := range nonEmptyLines(string(raw)) {
		set[l] = true
	}
	return set, nil
}

func gitOutput(args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("git", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out), nil
}

func nonEmptyLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out = append(out, l)
		}
	}
	return out
}
