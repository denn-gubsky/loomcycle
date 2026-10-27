package authz

import (
	"fmt"
	"sort"
)

// GroupGraph is the tenant's group nesting: for each group, the groups that
// are its direct members. Users' direct memberships are passed separately
// to Closure.
type GroupGraph map[string][]string

// Closure returns the transitive set of groups a subject belongs to: its
// direct groups plus, for each, the groups that contain it, up to
// MaxGroupNestingDepth hops. The result is sorted and de-duplicated. A
// cycle in the graph terminates at the depth cap (ValidateGroupGraph
// refuses cycles at write time; this is the read-side guard).
//
// Direction: `parent` CONTAINS `child` (graph[parent] lists child groups).
// A member of child is a member of parent, so the walk goes child → parent;
// the reverse index is built once per call (graphs are small).
func Closure(graph GroupGraph, direct []string) []string {
	out, _ := ClosureVia(graph, direct)
	return out
}

// ViaDirect is the ClosureVia value of a group the subject is a direct
// member of.
const ViaDirect = "direct"

// ClosureVia is Closure that also says, for each group of the result, how
// the subject came to be in it: ViaDirect, or the group one step closer to
// the subject through which it was reached (a member of `support` is in
// `staff` via `support` when staff contains support). The walk is breadth
// first, so a group reachable both directly and through a parent reads as
// direct, and of two equally short paths the one through the
// alphabetically first group wins — the answer is stable between calls.
func ClosureVia(graph GroupGraph, direct []string) ([]string, map[string]string) {
	parents := map[string][]string{}
	for parent, children := range graph {
		for _, c := range children {
			parents[c] = append(parents[c], parent)
		}
	}
	for c := range parents {
		sort.Strings(parents[c])
	}
	type step struct{ group, via string }
	sortedDirect := append([]string(nil), direct...)
	sort.Strings(sortedDirect)
	frontier := make([]step, 0, len(sortedDirect))
	for _, g := range sortedDirect {
		frontier = append(frontier, step{g, ViaDirect})
	}
	via := map[string]string{}
	for depth := 0; depth <= MaxGroupNestingDepth && len(frontier) > 0; depth++ {
		var next []step
		for _, s := range frontier {
			if _, seen := via[s.group]; seen {
				continue
			}
			via[s.group] = s.via
			for _, p := range parents[s.group] {
				next = append(next, step{p, s.group})
			}
		}
		frontier = next
	}
	out := make([]string, 0, len(via))
	for g := range via {
		out = append(out, g)
	}
	sort.Strings(out)
	return out, via
}

// ValidateGroupGraph refuses a graph with a cycle or a nesting deeper than
// MaxGroupNestingDepth, and names the offending group. It is called before
// a membership write lands.
func ValidateGroupGraph(graph GroupGraph) error {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(g string, depth int) error
	visit = func(g string, depth int) error {
		if depth > MaxGroupNestingDepth {
			return fmt.Errorf("group %q nests deeper than %d", g, MaxGroupNestingDepth)
		}
		switch color[g] {
		case grey:
			return fmt.Errorf("group %q is nested inside itself", g)
		case black:
			return nil
		}
		color[g] = grey
		for _, c := range graph[g] {
			if c == g {
				return fmt.Errorf("group %q lists itself as a member", g)
			}
			if err := visit(c, depth+1); err != nil {
				return err
			}
		}
		color[g] = black
		return nil
	}
	names := make([]string, 0, len(graph))
	for g := range graph {
		names = append(names, g)
	}
	sort.Strings(names)
	for _, g := range names {
		if !ValidName(g) {
			return fmt.Errorf("group %q is not a valid name", g)
		}
		if err := visit(g, 0); err != nil {
			return err
		}
	}
	return nil
}
