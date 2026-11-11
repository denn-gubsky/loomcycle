package memory

// ontology_policy.go — the effective policy of a concept and of a document (RFC FO
// decision 10, P1).
//
// A file-plan concept may carry `@retention <period>` and `@acl <group>`. This file
// says what those declarations MEAN once inheritance and several concepts are
// accounted for, so that every later consumer (a retention report, an access check)
// reads one answer instead of re-deriving it:
//
//   - retention is the concept's own valid @retention, else the nearest ancestor's;
//   - groups are the union of @acl along the path from the scheme's top concept down,
//     so a narrower concept only ever adds a group;
//   - a document under several concepts keeps the LONGEST retention and requires
//     EVERY group.
//
// Computing it enforces nothing. Pure — no I/O; the caller supplies the group
// catalogue as a function.

import (
	"sort"
	"strconv"
	"strings"
)

// ConceptPolicy is one concept's effective policy.
type ConceptPolicy struct {
	// Retention is the effective period, "" when neither the concept nor an
	// ancestor declares a valid one.
	Retention string
	// RetentionFrom is the tag of the concept whose @retention applies: the
	// concept itself, or the nearest ancestor that declares one.
	RetentionFrom string
	// Groups are the groups a reader must be in, accumulated from the top of the
	// scheme down, sorted. Only groups the catalogue knows are counted.
	Groups []string
}

// InheritedRetention reports whether the retention comes from an ancestor.
func (p ConceptPolicy) InheritedRetention(tag string) bool {
	return p.RetentionFrom != "" && p.RetentionFrom != tag
}

// EffectiveConceptPolicies computes the policy of every concept of the schemes,
// keyed by the concept's chunk id, and the issues found on the way.
//
// groupIssue says why a group named by @acl cannot be counted ("" when it can); a
// group it refuses contributes nothing and is reported once, on the concept that
// names it. nil counts every group.
//
// Concepts arrive in document order, so an ancestor is always computed before the
// concepts under it; an ancestor is found by its tag, the first concept holding a tag
// standing for it.
func EffectiveConceptPolicies(schemes []OntologyScheme, groupIssue func(group string) string) (map[string]ConceptPolicy, []string) {
	out := map[string]ConceptPolicy{}
	var issues []string
	for _, s := range schemes {
		byTag := map[string]ConceptPolicy{}
		for _, c := range s.Concepts {
			var parent ConceptPolicy
			if len(c.Path) > 1 {
				parent = byTag[MirrorTag(s.Name, c.Path[:len(c.Path)-1])]
			}
			p := ConceptPolicy{Retention: parent.Retention, RetentionFrom: parent.RetentionFrom}
			if c.Retention != "" {
				p.Retention, p.RetentionFrom = c.Retention, c.Tag
			}
			groups := append([]string(nil), parent.Groups...)
			if c.ACL != "" {
				if why := groupIssueOf(groupIssue, c.ACL); why != "" {
					issues = append(issues, c.Tag+": @acl "+strconv.Quote(c.ACL)+" "+why+"; ignored")
				} else {
					groups = append(groups, c.ACL)
				}
			}
			p.Groups = sortedUnique(groups)
			for _, iss := range c.Issues {
				// The directive errors the parser already caught: shown on the concept,
				// repeated here so the policy's issue list is complete on its own.
				if strings.HasPrefix(iss, "@retention ") || strings.HasPrefix(iss, "@acl ") {
					issues = append(issues, c.Tag+": "+iss)
				}
			}
			out[c.ChunkID] = p
			if _, seen := byTag[c.Tag]; !seen {
				byTag[c.Tag] = p
			}
		}
	}
	return out, issues
}

func groupIssueOf(f func(string) string, group string) string {
	if f == nil {
		return ""
	}
	return f(group)
}

// GoverningConcept is one concept a document is under, with its effective policy.
type GoverningConcept struct {
	Tag    string
	Policy ConceptPolicy
}

// CombinePolicies is a document's policy from the concepts it is under: the longest
// retention (and the concept it comes from — on a tie, the first in tag order), and
// every group any of them requires.
func CombinePolicies(concepts []GoverningConcept) (retention, governedBy string, groups []string) {
	sorted := append([]GoverningConcept(nil), concepts...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Tag < sorted[j].Tag })
	var longest int64 = -1
	var all []string
	for _, c := range sorted {
		all = append(all, c.Policy.Groups...)
		d, ok := ParseRetention(c.Policy.Retention)
		if !ok {
			continue
		}
		if int64(d) > longest {
			longest, retention, governedBy = int64(d), c.Policy.Retention, c.Tag
		}
	}
	return retention, governedBy, sortedUnique(all)
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
