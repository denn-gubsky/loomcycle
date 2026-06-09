package lookup

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A2AServerCardStore is the subset of store.Store the server-card
// resolver uses. Declared here so tests + callers can mock without
// depending on the full store interface.
type A2AServerCardStore interface {
	A2AServerCardDefGetActive(ctx context.Context, name string) (store.A2AServerCardDefRow, error)
}

// A2AAgentStore is the subset of store.Store the remote-peer resolver
// uses. RFC N: the substrate lookup carries a tenantID.
type A2AAgentStore interface {
	A2AAgentDefGetActive(ctx context.Context, tenantID, name string) (store.A2AAgentDefRow, error)
}

// A2AServerCard resolves a server-card NAME to its effective
// config.A2AServerCard by walking the lookup chain in precedence order:
//
//  1. static cfg.A2AServerCards (yaml-defined, pre-validated at boot)
//  2. a2a_server_card_def_active + a2a_server_card_defs (substrate path)
//
// Returns (zero, false) when no source has the name. Malformed
// persistence JSON also returns (zero, false) — defensive against
// future-field churn or hand-edited rows.
//
// Mirrors lookup.Schedule for the v1.x RFC G A2AServerCardDef substrate.
func A2AServerCard(ctx context.Context, s A2AServerCardStore, cfg *config.Config, name string) (config.A2AServerCard, bool) {
	if cfg != nil {
		if c, ok := cfg.A2AServerCards[name]; ok {
			return c, true
		}
	}
	if s == nil {
		return config.A2AServerCard{}, false
	}
	activeRow, err := s.A2AServerCardDefGetActive(ctx, name)
	if err != nil {
		return config.A2AServerCard{}, false
	}
	var sd SubstrateA2AServerCardDef
	if uerr := json.Unmarshal(activeRow.Definition, &sd); uerr != nil {
		return config.A2AServerCard{}, false
	}
	return sd.ToConfigDef(), true
}

// A2AAgent resolves a remote-peer NAME to its effective config.A2AAgent
// within the caller's tenant, walking the lookup chain in precedence order
// (mirrors lookup.Agent / lookup.MemoryBackend):
//
//  1. (tenantID != "") tenant-scoped substrate (a2a_agent_def_active
//     WHERE tenant_id=tenantID)
//  2. static cfg.A2AAgents (yaml-defined, the shared operator base)
//  3. shared substrate (tenant_id="")
//
// For the default tenant "" step 1 is skipped, collapsing to
// static-cfg → shared-substrate — identical to the pre-RFC-N behavior.
//
// Returns (zero, false) when no source has the name. Malformed
// persistence JSON also returns (zero, false).
func A2AAgent(ctx context.Context, s A2AAgentStore, cfg *config.Config, tenantID, name string) (config.A2AAgent, bool) {
	// 1. Tenant-scoped substrate (skipped for the shared "" tenant).
	if tenantID != "" {
		if a, ok := resolveA2AAgentSubstrate(ctx, s, tenantID, name); ok {
			return a, true
		}
	}
	// 2. Static cfg.A2AAgents — the shared operator base.
	if cfg != nil {
		if a, ok := cfg.A2AAgents[name]; ok {
			return a, true
		}
	}
	// 3. Shared substrate (tenant_id="").
	return resolveA2AAgentSubstrate(ctx, s, "", name)
}

// resolveA2AAgentSubstrate reads the a2a_agent_def_active overlay for one
// tenant pass. Returns (zero, false) on nil store, no active pointer for
// that tenant, or malformed row JSON.
func resolveA2AAgentSubstrate(ctx context.Context, s A2AAgentStore, tenantID, name string) (config.A2AAgent, bool) {
	if s == nil {
		return config.A2AAgent{}, false
	}
	activeRow, err := s.A2AAgentDefGetActive(ctx, tenantID, name)
	if err != nil {
		return config.A2AAgent{}, false
	}
	var ad SubstrateA2AAgentDef
	if uerr := json.Unmarshal(activeRow.Definition, &ad); uerr != nil {
		return config.A2AAgent{}, false
	}
	return ad.ToConfigDef(), true
}

// SubstrateA2AServerCardDef mirrors the JSON shape `A2AServerCardDef`
// create/fork persists in `a2a_server_card_defs.definition` (snake_case
// JSON tags via the `mergedA2AServerCardDef` adapter in
// internal/tools/builtin/a2aservercarddef.go). The runtime consumer
// (`config.A2AServerCard`) carries ONLY yaml tags — unmarshalling
// substrate JSON directly into it silently drops every field.
//
// This adapter + ToConfigDef is the seam. Kept in sync with
// `mergedA2AServerCardDef`; a drift test in the builtin package pins
// the field set so a future field added to either side without the
// matching addition here fails CI.
type SubstrateA2AServerCardDef struct {
	Name            string                       `json:"name,omitempty"`
	Description     string                       `json:"description,omitempty"`
	Provider        SubstrateA2AProvider         `json:"provider,omitempty"`
	Capabilities    SubstrateA2ACapabilities     `json:"capabilities,omitempty"`
	ExposedAgents   []SubstrateA2AExposedAgent   `json:"exposed_agents,omitempty"`
	SecuritySchemes []SubstrateA2ASecurityScheme `json:"security_schemes,omitempty"`
	SignWithKeyEnv  string                       `json:"sign_with_key_env,omitempty"`
}

