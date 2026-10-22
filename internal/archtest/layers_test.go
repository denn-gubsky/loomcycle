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
	graph := importGraph(t, moduleRoot(t))
	for _, p := range checkGraph(repoModel, graph) {
		t.Error(p)
	}
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

// importGraph maps every package of the module to its non-test imports within
// the module. Paths are relative to the module root. One go list call: the
// whole check costs about as much as listing the packages.
func importGraph(t *testing.T, root string) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f",
		`{{.Module.Path}} {{.ImportPath}} {{join .Imports " "}}`, "./...")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}

	graph := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			t.Fatalf("go list: unexpected line %q", line)
		}
		mod := f[0]
		from, ok := relativeTo(mod, f[1])
		if !ok {
			t.Fatalf("go list: package %s is not in module %s", f[1], mod)
		}
		imports := []string{}
		for _, ip := range f[2:] {
			if rel, ok := relativeTo(mod, ip); ok {
				imports = append(imports, rel)
			}
		}
		graph[from] = imports
	}
	if len(graph) == 0 {
		t.Fatal("go list: no packages")
	}
	return graph
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
