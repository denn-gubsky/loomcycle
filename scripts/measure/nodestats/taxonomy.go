package main

import (
	"fmt"
	"sort"
)

// Concept is one concept of the measured scheme. Parent is "" for a top-level
// concept (a direct child of the scheme root); Depth is 1 there.
type Concept struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Parent   string   `json:"parent,omitempty"`
	Depth    int      `json:"depth"`
	Position int      `json:"-"`
	Meaning  string   `json:"meaning,omitempty"`
	Alt      []string `json:"alt,omitempty"`
}

// Taxonomy is the scheme's concept tree.
type Taxonomy struct {
	byID     map[string]*Concept
	children map[string][]string // "" holds the top level
	order    []string            // depth-first, siblings by position then id
}

// NewTaxonomy builds the tree. Every Parent must name another concept or be "";
// depths are recomputed from the parent links, so the caller's Depth is ignored.
func NewTaxonomy(cs []Concept) (*Taxonomy, error) {
	t := &Taxonomy{byID: map[string]*Concept{}, children: map[string][]string{}}
	for i := range cs {
		c := cs[i]
		if c.ID == "" {
			return nil, fmt.Errorf("concept %d: empty id", i)
		}
		if _, dup := t.byID[c.ID]; dup {
			return nil, fmt.Errorf("concept %q: duplicate", c.ID)
		}
		t.byID[c.ID] = &c
	}
	for _, c := range t.byID {
		if c.Parent != "" {
			if _, ok := t.byID[c.Parent]; !ok {
				return nil, fmt.Errorf("concept %q: unknown parent %q", c.ID, c.Parent)
			}
		}
		t.children[c.Parent] = append(t.children[c.Parent], c.ID)
	}
	for p := range t.children {
		kids := t.children[p]
		sort.Slice(kids, func(i, j int) bool {
			a, b := t.byID[kids[i]], t.byID[kids[j]]
			if a.Position != b.Position {
				return a.Position < b.Position
			}
			return a.ID < b.ID
		})
	}
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, id := range t.children[parent] {
			t.byID[id].Depth = depth
			t.order = append(t.order, id)
			walk(id, depth+1)
		}
	}
	walk("", 1)
	if len(t.order) != len(t.byID) {
		return nil, fmt.Errorf("concept tree has a cycle (%d reachable of %d)", len(t.order), len(t.byID))
	}
	return t, nil
}

// Get returns a concept, or nil.
func (t *Taxonomy) Get(id string) *Concept { return t.byID[id] }

// Order is every concept id, depth-first.
func (t *Taxonomy) Order() []string { return t.order }

// Children of a concept ("" for the top level).
func (t *Taxonomy) Children(id string) []string { return t.children[id] }

// Path is the ids from the top-level concept down to id.
func (t *Taxonomy) Path(id string) []string {
	var out []string
	for c := t.byID[id]; c != nil; c = t.byID[c.Parent] {
		out = append([]string{c.ID}, out...)
	}
	return out
}

// Siblings of a concept, without itself.
func (t *Taxonomy) Siblings(id string) []string {
	c := t.byID[id]
	if c == nil {
		return nil
	}
	var out []string
	for _, s := range t.children[c.Parent] {
		if s != id {
			out = append(out, s)
		}
	}
	return out
}

// Members maps each concept to the documents placed on it or anywhere below it,
// each document once, sorted. placed maps a document to its declared concepts.
func (t *Taxonomy) Members(placed map[string][]string) map[string][]string {
	sets := map[string]map[string]bool{}
	for doc, cs := range placed {
		for _, c := range cs {
			for _, a := range t.Path(c) {
				if sets[a] == nil {
					sets[a] = map[string]bool{}
				}
				sets[a][doc] = true
			}
		}
	}
	out := map[string][]string{}
	for c, s := range sets {
		out[c] = sortedKeys(s)
	}
	return out
}

// Level is one comparison set: centroids of its classes are comparable because
// they sit at one depth under one parent (RFC GM section 3.3).
type Level struct {
	Name    string
	Parent  string // "" for the top level
	Classes []string
	// Labels maps a document to the classes of this level it is placed in,
	// through a declared concept at or below the class.
	Labels map[string][]string
}

// Levels returns L1 (the top-level concepts) and one L2 level per concept with
// at least two children, holding the documents placed strictly below it.
func (t *Taxonomy) Levels(placed map[string][]string) []Level {
	parents := []string{""}
	for _, id := range t.order {
		if len(t.children[id]) >= 2 {
			parents = append(parents, id)
		}
	}
	var out []Level
	for _, p := range parents {
		lv := Level{Name: "L1", Parent: p, Classes: t.children[p], Labels: map[string][]string{}}
		if p != "" {
			lv.Name = "L2:" + t.byID[p].Name
		}
		depth := 0
		if p != "" {
			depth = t.byID[p].Depth
		}
		for doc, cs := range placed {
			set := map[string]bool{}
			for _, c := range cs {
				path := t.Path(c)
				// The class is the path element one below p, when p is on the path.
				if depth < len(path) && (p == "" || path[depth-1] == p) {
					set[path[depth]] = true
				}
			}
			if len(set) > 0 {
				lv.Labels[doc] = sortedKeys(set)
			}
		}
		out = append(out, lv)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
