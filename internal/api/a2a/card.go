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
	"os"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/denn-gubsky/loomcycle/internal/a2a/sign"
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
// pushNotifications is forced false (deferred). Signing is applied
// separately by signCardIfConfigured after the card is built — keeping
// buildAgentCard a pure function of its inputs (so card_test can assert
// card shape without touching the env or crypto).
func buildAgentCard(card config.A2AServerCard, baseURL, tenantPrefix string, extended, includeGRPC bool) *a2asdk.AgentCard {
	root := baseURL + tenantPrefix

	// Advertise only the bindings actually served. gRPC is dropped under
	// host/path tenancy (includeGRPC=false) so the card never points peers
	// at a binding whose tenancy boundary loomcycle cannot enforce — see
	// Server.grpcEnabled. REST + JSON-RPC are always served.
	interfaces := []*a2asdk.AgentInterface{
		a2asdk.NewAgentInterface(root+pathREST, a2asdk.TransportProtocolHTTPJSON),
		a2asdk.NewAgentInterface(root+pathJSONRPC, a2asdk.TransportProtocolJSONRPC),
	}
	if includeGRPC {
		interfaces = append(interfaces, a2asdk.NewAgentInterface(root+pathGRPC, a2asdk.TransportProtocolGRPC))
	}

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
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		SupportedInterfaces: interfaces,
		Skills:              skillsFromExposedAgents(card.ExposedAgents),
		SecuritySchemes:     securitySchemesFor(card.SecuritySchemes),
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
		// Key the advertised scheme by its KIND, not by s.Scheme. s.Scheme
		// is the HTTP auth token ("Bearer"/"Basic") and is empty for a bare
		// http scheme — keying by it would emit a security scheme named ""
		// (which peers reject) and collide two schemes that share a token.
		// The config has no dedicated name field; the kind is the stable,
		// non-empty identifier (one scheme per kind is the norm).
		name := a2asdk.SecuritySchemeName(s.Kind)
		switch s.Kind {
		case "http":
			scheme := s.Scheme
			if scheme == "" {
				scheme = "Bearer"
			}
			out[name] = a2asdk.HTTPAuthSecurityScheme{Scheme: scheme}
		case "apiKey":
			out[name] = a2asdk.APIKeySecurityScheme{
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

// signCardIfConfigured signs generated in place with the key named by
// cardCfg.SignWithKeyEnv, when (and only when) that env var is on the
// operator's allowlist AND holds a usable ECDSA P-256 PEM key. Any
// problem — no key configured, env var not allowlisted, var unset, or a
// malformed/wrong-type key — leaves the card UNSIGNED and emits a single
// tracing line via logf. Card serving NEVER fails on a signing problem
// (slice contract: serve unsigned + trace, don't 500).
//
// allowlist is the same operator-configured env-var gate the scheduler /
// RFC F credentials use (cfg.Env.SchedulerEnvAllowlist), so a signing key
// is subject to the identical "operator must opt this var in" floor — a
// substrate-authored card cannot name an arbitrary env var and exfiltrate
// it into a signature. logf must never receive the key VALUE.
func signCardIfConfigured(generated *a2asdk.AgentCard, cardCfg config.A2AServerCard, allowlist map[string]bool, logf func(string, ...any)) {
	envName := cardCfg.SignWithKeyEnv
	if envName == "" {
		return // unsigned by design — no signing key configured
	}
	if !allowlist[envName] {
		trace(logf, "a2a card: signing key env %q not in allowlist — serving card unsigned", envName)
		return
	}
	pem := os.Getenv(envName)
	if pem == "" {
		trace(logf, "a2a card: signing key env %q is unset/empty — serving card unsigned", envName)
		return
	}
	key, err := sign.ParseECPrivateKey([]byte(pem))
	if err != nil {
		// err carries no key material (it describes the parse failure),
		// so logging it is safe.
		trace(logf, "a2a card: signing key env %q failed to parse (%v) — serving card unsigned", envName, err)
		return
	}
	// Self-contained signing embeds the matching public key in the JWS
	// protected header so a peer (or loomcycle's own client when
	// verify_signed_card=true) can verify without separately fetching
	// the key — see internal/a2a/sign.SignCardSelfContained.
	if err := sign.SignCardSelfContained(generated, key); err != nil {
		trace(logf, "a2a card: signing failed (%v) — serving card unsigned", err)
		return
	}
}

// trace is a nil-safe logf invocation.
func trace(logf func(string, ...any), format string, args ...any) {
	if logf != nil {
		logf(format, args...)
	}
}
