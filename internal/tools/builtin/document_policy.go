package builtin

// document_policy.go — the effective policy a confirmed ontology's concepts set on
// documents (RFC FO decision 10, P1): describe_ontology's per-concept retention and
// groups, and Document op=effective_policy for one document.
//
// REPORTED, NOT ENFORCED. Nothing here removes access or deletes anything: the answer
// says what the policy would be, so an operator can see it before any phase acts on it.
// Every answer carries enforced:false and says so in words.
//
// Computed at read time over the memoised ontology read; nothing is stored. A draft
// ontology sets no policy, an inert concept contributes nothing, and an @acl group the
// tenant does not have is reported and ignored — never read as "deny everyone".

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	// policyNotEnforced is the sentence every policy answer carries.
	policyNotEnforced = "Reported only: no access is removed and nothing is deleted by age because of this policy yet."
	// policyOpenMode is added when the caller has no access-control rules at all.
	policyOpenMode = "No access-control rules apply to this caller (open mode), so groups would not limit access here even once they are enforced."
	// policyGroupReads bounds the group reads one readership check makes, nested
	// groups included.
	policyGroupReads = 64
)

// describedScheme is a concept scheme with each concept's effective policy.
type describedScheme struct {
	memrank.OntologyScheme
	Concepts []describedConcept `json:"concepts,omitempty"`
}

// describedConcept is a concept with its effective policy beside what it declares.
type describedConcept struct {
	memrank.OntologyConcept
	// EffectiveRetention is the concept's own @retention, else its nearest ancestor's.
	EffectiveRetention string `json:"effective_retention,omitempty"`
	// InheritedRetention: EffectiveRetention comes from RetentionFrom, an ancestor.
	InheritedRetention bool   `json:"inherited_retention,omitempty"`
	RetentionFrom      string `json:"retention_from,omitempty"`
	// EffectiveGroups are the groups a reader must be in: every @acl from the top of
	// the scheme down to this concept.
	EffectiveGroups []string `json:"effective_groups,omitempty"`
}

// describedPolicy is describe_ontology's account of the policy as a whole.
type describedPolicy struct {
	Enforced bool   `json:"enforced"`
	Note     string `json:"note"`
	OpenMode bool   `json:"open_mode,omitempty"`
	// Issues are the declarations that contribute nothing (an unknown group, a period
	// that does not parse, a proposed concept) and the readership warnings.
	Issues []string `json:"issues,omitempty"`
}

// policyGroups is the caller's tenant's group catalogue: stored groups and the
// operator config's, merged as the access-control resolver merges them.
type policyGroups struct {
	tenant string
	graph  authz.GroupGraph // every known group is a key
	static *authz.Static
	// fault is set when the catalogue could not be read; groups are then counted
	// unchecked and the answer says so.
	fault bool
}

func (d *Document) loadPolicyGroups(ctx context.Context) policyGroups {
	g := policyGroups{tenant: tools.RunIdentity(ctx).TenantID}
	if d.Cfg != nil {
		g.static = d.Cfg.ResolvedACL
	}
	stored, err := d.Store.GroupGraph(ctx, g.tenant)
	if err != nil {
		g.fault = true
		return g
	}
	_, staticGraph := g.static.GroupsOf(g.tenant, "")
	g.graph = authz.MergeGraph(stored, staticGraph)
	return g
}

// issue says why a group named by @acl is not counted, "" when it is.
func (g policyGroups) issue(name string) string {
	if !authz.ValidName(name) {
		return "is not a valid group name"
	}
	if g.fault {
		return ""
	}
	if _, ok := g.graph[name]; !ok {
		return "is not a group of this tenant"
	}
	return ""
}

func (g policyGroups) issues() []string {
	if g.fault {
		return []string{"the tenant's groups could not be read, so @acl groups are listed without checking that they exist; read again"}
	}
	return nil
}

// conceptPolicies is every concept's effective policy for a confirmed read, with the
// issues; nil for a draft or an absent ontology, which set no policy.
func conceptPolicies(read OntologyRead, groups policyGroups) (map[string]memrank.ConceptPolicy, []string) {
	if !read.Confirmed || read.DocumentID == "" {
		return nil, nil
	}
	policies, issues := memrank.EffectiveConceptPolicies(read.Schemes, groups.issue)
	issues = append(groups.issues(), issues...)
	for _, p := range read.Proposals {
		if p.Section != memrank.SectionSchemes {
			continue
		}
		for _, dir := range memrank.ParseDirectives(p.Body) {
			if dir.Name == "retention" || dir.Name == "acl" {
				issues = append(issues, "concept "+strconv.Quote(p.Name)+" ("+p.Status+"): its @"+dir.Name+
					" is not in force until an operator accepts the concept")
			}
		}
	}
	return policies, issues
}

