package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA §4 / P5 — per-tenant strict policy: a tenant whose people may come in
// only through named gates ("acme signs in through google").
//
// Two sources, and the stricter wins. auth.tenant_policies in config is the
// floor an operator ships; an admin-managed row (PUT
// /v1/_auth/tenant-policies/{tenant}) can only narrow it — the effective list is
// the intersection, so a row can never re-open a gate the config closed. No
// source at all: the tenant has no restriction.
//
// It is enforced where an identity becomes a credential: when a Web UI session
// is established (establishSession, every gate) and when a gate issues a token
// (issueGateToken: device, assertion and AWS IAM logins). Narrowing a policy
// takes effect on what was already issued too: the tenant's sessions and
// gate-issued tokens from a gate it no longer lists are ended at once. A store
// error refuses: an unreadable policy is not an absent one.

var (
	// errTenantGatePolicy: the tenant's policy does not list the gate.
	errTenantGatePolicy = errors.New("auth: the tenant's gate policy does not allow this gate")
	// errTenantPolicyUnavailable: the policy could not be read.
	errTenantPolicyUnavailable = errors.New("auth: the tenant's gate policy could not be read")
)

// effectiveTenantGates is the gates tenant's people may come through, and
// whether any restriction applies. Memoised briefly per replica (it is asked
// on every request a gate-issued token or a session makes); a policy change
// flushes every replica's memo through the token-invalidation backplane.
func (s *Server) effectiveTenantGates(ctx context.Context, tenant string) ([]string, bool, error) {
	cfg, now := s.cfg(), time.Now()
	if e, ok := s.tenantPolicyCache.get(cfg, tenant, now); ok {
		return e.gates, e.restricted, nil
	}
	gen := s.tenantPolicyCache.begin()
	gates, restricted, err := s.loadTenantGates(ctx, cfg, tenant)
	if err == nil {
		s.tenantPolicyCache.put(cfg, tenant, tenantPolicyEntry{gates: gates, restricted: restricted, at: now}, gen)
	}
	return gates, restricted, err
}

// issuedTokenAllowed reports whether a resolved bearer may be used under its
// tenant's gate policy: a token a gate issued only while the policy lists that
// gate. A token no gate issued is the operator's and is not judged here. An
// unreadable policy refuses.
func (s *Server) issuedTokenAllowed(ctx context.Context, p auth.Principal) bool {
	if p.IssuedByGate == "" {
		return true
	}
	if err := s.tenantGateAllowed(ctx, p.TenantID, p.IssuedByGate); err != nil {
		if !errors.Is(err, errTenantGatePolicy) {
			slog.Warn("auth: tenant policy unreadable; refusing a gate-issued token", "tenant", p.TenantID, "err", err)
		}
		return false
	}
	return true
}

// loadTenantGates reads both policy sources; see effectiveTenantGates.
func (s *Server) loadTenantGates(ctx context.Context, cfg *config.Config, tenant string) ([]string, bool, error) {
	cfgGates, cfgSet := cfg.ResolvedAuth.TenantGates(tenant)
	var stored []string
	storedSet := false
	if s.store != nil && tenant != "" {
		row, err := s.store.TenantAuthPolicyGet(ctx, tenant)
		switch {
		case err == nil:
			stored, storedSet = row.Gates, true
		case !isNotFound(err):
			return nil, true, fmt.Errorf("%w: %v", errTenantPolicyUnavailable, err)
		}
	}
	switch {
	case cfgSet && storedSet:
		return intersectGates(cfgGates, stored), true, nil
	case cfgSet:
		return cfgGates, true, nil
	case storedSet:
		return stored, true, nil
	}
	return nil, false, nil
}

