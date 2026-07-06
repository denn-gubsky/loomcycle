package http

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
)

// routingResponse is GET /v1/_routing: for each user_tier × tier, the ordered
// provider/model cascade a consumer would hit (top → fallbacks), so an operator
// can see which providers/models runs resolve to right now.
//
// Both principals get live availability per candidate + an active-providers
// header — a tenant needs to see which of its providers are actually up right
// now (RFC AX bring-your-own-key visibility). They differ in two ways:
//   - admin: the FULL provider set, with the raw last_error string per provider.
//   - substrate:tenant: last_error is redacted (it can leak operator infra
//     detail), and when the operator-key gate restricts this tenant the cascade
//     and header are filtered to providers the tenant can key itself.
type routingResponse struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Admin       bool              `json:"admin"`
	Providers   []routingProvider `json:"providers,omitempty"`
	UserTiers   []routingUserTier `json:"user_tiers"`
	// OperatorKeyRestricted is true when the RFC AX operator-key gate
	// (LOOMCYCLE_OPERATOR_KEY_RESTRICTION) is ON and this caller is a restricted
	// (non-admin) tenant — the cascade below has then been filtered to providers
	// the tenant can key itself (needs no operator key, or has an own
	// CredentialDef for it). Lets the UI show a "bring-your-own-key" note.
	OperatorKeyRestricted bool `json:"operator_key_restricted,omitempty"`

	// Search is the RFC BB web-search provider cascade — a single flat list
	// (search has no tier/model dimension), each with keyability + live
	// availability. Omitted when no search providers are configured.
	Search []searchRoutingProvider `json:"search,omitempty"`
}

// searchRoutingProvider is one entry in the search cascade for the routing view.
type searchRoutingProvider struct {
	Provider string `json:"provider"`
	Primary  bool   `json:"primary"` // first in the (post-filter) cascade
	// Keyable: this caller has a usable key (operator host key when allowed, an
	// own CredentialDef, or the provider is keyless). Available: keyable AND not
	// in a failure cooldown. Selected: the first available provider (what runs
	// now). Reachable: not in a cooldown, regardless of key.
	Keyable   *bool  `json:"keyable,omitempty"`
	Available *bool  `json:"available,omitempty"`
	Selected  *bool  `json:"selected,omitempty"`
	Reachable *bool  `json:"reachable,omitempty"`
	LastError string `json:"last_error,omitempty"` // admin-only
}

type routingProvider struct {
	Provider  string `json:"provider"`
	Reachable bool   `json:"reachable"`
	Excluded  bool   `json:"excluded"`
	// LastError is the raw provider probe error — admin-only (redacted to "" for
	// a tenant, since it can leak operator infra detail).
	LastError string `json:"last_error,omitempty"`
}

type routingUserTier struct {
	// Name is the user_tier name; "" in library-mode (no user_tiers configured).
	Name  string        `json:"name"`
	Tiers []routingTier `json:"tiers"`
}

type routingTier struct {
	Tier    string             `json:"tier"` // low / middle / high
	Cascade []routingCandidate `json:"cascade"`
}

type routingCandidate struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Primary  bool   `json:"primary"` // first in config order (the configured top)
	// Live-availability fields — populated for ALL principals (admin + tenant).
	Available   *bool `json:"available,omitempty"`
	Selected    *bool `json:"selected,omitempty"` // first AVAILABLE — what runs now
	Stalled     *bool `json:"stalled,omitempty"`
	RateLimited *bool `json:"rate_limited,omitempty"`
	Reachable   *bool `json:"reachable,omitempty"`
}

