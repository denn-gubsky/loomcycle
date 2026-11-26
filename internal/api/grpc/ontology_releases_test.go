package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// The RPCs hand the tenant and the sides to the connector, where the rules and the
// reads live, and render every field of what comes back — an absent before or after
// stays absent; a refusal carries HTTP's code as its ErrorInfo reason under the gRPC
// code matching HTTP's status, and an internal failure hides its text.
func TestOntologyReleasesRPCs_DelegateToTheConnector(t *testing.T) {
	mc := &mockConnector{
		relListRes: connector.ListOntologyReleasesResult{Tenant: "acme", Releases: []connector.OntologyRelease{
			{Seq: 2, ContentVersion: "0123456789abcdef", Fingerprint: "fp2", Reason: "apply", Actor: "ops", CreatedAt: 20, Entries: 9},
		}},
		relDiffRes: connector.DiffOntologyReleasesResult{
			Tenant: "acme",
			From:   connector.OntologyDiffSide{Seq: 1, ContentVersion: "fedcba9876543210"},
			To:     connector.OntologyDiffSide{Current: true, ContentVersion: "0123456789abcdef"},
			Changes: []connector.OntologyEntryDiff{{
				ChunkID: "c1", Kind: "class", Change: connector.OntologyDiffAdded,
				After: &connector.OntologyEntryState{Name: "Clause", Parent: "c0", ParentName: "Provision", Status: "retired",
					Meaning: "m", Directives: json.RawMessage(`{"fields":[]}`)},
			}},
			Issues: []string{"i1"},
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	list, err := client.ListOntologyReleases(context.Background(), &loomcyclepb.ListOntologyReleasesRequest{Tenant: "globex"})
	if err != nil {
		t.Fatalf("ListOntologyReleases: %v", err)
	}
	if mc.relListReq.Tenant != "globex" || list.GetTenant() != "acme" || len(list.GetReleases()) != 1 {
		t.Fatalf("list = %v, connector asked %+v", list, mc.relListReq)
	}
	r := list.GetReleases()[0]
	if got := (connector.OntologyRelease{Seq: r.GetSeq(), ContentVersion: r.GetContentVersion(), Fingerprint: r.GetFingerprint(),
		Reason: r.GetReason(), Actor: r.GetActor(), CreatedAt: r.GetCreatedAt(), Entries: int(r.GetEntries())}); got != mc.relListRes.Releases[0] {
		t.Errorf("release = %+v, want %+v", got, mc.relListRes.Releases[0])
	}

	diff, err := client.DiffOntologyReleases(context.Background(), &loomcyclepb.DiffOntologyReleasesRequest{Tenant: "globex", From: 1})
	if err != nil {
		t.Fatalf("DiffOntologyReleases: %v", err)
	}
	if mc.relDiffReq != (connector.DiffOntologyReleasesRequest{Tenant: "globex", From: 1}) {
		t.Errorf("diff asked the connector %+v", mc.relDiffReq)
	}
	c := diff.GetChanges()[0]
	a := c.GetAfter()
	if diff.GetTenant() != "acme" || diff.GetFrom().GetSeq() != 1 || !diff.GetTo().GetCurrent() ||
		diff.GetTo().GetContentVersion() != "0123456789abcdef" || !reflect.DeepEqual(diff.GetIssues(), []string{"i1"}) ||
		c.GetChunkId() != "c1" || c.GetKind() != "class" || c.GetChange() != "added" || c.GetBefore() != nil ||
		a.GetName() != "Clause" || a.GetParent() != "c0" || a.GetParentName() != "Provision" || a.GetStatus() != "retired" ||
		a.GetMeaning() != "m" || string(a.GetDirectivesJson()) != `{"fields":[]}` {
		t.Errorf("diff = %v", diff)
	}

	for _, tc := range []struct {
		err    error
		code   codes.Code
		reason string
	}{
		{connector.WithMessage(connector.ErrOntologyReleaseInvalid, "from must be a release"), codes.InvalidArgument, "invalid_request"},
		{connector.WithMessage(connector.ErrOntologyReleaseForbidden, "for an operator"), codes.PermissionDenied, "forbidden"},
		{connector.WithMessage(connector.ErrOntologyReleaseNotFound, "no release 9"), codes.NotFound, "release_not_found"},
		{connector.WithMessage(connector.ErrOntologyReleaseUnavailable, "no SQL Memory"), codes.Unavailable, "unavailable"},
		{errors.New("open /var/lib/secret.db: denied"), codes.Internal, "release_read_failed"},
	} {
		mc.relErr = tc.err
		_, lerr := client.ListOntologyReleases(context.Background(), &loomcyclepb.ListOntologyReleasesRequest{})
		_, derr := client.DiffOntologyReleases(context.Background(), &loomcyclepb.DiffOntologyReleasesRequest{From: 1})
		for _, err := range []error{lerr, derr} {
			code, reason, msg := reasonOf(err)
			if code != tc.code || reason != tc.reason {
				t.Errorf("%v: %s %q, want %s %q", tc.err, code, reason, tc.code, tc.reason)
			}
			if strings.Contains(msg, "/var/lib") {
				t.Errorf("an internal failure's text reached the caller: %s", msg)
			}
		}
	}
}

// RFC GQ P3.5 — the gRPC twins of GET /v1/_ontology/releases and
// /v1/_ontology/releases/diff.

// Who may call them: an admin and a tenant operator, as on HTTP. A release keeps
// the ontology's text after an edit or an erasure removed it, so a member, an
// isolated user and a token held to the run plane are refused at the scope gate.
func TestOntologyReleasesRPCs_AreOperatorOnly(t *testing.T) {
	as := func(scopes []string, r auth.Restrictions) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "s", Scopes: scopes, Restrictions: r})
	}
	for _, rpc := range []string{"ListOntologyReleases", "DiffOntologyReleases"} {
		method := grpcMethodPrefix + rpc
		if got := requiredScopeForRPC(method); got != auth.ScopeTenant {
			t.Errorf("%s: required scope = %q, want %q", rpc, got, auth.ScopeTenant)
		}
		for what, tc := range map[string]struct {
			ctx  context.Context
			want bool
		}{
			"an admin":           {as([]string{auth.ScopeAdmin}, auth.Restrictions{}), true},
			"a tenant operator":  {as([]string{auth.ScopeTenant}, auth.Restrictions{}), true},
			"a member":           {as([]string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, auth.Restrictions{}), false},
			"an isolated user":   {as([]string{auth.ScopeUser}, auth.Restrictions{}), false},
			"a restricted token": {as([]string{auth.ScopeTenant}, auth.Restrictions{Agents: []string{"judge"}}), false},
		} {
			if err := enforceScope(tc.ctx, method); (err == nil) != tc.want {
				t.Errorf("%s, %s: enforceScope = %v, want allowed=%v", rpc, what, err, tc.want)
			}
		}
	}
}
