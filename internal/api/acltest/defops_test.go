package acltest

import (
	"context"
	"net/http"
	"strings"
	"testing"

	googlegrpc "google.golang.org/grpc"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// RFC EZ §1.2 — a definition write asks its own verb of the definition it
// names. The `_<family>def` routes and RPCs ask only `read def:<fam>/*` (the
// op is in the body); the tool every transport reaches asks the op's. Each
// family, on each transport, must give one answer.

// Test fixtures, not secrets: bearers that exist only in this test's server.
const (
	noAuthor = "lct_acme_noauthor" // an admin denied authoring every definition
	noRetire = "lct_acme_noretire" // an admin denied retiring every definition
)

type defFamily struct {
	tool   string // the MCP meta-tool, and /v1/_<tool>
	retire string // the op that takes a definition out of service
	rpc    func(loomcyclepb.LoomcycleClient) func(context.Context, *loomcyclepb.SubstrateRequest, ...googlegrpc.CallOption) (*loomcyclepb.SubstrateResponse, error)
}

var defFamilies = []defFamily{
	{"agentdef", "retire", func(c loomcyclepb.LoomcycleClient) rpcFn { return c.AgentDef }},
	{"skilldef", "retire", func(c loomcyclepb.LoomcycleClient) rpcFn { return c.SkillDef }},
	{"teamdef", "retire", func(c loomcyclepb.LoomcycleClient) rpcFn { return c.TeamDef }},
	{"hookdef", "retire", func(c loomcyclepb.LoomcycleClient) rpcFn { return c.HookDef }},
	{"mcpserverdef", "retire", func(c loomcyclepb.LoomcycleClient) rpcFn { return c.MCPServerDef }},
	{"scheduledef", "retire", func(c loomcyclepb.LoomcycleClient) rpcFn { return c.ScheduleDef }},
	{"a2aservercarddef", "retire", func(c loomcyclepb.LoomcycleClient) rpcFn { return c.A2AServerCardDef }},
	{"a2aagentdef", "retire", func(c loomcyclepb.LoomcycleClient) rpcFn { return c.A2AAgentDef }},
	{"webhookdef", "retire", func(c loomcyclepb.LoomcycleClient) rpcFn { return c.WebhookDef }},
	{"memorybackenddef", "retire", func(c loomcyclepb.LoomcycleClient) rpcFn { return c.MemoryBackendDef }},
	{"documentsourcedef", "retire", func(c loomcyclepb.LoomcycleClient) rpcFn { return c.DocumentSourceDef }},
	{"volumedef", "delete", func(c loomcyclepb.LoomcycleClient) rpcFn { return c.VolumeDef }},
}

type rpcFn = func(context.Context, *loomcyclepb.SubstrateRequest, ...googlegrpc.CallOption) (*loomcyclepb.SubstrateResponse, error)

// wireDefTools gives the server every definition tool, as boot does, and
// the two admins the test binds denies to.
func wireDefTools(t *testing.T) func(cfg *config.Config, st store.Store) ([]tools.Tool, func(*lchttp.Server)) {
	return func(cfg *config.Config, st store.Store) ([]tools.Tool, func(*lchttp.Server)) {
		for _, tok := range []struct{ bearer, subject string }{{noAuthor, "noauthor"}, {noRetire, "noretire"}} {
			if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
				DefID: "def_" + tok.subject, Name: "acme-" + tok.subject, TenantID: "acme", Subject: tok.subject,
				TokenHash: auth.HashToken(pepper, tok.bearer), AllowedScopes: []string{auth.ScopeAdmin},
			}); err != nil {
				t.Fatal(err)
			}
		}
		agentDef := &builtin.AgentDef{Store: st, Cfg: cfg}
		skillDef := &builtin.SkillDef{Store: st}
		perAgent := []tools.Tool{agentDef, skillDef, &builtin.VolumeDef{Store: st, Cfg: cfg, MaxNameLen: 64}}
		return perAgent, func(srv *lchttp.Server) {
			srv.SetTeamDefTool(&builtin.TeamDef{Store: st, Agents: agentDef, Skills: skillDef})
			srv.SetHookDefTool(&builtin.HookDef{Store: st, Cfg: cfg})
			srv.SetMCPServerDefTool(&builtin.MCPServerDef{Store: st, Cfg: cfg, Registry: loommcp.NewDynamicRegistry()})
			srv.SetScheduleDefTool(&builtin.ScheduleDef{Store: st, Cfg: cfg})
			srv.SetA2AServerCardDefTool(&builtin.A2AServerCardDef{Store: st, Cfg: cfg})
			srv.SetA2AAgentDefTool(&builtin.A2AAgentDef{Store: st, Cfg: cfg})
			srv.SetWebhookDefTool(&builtin.WebhookDef{Store: st, Cfg: cfg})
			srv.SetMemoryBackendDefTool(&builtin.MemoryBackendDef{Store: st, Cfg: cfg})
			srv.SetDocumentSourceDefTool(&builtin.DocumentSourceDef{Store: st, Cfg: cfg})
		}
	}
}

