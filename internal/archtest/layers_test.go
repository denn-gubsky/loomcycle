package archtest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/scripts/ci/gomodules"
)

// model is the layering a graph is checked against: the package-level tables
// in production, small hand-built ones in the checker's own tests.
type model struct {
	layers   []layer
	sideways []subsystemEdge
	known    []knownViolation
}

var repoModel = model{layers: layers, sideways: allowedSideways, known: knownViolations}

func TestLayerMap_IsConsistent(t *testing.T) {
	for _, p := range checkModel(repoModel) {
		t.Error(p)
	}
}

func TestImportGraph_FollowsTheLayers(t *testing.T) {
	root := moduleRoot(t)
	mods := inTreeModules(t, root)
	graph, moduleOf := importGraph(t, root, mods)
	for _, p := range checkGraph(repoModel, graph) {
		t.Error(p)
	}
	for _, p := range checkModuleLayers(repoModel, moduleOf, nestedDirs(mods)) {
		t.Error(p)
	}
}

// TestGoModules_AreWiredOrDeclared fails on a go.mod that would take its
// directory out of the root module without the tooling knowing: one the root
// does not replace and standaloneModules does not list.
func TestGoModules_AreWiredOrDeclared(t *testing.T) {
	root := moduleRoot(t)
	mods := inTreeModules(t, root)
	// -co --exclude-standard: tracked files plus new ones not yet added, so a
	// go.mod created locally fails here before it is committed; ignored trees
	// (node_modules) are skipped.
	cmd := exec.Command("git", "ls-files", "-co", "--exclude-standard", "--", ":(glob)**/go.mod")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("git ls-files (not a git checkout?): %v", err)
	}
	var dirs []string
	for _, f := range strings.Fields(string(out)) {
		dirs = append(dirs, filepath.ToSlash(filepath.Dir(f)))
	}
	for _, p := range checkModuleCensus(dirs, nestedDirs(mods), standaloneModules) {
		t.Error(p)
	}
}

func inTreeModules(t *testing.T, root string) []gomodules.Module {
	t.Helper()
	mods, err := gomodules.InTree(root)
	if err != nil {
		t.Fatal(err)
	}
	return mods
}

func nestedDirs(mods []gomodules.Module) []string {
	var dirs []string
	for _, m := range mods[1:] {
		dirs = append(dirs, m.Dir)
	}
	return dirs
}

// moduleRoot is the directory holding go.mod. go test runs a package's tests in
// the package's own directory whatever the caller's working directory, so the
// root is two levels up. It is spelled as a relative literal on purpose:
// scripts/ci/affected selects a test whose files name a path through "..", and
// one that names the repository root is selected for every change — which this
// test must be, because any package's imports can break it.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root: no go.mod two levels above internal/archtest: %v", err)
	}
	return root
}

// importGraph maps every package of the in-tree modules to its non-test
// imports within them, and each package to the directory of its module.
// Paths are relative to the root module: a nested module's path extends the
// root's, so its packages and the imports of them read like any other. One go
// list call per module, from the module's directory, since go list ./... from
// the root does not enter a nested module.
func importGraph(t *testing.T, root string, mods []gomodules.Module) (map[string][]string, map[string]string) {
	t.Helper()
	rootPath := mods[0].Path
	graph := map[string][]string{}
	moduleOf := map[string]string{}
	for _, m := range mods {
		cmd := exec.Command("go", "list", "-f", `{{.ImportPath}} {{join .Imports " "}}`, "./...")
		cmd.Dir = filepath.Join(root, filepath.FromSlash(m.Dir))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list in %s: %v\n%s", m.Dir, err, stderr.String())
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			f := strings.Fields(line)
			if len(f) < 1 {
				t.Fatalf("go list in %s: unexpected line %q", m.Dir, line)
			}
			from, ok := relativeTo(rootPath, f[0])
			if !ok {
				t.Fatalf("go list in %s: package %s is not under %s", m.Dir, f[0], rootPath)
			}
			imports := []string{}
			for _, ip := range f[1:] {
				if rel, ok := relativeTo(rootPath, ip); ok {
					imports = append(imports, rel)
				}
			}
			graph[from] = imports
			moduleOf[from] = m.Dir
		}
	}
	if len(graph) == 0 {
		t.Fatal("go list: no packages")
	}
	return graph, moduleOf
}

