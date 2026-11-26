package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/memberpath"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// RFC GQ P3.5 — ontology ops releases and release_diff, the MCP twins of GET
// /v1/_ontology/releases and /v1/_ontology/releases/diff.

// A release keeps the ontology's text after an edit or an erasure removed it, so
// the reads are the operator's: each op's twin route is carved out of the member
// path and asks read document, a member's and an isolated user's session may not
// call the tool, and the tenant operator's may.
func TestOntologyTool_ReleasesAreRefusedAMemberAndAnIsolatedUser(t *testing.T) {
	memberScopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	as := func(scopes []string) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "s", Scopes: scopes})
	}
	for _, c := range []struct{ op, path string }{
		{"releases", "/v1/_ontology/releases"},
		{"release_diff", "/v1/_ontology/releases/diff"},
	} {
		tw, ok := mcpToolTwins["ontology"].Ops[c.op]
		if !ok || tw.Method != http.MethodGet || tw.Path != c.path {
			t.Errorf("ontology %s twin = %+v (%v), want GET %s", c.op, tw, ok, c.path)
			continue
		}
		if !memberpath.CarvedOut(tw.Method, tw.Path) {
			t.Errorf("ontology %s: its route %s is not carved out of the member path", c.op, tw.Path)
		}
		q, ok, err := mcpTwinSurface("ontology", json.RawMessage(`{"op":"`+c.op+`"}`))
		route, routed := authz.HTTPSurface(http.MethodGet, c.path)
		if err != nil || !ok || !routed || q != route || q.Action != "read" || q.Type != "document" {
			t.Errorf("ontology %s asks %+v (%v %v), its route %+v; want read document on both", c.op, q, ok, err, route)
		}
	}
	for what, tc := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"a member":          {as(memberScopes), false},
		"an isolated user":  {as([]string{auth.ScopeUser}), false},
		"a tenant operator": {as([]string{auth.ScopeTenant}), true},
	} {
		if got := principalMayCallTool(tc.ctx, "ontology"); got != tc.want {
			t.Errorf("%s may call the ontology tool = %v, want %v", what, got, tc.want)
		}
	}
}

