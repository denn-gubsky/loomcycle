// Package lookup is the canonical seam for resolving an agent / skill
// / MCP server NAME to its effective runtime definition. It walks the
// multi-tier lookup chain (static cfg → dynamic_agents → substrate
// active overlay) and applies the same normalizer chain the static
// boot-time config-load path applies, so a name resolved at runtime
// returns a byte-equivalent definition to one resolved at boot.
//
// # Why this package exists
//
// Loomcycle's pre-v0.8 substrate-era had ONE load path:
//
//	yaml → config.LoadConfig → resolveSkills / resolveAgent → cfg.Agents
//
// v0.8.15 added `dynamic_agents` and v0.8.22 added the AgentDef
// substrate (`agent_defs` + `agent_def_active`). Both new read paths
// skipped the boot-time normalizers — symptom: PR #186 ("set
// SystemPromptBase for runtime-resolved agents") had to bolt on
// `normalizeSystemPromptBase` at the read site after agents started
// silently losing their instructions on every skill-enabled run. The
// JSON-tag mismatch in PR #184 was a separate face of the same drift:
// `config.AgentDef` carries yaml-only tags, but the substrate persists
// snake_case JSON via `mergedDef` — every field silently dropped on
// unmarshal until a `substrateAgentDef` adapter was added.
//
// This package consolidates the lookup chain + normalizer chain + the
// adapter into one place. The runtime contract: a substrate-pushed
// AgentDef MUST produce a config.AgentDef byte-equivalent to the same
// content loaded from yaml. The equivalence is verified by the test
// in agent_equivalence_test.go.
//
// # Architectural pattern for future substrates
//
// When adding a dynamic substrate for a domain type:
//
//  1. The read path MUST walk the same normalizer chain as the
//     boot-time load. If `resolveSkills` (or analogue) sets a
//     derived field for the static path, your read path must set
//     it too. Add a normalizer to this package.
//  2. The persistence shape uses explicit JSON tags. Unmarshal
//     targets a struct WITH JSON tags, then convert to the runtime
//     consumer type via a single named adapter. NEVER `json.Unmarshal`
//     into a yaml-only struct (silently no-ops; the bug PR #184
//     fixed for AgentDef).
//  3. Add an equivalence test: yaml-load vs substrate-load of the
//     same content must produce byte-equivalent runtime defs.
//  4. Add a drift test: reflection-based audit pinning every field
//     in the runtime consumer struct has a corresponding tag in the
//     persistence-shape adapter, so a future field added to one
//     without the other fails the test instead of silently dropping.
package lookup

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// AgentStore is the subset of store.Store the agent resolver uses.
// Declared here so tests + callers can mock without depending on the
// full store interface.
type AgentStore interface {
	DynamicAgentGet(ctx context.Context, name string) (store.DynamicAgent, error)
	AgentDefGetActive(ctx context.Context, name string) (store.AgentDefRow, error)
}

// Agent resolves an agent NAME to its effective config.AgentDef by
// walking the lookup chain in precedence order:
//
//  1. static cfg.Agents (yaml-defined, pre-normalized at boot)
//  2. dynamic_agents table (v0.8.15 RegisterAgent path)
//  3. agent_def_active + agent_defs (v0.8.22 substrate path)
//
// Returns (zero, false) when no source has the name. Malformed
// persistence JSON also returns (zero, false) — defensive against
// future-field churn or hand-edited rows.
//
// Normalization: every dynamic path applies NormalizeAgentDef before
// returning, equalizing the runtime shape with what config-load
// would have produced for the same yaml content. Static cfg.Agents
// entries already went through config-load's resolveSkills /
// resolveAgent, so the cfg path returns directly without re-
// normalizing (avoids double-baking skill bodies into SystemPrompt).
func Agent(ctx context.Context, s AgentStore, cfg *config.Config, name string) (config.AgentDef, bool) {
	if cfg != nil {
		if def, ok := cfg.Agents[name]; ok {
			return def, true
		}
	}
	if s == nil {
		return config.AgentDef{}, false
	}
	// Tier 2 — dynamic_agents (RegisterAgent path). Persistence uses
	// config.AgentDef's JSON tags directly; safe to unmarshal into
	// config.AgentDef.
	if row, err := s.DynamicAgentGet(ctx, name); err == nil {
		var def config.AgentDef
		if uerr := json.Unmarshal(row.Definition, &def); uerr == nil {
			NormalizeAgentDef(&def)
			return def, true
		}
	}
	// Tier 3 — substrate (agent_def_active overlay → agent_defs row).
	// Persistence uses mergedDef's snake_case JSON tags; must unmarshal
	// into a json-tagged adapter then convert.
	activeRow, err := s.AgentDefGetActive(ctx, name)
	if err != nil {
		return config.AgentDef{}, false
	}
	var sd SubstrateAgentDef
	if uerr := json.Unmarshal(activeRow.Definition, &sd); uerr != nil {
		return config.AgentDef{}, false
	}
	def := sd.ToConfigDef()
	NormalizeAgentDef(&def)
	return def, true
}

