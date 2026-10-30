package memory

// ontology_instances.go — the Instances section of the ontology document (RFC FO P1c-2).
//
// Each entry under `@section instances` is a REAL THING — an organisation, a unit, a post
// — not a kind of thing. `@a <class>` says what it is; its child entries are other
// instances, related to it by the kind its `@nesting` directive names (an org chart:
// `@nesting sub_organization_of`). Any other directive that names a relation kind
// declares a statement to the instance titled by its value (`@reports_to Head of
// Compliance`).
//
// The section is an AUTHORING SURFACE, not a second registry (RFC FO D7): on an explicit
// confirm each instance is adopted into the tenant entity registry — the same subject
// node the memory consolidator writes — under the consolidator's own key. Pure parsing.

import "strings"

// InstanceRelation is one declared statement from an instance to another instance.
type InstanceRelation struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

// OntologyInstance is one instance entry.
type OntologyInstance struct {
	Name string `json:"name"`
	// Class is the `@a` class; an instance with none is reported and not adopted.
	Class string   `json:"class,omitempty"`
	Alt   []string `json:"alt,omitempty"`
	// Nesting is the relation kind this instance's CHILDREN have to it.
	Nesting string `json:"nesting,omitempty"`
	// Parent is the instance this one is nested under, and ParentKind the parent's
	// @nesting kind — the statement nesting declares, "" when the parent names none.
	Parent     string             `json:"parent,omitempty"`
	ParentKind string             `json:"parent_kind,omitempty"`
	Relations  []InstanceRelation `json:"relations,omitempty"`
	// RegistryKey is the key the instance was adopted under, recorded on the instance
	// chunk at adoption so a rename keeps it. "" until first adopted.
	RegistryKey string   `json:"registry_key,omitempty"`
	ChunkID     string   `json:"chunk_id,omitempty"`
	Issues      []string `json:"issues,omitempty"`
}

// ParseInstanceBody reads an instance entry's body. A directive that is not one of the
// instance's own and not another section's known directive is read as a relation kind:
// relation kinds are the tenant's own words, so they cannot be listed here, and whether
// the kind is in force is checked when the statement is written.
func ParseInstanceBody(name, body string) OntologyInstance {
	in := OntologyInstance{Name: name}
	for _, d := range ParseDirectives(body) {
		switch d.Name {
		case "a":
			in.Class = strings.TrimSpace(d.Value)
		case "identity":
			// The title is an instance's identity — the default; accepted for symmetry with
			// the RFC's examples.
		case "alt":
			if v := strings.TrimSpace(d.Value); v != "" {
				in.Alt = append(in.Alt, v)
			}
		case "nesting":
			in.Nesting = firstWord(d.Value)
		default:
			if knownDirectives[d.Name] {
				in.Issues = append(in.Issues, "@"+d.Name+" is not an instance directive; ignored")
				continue
			}
			target := strings.TrimSpace(d.Value)
			if target == "" {
				in.Issues = append(in.Issues, "@"+d.Name+" names no instance to relate to; ignored")
				continue
			}
			in.Relations = append(in.Relations, InstanceRelation{Kind: d.Name, Target: target})
		}
	}
	if in.Class == "" {
		in.Issues = append(in.Issues, "no @a class: this instance cannot be adopted until it says what it is")
	}
	if len(ParseOntologyFields(body)) > 0 {
		in.Issues = append(in.Issues, "an instance carries no fields; the field bullets here are ignored")
	}
	return in
}
