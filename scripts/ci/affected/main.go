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
// Some changes reach everything and select every package: any in-tree module's
// go.mod or go.sum, the CI workflow, and this command itself.
//
// The graph spans every in-tree module (scripts/ci/gomodules: the root module
// and each module the root go.mod replaces with a directory in the tree). Each
// module's packages are listed from its own directory, because `go list ./...`
// from the root does not enter a nested module; import paths are the same
// either way, so the graphs union into one. A changed file belongs to a
// package of the module whose directory holds it, never to one of an
// enclosing module.
//
// Usage:
//
//	affected [-base REV] [-head REV] [-within PATTERNS] [-exclude PATTERNS] [-explain] [-no-walkers]
//	affected -all [-within PATTERNS] [-exclude PATTERNS]
//
// -no-walkers ignores tests that walk the whole repository (a root read). Those
// census tests look at every file, so every change selects them; CI uses this
// flag only to decide whether a change concerns Go at all, and then runs the
// selection without it, walkers included.
//
// -within restricts the output to the packages that space-separated go list
// patterns name, for the CI steps that test a fixed subset (the Postgres
// tiers, one race shard); -exclude removes the packages its patterns name.
// The patterns are matched as go list matches them, against the packages of
// every in-tree module: a relative pattern ("./internal/store/...") by
// directory, any other by import path. The go list keywords (all, std, cmd, ...)
// are refused.
// -explain writes to stderr why each package was selected. The output is one
// import path per line, sorted; no output means no test can see the change.
//
// -all prints, with no diff, every package -within names less what -exclude
// names, tests or not, in the order go list would print it: by pattern, each
// pattern's packages sorted, or all of them sorted when -exclude is set. It is
// what a CI step runs outside a pull request.
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
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/scripts/ci/gomodules"
)

// fullRunFiles select every package when they change. A module's go.mod and
// go.sum change what its packages, and everything importing them, compile
// against; the workflow and this command change how the selection itself is
// made, so they must be proven on everything.
func fullRunFiles(mods []gomodules.Module) []string {
	files := []string{".github/workflows/ci.yml"}
	for _, m := range mods {
		files = append(files, path.Join(m.Dir, "go.mod"), path.Join(m.Dir, "go.sum"))
	}
	return files
}

// selfDirs hold this command and the module discovery it is built on.
var selfDirs = []string{"scripts/ci/affected", "scripts/ci/gomodules"}

type pkg struct {
	importPath string
	module     string // directory of the in-tree module it belongs to
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
	all := flag.Bool("all", false, "print every package the patterns name, with no diff")
	flag.Parse()

	var why io.Writer
	if *explain {
		why = os.Stderr
	}
	var err error
	if *all {
		err = runAll(*within, *exclude, os.Stdout)
	} else {
		err = run(*base, *head, *within, *exclude, *noWalkers, os.Stdout, why)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "affected:", err)
		os.Exit(1)
	}
}

func repoModules() (string, []gomodules.Module, error) {
	root, err := gitOutput("rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil, err
	}
	root = strings.TrimSpace(root)
	mods, err := gomodules.InTree(root)
	return root, mods, err
}

