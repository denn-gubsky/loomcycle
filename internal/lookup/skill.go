package lookup

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// SkillStore is the subset of store.Store the skill resolver uses.
type SkillStore interface {
	SkillDefGetActive(ctx context.Context, name string) (store.SkillDefRow, error)
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

// Skill resolves a skill NAME to its effective body + allowed_tools by
// walking the lookup chain in precedence order:
//
//  1. substrate (skill_def_active overlay → skill_defs row) —
//     operator pushed via `POST /v1/_skilldef create`.
//  2. static skill set (loaded from LOOMCYCLE_SKILLS_ROOT at boot).
//
// Returns (zero, false) when neither source has the name.
//
// This mirrors what resolveSkillBodiesForRun in api/http/server.go
// has done inline for skill-baking at run-start. Factored here so
// the multi-tier lookup is in one place + the SubstrateSkillDef
// adapter has a documented home alongside the AgentDef one.
func Skill(ctx context.Context, s SkillStore, set *skills.Set, name string) (SkillResolution, bool) {
	if s != nil {
		row, err := s.SkillDefGetActive(ctx, name)
		if err == nil {
			var sd SubstrateSkillDef
			if uerr := json.Unmarshal(row.Definition, &sd); uerr == nil {
				if sd.Body != "" {
					return SkillResolution{
						Body:         sd.Body,
						AllowedTools: sd.AllowedTools,
						DefID:        row.DefID,
						Source:       "substrate",
					}, true
				}
			}
		}
	}
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
