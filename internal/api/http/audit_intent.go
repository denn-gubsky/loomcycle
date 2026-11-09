package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// Token, access-control and user lifecycle changes are recorded BEFORE they
// are made, and refused when the record cannot be written (RFC GD D3): a
// change the audit trail cannot show is not made. The record is written
// ahead rather than in the change's own transaction because the chain
// serializes on its head lock in its own transaction; it is marked
// ChangeStatus "intent", and a change that then fails gets a best-effort
// "failed" record after it.
//
// With no audit sink configured (a single-operator deployment) nothing is
// recorded and nothing is refused. Sign-in events stay best-effort: a
// failed append never stops a person signing in (SECURITY.md).

// errAuditUnavailable is a change refused because its audit record could not
// be written.
var errAuditUnavailable = errors.New("audit_unavailable: the audit record of this change could not be written, so the change was not made")

// auditUnavailableMessage is what a caller is told.
const auditUnavailableMessage = "the audit record of this change could not be written, so the change was not made; retry, or check the audit store"

// recordIntent writes ev as the intent of a change about to be made, with
// the caller as actor when ev names none. It returns the event as recorded
// (for recordFailed) and errAuditUnavailable when it could not be written.
func (s *Server) recordIntent(ctx context.Context, ev audit.Event) (audit.Event, error) {
	if ev.ActorSubject == "" && ev.ActorTenant == "" && ev.ActorTokenSuffix == "" {
		if p, ok := auth.PrincipalFromContext(ctx); ok {
			ev.ActorTenant, ev.ActorSubject, ev.ActorTokenSuffix = p.TenantID, p.Subject, p.TokenSuffix
		}
	}
	ev.ChangeStatus = audit.ChangeIntent
	if s.tokenAudit == nil {
		return ev, nil
	}
	if err := s.tokenAudit.Record(ev); err != nil {
		slog.ErrorContext(ctx, "audit: a change was refused because its audit record could not be written",
			"action", ev.Action, "tenant", ev.TargetTenant, "err", err)
		return ev, errAuditUnavailable
	}
	return ev, nil
}

// recordFailed records, best-effort, that a change whose intent was
// recorded did not happen.
func (s *Server) recordFailed(ctx context.Context, ev audit.Event) {
	if s.tokenAudit == nil {
		return
	}
	// A fresh time: the sink stamps it, after the intent's.
	ev.ChangeStatus, ev.TS = audit.ChangeFailed, time.Time{}
	if err := s.tokenAudit.Record(ev); err != nil {
		slog.WarnContext(ctx, "audit: recording a failed change failed", "action", ev.Action, "err", err)
	}
}

// writeAuditUnavailable answers a change refused for its audit record.
func writeAuditUnavailable(w http.ResponseWriter) {
	writeJSONError(w, http.StatusServiceUnavailable, "audit_unavailable", auditUnavailableMessage)
}

// aclAuditUnavailable is writeAuditUnavailable for the access-control
// operations every transport shares.
func aclAuditUnavailable() aclReply {
	return aclFail(http.StatusServiceUnavailable, "audit_unavailable", auditUnavailableMessage)
}
