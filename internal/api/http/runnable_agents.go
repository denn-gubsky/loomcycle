package http

// runnable_agents.go — RFC BY: the user-facing runnable-agent catalog.
// GET /v1/_runnable-agents answers "which agents can I run", tiered by access
// mode, so a delegated user token can discover an agent without being told its
// name out of band. Member-read gated (runs:read via memberReadable); the
// tiering is decided SERVER-SIDE from the authoritative users.access_mode row
// (never the wire), with a hard isolation floor from the token scopes.

import (
	"context"
	"net/http"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// runnableAgent is one entry in the discovery response — intentionally LEAN: the
// name a user needs to run the agent + which tier it came from. No operator
// metadata (version counts, retired badges, content hashes) and no system
// prompt; that lives in the operator Library (/v1/_library/agents), which stays
// ScopeTenant.
type runnableAgent struct {
	Name   string `json:"name"`
	Source string `json:"source"` // "bundled" | "tenant" | "own"
}

type runnableAgentsResponse struct {
	Agents []runnableAgent `json:"agents"`
}

// handleRunnableAgents serves GET /v1/_runnable-agents (RFC BY). Tiers:
//
//   - bundled / system (static cfg.Agents) — always, for every principal (the
//     shared, operator-global runnable floor).
//   - tenant substrate agents (this tenant's AgentDefs) — only when the caller
//     may use tenant-shared defs (see runnableIncludesTenant).
//   - own user-scoped agents — none today (AgentDef is tenant-keyed), reserved.
//
// A name present in BOTH the static floor and the tenant substrate is reported
// once, as `bundled` (the shared floor a user runs regardless of the overlay).
func (s *Server) handleRunnableAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	includeTenant := s.runnableIncludesTenant(ctx)

	seen := make(map[string]struct{}, len(s.cfg().Agents))
	agents := make([]runnableAgent, 0, len(s.cfg().Agents))

	// Bundled / system catalog floor — shown to everyone.
	for name := range s.cfg().Agents {
		agents = append(agents, runnableAgent{Name: name, Source: "bundled"})
		seen[name] = struct{}{}
	}

	// Tenant substrate agents — only if the caller may use tenant-shared defs.
	if includeTenant && s.store != nil {
		tenantID, all := s.principalTenantScope(ctx, r.URL.Query().Get("tenant"))
		rows, err := s.store.AgentDefListNames(ctx)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		for _, row := range rows {
			if !all && row.TenantID != tenantID {
				continue // cross-tenant name — never leaked
			}
			if _, dup := seen[row.Name]; dup {
				continue // already in the bundled floor; keep the shared label
			}
			agents = append(agents, runnableAgent{Name: row.Name, Source: "tenant"})
			seen[row.Name] = struct{}{}
		}
	}

	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	writeJSONOK(w, runnableAgentsResponse{Agents: agents})
}

// runnableIncludesTenant decides whether the caller sees the tenant's shared
// agents (RFC BY tiering). Order matters:
//
//  1. An ISOLATED token never does — a hard floor from the token scopes
//     (auth.IsIsolated), so a stale users.access_mode column can never widen a
//     token that ConfineIsolatedScope already confines to its own data.
//  2. Otherwise a DELEGATED USER is governed by its authoritative
//     users.access_mode row: "tenant" yes, anything else no.
//  3. A non-user principal (tenant operator / admin / legacy / open mode) has
//     no users row → yes (its tenant's agents, tenant-scoped by
//     principalTenantScope; admin sees all / a ?tenant= focus).
func (s *Server) runnableIncludesTenant(ctx context.Context) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	if auth.IsIsolated(p, ok) {
		return false // (1) hard isolation floor
	}
	if !ok || s.store == nil {
		return true // (3) open mode / no store → operator-ish default
	}
	row, err := s.store.UserGet(ctx, p.TenantID, p.Subject)
	if err != nil {
		// No users row (an operator / admin / legacy principal) or a lookup miss:
		// (3) default to tenant-visible. Isolated tokens are already floored out
		// above, so this cannot widen an isolated member.
		return true
	}
	return row.AccessMode == "tenant" // (2) authoritative access mode
}
