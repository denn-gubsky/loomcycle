// identitydocs.go — provisioning the per-user and per-tenant identity Documents at the
// moment a principal is ESTABLISHED, rather than on the first run that happens to
// reference them.
package http

import (
	"context"
	"log"
)

// ProvisionIdentityDocs creates the tenant-root and user-root Documents for a principal
// if they do not exist yet.
//
// WHY EAGER, WHEN LAZY ALREADY WORKS. Both documents are seeded from a template the
// person is expected to fill in — the user-root Document's Identity section is what lets
// placement tell a fact about the user from a fact about a colleague, and it can only do
// that if somebody wrote their name in it. Lazily provisioning on the first run that
// references {{memory:user_info}} means the document does not exist until an agent asks
// for it, so a person who has not run anything yet has nothing to fill in and no way to
// discover there is something to fill in. The template is useless if it arrives after the
// moment it was needed.
//
// So it is created when the principal is established: at token mint, and at boot for
// config-declared principals. Those are the two moments a (tenant, subject) comes into
// existence, and both are rare — which is the other half of the reason. The underlying
// ensure* helpers note that their exists-check is cheap "dwarfed by the per-run LLM call
// that gates this path"; hanging them off something frequent, like authentication, would
// put an indexed read on the hot path for every request. Mint and boot happen once per
// principal.
//
// BEST-EFFORT, ALWAYS. A failure here must never fail the thing that triggered it: minting
// a token is a security operation and booting is a liveness one, while this is a
// convenience. Both ensure* helpers are already idempotent, singleflighted in-process and
// advisory-locked across replicas, so a skipped or failed pass is recovered by the next
// mint, the next boot, or the original lazy path on first reference.
//
// Empty subject provisions the tenant document alone — a tenant-scoped token with no
// distinct subject establishes the tenant but no person.
func (s *Server) ProvisionIdentityDocs(ctx context.Context, tenant, subject string) {
	if s.store == nil || s.sqlMem == nil {
		return // no SQL Memory: Documents are unavailable, and that is not an error here
	}
	// Read through the accessor, not a captured field, so a runtime config reload is
	// honoured on the next mint rather than needing a restart.
	//
	// NIL-SAFE, and nil means DO NOTHING. New() always installs the holder, so a nil
	// config is a Server assembled without one — a test, or a degraded construction.
	// Provisioning is a side effect on the operator's data, and doing it without knowing
	// whether the operator wants it is worse than skipping it: the lazy path still
	// creates the document on first reference either way.
	cfg := s.cfg()
	if cfg == nil || !cfg.Env.ProvisionIdentityDocs {
		return
	}
	mi := memInject{Tenant: tenant, UserID: subject}
	s.ensureTenantRootDoc(ctx, mi)
	if subject != "" {
		s.ensureUserRootDoc(ctx, mi)
	}
}

// ProvisionDeclaredPrincipalDocs provisions the identity Documents for every
// config-declared principal, at boot.
//
// Declared principals are the case lazy provisioning serves worst: they exist because an
// operator wrote them into the config, so they are established before anybody has
// authenticated, let alone started a run. Their documents should be waiting.
//
// Runs to completion synchronously against however many principals the config declares —
// a hand-written list, so tens at most. The caller decides whether to background it.
func (s *Server) ProvisionDeclaredPrincipalDocs(ctx context.Context) {
	if s.store == nil || s.sqlMem == nil {
		return
	}
	cfg := s.cfg()
	if cfg == nil || !cfg.Env.ProvisionIdentityDocs {
		return
	}
	seen := map[string]bool{}
	n := 0
	for _, dp := range cfg.ResolvedPrincipals {
		t, sub := dp.Principal.TenantID, dp.Principal.Subject
		// Dedup so two tokens for one person do not each pay the exists-check.
		k := t + "\x00" + sub
		if seen[k] {
			continue
		}
		seen[k] = true
		s.ProvisionIdentityDocs(ctx, t, sub)
		n++
	}
	if n > 0 {
		log.Printf("memory: provisioned identity documents for %d declared principal(s)", n)
	}
}