// bindDefDenies binds each admin a list denying it one verb on every
// definition, over HTTP as the tenant operator.
func (e *env) bindDefDenies() {
	e.t.Helper()
	for _, s := range []struct{ path, body string }{
		{"/v1/_acldef", `{"op":"create","name":"no-def-author","rules":[{"effect":"deny","actions":["author"],"resources":["def:*"]}]}`},
		{"/v1/_aclbinding", `{"acl":"no-def-author","subject_kind":"user","subject_id":"noauthor"}`},
		{"/v1/_acldef", `{"op":"create","name":"no-def-retire","rules":[{"effect":"deny","actions":["retire"],"resources":["def:*"]}]}`},
		{"/v1/_aclbinding", `{"acl":"no-def-retire","subject_kind":"user","subject_id":"noretire"}`},
	} {
		if resp, raw := e.do(http.MethodPost, s.path, operator, s.body, nil); resp.StatusCode != http.StatusCreated {
			e.t.Fatalf("setup %s: %d %s", s.path, resp.StatusCode, raw)
		}
	}
}

func TestDefOps_AskTheOpsVerbOnEveryTransport(t *testing.T) {
	// An op the ACL refuses is acl_denied everywhere; one it admits reaches
	// the tool, which may still refuse it for its own reasons (nothing of
	// the name exists here) — so "admitted" is any answer but an ACL or
	// scope refusal.
	refusedByGate := map[string]bool{"acl_denied": true, "acl_unavailable": true, "insufficient_scope": true}
	type op struct {
		name   string
		bearer string
		input  func(f defFamily) string
		denied bool
	}
	ops := []op{
		{"author-denied create", noAuthor, func(f defFamily) string { return `{"op":"create","name":"acl-probe"}` }, true},
		{"author-denied unknown op asks author", noAuthor, func(f defFamily) string { return `{"op":"frobnicate","name":"acl-probe"}` }, true},
		{"author-denied list", noAuthor, func(f defFamily) string { return `{"op":"list","name":"acl-probe"}` }, false},
		{"author-denied retire", noAuthor, func(f defFamily) string { return retireInput(f) }, false},
		{"retire-denied retire", noRetire, func(f defFamily) string { return retireInput(f) }, true},
		{"retire-denied get", noRetire, func(f defFamily) string { return `{"op":"get","name":"acl-probe","def_id":"def_missing"}` }, false},
	}
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			e := newEnvWith(t, mode, wireDefTools(t))
			e.setup()
			e.bindDefDenies()
			for _, f := range defFamilies {
				for _, o := range ops {
					t.Run(f.tool+"/"+o.name, func(t *testing.T) {
						in := o.input(f)
						c := call{family: f.tool, input: in, method: http.MethodPost, path: "/v1/_" + f.tool, body: in, grpc: f.rpc}
						for transport, got := range map[string]outcome{
							"http": e.viaHTTP(o.bearer, c),
							"grpc": e.viaGRPC(o.bearer, c),
							"mcp":  e.viaMCP(o.bearer, c),
						} {
							switch {
							case o.denied && (got.allowed || got.code != "acl_denied"):
								t.Errorf("%s: want acl_denied, got allowed=%v code=%q (%s)", transport, got.allowed, got.code, got.raw)
							case !o.denied && refusedByGate[got.code]:
								t.Errorf("%s: want admitted, got %q (%s)", transport, got.code, got.raw)
							case !o.denied && strings.Contains(got.raw, "acl_denied"):
								t.Errorf("%s: want admitted, got %s", transport, got.raw)
							}
						}
					})
				}
			}
		})
	}
}

func retireInput(f defFamily) string {
	if f.retire == "delete" {
		return `{"op":"delete","name":"acl-probe"}`
	}
	return `{"op":"retire","def_id":"def_missing","retired":true}`
}
