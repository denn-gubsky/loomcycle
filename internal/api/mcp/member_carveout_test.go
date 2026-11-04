package mcp

import (
	"context"
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/memberpath"
	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// Every meta-tool whose HTTP twin the member path carves out is withheld from
// a member's session too, and stays a tenant operator's (RFC FY Part B). The
// twins are listed by hand; a tool with no carved-out twin is a member's on
// both transports, so it is not listed. Checked against memberpath.CarvedOut:
// users and their tokens (identity is read-only: no twin), authgate (no tool),
// erasure, ontology subject merge and binding, limits writes (no tool), the ACL
// management families, clientdef, retention overrides, the change feeds and
// memory maintenance sweeps (no memory or document op reaches them), crypto,
// and the ontology confirm/adopt/proposals routes (no tool reaches them).
func TestMCPTools_AMemberIsWithheldEveryToolWhoseHTTPTwinIsCarvedOut(t *testing.T) {
	twins := map[string]struct{ method, path string }{
		"erasure":            {http.MethodPost, "/v1/_erasure"},
		"ontology":           {http.MethodPost, "/v1/_ontology/subjects/merge"},
		"retention_override": {http.MethodPut, "/v1/_retention/overrides"},
		"crypto":             {http.MethodPost, "/v1/_crypto"},
		"groupdef":           {http.MethodPost, "/v1/_groups"},
		"acldef":             {http.MethodPost, "/v1/_acldef"},
		"aclbinding":         {http.MethodPost, "/v1/_aclbinding"},
		"clientdef":          {http.MethodPost, "/v1/_clientdef"},
	}
	memberScopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	member := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: memberScopes})
	operator := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	for tool, twin := range twins {
		if !memberpath.CarvedOut(twin.method, twin.path) {
			t.Errorf("fixture: %s's twin %s %s is not carved out; re-derive the list", tool, twin.method, twin.path)
		}
		if principalMayCallTool(member, tool) {
			t.Errorf("a member's session may call %s, whose HTTP twin %s %s refuses it", tool, twin.method, twin.path)
		}
		if !principalMayCallTool(operator, tool) {
			t.Errorf("the tenant operator's session may not call %s", tool)
		}
	}
}

// A member's session cannot report on or erase a subject, dry run or not:
// POST /v1/_erasure is carved out of the member path, and neither the handler
// nor the connector asks who is calling. A tenant operator's call still
// reaches the connector.
func TestErasureTool_IsRefusedAMemberAndServesTheOperator(t *testing.T) {
	memberScopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	member := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: memberScopes})
	operator := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	for _, args := range []string{
		`{"op":"report","subject":"alice"}`,
		`{"op":"execute","subject":"alice"}`,
		`{"op":"execute","subject":"alice","dry_run":false,"confirm":"alice"}`,
	} {
		in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"erasure","arguments":` + args + `}}` + "\n"
		srv := New(Config{Connector: &mockConnector{}, Logger: discardLogger()})
		resps, _ := driveServerCtx(t, srv, member, in)
		if len(resps) != 1 || resps[0].Error == nil || resps[0].Error.Code != mcpErrForbidden {
			t.Errorf("a member's erasure %s was not refused: %+v", args, resps)
		}
		srv = New(Config{Connector: &mockConnector{}, Logger: discardLogger()})
		resps, _ = driveServerCtx(t, srv, operator, in)
		if len(resps) != 1 || (resps[0].Error != nil && resps[0].Error.Code == mcpErrForbidden) {
			t.Errorf("the tenant operator's erasure %s was refused: %+v", args, resps)
		}
	}
}