func relativeTo(mod, importPath string) (string, bool) {
	if importPath == mod {
		return ".", true
	}
	return strings.CutPrefix(importPath, mod+"/")
}

type placement struct {
	layer     int
	subsystem string
}

// place returns the layer of the subsystem whose directory is the longest
// prefix of pkg, matched on whole path segments.
func place(layers []layer, pkg string) (placement, bool) {
	best, found := placement{}, false
	for i, l := range layers {
		for _, s := range l.subsystems {
			if (pkg == s || strings.HasPrefix(pkg, s+"/")) && (!found || len(s) > len(best.subsystem)) {
				best, found = placement{layer: i, subsystem: s}, true
			}
		}
	}
	return best, found
}

type packageEdge struct {
	from, to string
}

// checkGraph returns one message per problem with the graph, in a stable order.
func checkGraph(m model, graph map[string][]string) []string {
	var problems []string
	describe := func(pkg string, p placement) string {
		return fmt.Sprintf("%s [%s, subsystem %s]", pkg, m.layers[p.layer].name, p.subsystem)
	}

	pkgs := make([]string, 0, len(graph))
	for pkg := range graph {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)

	unplaced := map[string]bool{}
	for _, pkg := range pkgs {
		if _, ok := place(m.layers, pkg); !ok {
			unplaced[pkg] = true
		}
		for _, imp := range graph[pkg] {
			if _, ok := place(m.layers, imp); !ok {
				unplaced[imp] = true
			}
		}
	}
	for _, pkg := range sortedKeys(unplaced) {
		problems = append(problems, fmt.Sprintf(
			"package %s is in no layer: add its directory, or the subsystem it belongs to, "+
				"to the layer in internal/archtest/layers.go that its role puts it in. "+
				"Its imports must then point down, or sideways along allowedSideways.", pkg))
	}

	sideways := map[subsystemEdge]bool{}
	for _, e := range m.sideways {
		sideways[e] = true
	}
	known := map[packageEdge]bool{}
	for _, k := range m.known {
		known[packageEdge{k.from, k.to}] = true
	}
	matched := map[packageEdge]bool{}
	imports := map[packageEdge]bool{}

	for _, pkg := range pkgs {
		pf, ok := place(m.layers, pkg)
		if !ok {
			continue
		}
		deps := append([]string(nil), graph[pkg]...)
		sort.Strings(deps)
		for _, imp := range deps {
			e := packageEdge{pkg, imp}
			imports[e] = true
			pt, ok := place(m.layers, imp)
			if !ok || pf.subsystem == pt.subsystem || pt.layer < pf.layer {
				continue
			}
			if pt.layer == pf.layer && sideways[subsystemEdge{pf.subsystem, pt.subsystem}] {
				continue
			}
			if known[e] {
				matched[e] = true
				continue
			}
			const notTheAllowlist = " Adding it to knownViolations is not the fix: that list records " +
				"the imports RFC EX Phase 2 removes, and it only shrinks."
			if pt.layer > pf.layer {
				problems = append(problems, fmt.Sprintf(
					"%s imports %s: a package may only import a lower layer. Fix the import: "+
						"move what both need down to %s's layer, or have the caller pass it in."+
						notTheAllowlist,
					describe(pkg, pf), describe(imp, pt), pkg))
			} else {
				problems = append(problems, fmt.Sprintf(
					"%s imports %s: same layer, different subsystem, and the pair is not in "+
						"allowedSideways. If %s is meant to build on %s, add {%q, %q} to "+
						"allowedSideways in internal/archtest/layers.go (the set must stay acyclic); "+
						"otherwise fix the import."+notTheAllowlist,
					describe(pkg, pf), describe(imp, pt), pf.subsystem, pt.subsystem,
					pf.subsystem, pt.subsystem))
			}
		}
	}

	for _, k := range m.known {
		e := packageEdge{k.from, k.to}
		switch {
		case matched[e]:
		case !imports[e]:
			problems = append(problems, fmt.Sprintf(
				"knownViolations entry %s -> %s (cut: %s) is stale: %s no longer imports %s. "+
					"Delete the entry in internal/archtest/layers.go; the list only shrinks.",
				k.from, k.to, k.cut, k.from, k.to))
		default:
			problems = append(problems, fmt.Sprintf(
				"knownViolations entry %s -> %s (cut: %s) is stale: the import now follows the "+
					"layering. Delete the entry in internal/archtest/layers.go; the list only shrinks.",
				k.from, k.to, k.cut))
		}
	}
	return problems
}