func intersectGates(a, b []string) []string {
	in := map[string]bool{}
	for _, g := range a {
		in[g] = true
	}
	out := []string{}
	for _, g := range b {
		if in[g] {
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}

func containsGate(list []string, gate string) bool {
	for _, g := range list {
		if g == gate {
			return true
		}
	}
	return false
}

// tenantGateAllowed refuses an identity of tenant coming in through gate when
// the tenant's policy does not list it (errTenantGatePolicy), or when the
// policy cannot be read (errTenantPolicyUnavailable).
func (s *Server) tenantGateAllowed(ctx context.Context, tenant, gate string) error {
	gates, restricted, err := s.effectiveTenantGates(ctx, tenant)
	if err != nil {
		return err
	}
	if restricted && !containsGate(gates, gate) {
		return fmt.Errorf("%w: tenant %q, gate %q", errTenantGatePolicy, tenant, gate)
	}
	return nil
}

// writeTenantPolicyRefusal answers a sign-in the tenant policy refused; false
// when err is not a policy error.
func writeTenantPolicyRefusal(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, errTenantGatePolicy):
		writeJSONError(w, http.StatusForbidden, "gate_not_allowed",
			"this account's organisation requires signing in through another sign-in option")
		return true
	case errors.Is(err, errTenantPolicyUnavailable):
		writeJSONError(w, http.StatusServiceUnavailable, "policy_unavailable", "sign-in is unavailable; try again")
		return true
	}
	return false
}

// tenantPolicyView is one entry of the management API's answer.
type tenantPolicyView struct {
	TenantID string `json:"tenant_id"`
	// Gates is what applies: the intersection when both sources set one.
	Gates       []string  `json:"gates"`
	ConfigGates []string  `json:"config_gates,omitempty"`
	StoredGates []string  `json:"stored_gates,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
}

func (s *Server) tenantPolicyView(tenant string, row *store.TenantAuthPolicyRow) tenantPolicyView {
	v := tenantPolicyView{TenantID: tenant}
	cfgGates, cfgSet := s.cfg().ResolvedAuth.TenantGates(tenant)
	if cfgSet {
		v.ConfigGates = cfgGates
		v.Gates = cfgGates
	}
	if row != nil {
		v.StoredGates, v.UpdatedAt, v.UpdatedBy = row.Gates, row.UpdatedAt, row.UpdatedBy
		v.Gates = row.Gates
		if cfgSet {
			v.Gates = intersectGates(cfgGates, row.Gates)
		}
	}
	if v.Gates == nil {
		v.Gates = []string{}
	}
	return v
}

// handleListTenantPolicies is GET /v1/_auth/tenant-policies (admin): every
// tenant with a policy from either source.
func (s *Server) handleListTenantPolicies(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "tenant policies need a persistent store")
		return
	}
	rows, err := s.store.TenantAuthPolicyList(r.Context())
	if err != nil {
		slog.Error("auth: listing tenant policies failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "policy_list_failed", "the tenant policies could not be read")
		return
	}
	byTenant := map[string]*store.TenantAuthPolicyRow{}
	for i := range rows {
		byTenant[rows[i].TenantID] = &rows[i]
	}
	for t := range s.cfg().ResolvedAuth.TenantPolicies {
		if _, ok := byTenant[t]; !ok {
			byTenant[t] = nil
		}
	}
	tenants := make([]string, 0, len(byTenant))
	for t := range byTenant {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)
	out := make([]tenantPolicyView, 0, len(tenants))
	for _, t := range tenants {
		out = append(out, s.tenantPolicyView(t, byTenant[t]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": out})
}

// handlePutTenantPolicy is PUT /v1/_auth/tenant-policies/{tenant} {gates}
// (admin). The gates must be configured sign-in gates, and must leave the
// tenant at least one gate after the config's floor is applied.
func (s *Server) handlePutTenantPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "tenant policies need a persistent store")
		return
	}
	tenant := strings.TrimSpace(r.PathValue("tenant"))
	if tenant == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_tenant", "a tenant is required")
		return
	}
	var req struct {
		Gates []string `json:"gates"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "body must be {\"gates\": [\"<gate>\", ...]}")
		return
	}
	if len(req.Gates) == 0 {
		writeJSONError(w, http.StatusBadRequest, "policy_empty", "name at least one gate; DELETE removes the policy")
		return
	}
	for _, g := range req.Gates {
		cg, ok := s.configuredGate(g)
		if !ok || !config.TenantPolicyGateKind(cg.Kind) {
			writeJSONError(w, http.StatusBadRequest, "unknown_gate", fmt.Sprintf("%q is not a sign-in gate in auth.gates", g))
			return
		}
	}
	if cfgGates, ok := s.cfg().ResolvedAuth.TenantGates(tenant); ok && len(intersectGates(cfgGates, req.Gates)) == 0 {
		writeJSONError(w, http.StatusConflict, "policy_empty",
			"auth.tenant_policies allows this tenant only "+strings.Join(cfgGates, ", ")+"; the policy would leave no gate")
		return
	}
	row := store.TenantAuthPolicyRow{TenantID: tenant, Gates: req.Gates, UpdatedAt: time.Now().UTC(), UpdatedBy: principalSubject(ctx)}
	if err := s.store.TenantAuthPolicyPut(ctx, row); err != nil {
		slog.Error("auth: writing a tenant policy failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "policy_write_failed", "the tenant policy could not be written")
		return
	}
	// Every replica's memo of the policy goes now, so the next request a
	// session or an issued token makes is judged by the new policy whatever
	// the cleanup below manages.
	s.invalidateTokenCache(ctx)
	stored, err := s.store.TenantAuthPolicyGet(ctx, tenant)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "policy_write_failed", "the tenant policy could not be read back")
		return
	}
	view := s.tenantPolicyView(tenant, &stored)
	sessions, tokens, err := s.applyTenantPolicy(ctx, tenant, view.Gates)
	s.recordAuthEvent(audit.Event{Action: "auth_tenant_policy_set", ActorTenant: tenantFromCtx(ctx), ActorSubject: principalSubject(ctx),
		TargetTenant: tenant, AuthReason: "gates " + strings.Join(view.Gates, ","), SourceAddr: s.clientSource(r)})
	if err != nil {
		// The policy is stored and refuses new sign-ins; what was issued
		// before could not all be ended. Say so rather than report success.
		slog.Error("auth: ending sessions and tokens outside a tenant policy failed", "tenant", tenant, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "policy_partially_applied",
			"the policy is stored and applies to new sign-ins, but existing sessions or tokens could not all be ended; retry the PUT")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": view, "sessions_ended": sessions, "tokens_retired": tokens})
}

