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
	parents := map[string][]string{}
	for parent, children := range graph {
		for _, c := range children {
			parents[c] = append(parents[c], parent)
		}
	}
	seen := map[string]bool{}
	frontier := append([]string(nil), direct...)
	for depth := 0; depth <= MaxGroupNestingDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, g := range frontier {
			if seen[g] {
				continue
			}
			seen[g] = true
			next = append(next, parents[g]...)
		}
		frontier = next
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
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
