package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// Token, access-control and user lifecycle changes are recorded BEFORE they
// are made (RFC GD D3). The record is written ahead rather than in the
// change's own transaction because the chain serializes on its head lock in
// its own transaction; it is marked ChangeStatus "intent", and a change that
// then fails gets a best-effort "failed" record after it.
//
// What happens when the record cannot be written depends on the change
// (audit.ChangeKindOf): one that grants or widens access — or may — is
// refused, so the trail never lacks a grant; one that only takes access away
// (a token retired, a user disabled, deleted or deprovisioned) goes ahead,
// so an operator can still revoke a compromised credential during an audit
// outage, and its record is retried after the change, counted
// (loomcycle_audit_unrecorded_total) and logged at error.
//
// With no audit sink configured (a single-operator deployment) nothing is
// recorded and nothing is refused. Sign-in events stay best-effort: a
// failed append never stops a person signing in (SECURITY.md).

// errAuditUnavailable is a change refused because its audit record could not
// be written.
var errAuditUnavailable = errors.New("audit_unavailable: the audit record of this change could not be written, so the change was not made")

// auditUnavailableMessage is what a caller is told.
const auditUnavailableMessage = "the audit record of this change could not be written, so the change was not made; retry, or check the audit store"

// changeIntent is a change recorded ahead: the event, and whether its record
// is still owed (a reducing change whose intent could not be written).
type changeIntent struct {
	ev    audit.Event
	owed  bool
	noted bool // a sink is configured: there is something to record
}

// recordIntent writes ev as the intent of a change about to be made, with
// the caller as actor when ev names none. errAuditUnavailable means the
// change must not be made; a reducing change is never refused.
func (s *Server) recordIntent(ctx context.Context, ev audit.Event) (changeIntent, error) {
	if ev.ActorSubject == "" && ev.ActorTenant == "" && ev.ActorTokenSuffix == "" {
		if p, ok := auth.PrincipalFromContext(ctx); ok {
			ev.ActorTenant, ev.ActorSubject, ev.ActorTokenSuffix = p.TenantID, p.Subject, p.TokenSuffix
		}
	}
	ev.ChangeStatus = audit.ChangeIntent
	in := changeIntent{ev: ev, noted: s.tokenAudit != nil}
	if !in.noted {
		return in, nil
	}
	err := s.tokenAudit.Record(ev)
	if err == nil {
		return in, nil
	}
	kind, known := audit.ChangeKindOf(ev.Action)
	if !known {
		slog.ErrorContext(ctx, "audit: an unclassified change was recorded ahead; it is treated as granting", "action", ev.Action)
	}
	if known && kind == audit.ChangeReduces {
		s.countUnrecorded(ev.Action)
		slog.ErrorContext(ctx, "audit: a change that takes access away goes ahead although its audit record could not be written; the record is retried after it",
			"action", ev.Action, "tenant", ev.TargetTenant, "subject", ev.TargetSubject, "err", err)
		in.owed = true
		return in, nil
	}
	slog.ErrorContext(ctx, "audit: a change was refused because its audit record could not be written",
		"action", ev.Action, "tenant", ev.TargetTenant, "err", err)
	return in, errAuditUnavailable
}

// recordDone is a change's epilogue: a record still owed is written now,
// best-effort (the change is made; a second miss is logged).
func (s *Server) recordDone(ctx context.Context, in changeIntent) {
	if !in.owed || s.tokenAudit == nil {
		return
	}
	ev := in.ev
	ev.ChangeStatus, ev.TS = "", time.Time{}
	if err := s.tokenAudit.Record(ev); err != nil {
		slog.ErrorContext(ctx, "audit: a change was made and its audit record is missing", "action", ev.Action,
			"tenant", ev.TargetTenant, "subject", ev.TargetSubject, "err", err)
	}
}

// recordFailed records, best-effort, that a change recorded ahead did not
// happen (or did not finish).
func (s *Server) recordFailed(ctx context.Context, in changeIntent) {
	if !in.noted || s.tokenAudit == nil {
		return
	}
	// A fresh time: the sink stamps it, after the intent's.
	ev := in.ev
	ev.ChangeStatus, ev.TS = audit.ChangeFailed, time.Time{}
	if err := s.tokenAudit.Record(ev); err != nil {
		slog.WarnContext(ctx, "audit: recording a failed change failed", "action", ev.Action, "err", err)
	}
}

// recordStopped settles a change that returned an error: "failed" when
// nothing was applied, else "partial" with what was (applied: kind → rows,
// zero kinds left out), so the trail never calls a half-made grant failed.
func (s *Server) recordStopped(ctx context.Context, in changeIntent, applied map[string]int) {
	done := map[string]int{}
	for k, n := range applied {
		if n > 0 {
			done[k] = n
		}
	}
	if len(done) == 0 {
		s.recordFailed(ctx, in)
		return
	}
	if !in.noted || s.tokenAudit == nil {
		return
	}
	ev := in.ev
	ev.ChangeStatus, ev.ChangeCounts, ev.TS = audit.ChangePartial, done, time.Time{}
	if err := s.tokenAudit.Record(ev); err != nil {
		slog.ErrorContext(ctx, "audit: recording a change that stopped part-way failed", "action", ev.Action, "applied", done, "err", err)
	}
}

// CountUnrecordedAudit counts a change that took access away with no audit
// record written ahead (loomcycle_audit_unrecorded_total). The operator-token
// tool reports its own through it.
func (s *Server) CountUnrecordedAudit(action string) { s.countUnrecorded(action) }

func (s *Server) countUnrecorded(action string) {
	s.auditUnrecordedMu.Lock()
	defer s.auditUnrecordedMu.Unlock()
	if s.auditUnrecorded == nil {
		s.auditUnrecorded = map[string]int64{}
	}
	// Only classified actions become label values, so the series stay few.
	if _, ok := audit.ChangeKindOf(action); !ok {
		action = "other"
	}
	s.auditUnrecorded[action]++
}

// writeAuditUnrecordedMetric writes loomcycle_audit_unrecorded_total.
func (s *Server) writeAuditUnrecordedMetric(w io.Writer, replicaLabels string) {
	s.auditUnrecordedMu.Lock()
	counts := make(map[string]int64, len(s.auditUnrecorded))
	for a, n := range s.auditUnrecorded {
		counts[a] = n
	}
	s.auditUnrecordedMu.Unlock()
	fmt.Fprintln(w, "# HELP loomcycle_audit_unrecorded_total Changes that took access away (a token retired, a user disabled or deleted) although their audit record could not be written ahead; non-zero means the audit store failed.")
	fmt.Fprintln(w, "# TYPE loomcycle_audit_unrecorded_total counter")
	actions := make([]string, 0, len(counts))
	for a := range counts {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	for _, a := range actions {
		fmt.Fprintf(w, "loomcycle_audit_unrecorded_total%s %d\n", mergeLabels(replicaLabels, map[string]string{"action": a}), counts[a])
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
