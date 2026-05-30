package a2a

import (
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// fixtureCard is a representative A2AServerCardDef exposing two
// loomcycle agents as two A2A skills.
func fixtureCard() config.A2AServerCard {
	return config.A2AServerCard{
		Name:        "loomcycle-fleet",
		Description: "the test fleet",
		Provider: config.A2AServerCardProvider{
			Organization: "Acme",
			URL:          "https://acme.example",
		},
		Capabilities: config.A2AServerCardCaps{Streaming: true},
		ExposedAgents: []config.A2AExposedAgent{
			{
				AgentName:   "researcher",
				SkillID:     "research",
				SkillName:   "Research",
				Description: "does research",
				Tags:        []string{"web", "search"},
				InputModes:  []string{"text/plain"},
				OutputModes: []string{"text/plain"},
			},
			{
				AgentName: "writer",
				SkillID:   "write",
				SkillName: "Write",
			},
		},
		SecuritySchemes: []config.A2ASecurityScheme{
			{Kind: "http", Scheme: "bearer"},
		},
	}
}

func TestBuildAgentCard_SkillsFromExposedAgents(t *testing.T) {
	card := buildAgentCard(fixtureCard(), "https://agents.example", "", false)

	if card.Name != "loomcycle-fleet" {
		t.Errorf("name = %q, want loomcycle-fleet", card.Name)
	}
	if card.Provider == nil || card.Provider.Org != "Acme" {
		t.Fatalf("provider = %+v, want Org=Acme", card.Provider)
	}
	if len(card.Skills) != 2 {
		t.Fatalf("skills = %d, want 2 (one per exposed agent)", len(card.Skills))
	}
	// Skill id must be the routing key (skill_id), not the agent name.
	bySkill := map[string]a2asdk.AgentSkill{}
	for _, s := range card.Skills {
		bySkill[s.ID] = s
	}
	research, ok := bySkill["research"]
	if !ok {
		t.Fatalf("missing skill id=research; got %+v", bySkill)
	}
	if research.Name != "Research" || research.Description != "does research" {
		t.Errorf("research skill = %+v", research)
	}
	if len(research.Tags) != 2 {
		t.Errorf("research tags = %v, want 2", research.Tags)
	}
}

func TestBuildAgentCard_PushNotificationsAlwaysFalse(t *testing.T) {
	// Even if a future config flips push on, this slice forces it off
	// (the server cannot honour push configs yet).
	c := fixtureCard()
	c.Capabilities.PushNotifications = true
	card := buildAgentCard(c, "", "", false)
	if card.Capabilities.PushNotifications {
		t.Error("pushNotifications must be false (deferred), got true")
	}
	if !card.Capabilities.Streaming {
		t.Error("streaming should reflect the card (true)")
	}
}

func TestBuildAgentCard_BindingInterfaceURLs(t *testing.T) {
	card := buildAgentCard(fixtureCard(), "https://agents.example", "", false)
	want := map[a2asdk.TransportProtocol]string{
		a2asdk.TransportProtocolHTTPJSON: "https://agents.example/a2a/v1",
		a2asdk.TransportProtocolJSONRPC:  "https://agents.example/a2a/jsonrpc",
		a2asdk.TransportProtocolGRPC:     "https://agents.example/a2a/grpc",
	}
	if len(card.SupportedInterfaces) != 3 {
		t.Fatalf("interfaces = %d, want 3", len(card.SupportedInterfaces))
	}
	for _, iface := range card.SupportedInterfaces {
		if want[iface.ProtocolBinding] != iface.URL {
			t.Errorf("binding %s URL = %q, want %q", iface.ProtocolBinding, iface.URL, want[iface.ProtocolBinding])
		}
	}
}

func TestBuildAgentCard_PathModePrefixesInterfaceURLs(t *testing.T) {
	// Path-mode tenancy prepends /{tenant} to the interface URLs so a
	// peer that fetched the per-tenant card POSTs back to the same
	// tenant-prefixed binding.
	card := buildAgentCard(fixtureCard(), "https://agents.example", "/acme", false)
	for _, iface := range card.SupportedInterfaces {
		if iface.ProtocolBinding == a2asdk.TransportProtocolHTTPJSON {
			if iface.URL != "https://agents.example/acme/a2a/v1" {
				t.Errorf("path-mode REST URL = %q, want .../acme/a2a/v1", iface.URL)
			}
		}
	}
}

func TestSecuritySchemesFor_SkipsUnenforceableKinds(t *testing.T) {
	in := []config.A2ASecurityScheme{
		{Kind: "http", Scheme: "bearer"},
		{Kind: "oauth2", Scheme: "oauth"}, // not enforceable this slice
		{Kind: "mtls", Scheme: "mtls"},    // not enforceable this slice
	}
	out := securitySchemesFor(in)
	if len(out) != 1 {
		t.Fatalf("schemes = %d, want 1 (only http kept); got %+v", len(out), out)
	}
	// Keyed by KIND ("http"), not by the (possibly empty / colliding) HTTP
	// auth token. The token is carried in the scheme value.
	scheme, ok := out["http"]
	if !ok {
		t.Fatalf("expected http scheme keyed by kind %q; got %+v", "http", out)
	}
	if hs, isHTTP := scheme.(a2asdk.HTTPAuthSecurityScheme); !isHTTP || hs.Scheme != "bearer" {
		t.Errorf("http scheme value = %+v, want HTTPAuthSecurityScheme{Scheme: bearer}", scheme)
	}
}
