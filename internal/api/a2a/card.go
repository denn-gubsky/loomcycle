// Package a2a implements the loomcycle A2A server HTTP surface (RFC G
// slice A2A-5): the well-known AgentCard URI, the three protocol-binding
// mounts (REST / JSON-RPC / gRPC), and multi-tenant routing. It consumes
// the SDK bridge in internal/a2a (Executor + TaskStore + auth) and the
// active A2AServerCardDef resolved via internal/lookup.
//
// This package builds the SERVER surface; the outbound client proxy and
// signed cards are later slices (A2A-6).
package a2a

import (
	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// bindingPaths are the sub-paths each protocol binding is mounted at,
// relative to the (optionally tenant-prefixed) root. Kept here so the
// AgentCard interface URLs and the mux mounts cannot drift.
const (
	pathWellKnown = "/.well-known/agent-card.json"
	pathREST      = "/a2a/v1"
	pathJSONRPC   = "/a2a/jsonrpc"
	pathGRPC      = "/a2a/grpc"
)

// buildAgentCard generates the A2A AgentCard for a resolved
// A2AServerCardDef. baseURL is the externally-reachable origin the
// binding interface URLs are anchored to (may be ""); tenantPrefix is
// the path segment prepended for path-mode tenancy (e.g. "/acme"), or
// "" for host/none modes — host-mode tenancy is reflected in baseURL by
// the caller, not the path. extended controls whether the full card
// (every field) or the base card is produced; this slice serves the
// same field set either way (no fields are gated yet), but the flag is
// threaded so A2A-6 can split them without a signature change.
//
// pushNotifications is forced false (deferred). Signing is a no-op stub
// this slice — sign_with_key_env is read into the card's provider data
// but no JWS is computed (A2A-6).
func buildAgentCard(card config.A2AServerCard, baseURL, tenantPrefix string, extended bool) *a2asdk.AgentCard {
	root := baseURL + tenantPrefix

	out := &a2asdk.AgentCard{
		Name:        card.Name,
		Description: card.Description,
		Version:     "1.0.0",
		Capabilities: a2asdk.AgentCapabilities{
			Streaming: card.Capabilities.Streaming,
			// Deferred to a later slice; advertise false so peers do not
			// attempt to register push configs we cannot honour.
			PushNotifications: false,
			ExtendedAgentCard: card.Capabilities.ExtendedAgentCard,
		},
		// loomcycle agents accept and emit text; richer modes are a
		// later concern (the bridge rejects non-text parts today).
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		SupportedInterfaces: []*a2asdk.AgentInterface{
			a2asdk.NewAgentInterface(root+pathREST, a2asdk.TransportProtocolHTTPJSON),
			a2asdk.NewAgentInterface(root+pathJSONRPC, a2asdk.TransportProtocolJSONRPC),
			a2asdk.NewAgentInterface(root+pathGRPC, a2asdk.TransportProtocolGRPC),
		},
		Skills:          skillsFromExposedAgents(card.ExposedAgents),
		SecuritySchemes: securitySchemesFor(card.SecuritySchemes),
	}
	if card.Provider.Organization != "" || card.Provider.URL != "" {
		out.Provider = &a2asdk.AgentProvider{
			Org: card.Provider.Organization,
			URL: card.Provider.URL,
		}
	}
	return out
}

// skillsFromExposedAgents maps each exposed loomcycle agent to one A2A
// AgentCard skill. The skill id is the routing key a peer echoes back in
// Message.Metadata["skillId"] to select which loomcycle agent handles
// the request (see internal/a2a Executor.agentFor).
func skillsFromExposedAgents(exposed []config.A2AExposedAgent) []a2asdk.AgentSkill {
	skills := make([]a2asdk.AgentSkill, 0, len(exposed))
	for _, e := range exposed {
		skills = append(skills, a2asdk.AgentSkill{
			ID:          e.SkillID,
			Name:        e.SkillName,
			Description: e.Description,
			Tags:        e.Tags,
			InputModes:  e.InputModes,
			OutputModes: e.OutputModes,
		})
	}
	return skills
}

// securitySchemesFor maps the config security schemes onto the SDK's
// sealed-union scheme types. Only the two schemes loomcycle can actually
// enforce at the frontier today — HTTP auth (bearer) and API key — are
// translated; unknown kinds are skipped rather than guessed, so the
// served card never advertises a scheme the server can't honour.
func securitySchemesFor(in []config.A2ASecurityScheme) a2asdk.NamedSecuritySchemes {
	if len(in) == 0 {
		return nil
	}
	out := make(a2asdk.NamedSecuritySchemes, len(in))
	for _, s := range in {
		switch s.Kind {
		case "http":
			scheme := s.Scheme
			if scheme == "" {
				scheme = "Bearer"
			}
			out[a2asdk.SecuritySchemeName(s.Scheme)] = a2asdk.HTTPAuthSecurityScheme{Scheme: scheme}
		case "apiKey":
			out[a2asdk.SecuritySchemeName(s.Scheme)] = a2asdk.APIKeySecurityScheme{
				Location: a2asdk.APIKeySecuritySchemeLocation("header"),
				Name:     "Authorization",
			}
		default:
			// oauth2 / mtls / unknown: not enforceable at this frontier
			// yet (A2A-6). Skip so the card stays honest.
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
