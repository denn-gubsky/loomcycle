package archtest

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/scripts/ci/gomodules"
)

// TestProcessStarters_OnlyTheAllowedPackages holds RFC GW §11's list: no
// package under internal/ or cmd/ makes a process unless processStarters
// names it, every entry still makes one, test-support starters are reached
// only from tests, and the workspace engines only from the worker role.
func TestProcessStarters_OnlyTheAllowedPackages(t *testing.T) {
	root := moduleRoot(t)
	mods := inTreeModules(t, root)
	pkgs := listForProcesses(t, root, mods)

	var sites []processSite
	graph := map[string][]string{}
	for _, p := range pkgs {
		graph[p.path] = p.imports
		if !underCoreTree(p.path) {
			continue
		}
		for _, f := range p.files {
			src, err := os.ReadFile(filepath.Join(p.dir, f))
			if err != nil {
				t.Fatal(err)
			}
			got, err := processSites(p.path, p.path+"/"+f, src)
			if err != nil {
				t.Fatalf("parse %s/%s: %v", p.path, f, err)
			}
			sites = append(sites, got...)
		}
	}
	for _, p := range checkProcessSites(processStarters, sites) {
		t.Error(p)
	}
	for _, p := range checkTestSupport(processStarters, graph) {
		t.Error(p)
	}

	fileImports, fileIdents := mainPackageFiles(t, mods[0].Path, pkgs, workerRole.mainPkg)
	for _, p := range checkWorkerRole(workerRole, graph, fileImports, fileIdents) {
		t.Error(p)
	}
}

