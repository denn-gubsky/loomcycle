package acltest

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC EZ §1.2 — a team walk's ops ask the team: run asks run team:<name>.
// The `_teamdef` route and the TeamDef RPC ask only `read def:team/*`; the
// tool every transport reaches asks the walk op's own verb.

// Test fixture, not a secret: a bearer that exists only in this test's server.
const noTeam = "lct_acme_noteam" // an admin denied run, read and cancel of team "locked"

func wireTeamTool(t *testing.T) func(cfg *config.Config, st store.Store) ([]tools.Tool, func(*lchttp.Server)) {
	return func(cfg *config.Config, st store.Store) ([]tools.Tool, func(*lchttp.Server)) {
		if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
			DefID: "def_noteam", Name: "acme-noteam", TenantID: "acme", Subject: "noteam",
			TokenHash: auth.HashToken(pepper, noTeam), AllowedScopes: []string{auth.ScopeAdmin},
		}); err != nil {
			t.Fatal(err)
		}
		agentDef := &builtin.AgentDef{Store: st, Cfg: cfg}
		skillDef := &builtin.SkillDef{Store: st}
		return []tools.Tool{agentDef, skillDef}, func(srv *lchttp.Server) {
			srv.SetTeamDefTool(&builtin.TeamDef{Store: st, Agents: agentDef, Skills: skillDef})
		}
	}
}

func TestTeamOps_AskTheTeamOnEveryTransport(t *testing.T) {
	refusedByGate := map[string]bool{"acl_denied": true, "acl_unavailable": true, "insufficient_scope": true}
	cases := []struct {
		name   string
		input  string
		denied bool
	}{
		{"run of the denied team", `{"op":"run","name":"locked","input":"x","mode":"detach"}`, true},
		{"run of another team", `{"op":"run","name":"open","input":"x","mode":"detach"}`, false},
		// Off a run there is no walk table: poll and cancel name no walk, so
		// they ask no team (and answer that there is nothing to read or end).
		{"poll naming no walk", `{"op":"poll"}`, false},
		{"cancel naming no walk", `{"op":"cancel","run_ids":["r_missing"]}`, false},
	}
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			e := newEnvWith(t, mode, wireTeamTool(t))
			e.setup()
			for _, s := range []struct{ path, body string }{
				{"/v1/_acldef", `{"op":"create","name":"no-locked-team","rules":[{"effect":"deny","actions":["run","read","cancel"],"resources":["team:locked"]}]}`},
				{"/v1/_aclbinding", `{"acl":"no-locked-team","subject_kind":"user","subject_id":"noteam"}`},
			} {
				if resp, raw := e.do(http.MethodPost, s.path, operator, s.body, nil); resp.StatusCode != http.StatusCreated {
					t.Fatalf("setup %s: %d %s", s.path, resp.StatusCode, raw)
				}
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					cl := call{family: "teamdef", input: c.input, method: http.MethodPost, path: "/v1/_teamdef", body: c.input, grpc: func(c loomcyclepb.LoomcycleClient) rpcFn { return c.TeamDef }}
					for transport, got := range map[string]outcome{
						"http": e.viaHTTP(noTeam, cl),
						"grpc": e.viaGRPC(noTeam, cl),
						"mcp":  e.viaMCP(noTeam, cl),
					} {
						switch {
						case c.denied && (got.allowed || got.code != "acl_denied" || !strings.Contains(got.raw, "team:locked")):
							t.Errorf("%s: want acl_denied naming team:locked, got allowed=%v code=%q (%s)", transport, got.allowed, got.code, got.raw)
						case !c.denied && (refusedByGate[got.code] || strings.Contains(got.raw, "acl_denied")):
							t.Errorf("%s: want admitted, got %q (%s)", transport, got.code, got.raw)
						}
					}
				})
			}
		})
	}
}

// TestTeamOps_DeniedRunIsForbiddenOverHTTP pins the status: a refusal of the
// walk op is the route gate's 403, as a definition op's is.
func TestTeamOps_DeniedRunIsForbiddenOverHTTP(t *testing.T) {
	e := newEnvWith(t, authz.ModeEnforce, wireTeamTool(t))
	e.setup()
	for _, s := range []struct{ path, body string }{
		{"/v1/_acldef", `{"op":"create","name":"no-locked-team","rules":[{"effect":"deny","actions":["run"],"resources":["team:locked"]}]}`},
		{"/v1/_aclbinding", `{"acl":"no-locked-team","subject_kind":"user","subject_id":"noteam"}`},
	} {
		if resp, raw := e.do(http.MethodPost, s.path, operator, s.body, nil); resp.StatusCode != http.StatusCreated {
			t.Fatalf("setup %s: %d %s", s.path, resp.StatusCode, raw)
		}
	}
	resp, raw := e.do(http.MethodPost, "/v1/_teamdef", noTeam, `{"op":"run","name":"locked","input":"x","mode":"detach"}`, nil)
	if resp.StatusCode != http.StatusForbidden || codeOf(raw) != "acl_denied" {
		t.Errorf("POST /v1/_teamdef run of a denied team = %d %s, want 403 acl_denied", resp.StatusCode, raw)
	}
}
