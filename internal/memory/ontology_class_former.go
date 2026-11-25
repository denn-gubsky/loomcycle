package memory

// ontology_class_former.go — a class's former name reads as the class (RFC GQ 4.6, P5.1).
//
// A class declares the name it used to have with `@renamed_from <old>`. Rows written
// before the rename keep the old name in chunks.type and documents.type until the
// rename job rewrites them (and, for a type nobody ever declared, for good: the
// directive is also how an operator maps an undeclared type onto a class). Every place
// that asks "which class is this type" therefore resolves a former name to its class,
// so those rows stay declared, filterable and counted.
//
// WHEN A FORMER NAME IS NOT HONOURED. Both cases are reported (FormerClassNameIssues):
//
//   - It names a class in force (its own class included). The class in force keeps the
//     name: its rows are its own, and letting another class claim them by a directive
//     would move them without anybody retyping anything.
//   - Two classes claim it. It resolves to NEITHER: picking one would file the rows
//     under a class — and its patterns, fields and policy — chosen by document order.
//     The rows read as undeclared, which is what the operator is shown and can fix.
//
// A retired class is a class in force for reads, so it resolves its former name as it
// resolves its own.

import (
	"sort"
	"strconv"
	"strings"
)

// ClassAliases maps the former class names honoured at read to their classes. The zero
// value honours none, and is what an ontology with no @renamed_from yields.
type ClassAliases struct {
	classOf  map[string]string // lower-cased former name → the class's name
	formerOf map[string]string // lower-cased class name → its former name, as written
}

// FormerClassNames reads the former names the classes in force declare. classes is the
// list the caller resolves against (EffectiveClasses, seed included), so a former name
// equal to a seed class is caught too.
func FormerClassNames(classes []OntologyTerm) ClassAliases {
	a, _ := formerClassNames(classes)
	return a
}

// FormerClassNameIssues are the advisories for the former names that are not honoured.
// nil when every one is, and when none is declared.
func FormerClassNameIssues(classes []OntologyTerm) []string {
	_, issues := formerClassNames(classes)
	return issues
}

func formerClassNames(classes []OntologyTerm) (ClassAliases, []string) {
	// The common case declares none: nothing is built, so a lookup costs one pass.
	declared := false
	for _, c := range classes {
		if c.RenamedFrom != "" {
			declared = true
			break
		}
	}
	if !declared {
		return ClassAliases{}, nil
	}
	inForce := make(map[string]string, len(classes))
	for _, c := range classes {
		inForce[strings.ToLower(c.Name)] = c.Name
	}
	claims := map[string][]string{} // lower-cased former name → the classes claiming it
	spelling := map[string]string{}
	var issues []string
	for _, c := range classes {
		if c.RenamedFrom == "" {
			continue
		}
		f := strings.ToLower(c.RenamedFrom)
		if held, ok := inForce[f]; ok {
			issues = append(issues, "@renamed_from "+c.RenamedFrom+" on the class "+strconv.Quote(c.Name)+
				" names a class in force ("+strconv.Quote(held)+"), which keeps the name; the directive is ignored")
			continue
		}
		claims[f] = append(claims[f], c.Name)
		if _, ok := spelling[f]; !ok {
			spelling[f] = c.RenamedFrom
		}
	}
	a := ClassAliases{classOf: map[string]string{}, formerOf: map[string]string{}}
	for f, names := range claims {
		if len(names) > 1 {
			sort.Strings(names)
			issues = append(issues, "the former name "+strconv.Quote(spelling[f])+" is claimed by @renamed_from on more than one class ("+
				strings.Join(names, ", ")+"), so it reads as none of them; keep the directive on one")
			continue
		}
		a.classOf[f] = names[0]
		a.formerOf[strings.ToLower(names[0])] = spelling[f]
	}
	sort.Strings(issues)
	return a, issues
}

// TenantClassAliases is FormerClassNames over the classes a tenant layer puts in force:
// the tenant's terms over the seed, as EffectiveClasses layers them. A draft
// (confirmed false) honours none, since its classes are not in force; and a layer that
// declares no @renamed_from costs one pass over it, with no layering.
func TenantClassAliases(tenantTerms []OntologyTerm, confirmed bool) ClassAliases {
	if !confirmed {
		return ClassAliases{}
	}
	for _, t := range tenantTerms {
		if t.RenamedFrom != "" {
			return FormerClassNames(EffectiveClasses(tenantTerms, true))
		}
	}
	return ClassAliases{}
}

// Any reports whether any former name is honoured.
func (a ClassAliases) Any() bool { return len(a.classOf) > 0 }

// Resolve is the name of the class a type names: the class's own name for a former
// name (case ignored, as a type is matched), and name itself otherwise — a class in
// force, an undeclared type and "" all come back unchanged.
func (a ClassAliases) Resolve(name string) string {
	if len(a.classOf) == 0 {
		return name
	}
	if c, ok := a.classOf[strings.ToLower(strings.TrimSpace(name))]; ok {
		return c
	}
	return name
}

// Former is the honoured former name of the class named class, as its directive
// spells it; "" when it has none.
func (a ClassAliases) Former(class string) string {
	return a.formerOf[strings.ToLower(strings.TrimSpace(class))]
}

// ResolveClassName is FormerClassNames(classes).Resolve(name), for a caller that asks
// once.
func ResolveClassName(classes []OntologyTerm, name string) string {
	return FormerClassNames(classes).Resolve(name)
}