// handleRouting serves GET /v1/_routing. Tenant-readable (see requiredScopeFor):
// every principal gets the cascade + live availability + the active-providers
// header; a non-admin has last_error redacted, and a gate-restricted tenant sees
// only the providers it can key itself.
func (s *Server) handleRouting(w http.ResponseWriter, r *http.Request) {
	if s.resolver == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "resolver_unavailable",
			"resolver not configured; the server is in degraded startup mode")
		return
	}
	// admin gates last_error exposure (raw provider errors can leak infra detail)
	// and disables the operator-key keyable filter. Open mode (no principal
	// stamped) is dev/admin, so it gets the full view.
	admin := true
	if p, ok := auth.PrincipalFromContext(r.Context()); ok {
		admin = auth.HasScope(p.Scopes, auth.ScopeAdmin)
	}

	snap := s.resolver.Snapshot()

	// user_tiers, sorted; library-mode (none configured) → a single "" entry.
	utNames := make([]string, 0, len(s.cfg.UserTiers))
	for name := range s.cfg.UserTiers {
		utNames = append(utNames, name)
	}
	sort.Strings(utNames)
	if len(utNames) == 0 {
		utNames = []string{""}
	}

	tierNames := s.routingTierNames()

	// RFC AX operator-key gate: when LOOMCYCLE_OPERATOR_KEY_RESTRICTION is ON and
	// this is a non-admin (tenant) caller, filter the advertised cascade to
	// providers the tenant can actually reach at run time — otherwise it would see
	// a candidate it would be refused with ErrOperatorKeyRestricted. gate-off ⇒
	// off; admin/legacy/open (admin==true) ⇒ off (unchanged, show all).
	//
	// NOTE: keyed off (gate && !admin), NOT operatorKeyRestrictedForCtx, on
	// purpose. That helper returns false for a substrate:tenant principal because
	// ScopeProvidersOperatorKey is tenant-implied (auth.tenantImplied) — and since
	// /v1/_routing is gated at ScopeTenant, EVERY non-admin caller reaching this
	// handler holds substrate:tenant, so the helper would never fire here (dead
	// filter). This predicate meets the two documented criteria (admin ⇒ off,
	// gate-off ⇒ off) while actually filtering the tenant view.
	restricted := s.cfg.Env.OperatorKeyRestriction && !admin
	var restrictTenant, restrictUser string
	if restricted {
		if p, ok := auth.PrincipalFromContext(r.Context()); ok {
			restrictTenant, restrictUser = p.TenantID, p.Subject
		}
	}

	resp := routingResponse{GeneratedAt: time.Now().UTC(), Admin: admin, OperatorKeyRestricted: restricted}
	// When restricted, the active-providers header is filtered to the union of
	// the tenant's keyable providers across all tiers (never advertise a provider
	// it can't use). nil ⇒ unrestricted (the header lists every snapshot provider).
	var keyableUnion map[string]bool
	if restricted {
		keyableUnion = map[string]bool{}
	}
	for _, ut := range utNames {
		overlay := s.userTierOverlay(ut)
		rut := routingUserTier{Name: ut}
		for _, tier := range tierNames {
			req := resolve.AgentRequest{Name: "routing-view", Tier: tier, UserTier: overlay}
			casc := s.resolver.Cascade(req)
			// Keyability is provider-based (KeyEnvName + own CredentialDef),
			// independent of rank. keyableProvidersFor walks this same cascade, so
			// the set is exactly the providers appearing here the tenant can key
			// (agent="" — routing has no agent, so only tenant/user creds count).
			// An empty set ⇒ the tier renders no candidates, the true picture of
			// what the tenant may run.
			var keyable map[string]bool
			if restricted {
				keyable = s.keyableProvidersFor(r.Context(), req, restrictTenant, "", restrictUser)
				for p := range keyable {
					keyableUnion[p] = true
				}
			}
			rt := routingTier{Tier: tier}
			selectedMarked := false
			for _, c := range casc {
				if restricted && !keyable[c.Provider] {
					continue
				}
				// Primary = first entry in the (post-filter) displayed cascade. For
				// an unrestricted caller nothing is filtered, so this equals i==0.
				rc := routingCandidate{Provider: c.Provider, Model: c.Model, Primary: len(rt.Cascade) == 0}
				// WHY availability for all principals: a tenant needs to see which
				// of its (keyable) providers are up right now (RFC AX). Only the raw
				// last_error string stays admin-only — redacted in the header below.
				av, stalled, rateLimited, reachable := availStatus(snap, c.Provider, c.Model)
				sel := av && !selectedMarked
				if sel {
					selectedMarked = true
				}
				rc.Available = &av
				rc.Selected = &sel
				rc.Stalled = &stalled
				rc.RateLimited = &rateLimited
				rc.Reachable = &reachable
				rt.Cascade = append(rt.Cascade, rc)
			}
			rut.Tiers = append(rut.Tiers, rt)
		}
		resp.UserTiers = append(resp.UserTiers, rut)
	}

	// Active-providers header — shown to every principal. WHY the admin split:
	// reachable/excluded status is tenant-safe visibility, but the raw last_error
	// string can leak operator infra detail (DSNs, internal hostnames, upstream
	// bodies), so it is populated for admins only. Restricted ⇒ list only the
	// tenant's keyable providers.
	provNames := make([]string, 0, len(snap))
	for p := range snap {
		if restricted && !keyableUnion[p] {
			continue
		}
		provNames = append(provNames, p)
	}
	sort.Strings(provNames)
	for _, p := range provNames {
		a := snap[p]
		rp := routingProvider{Provider: p, Reachable: a.Reachable, Excluded: a.Excluded}
		if admin {
			rp.LastError = a.LastError
		}
		resp.Providers = append(resp.Providers, rp)
	}

	// RFC BB: the search-provider cascade — a flat list with keyability + live
	// availability, same admin/tenant posture as the LLM cascade above (admin
	// sees last_error; a restricted tenant sees only providers it can key).
	if s.searchResolver != nil && s.searchRegistry != nil {
		callerTenant, callerUser := "", ""
		if !admin {
			if p, ok := auth.PrincipalFromContext(r.Context()); ok {
				callerTenant, callerUser = p.TenantID, p.Subject
			}
		}
		allowOperatorKey := !restricted // a restricted tenant can't spend the operator host key
		snap := s.searchResolver.Snapshot()
		selectedMarked := false
		for _, id := range s.searchResolver.Cascade(nil) {
			p, ok := s.searchRegistry.Get(id)
			if !ok {
				continue
			}
			keyable := s.searchProviderKeyable(r.Context(), id, p.KeyEnvName(), callerTenant, callerUser, allowOperatorKey)
			if restricted && !keyable {
				continue // a restricted tenant sees only providers it can key
			}
			av := snap[id]
			available := keyable && av.Reachable
			sel := available && !selectedMarked
			if sel {
				selectedMarked = true
			}
			reachable := av.Reachable
			sp := searchRoutingProvider{
				Provider:  id,
				Primary:   len(resp.Search) == 0,
				Keyable:   &keyable,
				Available: &available,
				Selected:  &sel,
				Reachable: &reachable,
			}
			if admin {
				sp.LastError = av.LastError
			}
			resp.Search = append(resp.Search, sp)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// searchProviderKeyable reports whether this caller can supply a key for a
// search provider: keyless (searxng) is always keyable; an operator host key
// counts when allowed (a restricted tenant is denied it); otherwise the caller
// must hold its own CredentialDef of the provider's env-var name (agent="" —
// the routing view has no agent).
func (s *Server) searchProviderKeyable(ctx context.Context, id, env, tenantID, userID string, allowOperatorKey bool) bool {
	if env == "" {
		return true
	}
	if allowOperatorKey && s.searchHostKeys[id] != "" {
		return true
	}
	return s.credKeyable != nil && s.credKeyable(ctx, tenantID, "", userID, env)
}

// routingTierNames returns the configured tier names (keys of cfg.Tiers) in a
// stable, human order: low → middle → high first, then any others sorted.
func (s *Server) routingTierNames() []string {
	pref := []string{"low", "middle", "high"}
	seen := map[string]bool{}
	var out []string
	for _, p := range pref {
		if _, ok := s.cfg.Tiers[p]; ok {
			out = append(out, p)
			seen[p] = true
		}
	}
	var extra []string
	for name := range s.cfg.Tiers {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// availStatus derives one candidate's live availability from the resolver
// snapshot, mirroring ModelStatus's usability rule (provider reachable + model
// listed + not stalled + not rate-limited-unexpired).
func availStatus(snap map[string]resolve.Availability, provider, model string) (available, stalled, rateLimited, reachable bool) {
	a, ok := snap[provider]
	if !ok {
		return false, false, false, false
	}
	reachable = a.Reachable && !a.Excluded
	ms, ok := a.Models[model]
	if !ok {
		return false, false, false, reachable
	}
	stalled = ms.Stalled
	rateLimited = ms.RateLimited && time.Now().Before(ms.RateLimitedUntil)
	available = reachable && ms.Listed && !ms.Stalled && !rateLimited
	return available, stalled, rateLimited, reachable
}
