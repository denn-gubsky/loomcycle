package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tenanttelemetry"
)

// A tenant's own telemetry export: where the tenant's traces and its own
// metric series go beside the operator's export. GET/PUT/DELETE /v1/_telemetry and POST
// /v1/_telemetry:test, the gRPC TenantTelemetry RPC and the MCP telemetry
// tool all call TenantTelemetry below, which holds who may, the rules (in
// internal/tenanttelemetry) and the audit.
//
// Who: an admin for any tenant (?tenant= / the tenant field), a tenant
// operator for its own; never a member (memberpath.CarvedOut), and never a
// token held to the run plane. Sending a tenant's traces somewhere is the
// operator's decision, as is where its credentials go.

// isTenantTelemetryPath reports whether path is a tenant telemetry route. Its
// scope is substrate:tenant (requiredScopeFor): the tenant operator's for its
// own tenant, an admin's for any, the connector confining the tenant; the
// routes are carved out of the member path.
func isTenantTelemetryPath(path string) bool {
	return path == "/v1/_telemetry" || path == "/v1/_telemetry:test"
}

// SetTenantTelemetry turns on the tenant telemetry surfaces, backed by m. Nil
// (the default) leaves every route answering 404 feature_disabled.
func (s *Server) SetTenantTelemetry(m *tenanttelemetry.Manager) { s.tenantTelemetry = m }

// tenantTelemetryOperator reports whether the caller may touch tenant
// telemetry at all. Asked here as well as at each transport's gate, because
// this is the one function all of them call.
func tenantTelemetryOperator(ctx context.Context) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return true // open mode
	}
	return auth.HasScope(p.Scopes, auth.ScopeTenant) && !p.Restrictions.RunPlaneOnly()
}

// tenantTelemetryTenant is the tenant a call is about: an admin's named one or
// its own; a tenant operator's own, and naming another is refused rather than
// silently answered for its own.
func tenantTelemetryTenant(ctx context.Context, wire string) (string, error) {
	wire = strings.TrimSpace(wire)
	tenant := wire
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		admin := auth.HasScope(p.Scopes, auth.ScopeAdmin)
		switch {
		case wire == "" || (!admin && wire == p.TenantID):
			tenant = p.TenantID
		case !admin:
			return "", connector.WithMessage(connector.ErrTenantTelemetryForbidden,
				"a tenant operator manages its own tenant's telemetry only")
		}
	}
	if tenant == "" {
		return "", connector.WithMessage(connector.ErrTenantTelemetryInvalid,
			"name the tenant: a telemetry policy belongs to one tenant, and work of no tenant is never exported to one")
	}
	return tenant, nil
}

// TenantTelemetry implements connector.Connector.
func (s *Server) TenantTelemetry(ctx context.Context, req connector.TenantTelemetryRequest) (connector.TenantTelemetryResult, error) {
	none := connector.TenantTelemetryResult{}
	m := s.tenantTelemetry
	if m == nil {
		return none, connector.WithMessage(connector.ErrTenantTelemetryDisabled,
			"tenant telemetry export is not enabled on this deployment")
	}
	if !tenantTelemetryOperator(ctx) {
		return none, connector.WithMessage(connector.ErrTenantTelemetryForbidden,
			"tenant telemetry is for an admin or a tenant operator")
	}
	switch req.Op {
	case "get", "set", "delete", "test":
	default:
		return none, connector.WithMessage(connector.ErrTenantTelemetryInvalid, "op must be one of: get, set, delete, test")
	}
	tenant, err := tenantTelemetryTenant(ctx, req.Tenant)
	if err != nil {
		return none, err
	}
	// A write's own question, asked here as well as at each transport's gate
	// (PUT, DELETE and POST :test ask it), so a gate that read the op wrongly
	// cannot skip it. A get is a read, which every gate asks.
	if req.Op != "get" {
		if err := s.connectorAsk(ctx, authz.Surface{Action: "manage", Type: "runtime", Name: "telemetry"}); err != nil {
			return none, err
		}
	}
	switch req.Op {
	case "set":
		return s.tenantTelemetrySet(ctx, m, tenant, req.Policy)
	case "delete":
		had, err := m.Delete(ctx, tenant)
		if err != nil {
			return none, err
		}
		if had {
			// A reduction: written whatever the audit log says, and recorded
			// when it can be.
			s.recordTenantTelemetryAudit(ctx, "tenant_telemetry_delete", tenant, "")
			slog.Info("telemetry: tenant export policy deleted", "by", principalSubject(ctx), "tenant", tenant)
		}
		return connector.TenantTelemetryResult{Tenant: tenant, Deleted: had, Status: s.tenantTelemetryStatus(m, tenant)}, nil
	case "test":
		return s.tenantTelemetryTest(ctx, m, tenant)
	}
	return s.tenantTelemetryView(ctx, m, tenant)
}

