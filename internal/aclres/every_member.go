package aclres

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// EveryMemberSets compiles, for one tenant, the two sets that answer whether EVERY
// member of the tenant may do something, when no principal is at hand: the node
// statistics pass (RFC GM C8) has no caller, and computes a taxonomy's figures only
// from the documents every member may read.
//
//   - member is a plain member: the member baseline and the tenant-wide bindings, no
//     group, no user binding, no token binding. Every non-isolated member's set holds
//     at least its allows (they are a subset of every member's), so what it allows,
//     every member is allowed — unless one of the member's own bindings denies it.
//   - denies is every live binding of the tenant, to anyone (a user, a group, a
//     token, the tenant; stored or config-declared), compiled as if all applied to
//     one subject. Only its deny rules are asked, unattended
//     (authz.Effective.DeniesUnattended: a condition no caller answers reads as
//     holding): a deny bound to anyone in the tenant may apply to some member.
//
// So "every member may" is: member allows (under the rollout mode) and denies denies
// nothing. That is sound because a decision is deny-overrides: a member's set is the
// baseline plus the tenant's bindings plus its own, so it can be denied only by a
// rule in denies and is allowed at least where member is. It over-restricts — a deny
// bound to a group nobody is in still counts — which leaves a document out of the
// figures, never in.
//
// Class baselines other than the member's are not asked. An isolated user's
// baseline denies the whole tenant document scope, so such a user reads neither the
// ontology nor any figure computed over it; an operator's or an admin's baseline
// denies no document. Nothing is cached: a pass compiles once and asks per document.
func (r *Resolver) EveryMemberSets(ctx context.Context, tenant string) (member, denies *authz.Effective, err error) {
	static := r.staticNow()
	rows, err := r.st.AclBindingList(ctx, tenant, false)
	if err != nil {
		return nil, nil, fmt.Errorf("bindings: %w", err)
	}
	bindings := make([]authz.Binding, 0, len(rows))
	for _, row := range rows {
		bindings = append(bindings, authz.Binding{ID: row.BindingID, ACL: row.ACLName, SubjectKind: authz.SubjectKind(row.SubjectKind),
			SubjectID: row.SubjectID, ExpiresAt: row.ExpiresAt})
	}
	bindings = append(bindings, static.BindingsIn(tenant)...)
	acls := map[string]authz.ACLVersion{}
	for _, b := range bindings {
		// A built-in name reached through a binding resolves to nothing, as in input.
		if _, seen := acls[b.ACL]; seen || authz.IsBuiltin(b.ACL) {
			continue
		}
		av, found, err := r.lookupACL(ctx, tenant, b.ACL, static)
		if err != nil {
			return nil, nil, err
		}
		if found {
			acls[b.ACL] = av
		}
	}
	now := r.now()
	base := authz.BaselineMember
	memberIn := authz.Input{Subject: authz.Subject{TenantID: tenant, Class: "member"}, Now: now, Baseline: base,
		ACLs: map[string]authz.ACLVersion{base: {ACL: authz.Baselines()[base]}}, Bindings: []authz.Binding{{ACL: base, SubjectKind: authz.SubjectTenant}}}
	deniesIn := authz.Input{Subject: authz.Subject{TenantID: tenant}, Now: now, ACLs: acls}
	for _, b := range bindings {
		if b.SubjectKind == authz.SubjectTenant {
			memberIn.Bindings = append(memberIn.Bindings, b)
		}
		everyone := b
		everyone.SubjectKind, everyone.SubjectID = authz.SubjectTenant, ""
		deniesIn.Bindings = append(deniesIn.Bindings, everyone)
	}
	for name, av := range acls {
		memberIn.ACLs[name] = av
	}
	if member, err = authz.Compile(memberIn); err != nil {
		return nil, nil, err
	}
	if denies, err = authz.Compile(deniesIn); err != nil {
		return nil, nil, err
	}
	return member, denies, nil
}
