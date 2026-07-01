package http

import (
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
// Two views by principal (RFC AS posture):
//   - admin: live availability per candidate (reachable/stalled/rate-limited),
//     which entry is currently SELECTED (first available = what runs now), plus
//     an active-providers header.
//   - substrate:tenant: the config cascade only (provider/model per tier,
//     `primary` = the configured top). No provider reachability / infra detail.
type routingResponse struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Admin       bool              `json:"admin"`
	Providers   []routingProvider `json:"providers,omitempty"` // admin-only
	UserTiers   []routingUserTier `json:"user_tiers"`
}

type routingProvider struct {
	Provider  string `json:"provider"`
	Reachable bool   `json:"reachable"`
	Excluded  bool   `json:"excluded"`
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
	// Admin-only live-availability fields (nil/omitted for a tenant view).
	Available   *bool `json:"available,omitempty"`
	Selected    *bool `json:"selected,omitempty"` // first AVAILABLE — what runs now
	Stalled     *bool `json:"stalled,omitempty"`
	RateLimited *bool `json:"rate_limited,omitempty"`
	Reachable   *bool `json:"reachable,omitempty"`
}

// handleRouting serves GET /v1/_routing. Tenant-readable (see requiredScopeFor):
// a substrate:tenant operator gets the config cascade; an admin (or legacy/open)
// additionally gets live availability + the active-providers header.
func (s *Server) handleRouting(w http.ResponseWriter, r *http.Request) {
	if s.resolver == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "resolver_unavailable",
			"resolver not configured; the server is in degraded startup mode")
		return
	}
	// Admin ⇒ full view (availability + infra). A non-admin principal
	// (substrate:tenant) ⇒ config cascade only. Open mode (no principal stamped)
	// is dev/admin, so it gets the full view.
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

	resp := routingResponse{GeneratedAt: time.Now().UTC(), Admin: admin}
	for _, ut := range utNames {
		overlay := s.userTierOverlay(ut)
		rut := routingUserTier{Name: ut}
		for _, tier := range tierNames {
			casc := s.resolver.Cascade(resolve.AgentRequest{Name: "routing-view", Tier: tier, UserTier: overlay})
			rt := routingTier{Tier: tier}
			selectedMarked := false
			for i, c := range casc {
				rc := routingCandidate{Provider: c.Provider, Model: c.Model, Primary: i == 0}
				if admin {
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
				}
				rt.Cascade = append(rt.Cascade, rc)
			}
			rut.Tiers = append(rut.Tiers, rt)
		}
		resp.UserTiers = append(resp.UserTiers, rut)
	}

	if admin {
		provNames := make([]string, 0, len(snap))
		for p := range snap {
			provNames = append(provNames, p)
		}
		sort.Strings(provNames)
		for _, p := range provNames {
			a := snap[p]
			resp.Providers = append(resp.Providers, routingProvider{
				Provider:  p,
				Reachable: a.Reachable,
				Excluded:  a.Excluded,
				LastError: a.LastError,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
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