func (s *Server) tenantTelemetrySet(ctx context.Context, m *tenanttelemetry.Manager, tenant string, in *connector.TenantTelemetryPolicyInput) (connector.TenantTelemetryResult, error) {
	none := connector.TenantTelemetryResult{}
	if in == nil {
		return none, connector.WithMessage(connector.ErrTenantTelemetryInvalid, "set needs a policy")
	}
	row, err := m.Check(ctx, tenant, tenanttelemetry.PolicyInput{
		Endpoint: in.Endpoint, Protocol: in.Protocol, HeadersCredential: in.HeadersCredential,
		Signals: in.Signals, SamplerRatio: in.SamplerRatio, ContentCapture: in.ContentCapture,
	})
	if err != nil {
		return none, tenantTelemetryErr(err)
	}
	// Audited before it takes effect, and refused if it cannot be: a new
	// place a tenant's traces are sent to is what an audit log is for.
	if !s.recordTenantTelemetryAudit(ctx, "tenant_telemetry_set", tenant, row.Endpoint) {
		return none, connector.WithMessage(connector.ErrTenantTelemetryAuditUnavailable,
			"the policy was not set: its audit record could not be written")
	}
	row, err = m.Store(ctx, row, principalSubject(ctx))
	if err != nil {
		return none, err
	}
	slog.Info("telemetry: tenant export policy set", "by", row.UpdatedBy, "tenant", tenant, "endpoint", row.Endpoint, "protocol", row.Protocol)
	return connector.TenantTelemetryResult{Tenant: tenant, Policy: tenantTelemetryPolicyView(&row), Status: s.tenantTelemetryStatus(m, tenant)}, nil
}

func (s *Server) tenantTelemetryTest(ctx context.Context, m *tenanttelemetry.Manager, tenant string) (connector.TenantTelemetryResult, error) {
	res, err := s.tenantTelemetryView(ctx, m, tenant)
	if err != nil {
		return connector.TenantTelemetryResult{}, err
	}
	if res.Policy == nil {
		return connector.TenantTelemetryResult{}, connector.WithMessage(connector.ErrTenantTelemetryInvalid,
			"this tenant has no telemetry policy to test: set one first")
	}
	start := time.Now()
	terr := m.Test(ctx, tenant)
	if errors.Is(terr, tenanttelemetry.ErrRateLimited) {
		return connector.TenantTelemetryResult{}, connector.WithMessage(connector.ErrTenantTelemetryRateLimited,
			"a test was run moments ago: wait "+tenanttelemetry.TestInterval.String()+" between tests")
	}
	res.Test = &connector.TenantTelemetryTest{OK: terr == nil, DurationMS: time.Since(start).Milliseconds()}
	if terr != nil {
		res.Test.Error = tenanttelemetry.PublicError(tenant, terr)
	}
	return res, nil
}

func (s *Server) tenantTelemetryView(ctx context.Context, m *tenanttelemetry.Manager, tenant string) (connector.TenantTelemetryResult, error) {
	row, err := m.Get(ctx, tenant)
	if err != nil {
		return connector.TenantTelemetryResult{}, err
	}
	return connector.TenantTelemetryResult{Tenant: tenant, Policy: tenantTelemetryPolicyView(row), Status: s.tenantTelemetryStatus(m, tenant)}, nil
}

