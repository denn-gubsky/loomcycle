package http

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// Why a field is not overridable. The token's lock is named first when both
// apply because the run path checks it first, so this is the refusal a run
// setting the field would actually be given.
const (
	overrideLockedByToken = "token_locked" // the caller's token carries an overrides policy that leaves the field out
	overrideLockedByAgent = "agent_locked" // the definition says allow_overrides: false
)

// overridableField is one per-run override, as the run path would treat it for
// this caller and this agent.
//
// Max and Values describe what the run path then accepts, and appear only for
// an overridable field whose accepted set this definition closes. Absent means
// "not a closed set here": any value the deployment accepts (GET /v1/_models,
// GET /v1/_routing) — not "anything at all". Each describes the field set on
// its own; a run that also overrides the tier, say, is validated against the
// combination.
type overridableField struct {
	Overridable bool   `json:"overridable"`
	Reason      string `json:"reason,omitempty"`
	// NarrowOnly: a run may only make it smaller than the definition allows
	// (agentDefOverridability's runMayNarrow).
	NarrowOnly bool     `json:"narrow_only,omitempty"`
	Max        int      `json:"max,omitempty"`
	Values     []string `json:"values,omitempty"`
}

// handleAgentOverridable serves GET /v1/_agentdef/{name}/overridable: for
// every per-run override, whether a run of this agent started by THIS caller
// may set it.
//
// A client building a run form (the n8n Spawn and Retune nodes) needs the
// answer before it sends anything; the only report until now was per RUN
// (GET /v1/runs/{id}/effective-config), which needs a run to exist.
//
// The answer is the caller's, not the agent's alone: the token's overrides
// policy and the definition's allow_overrides are both folded in, through the
// same two predicates the run path refuses with (tokenLocksOverride,
// agentLocksOverrides), so the report cannot say yes to what a run is then
// refused. restricted_by_token says the caller's token took part, so a client
// does not cache one caller's answer for another.
//
// Resolution is a run's: the name resolves static then dynamic in the caller's
// tenant, on the active version. A top-level run cannot pin a version, so
// there is no def_id parameter — the active version is the only one a run
// started from this answer could get.
func (s *Server) handleAgentOverridable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	if name == "" || len(name) > 256 {
		writeJSONError(w, http.StatusBadRequest, "invalid_argument", "agent name must be 1-256 characters")
		return
	}
	// Before the lookup, as on POST /v1/runs: a token limited to certain
	// agents gets the same refusal whether or not the other agent exists.
	if err := s.admitPrincipalAgent(ctx, name); err != nil {
		writeAgentNotAllowed(w, err)
		return
	}
	tenant := s.adminFocusedTenant(ctx, r.URL.Query().Get("tenant"))
	def, ok := s.lookupAgent(ctx, tenant, name)
	if !ok {
		// Another tenant's agent does not resolve here, so it reads exactly
		// like one that does not exist.
		writeJSONError(w, http.StatusNotFound, "agent_not_found", fmt.Sprintf("no agent named %q", name))
		return
	}
	p, hasPrincipal := auth.PrincipalFromContext(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"agent":               name,
		"def_id":              def.DefID,
		"allow_overrides":     !agentLocksOverrides(def),
		"restricted_by_token": hasPrincipal && p.Restrictions.Overrides != nil,
		"fields":              s.overridableFields(def, p, hasPrincipal),
	})
}

// overridableFields has one entry per lockable per-run override
// (auth.LockableOverrides): the set the run path's lock decides on, keyed by
// the wire name a run request uses. That set covers every field
// agentDefOverridability lets a run set except Tools, which only narrows and
// is never locked (TestAgentOverridable_CoversEveryOverridableDefField).
func (s *Server) overridableFields(def config.AgentDef, p auth.Principal, hasPrincipal bool) map[string]overridableField {
	agentLocked := agentLocksOverrides(def)
	out := make(map[string]overridableField, len(auth.LockableOverrides))
	for _, wire := range auth.LockableOverrides {
		switch {
		case tokenLocksOverride(p, hasPrincipal, wire):
			out[wire] = overridableField{Reason: overrideLockedByToken}
			continue
		case agentLocked:
			out[wire] = overridableField{Reason: overrideLockedByAgent}
			continue
		}
		f := overridableField{Overridable: true}
		f.NarrowOnly = agentDefOverridability[lockableOverrideFields[wire]] == runMayNarrow
		switch wire {
		case "max_concurrent_children":
			f.Max = fanoutCeiling(def)
		case "effort":
			f.Values = sortedNames(knownEfforts)
		case "tier":
			if tiers := s.cfg().Tiers; len(tiers) > 0 {
				f.Values = sortedNames(tiers)
			}
		case "provider":
			// Each declared provider through the check a run's override goes
			// through, so one the deployment no longer configures drops out.
			for _, id := range def.Providers {
				if s.checkProviderAllowed(def, id) == nil {
					f.Values = append(f.Values, id)
				}
			}
		case "model":
			f.Values = declaredModels(def)
		}
		out[wire] = f
	}
	return out
}

// declaredModels is the model allowlist checkModelAllowed enforces first: the
// definition's `models:` candidates. A definition without one is bounded by
// its tier's cascade or not at all, and that is not this definition's to list.
func declaredModels(def config.AgentDef) []string {
	seen := map[string]bool{}
	var out []string
	for _, cands := range def.Models {
		for _, c := range cands {
			if c.Model != "" && !seen[c.Model] {
				seen[c.Model] = true
				out = append(out, c.Model)
			}
		}
	}
	sort.Strings(out)
	return out
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