// describePolicy adds each concept's effective policy and the policy's issues to a
// description. The read is the one the description was derived from.
func (d *Document) describePolicy(ctx context.Context, read OntologyRead, out *ontologyDescription) {
	out.Policy = &describedPolicy{Note: policyNotEnforced, OpenMode: tools.CallerACL(ctx) == nil}
	if out.Policy.OpenMode {
		out.Policy.Note += " " + policyOpenMode
	}
	if !read.Confirmed {
		if out.Exists {
			out.Policy.Note += " The ontology is a draft, so it sets no policy."
		}
		return
	}
	groups := d.loadPolicyGroups(ctx)
	policies, issues := conceptPolicies(read, groups)
	out.Policy.Issues = issues
	rc := newReadershipCheck(d, ctx, groups)
	// Groups only accumulate down a scheme, so a set nobody can satisfy repeats on
	// every concept under the first one that requires it: warned once, there.
	warned := map[string]bool{}
	for i := range out.Schemes {
		for j := range out.Schemes[i].Concepts {
			c := &out.Schemes[i].Concepts[j]
			p := policies[c.ChunkID]
			c.EffectiveRetention, c.RetentionFrom = p.Retention, p.RetentionFrom
			c.InheritedRetention = p.InheritedRetention(c.Tag)
			if !c.InheritedRetention {
				c.RetentionFrom = ""
			}
			c.EffectiveGroups = p.Groups
			if set := strings.Join(p.Groups, "\x00"); !warned[set] {
				warned[set] = true
				if w := rc.warn(c.Tag, p.Groups); w != "" {
					out.Policy.Issues = append(out.Policy.Issues, w)
				}
			}
		}
	}
	out.Policy.Issues = append(out.Policy.Issues, rc.notes()...)
}

// ---- effective_policy ----

// governingConcept is one concept a document is under, as effective_policy reports it.
type governingConcept struct {
	Concept string `json:"concept"`
	Tag     string `json:"tag"`
	ChunkID string `json:"chunk_id"`
	// Via is how the document is under it: the statement kind (tenant scope) or "tag".
	Via                string   `json:"via"`
	EffectiveRetention string   `json:"effective_retention,omitempty"`
	RetentionFrom      string   `json:"retention_from,omitempty"`
	EffectiveGroups    []string `json:"effective_groups,omitempty"`
}

// effectivePolicyAnswer is effective_policy's answer.
type effectivePolicyAnswer struct {
	DocumentID string `json:"document_id"`
	Scope      string `json:"scope"`
	Enforced   bool   `json:"enforced"`
	Note       string `json:"note"`
	OpenMode   bool   `json:"open_mode,omitempty"`
	// OntologyStatus is the tenant ontology's: confirmed, draft, or "" without one.
	OntologyStatus string             `json:"ontology_status"`
	Concepts       []governingConcept `json:"concepts"`
	// Retention is the longest effective retention of the document's concepts, and
	// RetentionFrom the concept (by tag) it comes from.
	Retention     string `json:"retention,omitempty"`
	RetentionFrom string `json:"retention_from,omitempty"`
	// Groups are every group any of its concepts requires: a reader would have to be
	// in all of them. Always present ([] when none).
	Groups []string `json:"groups"`
	Issues []string `json:"issues,omitempty"`
}