func tenantTelemetryPolicyView(row *store.TenantTelemetryPolicyRow) *connector.TenantTelemetryPolicy {
	if row == nil {
		return nil
	}
	out := &connector.TenantTelemetryPolicy{
		Endpoint: row.Endpoint, Protocol: row.Protocol, HeadersCredential: row.HeadersCredential,
		Signals: append([]string{}, row.Signals...), SamplerRatio: row.SamplerRatio, UpdatedBy: row.UpdatedBy,
	}
	if !row.UpdatedAt.IsZero() {
		out.UpdatedAt = row.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// tenantTelemetryStatus is the tenant's export on this replica.
func (s *Server) tenantTelemetryStatus(m *tenanttelemetry.Manager, tenant string) *connector.TenantTelemetryStatus {
	st := m.Status(tenant)
	out := &connector.TenantTelemetryStatus{
		Replica: s.replicaID, Exporting: st.Configured,
		ExportedSpans: st.Exported, DroppedSpans: st.Dropped, ExportFailures: st.ExportFailures,
		LastError: st.LastError,
	}
	if !st.LastSuccessAt.IsZero() {
		out.LastSuccessAt = st.LastSuccessAt.UTC().Format(time.RFC3339)
	}
	if !st.LastErrorAt.IsZero() {
		out.LastErrorAt = st.LastErrorAt.UTC().Format(time.RFC3339)
	}
	if st.ApplyError != "" {
		out.LastError = st.ApplyError
	}
	return out
}

// tenantTelemetryErr maps a policy refusal to the connector's sentinel; any
// other error (a store fault) passes through.
func tenantTelemetryErr(err error) error {
	if errors.Is(err, tenanttelemetry.ErrInvalid) {
		return connector.WithMessage(connector.ErrTenantTelemetryInvalid, tenanttelemetry.PublicError("", err))
	}
	return err
}

// recordTenantTelemetryAudit writes the audit record of a policy set or
// deleted, and reports whether it was written. With no audit log configured
// there is nothing to write, and the process log line is the record.
func (s *Server) recordTenantTelemetryAudit(ctx context.Context, action, tenant, endpoint string) bool {
	if s.tokenAudit == nil {
		return true
	}
	ev := audit.Event{
		Action: action, TargetTenant: tenant, TargetName: endpoint,
		ActorTenant: tenantFromCtx(ctx), ActorSubject: principalSubject(ctx),
	}
	if err := s.tokenAudit.Record(ev); err != nil {
		slog.Error("telemetry: tenant export audit write failed", "err", err)
		return false
	}
	return true
}

// restoredTenantTelemetryValidator checks a restored policy (a
// snapshot.TenantTelemetryEntry as JSON) against this host's operator rules.
// With the feature off here there are no rules to hold it to, and it is not
// restored.
func (s *Server) restoredTenantTelemetryValidator(body json.RawMessage) error {
	m := s.tenantTelemetry
	if m == nil {
		return errors.New("tenant telemetry export is not enabled on this host")
	}
	var e snapshot.TenantTelemetryEntry
	if err := json.Unmarshal(body, &e); err != nil {
		return err
	}
	_, err := m.ValidStored(store.TenantTelemetryPolicyRow{
		TenantID: e.TenantID, Endpoint: e.Endpoint, Protocol: e.Protocol, HeadersCredential: e.HeadersCredential,
		Signals: e.Signals, SamplerRatio: e.SamplerRatio,
	})
	if err != nil {
		return errors.New(tenanttelemetry.PublicError(e.TenantID, err))
	}
	return nil
}

// writeTenantTelemetryError is the HTTP form of a TenantTelemetry error.
func writeTenantTelemetryError(w http.ResponseWriter, err error) {
	var denied *errACLDenied
	switch {
	case errors.As(err, &denied) && denied.unavailable:
		writeJSONError(w, http.StatusServiceUnavailable, "acl_unavailable", err.Error())
	case errors.As(err, &denied):
		writeACLDenied(w, denied.surface, denied.decision)
	case errors.Is(err, connector.ErrTenantTelemetryDisabled):
		writeJSONError(w, http.StatusNotFound, "feature_disabled", err.Error())
	case errors.Is(err, connector.ErrTenantTelemetryInvalid):
		writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, connector.ErrTenantTelemetryForbidden):
		writeJSONError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, connector.ErrTenantTelemetryAuditUnavailable):
		writeJSONError(w, http.StatusServiceUnavailable, "audit_unavailable", err.Error())
	case errors.Is(err, connector.ErrTenantTelemetryRateLimited):
		writeJSONError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
	default:
		slog.Error("telemetry: tenant export request failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal", "the tenant telemetry request failed")
	}
}

// tenantTelemetryPutRequest is the PUT body: the policy, and (an admin's) the
// tenant it is for.
type tenantTelemetryPutRequest struct {
	Tenant string `json:"tenant,omitempty"`
	connector.TenantTelemetryPolicyInput
}

func (s *Server) serveTenantTelemetry(w http.ResponseWriter, r *http.Request, req connector.TenantTelemetryRequest) {
	res, err := s.TenantTelemetry(r.Context(), req)
	if err != nil {
		writeTenantTelemetryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleTenantTelemetryGet serves GET /v1/_telemetry?tenant=.
func (s *Server) handleTenantTelemetryGet(w http.ResponseWriter, r *http.Request) {
	s.serveTenantTelemetry(w, r, connector.TenantTelemetryRequest{Op: "get", Tenant: r.URL.Query().Get("tenant")})
}

// handleTenantTelemetryPut serves PUT /v1/_telemetry.
func (s *Server) handleTenantTelemetryPut(w http.ResponseWriter, r *http.Request) {
	var body tenantTelemetryPutRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	tenant := body.Tenant
	if tenant == "" {
		tenant = r.URL.Query().Get("tenant")
	}
	s.serveTenantTelemetry(w, r, connector.TenantTelemetryRequest{Op: "set", Tenant: tenant, Policy: &body.TenantTelemetryPolicyInput})
}

// handleTenantTelemetryDelete serves DELETE /v1/_telemetry?tenant=.
func (s *Server) handleTenantTelemetryDelete(w http.ResponseWriter, r *http.Request) {
	s.serveTenantTelemetry(w, r, connector.TenantTelemetryRequest{Op: "delete", Tenant: r.URL.Query().Get("tenant")})
}

// handleTenantTelemetryTest serves POST /v1/_telemetry:test?tenant=: one
// synthetic span to the stored destination, answered synchronously.
func (s *Server) handleTenantTelemetryTest(w http.ResponseWriter, r *http.Request) {
	s.serveTenantTelemetry(w, r, connector.TenantTelemetryRequest{Op: "test", Tenant: r.URL.Query().Get("tenant")})
}
