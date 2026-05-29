package lookup_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ---- A2AServerCardDef resolver ----

type stubA2AServerCardStore struct {
	defs map[string]store.A2AServerCardDefRow // keyed by name (active)
}

func (s *stubA2AServerCardStore) A2AServerCardDefGetActive(_ context.Context, name string) (store.A2AServerCardDefRow, error) {
	if row, ok := s.defs[name]; ok {
		return row, nil
	}
	return store.A2AServerCardDefRow{}, &store.ErrNotFound{Kind: "a2a_server_card_def_active", ID: name}
}

// TestA2AServerCard_EquivalenceYamlVsSubstrate pins the architectural
// contract: loading the same content via the yaml path
// (cfg.A2AServerCards) vs the substrate path (a2a_server_card_defs row,
// decoded by lookup.A2AServerCard) MUST produce equivalent
// config.A2AServerCard structs.
func TestA2AServerCard_EquivalenceYamlVsSubstrate(t *testing.T) {
	yamlCard := config.A2AServerCard{
		Name:        "jobs-card",
		Description: "exposes the jobs agents",
		Provider:    config.A2AServerCardProvider{Organization: "Acme", URL: "https://acme.example"},
		Capabilities: config.A2AServerCardCaps{
			Streaming: true, PushNotifications: true, ExtendedAgentCard: false,
		},
		ExposedAgents: []config.A2AExposedAgent{
			{AgentName: "job-search", SkillID: "search", SkillName: "Search jobs",
				Tags: []string{"jobs"}, InputModes: []string{"text"}, OutputModes: []string{"text"}},
		},
		SecuritySchemes: []config.A2ASecurityScheme{{Kind: "http", Scheme: "bearer"}},
		SignWithKeyEnv:  "LOOMCYCLE_A2A_SIGNING_KEY",
	}

	substrateShape := lookup.SubstrateA2AServerCardDef{
		Name:        yamlCard.Name,
		Description: yamlCard.Description,
		Provider:    lookup.SubstrateA2AProvider{Organization: "Acme", URL: "https://acme.example"},
		Capabilities: lookup.SubstrateA2ACapabilities{
			Streaming: true, PushNotifications: true,
		},
		ExposedAgents: []lookup.SubstrateA2AExposedAgent{
			{AgentName: "job-search", SkillID: "search", SkillName: "Search jobs",
				Tags: []string{"jobs"}, InputModes: []string{"text"}, OutputModes: []string{"text"}},
		},
		SecuritySchemes: []lookup.SubstrateA2ASecurityScheme{{Kind: "http", Scheme: "bearer"}},
		SignWithKeyEnv:  yamlCard.SignWithKeyEnv,
	}
	defJSON, err := json.Marshal(substrateShape)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	ss := &stubA2AServerCardStore{
		defs: map[string]store.A2AServerCardDefRow{
			"jobs-card": {DefID: "ascd_v1", Name: "jobs-card", Version: 1, Definition: defJSON, CreatedAt: time.Now()},
		},
	}
	resolved, ok := lookup.A2AServerCard(context.Background(), ss, &config.Config{}, "jobs-card")
	if !ok {
		t.Fatal("resolver returned !ok")
	}
	if !reflect.DeepEqual(resolved, yamlCard) {
		t.Errorf("substrate-resolved card != yaml card:\n got %+v\nwant %+v", resolved, yamlCard)
	}
}

// TestA2AServerCard_StaticBeforeSubstrate pins precedence: static yaml
// wins when a name exists in both sources.
func TestA2AServerCard_StaticBeforeSubstrate(t *testing.T) {
	cfg := &config.Config{
		A2AServerCards: map[string]config.A2AServerCard{
			"card": {Name: "yaml-card", ExposedAgents: []config.A2AExposedAgent{{AgentName: "a"}}},
		},
	}
	ss := &stubA2AServerCardStore{
		defs: map[string]store.A2AServerCardDefRow{
			"card": {DefID: "ascd_v1", Name: "card", Definition: json.RawMessage(`{"name":"substrate-card"}`)},
		},
	}
	got, ok := lookup.A2AServerCard(context.Background(), ss, cfg, "card")
	if !ok {
		t.Fatal("resolver returned !ok")
	}
	if got.Name != "yaml-card" {
		t.Errorf("Name = %q, want yaml-card (static must win)", got.Name)
	}
}

// ---- A2AAgentDef resolver ----

type stubA2AAgentStore struct {
	defs map[string]store.A2AAgentDefRow
}

func (s *stubA2AAgentStore) A2AAgentDefGetActive(_ context.Context, name string) (store.A2AAgentDefRow, error) {
	if row, ok := s.defs[name]; ok {
		return row, nil
	}
	return store.A2AAgentDefRow{}, &store.ErrNotFound{Kind: "a2a_agent_def_active", ID: name}
}