// TestProcessStarters_ReportsAnUnlistedCall shows the rule can fail: a
// package outside the list that makes a process — through each maker, a
// renamed import and a function value — is reported, and a listed package
// with no site left is reported as stale.
func TestProcessStarters_ReportsAnUnlistedCall(t *testing.T) {
	src := []byte(`package fixture

import (
	"os"
	run "os/exec"
	"syscall"
)

var start = run.Command

func f() {
	_ = run.CommandContext(nil, "x")
	_ = &run.Cmd{Path: "/bin/true"}
	_, _ = os.StartProcess("/bin/true", nil, nil)
	_, _ = syscall.ForkExec("/bin/true", nil, nil)
	_ = syscall.Exec("/bin/true", nil, nil)
	_ = run.LookPath("x")   // not a maker
	var c *run.Cmd          // not a maker: a command made elsewhere
	_ = c.Start()           // not a maker
}
`)
	sites, err := processSites("internal/fixture", "fixture.go", src)
	if err != nil {
		t.Fatal(err)
	}
	var whats []string
	for _, s := range sites {
		whats = append(whats, s.what)
	}
	want := []string{
		"os/exec.Command", "os/exec.CommandContext", "os/exec.Cmd literal",
		"os.StartProcess", "syscall.ForkExec", "syscall.Exec",
	}
	if strings.Join(whats, ",") != strings.Join(want, ",") {
		t.Fatalf("sites: got %v, want %v", whats, want)
	}

	allowed := []processStarter{{pkg: "internal/stale", reason: "fixture"}}
	problems := checkProcessSites(allowed, sites)
	var outside, stale int
	for _, p := range problems {
		switch {
		case strings.Contains(p, "internal/fixture") && strings.Contains(p, "outside the allowed packages"):
			outside++
		case strings.Contains(p, "internal/stale") && strings.Contains(p, "no longer starts a process"):
			stale++
		}
	}
	if outside != len(want) || stale != 1 || len(problems) != len(want)+1 {
		t.Errorf("problems: %d outside, %d stale, %d in all; want %d, 1, %d:\n%s",
			outside, stale, len(problems), len(want), len(want)+1, strings.Join(problems, "\n"))
	}

	// The same calls in a listed package are allowed.
	if p := checkProcessSites([]processStarter{{pkg: "internal/fixture"}}, sites); len(p) != 0 {
		t.Errorf("listed package reported: %v", p)
	}

	// A dot import is refused rather than guessed at.
	dot, err := processSites("internal/fixture", "dot.go", []byte("package fixture\n\nimport . \"os/exec\"\n\nvar _ = Command\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dot) != 1 || dot[0].what != "os/exec dot-imported" {
		t.Errorf("dot import: got %v", dot)
	}
}

// TestProcessStarters_BoundsTheWorkerRoleAndTestSupport shows the two
// reachability rules can fail.
func TestProcessStarters_BoundsTheWorkerRoleAndTestSupport(t *testing.T) {
	b := workerRoleBound{
		engine: "internal/eng", mainPkg: "cmd/app",
		packages: []string{"internal/wrk", "cmd/app"},
		files:    []string{"worker.go"}, entry: "runWorker", entryCallers: []string{"cli.go"},
	}
	graph := map[string][]string{
		"internal/wrk":     {"internal/eng"},
		"internal/wrktest": {"internal/wrk"},
		"internal/server":  {"internal/wrk"},
		"cmd/app":          {"internal/wrk", "internal/server"},
	}
	fileImports := map[string][]string{
		"worker.go": {"internal/wrk"},
		"boot.go":   {"internal/wrk"},
		"cli.go":    nil,
	}
	fileIdents := map[string][]string{
		"worker.go": {"runWorker"},
		"cli.go":    {"runWorker"},
		"serve.go":  {"runWorker"},
	}
	got := strings.Join(checkWorkerRole(b, graph, fileImports, fileIdents), "\n")
	for _, want := range []string{
		"internal/server reaches internal/eng",
		"cmd/app/boot.go imports internal/wrk",
		"cmd/app/serve.go names runWorker",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("worker role: missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "internal/wrktest reaches") || strings.Contains(got, "cmd/app/worker.go") || strings.Contains(got, "cmd/app/cli.go") {
		t.Errorf("worker role: reported an allowed reacher or file:\n%s", got)
	}

	ts := []processStarter{{pkg: "internal/gst", testSupport: true}}
	tsGraph := map[string][]string{
		"internal/clustertest": {"internal/gst"},
		"internal/prod":        {"internal/clustertest"},
	}
	tsGot := checkTestSupport(ts, tsGraph)
	if len(tsGot) != 1 || !strings.Contains(tsGot[0], "internal/prod imports the test-support package internal/gst") {
		t.Errorf("test support: got %v", tsGot)
	}
}

// underCoreTree reports whether a package is in the tree §11 governs.
func underCoreTree(pkg string) bool {
	return strings.HasPrefix(pkg, "internal/") || strings.HasPrefix(pkg, "cmd/")
}

type listedPackage struct {
	path    string   // relative to the root module
	dir     string   // absolute
	files   []string // every non-test .go file, whatever its build tags
	imports []string // non-test in-module imports, relative
}

// listForProcesses lists every package of the in-tree modules, one go list
// call per module as importGraph does. Files include IgnoredGoFiles so a
// file built only on another GOOS is still read.
func listForProcesses(t *testing.T, root string, mods []gomodules.Module) []listedPackage {
	t.Helper()
	rootPath := mods[0].Path
	var out []listedPackage
	for _, m := range mods {
		cmd := exec.Command("go", "list", "-e", "-f",
			`{{.ImportPath}}	{{.Dir}}	{{join .GoFiles " "}} {{join .CgoFiles " "}} {{join .IgnoredGoFiles " "}}	{{join .Imports " "}}`, "./...")
		cmd.Dir = filepath.Join(root, filepath.FromSlash(m.Dir))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list in %s: %v\n%s", m.Dir, err, stderr.String())
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			f := strings.Split(line, "\t")
			if len(f) != 4 {
				t.Fatalf("go list in %s: unexpected line %q", m.Dir, line)
			}
			rel, ok := relativeTo(rootPath, f[0])
			if !ok {
				t.Fatalf("go list in %s: package %s is not under %s", m.Dir, f[0], rootPath)
			}
			p := listedPackage{path: rel, dir: f[1]}
			for _, name := range strings.Fields(f[2]) {
				if !strings.HasSuffix(name, "_test.go") {
					p.files = append(p.files, name)
				}
			}
			for _, ip := range strings.Fields(f[3]) {
				if r, ok := relativeTo(rootPath, ip); ok {
					p.imports = append(p.imports, r)
				}
			}
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Fatal("go list: no packages")
	}
	return out
}

// mainPackageFiles parses each non-test file of pkg and returns, per file,
// its in-module imports (relative) and the identifiers it names.
func mainPackageFiles(t *testing.T, rootPath string, pkgs []listedPackage, pkg string) (map[string][]string, map[string][]string) {
	t.Helper()
	var p *listedPackage
	for i := range pkgs {
		if pkgs[i].path == pkg {
			p = &pkgs[i]
		}
	}
	if p == nil {
		t.Fatalf("package %s not listed", pkg)
	}
	imports := map[string][]string{}
	idents := map[string][]string{}
	for _, name := range p.files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(p.dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s/%s: %v", pkg, name, err)
		}
		imports[name] = nil
		for _, imp := range f.Imports {
			ip, _ := strconv.Unquote(imp.Path.Value)
			if r, ok := relativeTo(rootPath, ip); ok {
				imports[name] = append(imports[name], r)
			}
		}
		seen := map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && !seen[id.Name] {
				seen[id.Name] = true
				idents[name] = append(idents[name], id.Name)
			}
			return true
		})
	}
	return imports, idents
}

// processSite is one place that makes a process.
type processSite struct {
	pkg  string
	pos  string // file:line
	what string // e.g. os/exec.Command
}

func (s processSite) String() string { return fmt.Sprintf("%s: %s (package %s)", s.pos, s.what, s.pkg) }

