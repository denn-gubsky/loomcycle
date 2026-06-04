package lookup

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// SkillStore is the subset of store.Store the skill resolver uses.
// RFC N: the active-pointer lookup carries a tenantID.
type SkillStore interface {
	SkillDefGetActive(ctx context.Context, tenantID, name string) (store.SkillDefRow, error)
}

// SkillResolution is the effective skill body + metadata returned
// by Skill(). When the resolution came from a substrate row, DefID
// is non-empty (the system_prompt transcript event uses this for
// provenance — see resolveSkillBodiesForRun in api/http/server.go).
// When the resolution came from the static set, DefID is "".
type SkillResolution struct {
	Body         string
	AllowedTools []string
	// DefID — non-empty only for substrate-resolved skills.
	DefID string
	// Source — "substrate" or "static". Useful for the
	// system_prompt provenance event.
	Source string
}

// Skill resolves a skill NAME to its effective body + allowed_tools
// within the caller's tenant, walking the lookup chain in precedence
// order:
//
//  1. (tenantID != "") tenant-scoped substrate (skill_def_active overlay
//     → skill_defs row, WHERE tenant_id=tenantID). A per-tenant
//     promotion shadows the shared base by name.
//  2. shared-substrate (skill_def_active, tenant_id="") — the operator's
//     shared override.
//  3. static skill set (loaded from LOOMCYCLE_SKILLS_ROOT at boot) — the
//     shared base every tenant inherits.
//
// For the default tenant "" step 1 is skipped, so the order collapses to
// shared-substrate → static — byte-for-byte the pre-RFC-N
// "substrate-first then static" behavior. This is the critical
// back-compat property; the skill plane resolves substrate-FIRST (the
// opposite of agents, which is static-first), and that legacy order is
// preserved exactly for the "" tenant.
//
// The tenantID MUST come from the authoritative principal in ctx
// (auth.PrincipalFromContext → tools.RunIdentity fallback → ""), never
// from a wire/request field — see internal/api/http/server.go's
// tenantFromCtx.
//
// Returns (zero, false) when no source has the name.
//
// This mirrors what resolveSkillBodiesForRun in api/http/server.go
// has done inline for skill-baking at run-start. Factored here so
// the multi-tier lookup is in one place + the SubstrateSkillDef
// adapter has a documented home alongside the AgentDef one.
func Skill(ctx context.Context, s SkillStore, set *skills.Set, tenantID, name string) (SkillResolution, bool) {
	// 1. Tenant-scoped substrate shadow (skipped for the shared ""
	//    tenant so its order stays shared-substrate → static, exactly
	//    as pre-RFC-N).
	if s != nil && tenantID != "" {
		if res, ok := resolveSubstrateSkill(ctx, s, tenantID, name); ok {
			return res, true
		}
	}
	// 2. Shared substrate (tenant_id="").
	if s != nil {
		if res, ok := resolveSubstrateSkill(ctx, s, "", name); ok {
			return res, true
		}
	}
	// 3. Static skill set — the shared base.
	if set != nil {
		if sk, ok := set.Get(name); ok {
			return SkillResolution{
				Body:         sk.Body,
				AllowedTools: sk.AllowedTools,
				Source:       "static",
			}, true
		}
	}
	return SkillResolution{}, false
}

// resolveSubstrateSkill runs the skill_def_active → skill_defs lookup
// for one tenant pass. Returns (zero, false) when the tenant has no
// usable active row for the name. Preserves the parse-error +
// transient-error logging/fallback semantics exactly: a corrupt row or
// a transient store hiccup logs + returns false so the caller falls
// through to the next tier (the static bake still runs).
func resolveSubstrateSkill(ctx context.Context, s SkillStore, tenantID, name string) (SkillResolution, bool) {
	row, err := s.SkillDefGetActive(ctx, tenantID, name)
	switch {
	case err == nil:
		var sd SubstrateSkillDef
		if uerr := json.Unmarshal(row.Definition, &sd); uerr != nil {
			// Hand-edited / corrupted row — log + fall through to
			// static so the run can continue with the boot bake.
			log.Printf("lookup.Skill(%q): parse %s definition failed: %v", name, row.DefID, uerr)
		} else if sd.Body != "" {
			return SkillResolution{
				Body:         sd.Body,
				AllowedTools: sd.AllowedTools,
				DefID:        row.DefID,
				Source:       "substrate",
			}, true
		}
	default:
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			// Transient store hiccup. Static fallback preserves
			// correctness (the static bake still runs); the log line
			// is the operator's signal that a substrate override was
			// skipped this run.
			log.Printf("lookup.Skill(%q): SkillDefGetActive failed: %v", name, err)
		}
	}
	return SkillResolution{}, false
}

// SubstrateSkillDef mirrors the JSON shape `SkillDef.create` persists
// in `skill_defs.definition` (snake_case json tags via the
// `skillDefOverlay` struct in internal/tools/builtin/skilldef.go).
//
// Unlike the AgentDef case (where the runtime consumer config.AgentDef
// has yaml-only tags), the existing in-package skillDefOverlay struct
// in `internal/api/http/server.go` also uses json tags — so the
// "silent unmarshal drop" bug PR #184 exposed CAN'T fire here today.
// We define SubstrateSkillDef anyway to:
//
//  1. Document the wire shape explicitly + co-locate it with the
//     adapter pattern for AgentDef.
//  2. Make the drift test reflection-based audit symmetric across
//     all three substrates — a future refactor that introduces a
//     yaml-only intermediate struct would fail the test.
//  3. Eliminate the duplicate type declaration between
//     api/http/server.go's local skillDefOverlay + builtin/skilldef.go's
//     skillDefOverlay (both were the same fields independently).
type SubstrateSkillDef struct {
	Body         string   `json:"body,omitempty"`
	Description  string   `json:"description,omitempty"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
}