// SubstrateA2AProvider mirrors config.A2AServerCardProvider.
type SubstrateA2AProvider struct {
	Organization string `json:"organization,omitempty"`
	URL          string `json:"url,omitempty"`
}

// SubstrateA2ACapabilities mirrors config.A2AServerCardCaps.
type SubstrateA2ACapabilities struct {
	Streaming         bool `json:"streaming,omitempty"`
	PushNotifications bool `json:"push_notifications,omitempty"`
	ExtendedAgentCard bool `json:"extended_agent_card,omitempty"`
}

// SubstrateA2AExposedAgent mirrors config.A2AExposedAgent.
type SubstrateA2AExposedAgent struct {
	AgentName   string   `json:"agent_name,omitempty"`
	SkillID     string   `json:"skill_id,omitempty"`
	SkillName   string   `json:"skill_name,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	InputModes  []string `json:"input_modes,omitempty"`
	OutputModes []string `json:"output_modes,omitempty"`
}

// SubstrateA2ASecurityScheme mirrors config.A2ASecurityScheme.
type SubstrateA2ASecurityScheme struct {
	Kind   string `json:"kind,omitempty"`
	Scheme string `json:"scheme,omitempty"`
}

// ToConfigDef projects the substrate JSON shape onto config.A2AServerCard
// for the runtime to consume. Pure data shuffling.
func (s SubstrateA2AServerCardDef) ToConfigDef() config.A2AServerCard {
	out := config.A2AServerCard{
		Name:        s.Name,
		Description: s.Description,
		Provider: config.A2AServerCardProvider{
			Organization: s.Provider.Organization,
			URL:          s.Provider.URL,
		},
		Capabilities: config.A2AServerCardCaps{
			Streaming:         s.Capabilities.Streaming,
			PushNotifications: s.Capabilities.PushNotifications,
			ExtendedAgentCard: s.Capabilities.ExtendedAgentCard,
		},
		SignWithKeyEnv: s.SignWithKeyEnv,
	}
	if len(s.ExposedAgents) > 0 {
		out.ExposedAgents = make([]config.A2AExposedAgent, len(s.ExposedAgents))
		for i, e := range s.ExposedAgents {
			out.ExposedAgents[i] = config.A2AExposedAgent{
				AgentName:   e.AgentName,
				SkillID:     e.SkillID,
				SkillName:   e.SkillName,
				Description: e.Description,
				Tags:        e.Tags,
				InputModes:  e.InputModes,
				OutputModes: e.OutputModes,
			}
		}
	}
	if len(s.SecuritySchemes) > 0 {
		out.SecuritySchemes = make([]config.A2ASecurityScheme, len(s.SecuritySchemes))
		for i, sc := range s.SecuritySchemes {
			out.SecuritySchemes[i] = config.A2ASecurityScheme{Kind: sc.Kind, Scheme: sc.Scheme}
		}
	}
	return out
}

// SubstrateA2AAgentDef mirrors the JSON shape `A2AAgentDef` create/fork
// persists in `a2a_agent_defs.definition` (snake_case JSON tags via the
// `mergedA2AAgentDef` adapter in internal/tools/builtin/a2aagentdef.go).
// Kept in sync with `mergedA2AAgentDef`; a drift test in the builtin
// package pins parity.
type SubstrateA2AAgentDef struct {
	AgentCardURL     string                      `json:"agent_card_url,omitempty"`
	Endpoint         string                      `json:"endpoint,omitempty"`
	Binding          string                      `json:"binding,omitempty"`
	Auth             SubstrateA2AAgentAuth       `json:"auth,omitempty"`
	ExpectedSkills   []SubstrateA2AExpectedSkill `json:"expected_skills,omitempty"`
	VerifySignedCard bool                        `json:"verify_signed_card,omitempty"`
}

// SubstrateA2AAgentAuth mirrors config.A2AAgentAuth.
type SubstrateA2AAgentAuth struct {
	Scheme              string `json:"scheme,omitempty"`
	BearerCredentialRef string `json:"bearer_credential_ref,omitempty"`
}

// SubstrateA2AExpectedSkill mirrors config.A2AExpectedSkill.
type SubstrateA2AExpectedSkill struct {
	ID       string `json:"id,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// ToConfigDef projects the substrate JSON shape onto config.A2AAgent.
func (s SubstrateA2AAgentDef) ToConfigDef() config.A2AAgent {
	out := config.A2AAgent{
		AgentCardURL: s.AgentCardURL,
		Endpoint:     s.Endpoint,
		Binding:      s.Binding,
		Auth: config.A2AAgentAuth{
			Scheme:              s.Auth.Scheme,
			BearerCredentialRef: s.Auth.BearerCredentialRef,
		},
		VerifySignedCard: s.VerifySignedCard,
	}
	if len(s.ExpectedSkills) > 0 {
		out.ExpectedSkills = make([]config.A2AExpectedSkill, len(s.ExpectedSkills))
		for i, sk := range s.ExpectedSkills {
			out.ExpectedSkills[i] = config.A2AExpectedSkill{ID: sk.ID, Required: sk.Required}
		}
	}
	return out
}
