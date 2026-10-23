package authz

// Static is what an operator declares in config (RFC EY §9): rule lists,
// groups and bindings that exist without a store write. The config package
// builds one at load; the resolver merges it UNDER the store's rows — a
// stored list shadows a static one of the same name, as a dynamic agent
// shadows a static one (the internal/lookup rule) — so a bundle can ship a
// template an operator may later replace in place.
//
// A static list is operator-global: any tenant's binding may name it. A
// static group and a static binding belong to the tenant they name ("" is
// the shared tenant).
type Static struct {
	ACLs     map[string]ACL
	Groups   []StaticGroup
	Bindings []StaticBinding
}

// StaticGroup is one declared group with its direct members.
type StaticGroup struct {
	Tenant      string
	Name        string
	Description string
	ExternalID  string
	Members     []string // subjects
	Groups      []string // member groups
}

// StaticBinding is one declared binding in one tenant.
type StaticBinding struct {
	Tenant  string
	Binding Binding
}

// GroupsOf returns the static groups the subject is a direct member of in
// tenant, and the tenant's static nesting graph.
func (s *Static) GroupsOf(tenant, subject string) (direct []string, graph GroupGraph) {
	if s == nil {
		return nil, nil
	}
	graph = GroupGraph{}
	for _, g := range s.Groups {
		if g.Tenant != tenant {
			continue
		}
		graph[g.Name] = append(graph[g.Name], g.Groups...)
		for _, m := range g.Members {
			if m == subject {
				direct = append(direct, g.Name)
			}
		}
	}
	return direct, graph
}

// BindingsIn returns the static bindings of tenant.
func (s *Static) BindingsIn(tenant string) []Binding {
	if s == nil {
		return nil
	}
	var out []Binding
	for _, b := range s.Bindings {
		if b.Tenant == tenant {
			out = append(out, b.Binding)
		}
	}
	return out
}

// MergeGraph returns the union of two nesting graphs.
func MergeGraph(a, b GroupGraph) GroupGraph {
	out := GroupGraph{}
	for g, members := range a {
		out[g] = append(out[g], members...)
	}
	for g, members := range b {
		out[g] = append(out[g], members...)
	}
	return out
}
