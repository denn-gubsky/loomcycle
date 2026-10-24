package aclres

import (
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RefOf names p's set for a record that outlives p's request (a trigger
// definition's owner, an A2A peer's run): the subject its compiled set was
// compiled for, or — when it could not be compiled — p's own ids and
// ClassKey, so the set is compiled again (ResolveSubject) and a run refused
// while it cannot be, rather than the record naming no one and its runs
// going unnarrowed. nil when p has no subject: nothing is compiled for it
// (Resolve).
func RefOf(p auth.Principal) *authz.SubjectRef {
	if p.Subject == "" {
		return nil
	}
	if p.Effective != nil {
		return authz.RefOf(p.Effective.Subject)
	}
	return &authz.SubjectRef{TenantID: p.TenantID, Subject: p.Subject, TokenDefID: p.TokenDefID, Class: ClassKey(p)}
}