// checkModuleLayers returns one message per breach of the module rule: every
// nested module (nested, by directory) is listed in exactly one layer's
// modules, no listed module is missing, and every package of a nested module
// (moduleOf maps a package to its module's directory) sits in that layer or
// below.
func checkModuleLayers(m model, moduleOf map[string]string, nested []string) []string {
	var problems []string
	ceiling := map[string]int{}
	for i, l := range m.layers {
		for _, mod := range l.modules {
			ceiling[mod] = i
		}
	}
	isNested := map[string]bool{}
	for _, dir := range nested {
		isNested[dir] = true
		if _, ok := ceiling[dir]; !ok {
			problems = append(problems, fmt.Sprintf(
				"module %s is in no layer: add it to the modules of the layer in "+
					"internal/archtest/layers.go that its packages sit in (the highest of them).", dir))
		}
	}
	for _, mod := range sortedKeys(boolSet(ceiling)) {
		if !isNested[mod] {
			problems = append(problems, fmt.Sprintf(
				"layer %s lists module %s, which the root go.mod does not replace with a "+
					"directory in the tree: wire it (require + replace => ./%s) or remove the entry.",
				m.layers[ceiling[mod]].name, mod, mod))
		}
	}

	pkgs := sortedKeys(boolSet(moduleOf))
	for _, pkg := range pkgs {
		mod := moduleOf[pkg]
		top, ok := ceiling[mod]
		if !ok || !isNested[mod] {
			continue
		}
		p, ok := place(m.layers, pkg)
		if !ok || p.layer <= top {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"package %s [%s, subsystem %s] is in module %s, which is declared in %s: a "+
				"module's packages sit in its layer or below. Move the package out of the "+
				"module, or declare the module in the higher layer if that is its role.",
			pkg, m.layers[p.layer].name, p.subsystem, mod, m.layers[top].name))
	}
	return problems
}

// checkModuleCensus returns one message per go.mod directory (goModDirs,
// slash-separated, "." for the root) that is neither the root, an in-tree
// module (nested) nor a standalone module, and per standalone entry with no
// go.mod. A go.mod under a directory Go ignores (testdata, or a name starting
// with "." or "_") takes nothing out of the root module and is skipped.
func checkModuleCensus(goModDirs, nested, standalone []string) []string {
	var problems []string
	known := map[string]bool{".": true}
	for _, d := range nested {
		known[d] = true
	}
	isStandalone := map[string]bool{}
	for _, d := range standalone {
		known[d] = true
		isStandalone[d] = true
	}
	found := map[string]bool{}
	dirs := append([]string(nil), goModDirs...)
	sort.Strings(dirs)
	for _, d := range dirs {
		if ignoredByGo(d) {
			continue
		}
		found[d] = true
		if !known[d] {
			problems = append(problems, fmt.Sprintf(
				"%s/go.mod makes %s a module the build does not know: go list, go test and go vet "+
					"run from the root stop at it, so its packages would drop out of CI without a "+
					"word. Wire it into the root go.mod (require + replace => ./%s) and declare its "+
					"layer, or, if it builds on its own, add it to standaloneModules in "+
					"internal/archtest/layers.go.", d, d, d))
		}
	}
	for _, d := range standalone {
		if !found[d] {
			problems = append(problems, fmt.Sprintf(
				"standaloneModules lists %s, which has no go.mod: remove the entry.", d))
		}
	}
	return problems
}