// handleDeleteTenantPolicy is DELETE /v1/_auth/tenant-policies/{tenant}
// (admin): the stored row goes; the config's policy, if any, still applies.
func (s *Server) handleDeleteTenantPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "tenant policies need a persistent store")
		return
	}
	tenant := strings.TrimSpace(r.PathValue("tenant"))
	ok, err := s.store.TenantAuthPolicyDelete(ctx, tenant)
	if err != nil {
		slog.Error("auth: deleting a tenant policy failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "policy_delete_failed", "the tenant policy could not be deleted")
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "policy_not_found", "the tenant has no stored policy")
		return
	}
	s.invalidateTokenCache(ctx)
	s.recordAuthEvent(audit.Event{Action: "auth_tenant_policy_deleted", ActorTenant: tenantFromCtx(ctx), ActorSubject: principalSubject(ctx),
		TargetTenant: tenant, SourceAddr: s.clientSource(r)})
	writeJSON(w, http.StatusOK, map[string]any{"policy": s.tenantPolicyView(tenant, nil)})
}

// applyTenantPolicy is cleanup after a policy change: it ends the tenant's
// live sessions made through a gate not in gates and retires its gate-issued
// tokens from such a gate, so they stop holding rows and the cache window
// closes. It is not the control — every request is judged by the policy
// itself (sessionPrincipal, resolvePrincipal), which is also why nothing
// runs at boot to "catch up" with a config-declared policy. Hand-minted
// tokens are an operator's and are left alone (auth.strict.user_tokens
// decides those).
func (s *Server) applyTenantPolicy(ctx context.Context, tenant string, gates []string) (sessions, tokens int, err error) {
	sessions, err = s.store.TenantAuthPolicyRevokeSessions(ctx, tenant, gates, time.Now())
	if err != nil {
		return 0, 0, err
	}
	rows, err := s.store.TenantAuthPolicyIssuedTokens(ctx, tenant)
	if err != nil {
		return sessions, 0, err
	}
	now := time.Now().UTC()
	defer func() {
		if tokens > 0 {
			s.invalidateTokenCache(ctx)
		}
	}()
	for _, row := range rows {
		if row.IssuedByGate == "" || containsGate(gates, row.IssuedByGate) || !row.ValidAt(now) {
			continue
		}
		if err := s.store.OperatorTokenDefSetRetiredAt(ctx, row.DefID, now); err != nil {
			return sessions, tokens, err
		}
		tokens++
		s.recordTokenAudit(ctx, "retire", row)
	}
	return sessions, tokens, nil
}