// effectivePolicy answers op=effective_policy. Execute has already asked read on the
// document, exactly as get_document asks it.
func (d *Document) effectivePolicy(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	const op = "effective_policy"
	docID, err := d.docIDFromInput(ctx, key, in)
	if err != nil {
		return docFail(op+": ", err), nil
	}
	res, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if len(res.Rows) == 0 {
		return errNotFound(op+": no such document: "+docID, "Find the document with op=query_documents in the same scope."), nil
	}
	rootID := asStr(res.Rows[0][0])
	out := effectivePolicyAnswer{DocumentID: docID, Scope: key.Scope, Note: policyNotEnforced,
		OpenMode: tools.CallerACL(ctx) == nil, Concepts: []governingConcept{}, Groups: []string{}}
	if out.OpenMode {
		out.Note += " " + policyOpenMode
	}

	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		return errBusiness(op+": the tenant ontology could not be read", "Retry the call; a store fault is usually transient."), nil
	}
	if read.DocumentID != "" {
		out.OntologyStatus = memrank.OntologyDraftStatus
		if read.Confirmed {
			out.OntologyStatus = "confirmed"
		}
		// The concepts and their directives are the ontology's text: shown only to a
		// caller who may read the ontology, as describe_ontology asks.
		tkey, _, err := d.ontologyTenantKey(ctx)
		if err != nil {
			return docFail(op, err), nil
		}
		if acl := d.newDocACL(ctx, tkey); acl != nil {
			readable, err := acl.readable(ctx, read.DocumentID)
			if err != nil {
				return errFrom(op+": "+err.Error(), err), nil
			}
			if !readable {
				out.Issues = append(out.Issues, "the caller may not read the tenant ontology, so its policy is not shown")
				return jsonResult(out)
			}
		}
	}
	if !read.Confirmed {
		out.Note += " The tenant ontology is not confirmed, so it sets no policy."
		return jsonResult(out)
	}

	groups := d.loadPolicyGroups(ctx)
	policies, ontIssues := conceptPolicies(read, groups)
	byChunk := map[string]memrank.OntologyConcept{}
	byTag := map[string]memrank.OntologyConcept{}
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			byChunk[c.ChunkID] = c
			if _, seen := byTag[c.Tag]; !seen {
				byTag[c.Tag] = c
			}
		}
	}
	found, err := d.documentConcepts(ctx, key, rootID, docID, byChunk, byTag)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	var gov []memrank.GoverningConcept
	tags := map[string]bool{}
	for _, f := range found {
		p := policies[f.c.ChunkID]
		gc := governingConcept{Concept: f.c.Name, Tag: f.c.Tag, ChunkID: f.c.ChunkID, Via: f.via,
			EffectiveRetention: p.Retention, EffectiveGroups: p.Groups}
		if p.InheritedRetention(f.c.Tag) {
			gc.RetentionFrom = p.RetentionFrom
		}
		out.Concepts = append(out.Concepts, gc)
		gov = append(gov, memrank.GoverningConcept{Tag: f.c.Tag, Policy: p})
		tags[f.c.Tag] = true
	}
	retention, from, required := memrank.CombinePolicies(gov)
	out.Retention = retention
	if from != "" {
		// The concept that governs is the one the document is under; the period may
		// still be declared above it, which that concept's own entry says.
		out.RetentionFrom = from
	}
	if required != nil {
		out.Groups = required
	}
	// Only the issues about the concepts this document is under (and their
	// ancestors, whose declarations reach it), so the answer is about this document.
	for _, iss := range ontIssues {
		if tag, _, ok := strings.Cut(iss, ": "); ok && touchesAny(tag, tags) {
			out.Issues = append(out.Issues, iss)
		}
	}
	out.Issues = append(out.Issues, groups.issues()...)
	rc := newReadershipCheck(d, ctx, groups)
	if w := rc.warn("this document", out.Groups); w != "" {
		out.Issues = append(out.Issues, w)
	}
	out.Issues = append(out.Issues, rc.notes()...)
	return jsonResult(out)
}

// touchesAny reports whether tag is one of tags or an ancestor of one.
func touchesAny(tag string, tags map[string]bool) bool {
	for t := range tags {
		if t == tag || strings.HasPrefix(t, tag+"/") {
			return true
		}
	}
	return false
}

type foundConcept struct {
	c   memrank.OntologyConcept
	via string
}

// documentConcepts is the concepts a document is under (RFC FO decision 10.1). In the
// tenant scope: the concepts its root has a live ASSERTED subject (or a sub-kind such
// as primary_topic) or classified_as statement to — a proposed statement changes
// nothing. In a user or agent scope, where no statement reaches the ontology: the
// concepts whose mirror tag the root or the document carries. Sorted by tag; a concept
// reached two ways is listed once, by its first way in that order.
func (d *Document) documentConcepts(ctx context.Context, key sqlmem.ScopeKey, rootID, docID string,
	byChunk, byTag map[string]memrank.OntologyConcept) ([]foundConcept, error) {
	seen := map[string]bool{}
	var out []foundConcept
	add := func(c memrank.OntologyConcept, via string) {
		if !seen[c.ChunkID] {
			seen[c.ChunkID] = true
			out = append(out, foundConcept{c: c, via: via})
		}
	}
	if key.Scope == "tenant" {
		subjectKinds, _ := d.expandKindFilter(ctx, []string{"subject"})
		filed, _ := d.expandKindFilter(ctx, []string{"classified_as"})
		kinds := append(append([]string{}, subjectKinds...), filed...)
		kph, kargs := inPlaceholders(kinds)
		args := append([]any{rootID, edgeStatusAsserted}, kargs...)
		res, err := d.query(ctx, key, `SELECT e.to_id, e.kind FROM chunk_edges e
			 WHERE e.from_id = ? AND e.status = ? AND e.kind IN (`+kph+`) AND `+edgeLiveClause+`
			 ORDER BY e.kind, e.to_id`, args...)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			if c, ok := byChunk[asStr(r[0])]; ok {
				add(c, asStr(r[1]))
			}
		}
	} else {
		res, err := d.query(ctx, key, `SELECT tag FROM chunk_tags WHERE chunk_id = ?
			UNION SELECT tag FROM document_tags WHERE document_id = ?`, rootID, docID)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			if c, ok := byTag[asStr(r[0])]; ok {
				add(c, "tag")
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].c.Tag < out[j].c.Tag })
	return out, nil
}