func ignoredByGo(dir string) bool {
	for _, seg := range strings.Split(dir, "/") {
		if seg == "testdata" || (seg != "." && (strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, "_"))) {
			return true
		}
	}
	return false
}

func boolSet[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// checkModel returns one message per inconsistency in the tables themselves.
func checkModel(m model) []string {
	var problems []string
	layerOf := map[string]int{}
	for i, l := range m.layers {
		for _, s := range l.subsystems {
			if j, dup := layerOf[s]; dup {
				problems = append(problems, fmt.Sprintf(
					"subsystem %s is listed twice (%s, %s)", s, m.layers[j].name, l.name))
				continue
			}
			layerOf[s] = i
		}
	}

	adj := map[string][]string{}
	for _, e := range m.sideways {
		lf, okf := layerOf[e.from]
		lt, okt := layerOf[e.to]
		switch {
		case !okf || !okt:
			problems = append(problems, fmt.Sprintf(
				"allowedSideways {%q, %q} names a subsystem no layer lists", e.from, e.to))
		case e.from == e.to:
			problems = append(problems, fmt.Sprintf(
				"allowedSideways {%q, %q} names one subsystem twice; imports inside a subsystem are always allowed",
				e.from, e.to))
		case lf != lt:
			problems = append(problems, fmt.Sprintf(
				"allowedSideways {%q, %q} crosses layers (%s, %s); only same-layer imports need a permission",
				e.from, e.to, m.layers[lf].name, m.layers[lt].name))
		default:
			adj[e.from] = append(adj[e.from], e.to)
		}
	}
	if cycle := findCycle(adj); cycle != nil {
		problems = append(problems, fmt.Sprintf(
			"allowedSideways permits the cycle %s: remove one of its pairs",
			strings.Join(cycle, " -> ")))
	}

	moduleIn := map[string]string{}
	for _, l := range m.layers {
		for _, mod := range l.modules {
			if prev, dup := moduleIn[mod]; dup {
				problems = append(problems, fmt.Sprintf(
					"module %s is listed twice (%s, %s)", mod, prev, l.name))
				continue
			}
			moduleIn[mod] = l.name
		}
	}

	seen := map[packageEdge]bool{}
	for _, k := range m.known {
		e := packageEdge{k.from, k.to}
		if seen[e] {
			problems = append(problems, fmt.Sprintf("knownViolations lists %s -> %s twice", k.from, k.to))
		}
		seen[e] = true
		if k.cut == "" {
			problems = append(problems, fmt.Sprintf(
				"knownViolations entry %s -> %s names no Phase 2 cut", k.from, k.to))
		}
	}
	return problems
}

// findCycle returns a cycle in adj as a closed path (first == last), or nil.
func findCycle(adj map[string][]string) []string {
	const (
		unvisited = iota
		onStack
		done
	)
	state := map[string]int{}
	var stack []string
	var visit func(n string) []string
	visit = func(n string) []string {
		state[n] = onStack
		stack = append(stack, n)
		next := append([]string(nil), adj[n]...)
		sort.Strings(next)
		for _, m := range next {
			switch state[m] {
			case onStack:
				for i, s := range stack {
					if s == m {
						return append(append([]string(nil), stack[i:]...), m)
					}
				}
			case unvisited:
				if c := visit(m); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[n] = done
		return nil
	}
	nodes := make([]string, 0, len(adj))
	for n := range adj {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, n := range nodes {
		if state[n] == unvisited {
			if c := visit(n); c != nil {
				return c
			}
		}
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// toyLayers is a three-layer map for the checker's own tests. b/top sits two
// layers above its parent directory b, to exercise longest-prefix placement.
var toyLayers = []layer{
	{name: "L0 low", subsystems: []string{"a"}},
	{name: "L1 mid", subsystems: []string{"b", "c"}},
	{name: "L2 high", subsystems: []string{"b/top"}},
}

func TestCheckGraph_ReportsEachKindOfProblem(t *testing.T) {
	sideways := []subsystemEdge{{"b", "c"}}
	cases := []struct {
		name  string
		known []knownViolation
		graph map[string][]string
		want  []string // substrings of each problem, in order; empty = clean
	}{
		{name: "import inside a subsystem", graph: map[string][]string{"b": {"b/x"}, "b/x": nil}},
		{name: "import into a lower layer", graph: map[string][]string{"b": {"a"}, "a": nil}},
		{name: "listed sideways import", graph: map[string][]string{"b": {"c"}, "c": nil}},
		{
			name:  "import into a higher layer",
			graph: map[string][]string{"a": {"c"}, "c": nil},
			want:  []string{"a [L0 low, subsystem a] imports c [L1 mid, subsystem c]: a package may only import a lower layer"},
		},
		{
			name:  "unlisted sideways import",
			graph: map[string][]string{"c": {"b"}, "b": nil},
			want:  []string{`not in allowedSideways. If c is meant to build on b, add {"c", "b"}`},
		},
		{
			name:  "unplaced package",
			graph: map[string][]string{"z": nil},
			want:  []string{"package z is in no layer"},
		},
		{
			name:  "longest prefix places a subpackage above its parent",
			graph: map[string][]string{"b": {"b/top"}, "b/top": nil},
			want:  []string{"imports b/top [L2 high, subsystem b/top]"},
		},
		{
			name:  "prefix matches whole path segments only",
			graph: map[string][]string{"a": {"b/topper"}, "b/topper": nil},
			want:  []string{"imports b/topper [L1 mid, subsystem b]"},
		},
		{
			name:  "known violation is accepted",
			known: []knownViolation{{"a", "c", "cut x"}},
			graph: map[string][]string{"a": {"c"}, "c": nil},
		},
		{
			name:  "known violation whose import is gone is stale",
			known: []knownViolation{{"a", "c", "cut x"}},
			graph: map[string][]string{"a": nil, "c": nil},
			want:  []string{"entry a -> c (cut: cut x) is stale: a no longer imports c"},
		},
		{
			name:  "known violation that now follows the layering is stale",
			known: []knownViolation{{"b", "a", "cut x"}},
			graph: map[string][]string{"b": {"a"}, "a": nil},
			want:  []string{"entry b -> a (cut: cut x) is stale: the import now follows the layering"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkGraph(model{layers: toyLayers, sideways: sideways, known: tc.known}, tc.graph)
			assertProblems(t, got, tc.want)
		})
	}
}

func TestCheckModel_RejectsInconsistentTables(t *testing.T) {
	cases := []struct {
		name string
		m    model
		want []string
	}{
		{name: "consistent", m: model{layers: toyLayers, sideways: []subsystemEdge{{"b", "c"}}}},
		{
			name: "sideways cycle",
			m:    model{layers: toyLayers, sideways: []subsystemEdge{{"b", "c"}, {"c", "b"}}},
			want: []string{"permits the cycle b -> c -> b"},
		},
		{
			name: "sideways across layers",
			m:    model{layers: toyLayers, sideways: []subsystemEdge{{"b", "a"}}},
			want: []string{"crosses layers"},
		},
		{
			name: "sideways naming an unknown subsystem",
			m:    model{layers: toyLayers, sideways: []subsystemEdge{{"b", "nope"}}},
			want: []string{"names a subsystem no layer lists"},
		},
		{
			name: "subsystem listed twice",
			m: model{layers: []layer{
				{name: "L0", subsystems: []string{"a"}},
				{name: "L1", subsystems: []string{"a"}},
			}},
			want: []string{"subsystem a is listed twice (L0, L1)"},
		},
		{
			name: "module listed twice",
			m: model{layers: []layer{
				{name: "L0", subsystems: []string{"a"}, modules: []string{"a"}},
				{name: "L1", subsystems: []string{"b"}, modules: []string{"a"}},
			}},
			want: []string{"module a is listed twice (L0, L1)"},
		},
		{
			name: "known violation without a cut",
			m:    model{layers: toyLayers, known: []knownViolation{{"a", "c", ""}}},
			want: []string{"names no Phase 2 cut"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertProblems(t, checkModel(tc.m), tc.want)
		})
	}
}

// toyModuleLayers is toyLayers with module a declared in L0 and module b in
// L1. b's directory holds b/top, which the layers put in L2.
var toyModuleLayers = []layer{
	{name: "L0 low", subsystems: []string{"a"}, modules: []string{"a"}},
	{name: "L1 mid", subsystems: []string{"b", "c"}, modules: []string{"b"}},
	{name: "L2 high", subsystems: []string{"b/top"}},
}

func TestCheckModuleLayers_ReportsEachKindOfProblem(t *testing.T) {
	cases := []struct {
		name     string
		layers   []layer
		moduleOf map[string]string
		nested   []string
		want     []string
	}{
		{
			name:     "no nested module is vacuous",
			layers:   toyLayers,
			moduleOf: map[string]string{"a": ".", "b": ".", "b/top": "."},
		},
		{
			name:     "packages at or below the declared layer",
			layers:   toyModuleLayers,
			moduleOf: map[string]string{"a": "a", "a/x": "a", "b": "b", "b/y": "b", "c": "."},
			nested:   []string{"a", "b"},
		},
		{
			name:     "a package above its module's layer",
			layers:   toyModuleLayers,
			moduleOf: map[string]string{"a": "a", "b": "b", "b/top": "b"},
			nested:   []string{"a", "b"},
			want: []string{"package b/top [L2 high, subsystem b/top] is in module b, which is declared in L1 mid: " +
				"a module's packages sit in its layer or below"},
		},
		{
			name:     "a nested module no layer declares",
			layers:   toyLayers,
			moduleOf: map[string]string{"c": "c"},
			nested:   []string{"c"},
			want:     []string{"module c is in no layer"},
		},
		{
			name:     "a declared module the root does not wire",
			layers:   toyModuleLayers,
			moduleOf: map[string]string{"a": "a", "b": "."},
			nested:   []string{"a"},
			want:     []string{"layer L1 mid lists module b, which the root go.mod does not replace"},
		},
		{
			// The root's packages are never held to a module's ceiling, even
			// when the root's directory listing would match one by name.
			name:     "root packages are not checked against a module",
			layers:   toyModuleLayers,
			moduleOf: map[string]string{"a": "a", "b": "b", "b/top": "."},
			nested:   []string{"a", "b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkModuleLayers(model{layers: tc.layers}, tc.moduleOf, tc.nested)
			assertProblems(t, got, tc.want)
		})
	}
}

func TestCheckModuleCensus_EveryGoModIsWiredOrDeclared(t *testing.T) {
	cases := []struct {
		name                     string
		dirs, nested, standalone []string
		want                     []string
	}{
		{name: "root only", dirs: []string{"."}},
		{
			name:       "root, wired and standalone",
			dirs:       []string{".", "internal/errkind", "deploy/builder"},
			nested:     []string{"internal/errkind"},
			standalone: []string{"deploy/builder"},
		},
		{
			name: "a go.mod nobody wired or declared",
			dirs: []string{".", "internal/store"},
			want: []string{"internal/store/go.mod makes internal/store a module the build does not know"},
		},
		{
			name: "directories Go ignores are skipped",
			dirs: []string{".", "internal/x/testdata/mod", "web/.cache/y", "_scratch"},
		},
		{
			name:       "a standalone entry with no go.mod is stale",
			dirs:       []string{"."},
			standalone: []string{"deploy/builder"},
			want:       []string{"standaloneModules lists deploy/builder, which has no go.mod"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertProblems(t, checkModuleCensus(tc.dirs, tc.nested, tc.standalone), tc.want)
		})
	}
}

func assertProblems(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d problems, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			t.Errorf("problem %d = %q, want it to contain %q", i, got[i], want[i])
		}
	}
}
