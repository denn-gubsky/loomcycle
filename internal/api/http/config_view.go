package http

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/capabilities"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
)

// Disclosure levels for GET /v1/config. Named in the response so a consumer can
// tell "this deployment has no search providers" from "you weren't shown them".
const (
	configViewPublic = "public"
	configViewAuthed = "authenticated"
	configViewAdmin  = "admin"
)

// publicViewKey marks a request that reached the handler with NO credential under
// the LOOMCYCLE_PUBLIC_CONFIG opt-in.
//
// It is needed because "no principal in context" is ambiguous: it is also how
// OPEN mode looks (no auth configured at all), and open mode is dev/admin —
// the idiom the routing view and Context op=capabilities both use. Without this
// marker a public request would be indistinguishable from open mode and would be
// served the ADMIN view, which is the whole thing this endpoint must not do.
type publicViewKey struct{}

func markPublicView(ctx context.Context) context.Context {
	return context.WithValue(ctx, publicViewKey{}, true)
}

func isPublicView(ctx context.Context) bool {
	v, _ := ctx.Value(publicViewKey{}).(bool)
	return v
}

// publicOrAuthMiddleware permits an unauthenticated read of GET /v1/config when
// the operator opted in AND no credential was presented.
//
// A PRESENTED credential always goes through the normal auth path, so an invalid
// bearer fails rather than silently downgrading to the public view: a bad
// credential is an error, not a request for less detail. Without the opt-in this
// is exactly authMiddleware, so every existing deployment is unchanged.
func (s *Server) publicOrAuthMiddleware(next http.Handler) http.Handler {
	authed := s.authMiddleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, presented := extractBearer(r)
		if !presented && s.cfg != nil && s.cfg.Env.PublicConfig {
			next.ServeHTTP(w, r.WithContext(markPublicView(r.Context())))
			return
		}
		authed.ServeHTTP(w, r)
	})
}

// configResponse is GET /v1/config: what this instance is and what it can do.
//
// It is the out-of-band twin of the in-band `Context op=capabilities`, sharing
// the same deployment probe (internal/capabilities) so the two cannot disagree,
// and composing in the provider/model/search cascade so a consumer can render
// "which models are live right now" without an agent run.
type configResponse struct {
	GeneratedAt time.Time `json:"generated_at"`
	// View is the disclosure level this response was rendered at.
	View     string         `json:"view"`
	Instance configInstance `json:"instance"`
	// Features reduces each capability to a single boolean in the public view —
	// see publicFeatures for why that reduction is the security property.
	Features  map[string]any   `json:"features"`
	Providers []configProvider `json:"providers"`
	Models    []configModel    `json:"models"`
	Search    []configSearch   `json:"search"`
	// UserTiers are the configured plan names — omitted from the public view,
	// since a plan roster is commercial structure rather than a capability.
	UserTiers []string `json:"user_tiers,omitempty"`
	// Limits are the deployment's caps. Omitted from the public view: a request
	// ceiling is an operational detail, and publishing it hands an attacker the
	// exact edge to push against.
	Limits map[string]any `json:"limits,omitempty"`
}

