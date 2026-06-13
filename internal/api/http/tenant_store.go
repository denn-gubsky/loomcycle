package http

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// tenantScopedStore is a per-request, tenant-aware façade over store.Store. It
// turns the per-handler tenant-isolation CONVENTION — each read remembering to
// call tenantVisible / sessionOwnershipOK — into a single CHOKE-POINT: every
// tenant-bearing or run-keyed read flows through here, and a row outside the
// caller's tenant is folded into an opaque *store.ErrNotFound (the same
// no-existence-oracle posture handlers return by hand today). Build one per
// request with s.tenantStore(ctx); the scope is captured from the ctx principal
// at construction.
//
// WHOLE-TENANT model (RFC L/N): the boundary is the TENANT, not the subject —
// subjects within a tenant share its workspace (they collaborate). The
// cross-TENANT boundary is the security property. A super-admin (substrate:
// admin) and the single-operator legacy principal cross it by design; open dev
// mode (no principal) sees all. This matches tenantVisible / sessionOwnershipOK
// exactly so migrating a handler onto the accessor is behaviour-preserving.
type tenantScopedStore struct {
	store      store.Store
	tenantID   string
	allTenants bool
}

// tenantStore captures the caller's tenant scope from the ctx principal and
// returns a façade that gates every read against it.
func (s *Server) tenantStore(ctx context.Context) *tenantScopedStore {
	tenantID, allTenants := tenantScopeFromCtx(ctx)
	return &tenantScopedStore{store: s.store, tenantID: tenantID, allTenants: allTenants}
}

// tenantScopeFromCtx resolves (tenantID, allTenants) from the ctx principal,
// matching the exemption set of tenantVisible + sessionOwnershipOK: no
// principal (open dev mode), the single-operator legacy principal, and any
// substrate:admin principal all see every tenant. (Legacy is redundant with the
// admin scope it carries, but kept explicit to mirror sessionOwnershipOK.)
func tenantScopeFromCtx(ctx context.Context) (tenantID string, allTenants bool) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return "", true
	}
	return p.TenantID, false
}

// visible reports whether a row owned by rowTenant is in the caller's scope.
func (t *tenantScopedStore) visible(rowTenant string) bool {
	return t.allTenants || rowTenant == t.tenantID
}

// GetRun fetches a run and folds a cross-tenant row into an opaque
// *store.ErrNotFound — a cross-tenant probe is indistinguishable from a missing
// run (run_ids are not secret, so the gate must not become an existence
// oracle).
func (t *tenantScopedStore) GetRun(ctx context.Context, runID string) (store.Run, error) {
	run, err := t.store.GetRun(ctx, runID)
	if err != nil {
		return store.Run{}, err
	}
	if !t.visible(run.TenantID) {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: runID}
	}
	return run, nil
}

// GetRunByAgentID is GetRun keyed by agent_id (the live-run lookup the agent
// read endpoint uses). Same opaque cross-tenant fold.
func (t *tenantScopedStore) GetRunByAgentID(ctx context.Context, agentID string) (store.Run, error) {
	run, err := t.store.GetRunByAgentID(ctx, agentID)
	if err != nil {
		return store.Run{}, err
	}
	if !t.visible(run.TenantID) {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: agentID}
	}
	return run, nil
}

// GetSession fetches a session and folds a cross-tenant row into an opaque
// *store.ErrNotFound — mirrors sessionOwnershipOK's 404-on-mismatch (a
// continuation/transcript exposes the session's history, so the boundary is
// the tenant; session_ids are not secret).
func (t *tenantScopedStore) GetSession(ctx context.Context, sessionID string) (store.Session, error) {
	sess, err := t.store.GetSession(ctx, sessionID)
	if err != nil {
		return store.Session{}, err
	}
	if !t.visible(sess.TenantID) {
		return store.Session{}, &store.ErrNotFound{Kind: "session", ID: sessionID}
	}
	return sess, nil
}

// InterruptListByRun returns the run's interrupts, gated by the OWNING run's
// tenant: a cross-tenant (or missing) run returns *store.ErrNotFound so the
// list can't be used as a cross-tenant existence oracle. Interrupts carry no
// tenant column of their own — they inherit the run's tenant, so gating the run
// is the gate.
func (t *tenantScopedStore) InterruptListByRun(ctx context.Context, runID, statusFilter string) ([]store.InterruptRow, error) {
	if _, err := t.GetRun(ctx, runID); err != nil {
		return nil, err
	}
	return t.store.InterruptListByRun(ctx, runID, statusFilter)
}
