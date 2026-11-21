package http

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// The listings show the effective tools, so a default the stored list does
// not name is not invisible: MCP list_agents (ListAgents) and the Library.
func TestListAgents_EffectiveToolsShowCalcUnlessRemoved(t *testing.T) {
	h := newReviewHarness(t)
	h.srv.cfg().Agents["plain"] = config.AgentDef{Model: "stub-model", SystemPrompt: "p", Tools: []string{"Read", "Context"}}
	h.srv.cfg().Agents["lean"] = config.AgentDef{Model: "stub-model", SystemPrompt: "p", Tools: []string{"Read", "Context", "-Calc"}}
	reg, err := h.srv.RegisterAgent(context.Background(), connector.RegisterAgentRequest{Name: "registered", SystemPrompt: "x", Tools: []string{"Read"}})
	if err != nil {
		t.Fatal(err)
	}
	// A registered agent is read with the Context default too.
	if !slices.Equal(reg.EffectiveTools, []string{"Read", "Context", "Calc"}) || !slices.Equal(reg.Tools, []string{"Read"}) {
		t.Errorf("register_agent: tools %v, effective_tools %v; want [Read] and [Read Context Calc]", reg.Tools, reg.EffectiveTools)
	}
	list, err := h.srv.ListAgents(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]connector.AgentDescriptor{}
	for _, d := range list {
		by[d.Name] = d
	}
	if d := by["plain"]; !slices.Contains(d.EffectiveTools, "Calc") || slices.Contains(d.Tools, "Calc") {
		t.Errorf("plain: tools %v, effective_tools %v; want Calc in effective_tools only", d.Tools, d.EffectiveTools)
	}
	if d := by["lean"]; slices.Contains(d.EffectiveTools, "Calc") {
		t.Errorf("lean: effective_tools %v hold Calc despite -Calc", d.EffectiveTools)
	}
	if d := by["registered"]; !slices.Equal(d.EffectiveTools, []string{"Read", "Context", "Calc"}) {
		t.Errorf("registered: effective_tools %v; want [Read Context Calc]", d.EffectiveTools)
	}
}

func TestUnifiedLibrary_Agents_EffectiveToolsShowCalcUnlessRemoved(t *testing.T) {
	srv, _, cleanup := libraryUnifiedFixture(t, map[string]config.AgentDef{
		"plain": {Provider: "stub", Model: "m", Tools: []string{"Read"}},
		"lean":  {Provider: "stub", Model: "m", Tools: []string{"Read", "-Calc"}},
	}, nil)
	defer cleanup()
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, authedRequest("GET", "/v1/_library/agents", nil))
	by := map[string]LibraryEntry{}
	for _, e := range decodeLibraryEntries(t, rec) {
		by[e.Name] = e
	}
	if got := by["plain"].EffectiveTools; !slices.Contains(got, "Calc") || !slices.Contains(got, "Read") {
		t.Errorf("plain: effective_tools %v; want Read and Calc", got)
	}
	if got := by["lean"].EffectiveTools; slices.Contains(got, "Calc") || !slices.Contains(got, "Read") {
		t.Errorf("lean: effective_tools %v; want Read and no Calc", got)
	}
}