func TestA2AAgent_EquivalenceYamlVsSubstrate(t *testing.T) {
	yamlAgent := config.A2AAgent{
		Endpoint: "https://peer.example/a2a",
		Binding:  "jsonrpc",
		Auth:     config.A2AAgentAuth{Scheme: "http", BearerCredentialRef: "peer-token"},
		ExpectedSkills: []config.A2AExpectedSkill{
			{ID: "search", Required: true},
			{ID: "summarize", Required: false},
		},
		VerifySignedCard: true,
	}

	substrateShape := lookup.SubstrateA2AAgentDef{
		Endpoint: yamlAgent.Endpoint,
		Binding:  yamlAgent.Binding,
		Auth:     lookup.SubstrateA2AAgentAuth{Scheme: "http", BearerCredentialRef: "peer-token"},
		ExpectedSkills: []lookup.SubstrateA2AExpectedSkill{
			{ID: "search", Required: true},
			{ID: "summarize", Required: false},
		},
		VerifySignedCard: true,
	}
	defJSON, err := json.Marshal(substrateShape)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	ss := &stubA2AAgentStore{
		defs: map[string]store.A2AAgentDefRow{
			"peer": {DefID: "aad_v1", Name: "peer", Version: 1, Definition: defJSON, CreatedAt: time.Now()},
		},
	}
	resolved, ok := lookup.A2AAgent(context.Background(), ss, &config.Config{}, "peer")
	if !ok {
		t.Fatal("resolver returned !ok")
	}
	if !reflect.DeepEqual(resolved, yamlAgent) {
		t.Errorf("substrate-resolved agent != yaml agent:\n got %+v\nwant %+v", resolved, yamlAgent)
	}
}

func TestA2AAgent_StaticBeforeSubstrate(t *testing.T) {
	cfg := &config.Config{
		A2AAgents: map[string]config.A2AAgent{
			"peer": {AgentCardURL: "https://yaml.example/.well-known/agent-card.json"},
		},
	}
	ss := &stubA2AAgentStore{
		defs: map[string]store.A2AAgentDefRow{
			"peer": {DefID: "aad_v1", Name: "peer", Definition: json.RawMessage(`{"agent_card_url":"https://substrate.example"}`)},
		},
	}
	got, ok := lookup.A2AAgent(context.Background(), ss, cfg, "peer")
	if !ok {
		t.Fatal("resolver returned !ok")
	}
	if got.AgentCardURL != "https://yaml.example/.well-known/agent-card.json" {
		t.Errorf("AgentCardURL = %q, want the yaml value (static must win)", got.AgentCardURL)
	}
}

// ---- drift detection (substrate-read side) ----

// TestA2AServerCard_DriftDetection pins the SubstrateA2AServerCardDef
// field set against an explicit `want` enumeration. A field added to or
// removed from SubstrateA2AServerCardDef without updating this map fails
// CI. The complementary direction (mergedA2AServerCardDef ↔
// SubstrateA2AServerCardDef) lives in the builtin package.
func TestA2AServerCard_DriftDetection(t *testing.T) {
	want := map[string]bool{
		"name":              true,
		"description":       true,
		"provider":          true,
		"capabilities":      true,
		"exposed_agents":    true,
		"security_schemes":  true,
		"sign_with_key_env": true,
	}
	have := a2aJSONTagsOf(reflect.TypeOf(lookup.SubstrateA2AServerCardDef{}))
	assertTagSetsEqual(t, "SubstrateA2AServerCardDef", want, have)
}

func TestA2AAgent_DriftDetection(t *testing.T) {
	want := map[string]bool{
		"agent_card_url":     true,
		"endpoint":           true,
		"binding":            true,
		"auth":               true,
		"expected_skills":    true,
		"verify_signed_card": true,
	}
	have := a2aJSONTagsOf(reflect.TypeOf(lookup.SubstrateA2AAgentDef{}))
	assertTagSetsEqual(t, "SubstrateA2AAgentDef", want, have)
}

func assertTagSetsEqual(t *testing.T, typeName string, want, have map[string]bool) {
	t.Helper()
	for tag := range want {
		if !have[tag] {
			t.Errorf("%s missing json tag %q (must mirror config yaml tag)", typeName, tag)
		}
	}
	for tag := range have {
		if !want[tag] {
			t.Errorf("%s has json tag %q not in expected set — if deliberately added, update the `want` map to confirm the addition was conscious", typeName, tag)
		}
	}
}

func a2aJSONTagsOf(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		for j, c := range tag {
			if c == ',' {
				tag = tag[:j]
				break
			}
		}
		out[tag] = true
	}
	return out
}