// ---- readership lint (RFC FO decision 10.3) ----

// readershipCheck warns when no member of the tenant is in every group a set
// requires: once groups are enforced, only a rule naming the document's own path would
// let anyone read it.
//
// It reads group membership, so it runs only for a caller who may see who is in each
// of those groups (the rule Identity op=group applies); for anyone else the warning is
// left out and a note says it was not checked. Bounded at policyGroupReads group reads
// per answer.
type readershipCheck struct {
	d      *Document
	ctx    context.Context
	groups policyGroups
	caller identityCaller
	own    map[string]bool
	users  map[string]map[string]bool // group → its members, nested groups included
	reads  int
	// skipped notes why a check was not made, once per reason.
	skipped map[string]bool
}

func newReadershipCheck(d *Document, ctx context.Context, groups policyGroups) *readershipCheck {
	rc := &readershipCheck{d: d, ctx: ctx, groups: groups, caller: identityCallerOf(ctx),
		own: map[string]bool{}, users: map[string]map[string]bool{}, skipped: map[string]bool{}}
	if rc.caller.eff != nil {
		for _, g := range rc.caller.eff.Subject.Groups {
			rc.own[g] = true
		}
	}
	return rc
}

// warn returns the warning for what (a concept's tag, or "this document") requiring
// groups, "" when someone is in all of them or the check was not made.
func (rc *readershipCheck) warn(what string, groups []string) string {
	if len(groups) < 2 || rc.groups.fault {
		return ""
	}
	for _, g := range groups {
		if _, full := rc.caller.mayReadGroup(rc.ctx, g, rc.own); !full {
			rc.skipped["the caller may not see the members of every group involved"] = true
			return ""
		}
	}
	var common map[string]bool
	for _, g := range groups {
		members, ok := rc.members(g, map[string]bool{})
		if !ok {
			rc.skipped["the groups involved are too many or could not be read"] = true
			return ""
		}
		if common == nil {
			common = map[string]bool{}
			for u := range members {
				common[u] = true
			}
			continue
		}
		for u := range common {
			if !members[u] {
				delete(common, u)
			}
		}
	}
	if len(common) > 0 {
		return ""
	}
	return what + ": no member is in all of " + strings.Join(groups, ", ") +
		"; once groups are enforced, only a rule naming the document's own path would let anyone read it"
}

// members is everyone in g, through nested groups; false when the bound is reached or
// a read fails.
func (rc *readershipCheck) members(g string, visiting map[string]bool) (map[string]bool, bool) {
	if m, ok := rc.users[g]; ok {
		return m, true
	}
	if visiting[g] {
		return map[string]bool{}, true // a cycle adds nothing new
	}
	visiting[g] = true
	rc.reads++
	if rc.reads > policyGroupReads {
		return nil, false
	}
	m := map[string]bool{}
	var nested []string
	rows, err := rc.d.Store.GroupMembers(rc.ctx, rc.groups.tenant, g)
	if err != nil && !isNotFound(err) {
		return nil, false
	}
	for _, r := range rows {
		switch r.MemberKind {
		case store.GroupMemberUser:
			m[r.MemberID] = true
		case store.GroupMemberGroup:
			nested = append(nested, r.MemberID)
		}
	}
	if rc.groups.static != nil {
		for _, sg := range rc.groups.static.Groups {
			if sg.Tenant == rc.groups.tenant && sg.Name == g {
				for _, u := range sg.Members {
					m[u] = true
				}
				nested = append(nested, sg.Groups...)
			}
		}
	}
	for _, n := range nested {
		sub, ok := rc.members(n, visiting)
		if !ok {
			return nil, false
		}
		for u := range sub {
			m[u] = true
		}
	}
	rc.users[g] = m
	return m, true
}

// notes says which readership checks were not made, and why.
func (rc *readershipCheck) notes() []string {
	var out []string
	for why := range rc.skipped {
		out = append(out, "readership not checked: "+why)
	}
	sort.Strings(out)
	return out
}