type configInstance struct {
	// Version identifies the software, not the deployment, so it is public — the
	// same reasoning Context op=self already applies to it.
	Version string `json:"version,omitempty"`
	// Commit and BuildTime are build PROVENANCE rather than product identity and
	// pin the binary far more precisely, so they stop at an authenticated caller.
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"build_time,omitempty"`
	// URL is the operator's advertised public base URL. Authenticated-only, not
	// because it is secret but because a public caller already knows the address
	// it reached, so echoing it back adds nothing and would confirm topology on a
	// deployment sitting behind a proxy that rewrites it.
	URL string `json:"url,omitempty"`
}

type configProvider struct {
	Provider string `json:"provider"`
	// Active is reachable AND not excluded — one coarse boolean. Never an error
	// string: raw provider probe errors can carry hostnames, ports and upstream
	// response bodies. Admin keeps that detail on /v1/_routing.
	Active bool `json:"active"`
}

// configModel is one (provider, model) pair the deployment can route to,
// flattened and deduplicated across every user_tier — a landing page wants "we
// serve claude-opus-5", not the same model repeated once per plan.
type configModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Tiers are the canonical capability tiers (low/middle/high) this pair serves.
	// These are loomcycle's own tier names, not the operator's plan names.
	Tiers []string `json:"tiers"`
	// Active: at least one tier would route here right now. Selected: it is the
	// FIRST available candidate for at least one tier, i.e. what actually runs.
	Active   bool `json:"active"`
	Selected bool `json:"selected"`
}

type configSearch struct {
	Provider string `json:"provider"`
	Active   bool   `json:"active"`
	Primary  bool   `json:"primary"`
}

// configViewFor resolves the disclosure level for a request context.
//
// Open mode (no auth configured, no principal stamped) is dev/admin — the idiom
// the routing view and Context op=capabilities both use. The public marker is
// checked FIRST precisely because open mode looks identical otherwise.
func configViewFor(ctx context.Context) string {
	if isPublicView(ctx) {
		return configViewPublic
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		if auth.HasScope(p.Scopes, auth.ScopeAdmin) {
			return configViewAdmin
		}
		return configViewAuthed
	}
	return configViewAdmin
}

// handleConfig serves GET /v1/config. Reachable unauthenticated ONLY under
// LOOMCYCLE_PUBLIC_CONFIG (see publicOrAuthMiddleware); otherwise any
// authenticated principal, per requiredScopeFor's GET default.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildConfig(r.Context(), configViewFor(r.Context())))
}

// buildConfig assembles the report at the given disclosure level. Shared with the
// connector (and so with gRPC), because two transports answering the same
// question from two implementations is the drift this whole surface exists to
// avoid — it is why the capability probe was extracted in the first place.
func (s *Server) buildConfig(ctx context.Context, view string) configResponse {
	public := view == configViewPublic
	admin := view == configViewAdmin

	resp := configResponse{GeneratedAt: time.Now().UTC(), View: view}

	// --- instance ---
	resp.Instance = configInstance{Version: s.buildVersion}
	if !public {
		resp.Instance.Commit = s.buildCommit
		resp.Instance.BuildTime = s.buildTime
		if s.cfg != nil {
			resp.Instance.URL = s.cfg.Env.PublicURL
		}
	}

	// --- features (shared probe, so this cannot drift from Context op=capabilities) ---
	feat := capabilities.Deployment(capabilities.Inputs{
		Cfg:      s.cfg,
		Store:    s.store,
		Embedder: s.embedder,
		SQLMem:   s.sqlMem != nil,
		Admin:    admin,
	})
	if public {
		feat = publicFeatures(feat)
	}
	resp.Features = feat
	if !public {
		resp.Limits = capabilities.DeploymentLimits(s.cfg)
	}

	// The resolver is what knows about providers and models; without it (degraded
	// startup) report the instance honestly rather than 503 the whole view — a
	// landing page asking "are you up" should get an answer.
	if s.resolver == nil {
		resp.Providers, resp.Models, resp.Search = []configProvider{}, []configModel{}, []configSearch{}
		return resp
	}

	// RFC AX operator-key gate, mirroring /v1/_routing exactly: when the gate is
	// on and the caller is not an admin, advertise only providers the caller can
	// key itself. Applied to the public view too — a public reader is the least
	// entitled caller there is, so it must not learn more than a tenant would.
	restricted := s.cfg != nil && s.cfg.Env.OperatorKeyRestriction && !admin
	var restrictTenant, restrictUser string
	if restricted && !public {
		if p, ok := auth.PrincipalFromContext(ctx); ok {
			restrictTenant, restrictUser = p.TenantID, p.Subject
		}
	}

	snap := s.resolver.Snapshot()

	utNames := make([]string, 0, len(s.cfg.UserTiers))
	for name := range s.cfg.UserTiers {
		utNames = append(utNames, name)
	}
	sort.Strings(utNames)
	if !public {
		resp.UserTiers = append([]string{}, utNames...)
	}
	if len(utNames) == 0 {
		utNames = []string{""} // library mode: one implicit tier
	}

	// Aggregate the per-(user_tier × tier) cascades into one flat model list.
	// Dedup key is (provider, model) — the same model appears in every plan that
	// includes it, and a consumer wants it once.
	type modelAgg struct {
		tiers    map[string]bool
		active   bool
		selected bool
		order    int
	}
	agg := map[[2]string]*modelAgg{}
	keyableUnion := map[string]bool{}
	for _, ut := range utNames {
		overlay := s.userTierOverlay(ut)
		for _, tier := range s.routingTierNames() {
			req := resolve.AgentRequest{Name: "config-view", Tier: tier, UserTier: overlay}
			var keyable map[string]bool
			if restricted {
				keyable = s.keyableProvidersFor(ctx, req, restrictTenant, "", restrictUser)
				for p := range keyable {
					keyableUnion[p] = true
				}
			}
			selectedMarked := false
			for _, c := range s.resolver.Cascade(req) {
				if restricted && !keyable[c.Provider] {
					continue
				}
				av, _, _, _ := availStatus(snap, c.Provider, c.Model)
				sel := av && !selectedMarked
				if sel {
					selectedMarked = true
				}
				k := [2]string{c.Provider, c.Model}
				m, ok := agg[k]
				if !ok {
					m = &modelAgg{tiers: map[string]bool{}, order: len(agg)}
					agg[k] = m
				}
				m.tiers[tier] = true
				// OR across plans: a model is "active" if ANY tier would route to
				// it now, which is the question a reader is actually asking.
				m.active = m.active || av
				m.selected = m.selected || sel
			}
		}
	}
	resp.Models = make([]configModel, 0, len(agg))
	for k, m := range agg {
		tiers := make([]string, 0, len(m.tiers))
		for t := range m.tiers {
			tiers = append(tiers, t)
		}
		sort.Strings(tiers)
		resp.Models = append(resp.Models, configModel{
			Provider: k[0], Model: k[1], Tiers: tiers,
			Active: m.active, Selected: m.selected,
		})
	}
	// Stable order for a rendered page: provider, then model.
	sort.Slice(resp.Models, func(i, j int) bool {
		if resp.Models[i].Provider != resp.Models[j].Provider {
			return resp.Models[i].Provider < resp.Models[j].Provider
		}
		return resp.Models[i].Model < resp.Models[j].Model
	})

	// --- providers ---
	provNames := make([]string, 0, len(snap))
	for p := range snap {
		if restricted && !keyableUnion[p] {
			continue
		}
		provNames = append(provNames, p)
	}
	sort.Strings(provNames)
	resp.Providers = make([]configProvider, 0, len(provNames))
	for _, p := range provNames {
		a := snap[p]
		resp.Providers = append(resp.Providers, configProvider{
			Provider: p, Active: a.Reachable && !a.Excluded,
		})
	}

	// --- search (RFC BB cascade) ---
	resp.Search = []configSearch{}
	if s.searchResolver != nil && s.searchRegistry != nil {
		callerTenant, callerUser := "", ""
		if !admin && !public {
			if p, ok := auth.PrincipalFromContext(ctx); ok {
				callerTenant, callerUser = p.TenantID, p.Subject
			}
		}
		allowOperatorKey := !restricted
		ssnap := s.searchResolver.Snapshot()
		for _, id := range s.searchResolver.Cascade(nil) {
			p, ok := s.searchRegistry.Get(id)
			if !ok {
				continue
			}
			keyable := s.searchProviderKeyable(ctx, id, p.KeyEnvName(), callerTenant, callerUser, allowOperatorKey)
			if restricted && !keyable {
				continue
			}
			resp.Search = append(resp.Search, configSearch{
				Provider: id,
				Active:   keyable && ssnap[id].Reachable,
				Primary:  len(resp.Search) == 0,
			})
		}
	}

	return resp
}

// publicFeatures reduces the capability map to {name: bool} by reading ONLY each
// entry's `available` field.
//
// This is the load-bearing security property of the public view, and it is a
// whitelist by SHAPE rather than by name: a field added to the capability probe
// later — an embedder model, a base URL, a tuning threshold, a count — cannot
// reach a public reader, because nothing here ever copies a value it was not
// asked for. A name-based denylist would have to be updated in lockstep with a
// package it does not own, and would fail silently when it wasn't.
//
// An entry with no `available` key (only `storage`, which is admin-gated and so
// never present here) is dropped rather than passed through.
func publicFeatures(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for name, v := range in {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if av, ok := m["available"].(bool); ok {
			out[name] = av
		}
	}
	return out
}
