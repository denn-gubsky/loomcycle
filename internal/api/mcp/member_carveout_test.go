package mcp

import (
	"context"
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/memberpath"
	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// mcpHTTPTwin is the HTTP route each member-reachable meta-tool mirrors, or
// "" with the reason it has none. Every tool a non-admin session may call
// (tenantConfinableTools) must be classified here, so a new tool cannot skip
// the question; the census below then asks memberpath.CarvedOut of each twin,
// so a route newly carved out of the member path fails it until the tool is
// withheld from members too.
var mcpHTTPTwin = map[string]struct{ method, path, noTwin string }{
	"identity":               {method: http.MethodGet, path: "/v1/_users"},
	"erasure":                {method: http.MethodPost, path: "/v1/_erasure"},
	"retention_override":     {method: http.MethodPut, path: "/v1/_retention/overrides"},
	"ontology":               {method: http.MethodPost, path: "/v1/_ontology/subjects/merge"},
	"review":                 {method: http.MethodPost, path: "/v1/_statements/rule"},
	"spawn_run":              {method: http.MethodPost, path: "/v1/runs"},
	"spawn_runs":             {method: http.MethodPost, path: "/v1/runs:batch"},
	"cancel_run":             {method: http.MethodPost, path: "/v1/agents/a/cancel"},
	"get_run":                {method: http.MethodGet, path: "/v1/runs/r"},
	"compact_run":            {method: http.MethodPost, path: "/v1/runs/r/compact"},
	"retune_run":             {method: http.MethodPost, path: "/v1/runs/r/retune"},
	"review_run":             {method: http.MethodPost, path: "/v1/runs/r/review"},
	"configured_run":         {method: http.MethodPost, path: "/v1/runs"},
	"list_runs":              {method: http.MethodGet, path: "/v1/runs"},
	"delete_run_tree":        {method: http.MethodDelete, path: "/v1/runs/r/tree"},
	"register_agent":         {noTwin: "MCP only: a session-registered agent"},
	"unregister_agent":       {noTwin: "MCP only: a session-registered agent"},
	"list_agents":            {noTwin: "MCP only: lists static and session-registered agents"},
	"agentdef":               {method: http.MethodPost, path: "/v1/_agentdef"},
	"skilldef":               {method: http.MethodPost, path: "/v1/_skilldef"},
	"teamdef":                {method: http.MethodPost, path: "/v1/_teamdef"},
	"list_team_channels":     {method: http.MethodGet, path: "/v1/_teamdef/t/channels"},
	"peek_team_channel":      {method: http.MethodGet, path: "/v1/_teamdef/t/channels/c/peek"},
	"release_team_channel":   {method: http.MethodPost, path: "/v1/_teamdef/t/channels/c/release"},
	"hookdef":                {method: http.MethodPost, path: "/v1/_hookdef"},
	"mcpserverdef":           {method: http.MethodPost, path: "/v1/_mcpserverdef"},
	"scheduledef":            {method: http.MethodPost, path: "/v1/_scheduledef"},
	"a2aservercarddef":       {method: http.MethodPost, path: "/v1/_a2aservercarddef"},
	"a2aagentdef":            {method: http.MethodPost, path: "/v1/_a2aagentdef"},
	"webhookdef":             {method: http.MethodPost, path: "/v1/_webhookdef"},
	"memorybackenddef":       {method: http.MethodPost, path: "/v1/_memorybackenddef"},
	"documentsourcedef":      {method: http.MethodPost, path: "/v1/_documentsourcedef"},
	"volumedef":              {method: http.MethodPost, path: "/v1/_volumedef"},
	"credentialdef":          {method: http.MethodPost, path: "/v1/_credentialdef"},
	"crypto":                 {method: http.MethodPost, path: "/v1/_crypto"},
	"file":                   {method: http.MethodPost, path: "/v1/runs/r/files"},
	"groupdef":               {method: http.MethodPost, path: "/v1/_groups"},
	"acldef":                 {method: http.MethodPost, path: "/v1/_acldef"},
	"aclbinding":             {method: http.MethodPost, path: "/v1/_aclbinding"},
	"authz":                  {method: http.MethodPost, path: "/v1/_authz/evaluation"},
	"clientdef":              {method: http.MethodPost, path: "/v1/_clientdef"},
	"memory":                 {method: http.MethodPost, path: "/v1/_memory/search"},
	"channel":                {method: http.MethodPost, path: "/v1/_channels"},
	"channeldef":             {method: http.MethodPost, path: "/v1/_channels"},
	"path":                   {method: http.MethodPost, path: "/v1/_path"},
	"document":               {method: http.MethodPost, path: "/v1/_document"},
	"history":                {method: http.MethodPost, path: "/v1/_history"},
	"decision":               {method: http.MethodPost, path: "/v1/_decide"},
	"evaluation":             {method: http.MethodPost, path: "/v1/_evaluation"},
	"context":                {noTwin: "reads the session's own identity and policies; no route returns them"},
	"interruption_resolve":   {method: http.MethodPost, path: "/v1/runs/r/interrupts/i/resolve"},
	"publish_channel":        {method: http.MethodPost, path: "/v1/users/u/channels/c/publish"},
	"subscribe_channel":      {method: http.MethodPost, path: "/v1/users/u/channels/c/subscribe"},
	"peek_channel":           {method: http.MethodGet, path: "/v1/users/u/channels/c/peek"},
	"ack_channel":            {method: http.MethodPost, path: "/v1/users/u/channels/c/ack"},
	"stream_user_run_states": {method: http.MethodGet, path: "/v1/users/u/agents/stream"},
}

// Every meta-tool a member's session may call has an HTTP twin a member also
// reaches, and every tool whose twin memberpath.CarvedOut keeps from members
// is withheld from a member's session and kept for the tenant operator. Read
// from the registry (tenantConfinableTools, toolDescriptors), so a new tool
// must be classified in mcpHTTPTwin.
func TestMCPTools_AMemberIsWithheldEveryToolWhoseHTTPTwinIsCarvedOut(t *testing.T) {
	memberScopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	// The member's scopes plus runs:delete, so the census asks the member
	// path rather than a scope the token was not minted with.
	member := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: append(memberScopes, auth.ScopeRunsDelete)})
	operator := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	listed := map[string]bool{}
	for _, td := range toolDescriptors() {
		listed[td.Name] = true
	}
	carved := 0
	for tool := range tenantConfinableTools {
		if !listed[tool] {
			t.Errorf("%s is tenant-confinable but has no descriptor", tool)
		}
		twin, ok := mcpHTTPTwin[tool]
		if !ok {
			t.Errorf("%s is reachable by a non-admin session and is not classified in mcpHTTPTwin: name its HTTP twin, or why it has none", tool)
			continue
		}
		if twin.noTwin != "" {
			continue
		}
		if !principalMayCallTool(operator, tool) {
			t.Errorf("the tenant operator's session may not call %s", tool)
		}
		if memberpath.CarvedOut(twin.method, twin.path) {
			carved++
			if principalMayCallTool(member, tool) {
				t.Errorf("a member's session may call %s, whose HTTP twin %s %s is carved out of the member path", tool, twin.method, twin.path)
			}
		}
	}
	for tool := range mcpHTTPTwin {
		if !tenantConfinableTools[tool] {
			t.Errorf("mcpHTTPTwin classifies %s, which no non-admin session reaches; remove it", tool)
		}
	}
	if carved < 8 {
		t.Fatalf("only %d twins are carved out; the census is not reading the carve-outs", carved)
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
