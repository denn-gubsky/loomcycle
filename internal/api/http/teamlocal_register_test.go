package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// localTeamDef is a one-state team running its own agent "reviewer".
const localTeamDef = `{"entry":"review",` +
	`"local":{"agents":{"reviewer":{"model":"stub-model","system_prompt":"local reviewer"}}},` +
	`"states":[{"state":"review","handler":{"kind":"agent","agent":"./reviewer"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"review","to":"done","on":"success"}]}`

// A registered agent named like an active team's own agent would share its
// agent-scoped memory and channel cursors, exactly as an AgentDef version of
// that name would. Only the caller's tenant's teams reserve a name.
func TestRegisterAgent_RefusesNameOfAnActiveTeamsLocalAgent(t *testing.T) {
	h := newWalkHarness(t)
	seedTenantTeam(t, h.st, "acme", "sdlc", localTeamDef)
	register := func(ctx context.Context, name string) error {
		_, err := h.srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
			Name: name, SystemPrompt: "p", Tools: []string{"Read"}, Model: "stub-model",
		})
		return err
	}
	acme := alicePrincipal(context.Background())
	err := register(acme, "sdlc/reviewer")
	if err == nil || !strings.Contains(err.Error(), "sdlc") || !strings.Contains(err.Error(), "reviewer") {
		t.Fatalf("registering over a team's own agent must be refused, naming both; got %v", err)
	}
	for _, free := range []string{"sdlc/other", "sdlc/reviewer/deep", "reviewer"} {
		if err := register(acme, free); err != nil {
			t.Errorf("%q does not clash, but was refused: %v", free, err)
		}
	}
	// The shared tenant has no team sdlc.
	if err := register(context.Background(), "sdlc/reviewer"); err != nil {
		t.Errorf("another tenant's team must not reserve the name here: %v", err)
	}
}

// register_agent has no per-name grant, so its refusal of a name a team's own
// agent holds must not tell the caller what the team declares: it is the same
// refusal, byte for byte, as for a name a static agent holds. The write is
// still refused.
func TestRegisterAgent_TeamLocalNameRefusalDisclosesNothing(t *testing.T) {
	h := newWalkHarness(t)
	seedTenantTeam(t, h.st, "acme", "sdlc", localTeamDef)
	register := func(ctx context.Context, name string) error {
		_, err := h.srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
			Name: name, SystemPrompt: "p", Tools: []string{"Read"}, Model: "stub-model",
		})
		return err
	}
	acme := alicePrincipal(context.Background())
	teamErr := register(acme, "sdlc/reviewer")
	if teamErr == nil {
		t.Fatal("registering over a team's own agent must be refused")
	}
	if _, err := h.st.DynamicAgentGet(acme, "acme", "sdlc/reviewer"); err == nil {
		t.Error("the refused registration was stored")
	}

	// The same name held by a static agent, asked from a tenant with no team.
	withStatic := *h.srv.cfg()
	withStatic.Agents = map[string]config.AgentDef{"sdlc/reviewer": {Model: "stub-model", SystemPrompt: "static"}}
	h.srv.cfgHolder.Store(&withStatic)
	staticErr := register(context.Background(), "sdlc/reviewer")
	if staticErr == nil {
		t.Fatal("registering over a static agent must be refused")
	}
	if teamErr.Error() != staticErr.Error() {
		t.Errorf("refusals differ, so the team's declaration is disclosed:\n team:   %s\n static: %s", teamErr, staticErr)
	}
}
