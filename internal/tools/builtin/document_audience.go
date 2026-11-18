package builtin

// document_audience.go — which documents of the tenant scope EVERY member of the
// tenant may read (RFC GM C8). The taxonomy's figures — a node's statistics, its
// annotation's request, its built-in series — are computed from those documents
// alone and then served to every reader of the ontology, so nothing about a
// restricted document can be learned from a node, whoever asks.
//
// THE PREDICATE. A document is unrestricted when, at every Path name it is filed
// under (the bare tenant scope when it is filed nowhere), the tenant's rules let a
// plain member read it and no deny bound to anyone in the tenant refuses it:
//
//	member.Decide(read, document:tenant:<name>) resolves to allowed under the
//	rollout mode (compat: anything but a deny; enforce: an allow), AND
//	denies.DeniesUnattended(read, document:tenant:<name>) is false
//
// for the two sets MemberAudience compiles (aclres.Resolver.EveryMemberSets says
// why that is every member). It is asked through the same engine and the same name
// lookup (docACL.everyName) a real reader's question goes through, with no caller:
// the node statistics sweeper has none. With no rule at all, every document is
// unrestricted; with no Audience wired, every document counts.

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// MemberAudience is the slice of *aclres.Resolver the taxonomy's figures read.
type MemberAudience interface {
	EveryMemberSets(ctx context.Context, tenant string) (member, denies *authz.Effective, err error)
	Mode() authz.Mode
}

// unrestrictedFamily prefixes the hash every taxonomy figure is stored under since
// RFC GM C8: a node_stats row's inputs_sha256, a measure_points row's and a node
// annotation record's. A figure stored before it was computed over every member, a
// restricted one included, and is never served: a read takes only this family's
// rows, and the next pass computes them again (a node annotation: the next
// annotator pass writes it again).
const unrestrictedFamily = "u1:"

// audienceSets is one tenant spelling's pair of sets.
type audienceSets struct{ member, denies *authz.Effective }

// unrestrictedDocList is the tenant scope key names narrowed to the documents every
// member may read, in the shape a reader's narrowing has (docReadList): nil when
// every document is. Built once per pass; the figures ask it per document.
func (d *Document) unrestrictedDocList(ctx context.Context, key sqlmem.ScopeKey) (*docReadList, error) {
	if d.Audience == nil {
		return nil, nil
	}
	// The default tenant is spelled two ways across the planes (bodyTenantsFor): a
	// rule or a name under either counts.
	tenants := bodyTenantsFor(key.Tenant)
	a := &docACL{d: d, key: key}
	var sets []audienceSets
	for _, t := range tenants {
		member, denies, err := d.Audience.EveryMemberSets(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("the tenant's access-control rules: %w", err)
		}
		sets = append(sets, audienceSets{member: member, denies: denies})
		a.pathSensitive = a.pathSensitive || member.HasInstanceRules("document", key.Scope) || denies.HasInstanceRules("document", key.Scope)
	}
	mode := d.Audience.Mode()
	allows := func(res authz.Resource) bool {
		for _, s := range sets {
			req := authz.Request{Subject: s.member.Subject, Action: "read", Resource: res}
			if s.denies.DeniesUnattended(req) || !authz.Resolve(mode, s.member.Decide(req), true) {
				return false
			}
		}
		return true
	}
	unnamed, err := a.everyName(ctx, "", allows)
	if err != nil {
		return nil, err
	}
	if !a.pathSensitive {
		if unnamed {
			return nil, nil
		}
		return &docReadList{}, nil
	}
	// Every name of the scope, under every spelling of the tenant: a name missed here
	// would leave its document on the bare scope's verdict — counted when a rule on
	// that name refuses it.
	a.paths = map[string][]string{}
	for _, t := range tenants {
		rows, err := d.Store.DirentListUnder(ctx, t, key.Scope, direntScopeID(key), "/")
		if err != nil {
			return nil, err
		}
		for id, names := range namesByDocument(rows) {
			a.paths[id] = append(a.paths[id], names...)
		}
	}
	list := &docReadList{unnamedReadable: unnamed, differs: map[string]bool{}}
	for id := range a.paths {
		ok, err := a.everyName(ctx, id, allows)
		if err != nil {
			return nil, err
		}
		if ok != unnamed {
			list.differs[id] = true
		}
	}
	if unnamed && len(list.differs) == 0 {
		return nil, nil
	}
	return list, nil
}
