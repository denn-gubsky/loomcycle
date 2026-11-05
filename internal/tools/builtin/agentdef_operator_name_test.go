package builtin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// activeFaultStore fails the active-version reads the operator-name guards
// make, as a store outage would.
type activeFaultStore struct{ store.Store }

var errActiveFault = errors.New("store unavailable")

func (activeFaultStore) AgentDefGetActive(context.Context, string, string) (store.AgentDefRow, error) {
	return store.AgentDefRow{}, errActiveFault
}
func (activeFaultStore) TeamDefGetActive(context.Context, string, string) (store.TeamDefRow, error) {
	return store.TeamDefRow{}, errActiveFault
}
func (activeFaultStore) AgentDefListByName(context.Context, string) ([]store.AgentDefRow, error) {
	return nil, errActiveFault
}

func memberPrincipalCtx(t *testing.T, tenant string) context.Context {
	t.Helper()
	scopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, Subject: "bob", Scopes: scopes})
}

// register_agent's name guard covers a static agent too: a tenant's
// registered agent resolves before the static ones (lookup.Agent), so it
// would take a yaml agent over for every run of that name in the tenant.
func TestCheckRegisteredAgent_AMemberCannotTakeAStaticAgentsName(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Agents: map[string]config.AgentDef{"writer": {Tier: "middle"}}}
	req := connector.RegisterAgentRequest{Name: "writer", SystemPrompt: "mine", Tools: []string{"Read"}}
	if err := CheckRegisteredAgent(memberPrincipalCtx(t, "acme"), cfg, st, req, req.Tools); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("a member registered the static agent's name: %v", err)
	}
	operator := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	if err := CheckRegisteredAgent(operator, cfg, st, req, req.Tools); err != nil {
		t.Errorf("the tenant operator was refused: %v", err)
	}
}

// A shared-tier ("") operator team of the name a member's tenant has no
// version of is the team a run of that name resolves to: a member may not
// make its own version active over it.
func TestTeamDef_AMemberCannotPromoteOverASharedOperatorTeam(t *testing.T) {
	tool, _ := localTeamFixture(t)
	operator := tools.WithCallerTier(localAuthorCtx("", []string{"any"}, []string{"*"}), tools.TierOperatorPlane)
	member := tools.WithCallerTier(localAuthorCtx("acme", []string{"any"}, []string{"*"}), tools.TierMember)
	body := localTeam(`{"system_prompt":"review","tools":["Read"]}`)
	if res := teamOp(t, tool, operator, "create", "review", body); res.IsError {
		t.Fatalf("operator create: %s", res.Text)
	}
	if res := teamOp(t, tool, member, "create", "review", body); !res.IsError {
		t.Errorf("a member's create took over the shared operator team in its tenant: %s", res.Text)
	}
}

// A store fault in the operator-name guards refuses rather than treating the
// name as free.
func TestOperatorNameGuards_FailClosedOnAStoreError(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	faulty := activeFaultStore{st}
	member := tools.WithCallerTier(memberPrincipalCtx(t, "acme"), tools.TierMember)
	a := &AgentDef{Store: faulty, Cfg: &config.Config{}}
	if err := a.memberActivationGuard(member, "promote", "acme", "linker"); err == nil {
		t.Error("the agent activation guard passed on a store error")
	}
	team := &TeamDef{Store: faulty}
	if err := team.memberTeamActivationGuard(member, "promote", "acme", "review"); err == nil {
		t.Error("the team activation guard passed on a store error")
	}
	req := connector.RegisterAgentRequest{Name: "linker", SystemPrompt: "x", Tools: []string{"Read"}}
	if err := CheckRegisteredAgent(memberPrincipalCtx(t, "acme"), &config.Config{}, faulty, req, req.Tools); err == nil {
		t.Error("register_agent's name guard passed on a store error")
	}
}