func run(base, head, within, exclude string, noWalkers bool, out, why io.Writer) error {
	if err := checkArgs(base, head, within, exclude); err != nil {
		return err
	}
	root, mods, err := repoModules()
	if err != nil {
		return err
	}

	diff, err := gitOutput("diff", "--name-only", "--no-renames", base, head)
	if err != nil {
		return err
	}
	changed := nonEmptyLines(diff)

	var pkgs []pkg
	for _, m := range mods {
		ps, err := loadPackages(root, m, "./...")
		if err != nil {
			return err
		}
		pkgs = append(pkgs, ps...)
	}
	if noWalkers {
		pkgs = withoutWalkers(pkgs)
	}
	scope, err := inScope(pkgs, within, exclude)
	if err != nil {
		return err
	}

	for _, s := range selectPackages(mods, pkgs, changed) {
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

// checkArgs refuses a revision or a package pattern that starts with "-".
// They are handed to git and go list as arguments, with no shell, so nothing
// can run; but a leading dash would be read as an option (git diff
// --output=FILE writes wherever it is told). A revision or a go list pattern
// never needs one.
func checkArgs(base, head, within, exclude string) error {
	for _, rev := range []string{base, head} {
		if rev == "" || strings.HasPrefix(rev, "-") {
			return fmt.Errorf("revision %q: want a commit, not an option", rev)
		}
	}
	for _, pattern := range append(strings.Fields(within), strings.Fields(exclude)...) {
		if strings.HasPrefix(pattern, "-") {
			return fmt.Errorf("package pattern %q: want a go list pattern, not an option", pattern)
		}
	}
	return nil
}

// selectPackages returns, sorted by import path, the packages with tests that
// the changed files (repo-relative, slash-separated) can reach. mods are the
// in-tree modules pkgs come from.
func selectPackages(mods []gomodules.Module, pkgs []pkg, changed []string) []selection {
	full := fullRunFiles(mods)
	fullBy := ""
	for _, f := range changed {
		if isFullRunFile(full, f) {
			fullBy = f
			break
		}
	}

	byDir := make(map[string]pkg, len(pkgs))
	for _, p := range pkgs {
		byDir[p.dir] = p
	}
	// owners maps a package with a changed file to whether the change can reach
	// its importers. A _test.go file is compiled only into its own package's
	// test binary, so a change confined to test files selects the package and
	// nothing that imports it.
	owners := map[string]bool{}
	for _, f := range changed {
		if ip, ok := owningPackage(mods, byDir, f); ok {
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

func isFullRunFile(full []string, f string) bool {
	for _, ff := range full {
		if f == ff {
			return true
		}
	}
	for _, d := range selfDirs {
		if strings.HasPrefix(f, d+"/") {
			return true
		}
	}
	return false
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
// f: a migration or testdata file belongs to the package above it. The search
// stays inside the module whose directory holds f, so a file of a nested
// module is never charged to a package of the module around it.
func owningPackage(mods []gomodules.Module, byDir map[string]pkg, f string) (string, bool) {
	mod, ok := gomodules.DirOwner(mods, f)
	if !ok {
		return "", false
	}
	for d := path.Dir(f); ; d = path.Dir(d) {
		if p, ok := byDir[d]; ok && p.module == mod.Dir {
			return p.importPath, true
		}
		if d == mod.Dir || d == "." || d == "/" {
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

// loadPackages lists the packages pattern names in module m, run from m's
// directory; root is the repository root that package directories are made
// relative to.
func loadPackages(root string, m gomodules.Module, pattern string) ([]pkg, error) {
	// #nosec G204 -- fixed command and flags; pattern is "./..." or a pattern checkArgs refused to let start with "-".
	cmd := exec.Command("go", "list", "-test", "-e",
		"-json=ImportPath,Dir,ForTest,Deps,TestGoFiles,XTestGoFiles", pattern)
	cmd.Dir = filepath.Join(root, filepath.FromSlash(m.Dir))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list in %s: %v: %s", m.Dir, err, stderr.String())
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
		p := pkg{importPath: e.ImportPath, module: m.Dir, dir: rel, testDeps: testDeps[e.ImportPath]}
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

// runAll prints every package within names less those exclude names, across
// the in-tree modules, in the order go list prints them.
func runAll(within, exclude string, out io.Writer) error {
	if err := checkArgs("HEAD", "HEAD", within, exclude); err != nil {
		return err
	}
	root, mods, err := repoModules()
	if err != nil {
		return err
	}
	var pkgs []pkg
	for _, m := range mods {
		ps, err := listPackages(root, m)
		if err != nil {
			return err
		}
		pkgs = append(pkgs, ps...)
	}
	list, err := matchAll(pkgs, within, exclude)
	if err != nil {
		return err
	}
	for _, ip := range list {
		fmt.Fprintln(out, ip)
	}
	return nil
}

// matchAll returns the import paths within names less those exclude names.
// Without exclude the order is go list's for the same patterns: pattern by
// pattern, each one's packages sorted, a package named twice listed once.
// With exclude it is sorted, as the comm(1) of two sorted lists it replaces.
func matchAll(pkgs []pkg, within, exclude string) ([]string, error) {
	sorted := append([]pkg(nil), pkgs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].importPath < sorted[j].importPath })

	drop := map[string]bool{}
	for _, pattern := range strings.Fields(exclude) {
		match, err := compilePattern(pattern)
		if err != nil {
			return nil, err
		}
		for _, p := range sorted {
			if match(p) {
				drop[p.importPath] = true
			}
		}
	}

	seen := map[string]bool{}
	var out []string
	for _, pattern := range strings.Fields(within) {
		match, err := compilePattern(pattern)
		if err != nil {
			return nil, err
		}
		for _, p := range sorted {
			if match(p) && !seen[p.importPath] && !drop[p.importPath] {
				seen[p.importPath] = true
				out = append(out, p.importPath)
			}
		}
	}
	if exclude != "" {
		sort.Strings(out)
	}
	return out, nil
}

// inScope is the set of import paths among pkgs that within names less those
// exclude names.
func inScope(pkgs []pkg, within, exclude string) (map[string]bool, error) {
	list, err := matchAll(pkgs, within, exclude)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(list))
	for _, ip := range list {
		set[ip] = true
	}
	return set, nil
}

// goListKeywords are the patterns go list reads as a package set rather than a
// path. None names a set of this repository's packages a CI step wants.
var goListKeywords = map[string]bool{"all": true, "std": true, "cmd": true, "main": true, "tool": true, "work": true}

// compilePattern returns a matcher for one go list pattern, as go list run in
// the repository root would apply it if every in-tree module were the root
// module: a relative pattern ("./x/...", "./x/", ".") against the package's
// directory, anything else against its import path. "..." matches any
// string, and a trailing "/..." also matches the path before it.
func compilePattern(pattern string) (func(pkg) bool, error) {
	if goListKeywords[pattern] {
		return nil, fmt.Errorf("package pattern %q: a go list keyword, want a path pattern", pattern)
	}
	if !strings.HasPrefix(pattern, ".") {
		re := patternRegexp(pattern)
		return func(p pkg) bool { return re.MatchString(p.importPath) }, nil
	}
	rel := path.Clean(filepath.ToSlash(pattern))
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, fmt.Errorf("package pattern %q leaves the repository", pattern)
	}
	re := patternRegexp(rel)
	return func(p pkg) bool { return re.MatchString(p.dir) }, nil
}

// patternRegexp is cmd/go's pattern match (internal/pkgpattern.MatchPattern)
// without its vendor rule; this repository vendors nothing.
func patternRegexp(pattern string) *regexp.Regexp {
	re := strings.ReplaceAll(regexp.QuoteMeta(pattern), `\.\.\.`, `.*`)
	if strings.HasSuffix(re, `/.*`) {
		re = strings.TrimSuffix(re, `/.*`) + `(/.*)?`
	}
	return regexp.MustCompile(`^` + re + `$`)
}

// listPackages lists every package of module m (import path and directory
// only), run from m's directory.
func listPackages(root string, m gomodules.Module) ([]pkg, error) {
	cmd := exec.Command("go", "list", "-e", "-json=ImportPath,Dir", "./...")
	cmd.Dir = filepath.Join(root, filepath.FromSlash(m.Dir))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list in %s: %v: %s", m.Dir, err, stderr.String())
	}
	var pkgs []pkg
	dec := json.NewDecoder(bytes.NewReader(raw))
	for dec.More() {
		var e listEntry
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("decode go list output: %v", err)
		}
		rel, err := filepath.Rel(root, e.Dir)
		if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, pkg{importPath: e.ImportPath, module: m.Dir, dir: filepath.ToSlash(rel)})
	}
	return pkgs, nil
}

func gitOutput(args ...string) (string, error) {
	var stderr bytes.Buffer
	// #nosec G204 -- the callers pass fixed subcommands and flags plus revisions checkArgs vetted.
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