// ontology op=releases and op=release_diff hand their arguments to the connector as
// HTTP's routes do — the tenant as given, to omitted or "current" as 0 — and return
// the connector's answer unchanged; a refusal comes back as HTTP's {code, error}, an
// internal failure without its text, and a from or to that is not a release's number
// is invalid_request before the connector.
func TestOntologyTool_ReleasesMatchTheConnector(t *testing.T) {
	list := connector.ListOntologyReleasesResult{Tenant: "acme", Releases: []connector.OntologyRelease{
		{Seq: 2, ContentVersion: "0123456789abcdef", Fingerprint: "fp2", Reason: "apply", Actor: "ops", CreatedAt: 20, Entries: 9},
		{Seq: 1, ContentVersion: "fedcba9876543210", Fingerprint: "fp1", Reason: "confirm", CreatedAt: 10, Entries: 8},
	}}
	diff := connector.DiffOntologyReleasesResult{
		Tenant: "acme",
		From:   connector.OntologyDiffSide{Seq: 1, ContentVersion: "fedcba9876543210"},
		To:     connector.OntologyDiffSide{Current: true, ContentVersion: "0123456789abcdef"},
		Changes: []connector.OntologyEntryDiff{
			{ChunkID: "c1", Kind: "class", Change: connector.OntologyDiffAdded,
				After: &connector.OntologyEntryState{Name: "Clause", Parent: "c0", ParentName: "Provision", Directives: json.RawMessage(`{"fields":[]}`)}},
			{ChunkID: "c2", Kind: "relation", Change: connector.OntologyDiffChanged, Changed: []string{connector.OntologyDiffRenamed},
				Before: &connector.OntologyEntryState{Name: "cites", Directives: json.RawMessage(`{}`)},
				After:  &connector.OntologyEntryState{Name: "refers_to", Directives: json.RawMessage(`{}`)}},
		},
		Issues: []string{"a side could not be read"},
	}
	mc := &mockConnector{relListRes: list, relDiffRes: diff}
	env := &handlerEnv{connector: mc}
	call := func(args string) *loommcp.CallToolResult {
		t.Helper()
		res, err := handleOntology(context.Background(), env, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := call(`{"op":"releases","tenant":"globex"}`)
	var gotList connector.ListOntologyReleasesResult
	if res.IsError || json.Unmarshal([]byte(res.Content[0].Text), &gotList) != nil || !reflect.DeepEqual(gotList, list) {
		t.Errorf("releases = %s, want the connector's list", res.Content[0].Text)
	}
	if mc.relListReq != (connector.ListOntologyReleasesRequest{Tenant: "globex"}) {
		t.Errorf("releases asked the connector %+v", mc.relListReq)
	}

	for args, want := range map[string]connector.DiffOntologyReleasesRequest{
		`{"op":"release_diff","from":1,"to":2,"tenant":"globex"}`: {Tenant: "globex", From: 1, To: 2},
		`{"op":"release_diff","from":1}`:                          {From: 1},
		`{"op":"release_diff","from":1,"to":"current"}`:           {From: 1},
		`{"op":"release_diff","from":1,"to":null}`:                {From: 1},
	} {
		mc.relDiffReq = connector.DiffOntologyReleasesRequest{}
		res := call(args)
		var got connector.DiffOntologyReleasesResult
		if res.IsError || json.Unmarshal([]byte(res.Content[0].Text), &got) != nil || !reflect.DeepEqual(got, diff) {
			t.Errorf("%s = %s, want the connector's diff", args, res.Content[0].Text)
		}
		if mc.relDiffReq != want {
			t.Errorf("%s asked the connector %+v, want %+v", args, mc.relDiffReq, want)
		}
	}

	refusal := func(res *loommcp.CallToolResult) (code, msg string) {
		var r struct{ Code, Error string }
		if !res.IsError || json.Unmarshal([]byte(res.Content[0].Text), &r) != nil {
			return "", res.Content[0].Text
		}
		return r.Code, r.Error
	}
	for _, args := range []string{
		`{"op":"release_diff"}`, `{"op":"release_diff","from":0}`, `{"op":"release_diff","from":"1"}`,
		`{"op":"release_diff","from":-3}`, `{"op":"release_diff","from":1,"to":0}`, `{"op":"release_diff","from":1,"to":"later"}`,
	} {
		mc.relDiffReq = connector.DiffOntologyReleasesRequest{}
		if code, msg := refusal(call(args)); code != "invalid_request" || mc.relDiffReq != (connector.DiffOntologyReleasesRequest{}) {
			t.Errorf("%s = %s %q, connector got %+v; want invalid_request before the connector", args, code, msg, mc.relDiffReq)
		}
	}

	for _, tc := range []struct {
		err  error
		code string
	}{
		{connector.WithMessage(connector.ErrOntologyReleaseInvalid, "from must be a release"), "invalid_request"},
		{connector.WithMessage(connector.ErrOntologyReleaseForbidden, "for an operator"), "forbidden"},
		{connector.WithMessage(connector.ErrOntologyReleaseNotFound, "no release 9"), "release_not_found"},
		{connector.WithMessage(connector.ErrOntologyReleaseUnavailable, "no SQL Memory"), "unavailable"},
		{errors.New("open /var/lib/secret.db: denied"), "release_read_failed"},
	} {
		mc.relErr = tc.err
		for _, args := range []string{`{"op":"releases"}`, `{"op":"release_diff","from":1}`} {
			code, msg := refusal(call(args))
			if code != tc.code {
				t.Errorf("%s with %v: code %q (%s), want %s", args, tc.err, code, msg, tc.code)
			}
			if strings.Contains(msg, "/var/lib") {
				t.Errorf("an internal failure's text reached the caller: %s", msg)
			}
		}
	}
}
