package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// The RPC hands its request to the connector, where the rules and the audit
// live, and renders what comes back; each refusal keeps its meaning.
func TestTenantTelemetry_DelegatesToTheConnector(t *testing.T) {
	mc := &mockConnector{telemetryRes: connector.TenantTelemetryResult{
		Tenant: "acme",
		Policy: &connector.TenantTelemetryPolicy{Endpoint: "https://otel.acme.example", Protocol: "grpc", HeadersCredential: "otel",
			Signals: []string{"traces"}, SamplerRatio: 0.5, UpdatedAt: "2026-11-11T10:00:00Z", UpdatedBy: "ops",
			ContentCapture: "on", ContentMaxChars: 2048},
		Status: &connector.TenantTelemetryStatus{Replica: "r1", Exporting: true, ExportedSpans: 7, DroppedSpans: 2, ExportFailures: 1,
			LastSuccessAt: "2026-11-11T10:01:00Z", LastError: "401", LastErrorAt: "2026-11-11T10:00:30Z"},
		Test: &connector.TenantTelemetryTest{OK: true, DurationMS: 12},
	}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.TenantTelemetry(context.Background(), &loomcyclepb.TenantTelemetryRequest{
		Op: "set", Tenant: "globex",
		Policy: &loomcyclepb.TenantTelemetryPolicyInput{Endpoint: "https://otel.acme.example", Protocol: "grpc",
			HeadersCredential: "otel", Signals: []string{"traces"}, SamplerRatio: proto.Float64(0), ContentCapture: "on",
			ContentMaxChars: proto.Int32(1024)},
	})
	if err != nil {
		t.Fatalf("TenantTelemetry: %v", err)
	}
	got := mc.telemetryReq
	if got.Op != "set" || got.Tenant != "globex" || got.Policy == nil || got.Policy.Endpoint != "https://otel.acme.example" ||
		got.Policy.SamplerRatio == nil || *got.Policy.SamplerRatio != 0 || got.Policy.ContentCapture != "on" || got.Policy.HeadersCredential != "otel" ||
		got.Policy.ContentMaxChars == nil || *got.Policy.ContentMaxChars != 1024 {
		t.Errorf("connector got %+v (policy %+v)", got, got.Policy)
	}
	if resp.GetTenant() != "acme" || resp.GetPolicy().GetSamplerRatio() != 0.5 || resp.GetStatus().GetExportedSpans() != 7 ||
		resp.GetStatus().GetReplica() != "r1" || !resp.GetTest().GetOk() || resp.GetTest().GetDurationMs() != 12 ||
		resp.GetPolicy().GetContentCapture() != "on" || resp.GetPolicy().GetContentMaxChars() != 2048 {
		t.Errorf("response = %+v", resp)
	}
	// An unset ratio stays unset: it is not a request for 0.
	if _, err := client.TenantTelemetry(context.Background(), &loomcyclepb.TenantTelemetryRequest{Op: "set",
		Policy: &loomcyclepb.TenantTelemetryPolicyInput{Endpoint: "https://x.example"}}); err != nil {
		t.Fatal(err)
	}
	if mc.telemetryReq.Policy.SamplerRatio != nil {
		t.Errorf("an unset ratio reached the connector as %v", *mc.telemetryReq.Policy.SamplerRatio)
	}
	if mc.telemetryReq.Policy.ContentMaxChars != nil {
		t.Errorf("an unset content_max_chars reached the connector as %v", *mc.telemetryReq.Policy.ContentMaxChars)
	}

	for sentinel, want := range map[error]codes.Code{
		connector.ErrTenantTelemetryDisabled:         codes.Unimplemented,
		connector.ErrTenantTelemetryInvalid:          codes.InvalidArgument,
		connector.ErrTenantTelemetryForbidden:        codes.PermissionDenied,
		connector.ErrTenantTelemetryAuditUnavailable: codes.Unavailable,
		connector.ErrTenantTelemetryRateLimited:      codes.ResourceExhausted,
	} {
		mc.telemetryErr = connector.WithMessage(sentinel, "why")
		_, err := client.TenantTelemetry(context.Background(), &loomcyclepb.TenantTelemetryRequest{Op: "get"})
		if status.Code(err) != want {
			t.Errorf("%v maps to %v, want %v", sentinel, status.Code(err), want)
		}
	}
}

// Who may call it on this transport, op by op: an admin and a tenant operator.
// Its HTTP routes are carved out of the member path, so a member is refused
// every op here too; a token limited to certain agents is refused whatever its
// scopes.
func TestTenantTelemetry_IsOperatorOnlyOnEveryOp(t *testing.T) {
	if got := requiredScopeForRPC(grpcMethodPrefix + "TenantTelemetry"); got != auth.ScopeTenant {
		t.Fatalf("required scope = %q, want %q", got, auth.ScopeTenant)
	}
	for what, tc := range map[string]struct {
		p    auth.Principal
		want bool
	}{
		"an admin":           {auth.Principal{TenantID: "acme", Subject: "s", Scopes: []string{auth.ScopeAdmin}}, true},
		"a tenant operator":  {auth.Principal{TenantID: "acme", Subject: "s", Scopes: []string{auth.ScopeTenant}}, true},
		"a member":           {auth.Principal{TenantID: "acme", Subject: "s", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}}, false},
		"an isolated user":   {auth.Principal{TenantID: "acme", Subject: "s", Scopes: []string{auth.ScopeUser}}, false},
		"a restricted token": {auth.Principal{TenantID: "acme", Subject: "s", Scopes: []string{auth.ScopeTenant}, Restrictions: auth.Restrictions{Agents: []string{"judge"}}}, false},
	} {
		for _, op := range []string{"get", "set", "delete", "test"} {
			if got := RPCAdmits(tc.p, "TenantTelemetry", op); got != tc.want {
				t.Errorf("%s, op %s: admitted=%v, want %v", what, op, got, tc.want)
			}
		}
	}
}