// processSites returns the places in one file's source that make a process.
// file names the file in the positions reported.
func processSites(pkg, file string, src []byte) ([]processSite, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	local := map[string]string{} // name in this file -> watched import path
	var sites []processSite
	site := func(n ast.Node, what string) {
		p := fset.Position(n.Pos())
		sites = append(sites, processSite{pkg: pkg, pos: fmt.Sprintf("%s:%d", p.Filename, p.Line), what: what})
	}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || processMakers[path] == nil {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch name {
		case "_":
			continue
		case ".":
			// The makers would be bare names; refuse the form instead of
			// guessing which bare call is which.
			site(imp, path+" dot-imported")
			continue
		}
		local[name] = path
	}
	if len(local) == 0 {
		return sites, nil
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			// Any reference counts, a call or a function value.
			if id, ok := x.X.(*ast.Ident); ok {
				if path, ok := local[id.Name]; ok && processMakers[path][x.Sel.Name] {
					site(x, path+"."+x.Sel.Name)
				}
			}
		case *ast.CompositeLit:
			if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Cmd" {
				if id, ok := sel.X.(*ast.Ident); ok && local[id.Name] == "os/exec" {
					site(x, "os/exec.Cmd literal")
				}
			}
		}
		return true
	})
	return sites, nil
}

// checkProcessSites returns one problem per site outside the allowed
// packages and one per allowed package with no site, sorted.
func checkProcessSites(allowed []processStarter, sites []processSite) []string {
	allow := map[string]bool{}
	for _, a := range allowed {
		allow[a.pkg] = true
	}
	seen := map[string]bool{}
	var problems []string
	for _, s := range sites {
		seen[s.pkg] = true
		if !allow[s.pkg] {
			problems = append(problems, fmt.Sprintf(
				"%s starts a process outside the allowed packages (RFC GW §11): run the command on a workspace worker, or, if the owner amends §11, add the package to processStarters with its reason", s))
		}
	}
	for _, a := range allowed {
		if !seen[a.pkg] {
			problems = append(problems, fmt.Sprintf(
				"processStarters lists %s, which no longer starts a process: delete its entry (the list may only shrink)", a.pkg))
		}
	}
	sort.Strings(problems)
	return problems
}

// reachers returns every package of graph that imports target, directly or
// through other packages of graph.
func reachers(graph map[string][]string, target string) []string {
	rev := map[string][]string{}
	for from, imps := range graph {
		for _, to := range imps {
			rev[to] = append(rev[to], from)
		}
	}
	seen := map[string]bool{}
	queue := []string{target}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, from := range rev[p] {
			if !seen[from] {
				seen[from] = true
				queue = append(queue, from)
			}
		}
	}
	return sortedKeys(seen)
}

// checkTestSupport returns a problem for each non-test package that reaches
// a test-support starter without being test support itself (by convention a
// package whose last path element ends in "test").
func checkTestSupport(allowed []processStarter, graph map[string][]string) []string {
	var problems []string
	for _, a := range allowed {
		if !a.testSupport {
			continue
		}
		for _, p := range reachers(graph, a.pkg) {
			if !strings.HasSuffix(p[strings.LastIndex(p, "/")+1:], "test") {
				problems = append(problems, fmt.Sprintf(
					"%s imports the test-support package %s (directly or through others) from non-test code; it starts processes and only tests may use it", p, a.pkg))
			}
		}
	}
	return problems
}

// checkWorkerRole returns the problems with the worker-role bound. fileImports
// maps each non-test file of the main package to its in-module imports;
// fileIdents maps each to the identifiers it names.
func checkWorkerRole(b workerRoleBound, graph map[string][]string, fileImports, fileIdents map[string][]string) []string {
	var problems []string
	inRole := map[string]bool{b.engine: true}
	for _, p := range b.packages {
		inRole[p] = true
	}
	for _, p := range reachers(graph, b.engine) {
		if !inRole[p] && !strings.HasSuffix(p[strings.LastIndex(p, "/")+1:], "test") {
			problems = append(problems, fmt.Sprintf(
				"%s reaches %s, which only the worker role may run: keep it behind the worker command", p, b.engine))
		}
	}
	files := map[string]bool{}
	for _, f := range b.files {
		files[f] = true
	}
	callers := map[string]bool{}
	for _, f := range b.entryCallers {
		callers[f] = true
	}
	for _, file := range sortedKeys(setOf(fileImports)) {
		if files[file] {
			continue
		}
		for _, imp := range fileImports[file] {
			if inRole[imp] && imp != b.mainPkg {
				problems = append(problems, fmt.Sprintf(
					"%s/%s imports %s; in %s only %v may reach the workspace engines", b.mainPkg, file, imp, b.mainPkg, b.files))
			}
		}
	}
	for _, file := range sortedKeys(setOf(fileIdents)) {
		if files[file] || callers[file] {
			continue
		}
		for _, id := range fileIdents[file] {
			if id == b.entry {
				problems = append(problems, fmt.Sprintf(
					"%s/%s names %s, the worker role's entry; only %v may run it", b.mainPkg, file, b.entry, b.entryCallers))
			}
		}
	}
	return problems
}

func setOf(m map[string][]string) map[string]bool {
	s := make(map[string]bool, len(m))
	for k := range m {
		s[k] = true
	}
	return s
}