// SubstrateAgentDef mirrors the JSON shape `AgentDef.create` persists
// in `agent_defs.definition` (snake_case via the json tags on
// `mergedDef` in internal/tools/builtin/agentdef.go). The runtime
// consumer (`config.AgentDef`) carries ONLY yaml tags — unmarshalling
// substrate JSON directly into it silently drops every field because
// json.Unmarshal then matches against Go field names instead.
//
// This adapter + ToConfigDef is the seam. Kept in sync with
// `mergedDef`; TestAgentDef_DriftDetection in drift_test.go pins the
// field set so a future field added to mergedDef without a matching
// addition here fails the build.
type SubstrateAgentDef struct {
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	Tier          string `json:"tier,omitempty"`
	Effort        string `json:"effort,omitempty"`
	MaxTokens     int    `json:"max_tokens,omitempty"`
	MaxIterations int    `json:"max_iterations,omitempty"`
	// MaxConcurrentChildren caps how many sub-agents this agent may
	// spawn in parallel via Agent.parallel_spawn. 0 = use runtime
	// default (DefaultMaxConcurrentChildren = 4). Mirrors the
	// config.AgentDef yaml field.
	MaxConcurrentChildren int    `json:"max_concurrent_children,omitempty"`
	SystemPrompt          string `json:"system_prompt,omitempty"`
	// SystemPromptBase carries the pre-skill-bake snapshot when the
	// substrate write path (commit 3 of this PR) persisted it.
	// Read-side normalizers fall back to SystemPrompt when this is
	// empty (legacy rows that pre-date the write-side fix).
	SystemPromptBase string                            `json:"system_prompt_base,omitempty"`
	AllowedTools     []string                          `json:"allowed_tools,omitempty"`
	Skills           []string                          `json:"skills,omitempty"`
	Providers        []string                          `json:"providers,omitempty"`
	Models           map[string][]config.TierCandidate `json:"models,omitempty"`
	MemoryScopes     []string                          `json:"memory_scopes,omitempty"`
	MemoryQuotaBytes int                               `json:"memory_quota_bytes,omitempty"`
	// RetryAttempts mirrors config.AgentDef.RetryAttempts — per-agent
	// same-provider retry budget override. *int so substrate JSON can
	// persist the operator-meaningful "force 0" intent as distinct
	// from "field not set" (use the tier default).
	RetryAttempts *int `json:"retry_attempts,omitempty"`
}

// ToConfigDef projects the substrate JSON shape onto config.AgentDef
// for the runtime to consume. Pure data shuffling; no normalization
// happens here (NormalizeAgentDef is called afterward by Agent).
func (s SubstrateAgentDef) ToConfigDef() config.AgentDef {
	return config.AgentDef{
		Provider:              s.Provider,
		Model:                 s.Model,
		Tier:                  s.Tier,
		Effort:                s.Effort,
		MaxTokens:             s.MaxTokens,
		MaxIterations:         s.MaxIterations,
		MaxConcurrentChildren: s.MaxConcurrentChildren,
		SystemPrompt:          s.SystemPrompt,
		SystemPromptBase:      s.SystemPromptBase,
		AllowedTools:          s.AllowedTools,
		Skills:                s.Skills,
		Providers:             s.Providers,
		Models:                s.Models,
		MemoryScopes:          s.MemoryScopes,
		MemoryQuotaBytes:      s.MemoryQuotaBytes,
		RetryAttempts:         s.RetryAttempts,
	}
}

// NormalizeAgentDef applies the boot-time normalization chain that
// `config.LoadConfig` / `resolveSkills` applied to statically-yaml-
// defined agents. Called by Agent() on every dynamic-load path before
// returning, so substrate-resolved agents reach the runtime with the
// same effective shape as yaml-resolved ones.
//
// Steps (each is idempotent under the static cfg path — re-applying
// them to a static cfg.AgentDef is a no-op):
//
//  1. SystemPromptBase invariant — fills SystemPromptBase from
//     SystemPrompt when empty. `resolveSkills` sets this at boot for
//     any yaml agent that lists skills, capturing the pre-skill-bake
//     snapshot for resolveSkillBodiesForRun to rebuild from. Agents
//     resolved at runtime never go through resolveSkills, so without
//     this normalization the rebuild starts from "" and concatenates
//     the skill body — silently replacing the agent's instructions.
//     See PR #186 for the production bug + the symptom.
//
// Future normalizers go here. Each MUST be idempotent under the
// static cfg path (so re-applying to an already-normalized agent is a
// no-op) and MUST mirror exactly what the equivalent boot-time helper
// does for yaml-defined agents.
func NormalizeAgentDef(def *config.AgentDef) {
	if def.SystemPromptBase == "" {
		def.SystemPromptBase = def.SystemPrompt
	}
}
