package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// RFC GQ P5.7 (its transports in P5.6) — the gRPC twin of POST
// /v1/_ontology/rollback, and the plan's release ops.

// Who may call it: an admin and a tenant operator, as on HTTP. A rollback rewrites
// the tenant's vocabulary, so a member, an isolated user and a token held to the run
// plane are refused at the scope gate.
func TestApplyOntologyRollbackRPC_IsOperatorOnly(t *testing.T) {
	as := func(scopes []string, r auth.Restrictions) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "s", Scopes: scopes, Restrictions: r})
	}
	method := grpcMethodPrefix + "ApplyOntologyRollback"
	if got := requiredScopeForRPC(method); got != auth.ScopeTenant {
		t.Errorf("required scope = %q, want %q", got, auth.ScopeTenant)
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
			t.Errorf("%s: enforceScope = %v, want allowed=%v", what, err, tc.want)
		}
	}
}

// errorInfoOf is a status's ErrorInfo, nil when it carries none.
func errorInfoOf(err error) (codes.Code, *errdetails.ErrorInfo) {
	st := status.Convert(err)
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return st.Code(), info
		}
	}
	return st.Code(), nil
}

// The RPC hands the tenant, seq and fingerprint to the connector and renders every
// field of the report; a refusal carries HTTP's code as its ErrorInfo reason, with
// what HTTP's 409 body carries beside it in the ErrorInfo's metadata: the current
// fingerprint (plan_required, plan_stale) and the writes made (partially_applied, as
// HTTP's JSON). The plan's own refusals map as PlanOntology's; an internal failure
// hides its text.
func TestApplyOntologyRollbackRPC_DelegatesToTheConnector(t *testing.T) {
	rep := connector.OntologyRollbackReport{
		Release: 2, ReleaseContentVersion: "aaaa", ContentVersion: "bbbb", Matches: false, Risk: "stales",
		Applied:         []connector.OntologyRollbackStep{{Op: "rename", Entry: "c1", Name: "Memo"}, {Op: "add", Entry: "c2", Name: "Ledger", NewID: "c9"}},
		RenameJobs:      []connector.OntologyJobStatus{{ID: "j1", Kind: "class_rename", Entry: "c1", From: []string{"Note"}, To: "Memo", State: "done", ScopesDone: 3}},
		RollbackRelease: 3, Notes: []string{"n"}, Warnings: []string{"w"},
	}
	mc := &mockConnector{rollbackRes: connector.ApplyOntologyRollbackResult{Tenant: "acme", Rollback: rep}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	res, err := client.ApplyOntologyRollback(context.Background(), &loomcyclepb.ApplyOntologyRollbackRequest{Tenant: "globex", Seq: 2, PlanFingerprint: "fp"})
	if err != nil {
		t.Fatalf("ApplyOntologyRollback: %v", err)
	}
	if mc.rollbackReq != (connector.ApplyOntologyRollbackRequest{Tenant: "globex", Seq: 2, PlanFingerprint: "fp"}) {
		t.Errorf("asked the connector %+v", mc.rollbackReq)
	}
	r := res.GetRollback()
	got := connector.OntologyRollbackReport{Release: r.GetRelease(), ReleaseContentVersion: r.GetReleaseContentVersion(),
		ContentVersion: r.GetContentVersion(), Matches: r.GetMatches(), Risk: r.GetRisk(), RollbackRelease: r.GetRollbackRelease(),
		Notes: r.GetNotes(), Warnings: r.GetWarnings()}
	for _, a := range r.GetApplied() {
		got.Applied = append(got.Applied, connector.OntologyRollbackStep{Op: a.GetOp(), Entry: a.GetEntry(), Name: a.GetName(), NewID: a.GetNewId()})
	}
	for _, j := range r.GetRenameJobs() {
		got.RenameJobs = append(got.RenameJobs, *jobFromProto(j))
	}
	got.RenameJobs[0].Done = nil // proto3 reads an empty map back as empty, the fixture's is nil
	if res.GetTenant() != "acme" || !reflect.DeepEqual(got, rep) {
		t.Errorf("report = %+v\nwant %+v", got, rep)
	}

	applied, _ := json.Marshal([]connector.OntologyRollbackStep{{Op: "retire", Entry: "c1", Name: "Memo"}})
	for _, tc := range []struct {
		err    error
		code   codes.Code
		reason string
		meta   map[string]string
	}{
		{&connector.OntologyRollbackRefusal{Code: "plan_required", Message: "m", Fingerprint: "fp-now"}, codes.FailedPrecondition, "plan_required", map[string]string{"fingerprint": "fp-now"}},
		{&connector.OntologyRollbackRefusal{Code: "plan_stale", Message: "m", Fingerprint: "fp-now"}, codes.FailedPrecondition, "plan_stale", map[string]string{"fingerprint": "fp-now"}},
		{&connector.OntologyRollbackRefusal{Code: "partially_applied", Message: "m", Applied: []connector.OntologyRollbackStep{{Op: "retire", Entry: "c1", Name: "Memo"}}},
			codes.Aborted, "partially_applied", map[string]string{"applied": string(applied)}},
		{&connector.OntologyRollbackRefusal{Code: "rename_job_pending", Message: "m"}, codes.FailedPrecondition, "rename_job_pending", nil},
		{&connector.OntologyRollbackRefusal{Code: "entry_locked", Message: "m"}, codes.FailedPrecondition, "entry_locked", nil},
		{&connector.OntologyRollbackRefusal{Code: "invalid_request", Message: "m"}, codes.InvalidArgument, "invalid_request", nil},
		{connector.WithMessage(connector.ErrOntologyPlanForbidden, "for an operator"), codes.PermissionDenied, "forbidden", nil},
		{connector.WithMessage(connector.ErrOntologyPlanNotFound, "no release 9"), codes.NotFound, "entry_not_found", nil},
		{connector.WithMessage(connector.ErrOntologyPlanInvalid, "seq must be positive"), codes.InvalidArgument, "invalid_request", nil},
		{errors.New("open /var/lib/secret.db: denied"), codes.Internal, "ontology_write_failed", nil},
	} {
		mc.rollbackErr = tc.err
		_, err := client.ApplyOntologyRollback(context.Background(), &loomcyclepb.ApplyOntologyRollbackRequest{Seq: 2})
		code, info := errorInfoOf(err)
		if code != tc.code || info == nil || info.GetReason() != tc.reason || !reflect.DeepEqual(info.GetMetadata(), tc.meta) {
			t.Errorf("%v: %s %+v, want %s %s %v", tc.err, code, info, tc.code, tc.reason, tc.meta)
		}
		if strings.Contains(status.Convert(err).Message(), "/var/lib") {
			t.Errorf("an internal failure's text reached the caller: %s", status.Convert(err).Message())
		}
	}
}

// PlanOntology carries a change's seq (op release and add) to the connector, and the
// release a rollback's changes plan toward back.
func TestPlanOntology_ReleaseOpsRoundTrip(t *testing.T) {
	mc := &mockConnector{planRes: connector.PlanOntologyResult{Tenant: "acme", Changes: []connector.OntologyPlannedChange{
		{Op: "unretire", Entry: connector.OntologyPlanEntry{ChunkID: "c1"}, Release: 2},
	}}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()
	res, err := client.PlanOntology(context.Background(), &loomcyclepb.PlanOntologyRequest{Changes: []*loomcyclepb.OntologyChange{
		{Op: "release", Seq: 2},
		{Op: "add", Seq: 2, Entry: "c5"},
	}})
	if err != nil {
		t.Fatalf("PlanOntology: %v", err)
	}
	want := []connector.OntologyChange{{Op: "release", Seq: 2}, {Op: "add", Seq: 2, Entry: "c5"}}
	if !reflect.DeepEqual(mc.planReq.Changes, want) {
		t.Errorf("the connector was asked %+v, want %+v", mc.planReq.Changes, want)
	}
	if got := res.GetChanges()[0].GetRelease(); got != 2 {
		t.Errorf("the planned change's release = %d, want 2", got)
	}
}
