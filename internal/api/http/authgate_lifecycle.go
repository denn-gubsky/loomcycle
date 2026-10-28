package http

// authgate_lifecycle.go — RFC FA §3.7c, the removal policy carried out.
//
// The decisions (what is due, in which order) are internal/authgate/lifecycle;
// this file does them. A removal is three stages that never skip:
//
//	disable — at once, when the removal starts: users.status = disabled, every
//	          Web UI session ended, every delegated token retired. Nothing is
//	          deleted; the person can be re-enabled.
//	unbind  — at unbind_due_at: memberships in gate-managed groups (a group
//	          with an external_id) removed and the user's own ACL bindings
//	          retired. The login identities stay, so a returning person is
//	          recognised.
//	erase   — at erase_due_at, only when the policy set erase_after (default
//	          never: an operator runs POST /v1/_erasure): the RFC CF subject
//	          erasure, report then execute, with its erase_intent/erase_result
//	          audit pair.
//
// Every step is audited. A reactivated user re-enters at the §3.7a defaults,
// not at their old groups. The sweeper is cluster-singleton (an advisory
// lock), and every step is idempotent, so a crash between steps is finished
// by the next tick.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/authgate/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// lifecycleActor is who the sweeper's own steps are recorded as.
const lifecycleActor = "system:auth-lifecycle"

// lifecycleBatch bounds one sweep's work per kind; lifecycleBatches bounds
// how many batches one tick takes before leaving the rest to the next.
const (
	lifecycleBatch   = 100
	lifecycleBatches = 10
)

// removalPolicy is the deployment's removal timing as a lifecycle.Policy.
func (s *Server) removalPolicy() lifecycle.Policy {
	rp := s.cfg().ResolvedAuth.RemovalPolicy()
	return lifecycle.Policy{UnbindAfter: rp.UnbindAfter, EraseAfter: rp.EraseAfter}
}

// recordLifecycleEvent writes one removal-policy audit record and its log line.
func (s *Server) recordLifecycleEvent(action, tenant, subject, gate, reason, by string) {
	slog.Info("auth: "+action, "tenant", tenant, "subject", subject, "gate", gate, "reason", reason, "by", by)
	if s.tokenAudit == nil {
		return
	}
	if err := s.tokenAudit.Record(audit.Event{
		Action: action, ActorSubject: by, TargetTenant: tenant, TargetSubject: subject,
		AuthGate: gate, AuthReason: reason,
	}); err != nil {
		slog.Error("auth: audit write failed", "err", err)
	}
}

// startRemoval disables (tenant, subject) and records the removal that will
// unbind and maybe erase it later. The disable comes first: a removal row
// that outlived a failed disable would later unbind a user nobody disabled.
// An existing removal is kept as it is (a repeated revoke never postpones its
// stages); the disable is re-applied either way, so a retried call finishes
// what a failed one began. *store.ErrNotFound when the user does not exist.
func (s *Server) startRemoval(ctx context.Context, tenant, subject, gate, reason, by string) error {
	return s.startRemovalWith(ctx, s.removalPolicy(), tenant, subject, gate, reason, by)
}

// startRemovalWith is startRemoval on a given removal timing (a gate's own
// on_removed).
func (s *Server) startRemovalWith(ctx context.Context, policy lifecycle.Policy, tenant, subject, gate, reason, by string) error {
	u, err := s.store.UserGet(ctx, tenant, subject)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if u.Status != "disabled" {
		disabled := "disabled"
		if err := s.store.UserUpdate(ctx, tenant, subject, store.UserPatch{Status: &disabled}); err != nil {
			return fmt.Errorf("disable: %w", err)
		}
	}
	retired, err := s.retireDelegatedUserTokens(ctx, tenant, subject)
	for _, row := range retired {
		s.recordTokenAudit(ctx, "retire", row)
	}
	if err != nil {
		return fmt.Errorf("retire tokens: %w", err)
	}
	if _, err := s.store.UISessionRevokeSubject(ctx, tenant, subject, now); err != nil {
		return fmt.Errorf("end sessions: %w", err)
	}
	started, err := s.store.AuthRemovalStart(ctx, removalRow(policy.Start(now), tenant, subject, gate, reason, by))
	if err != nil {
		return fmt.Errorf("record removal: %w", err)
	}
	if started || u.Status != "disabled" {
		s.recordLifecycleEvent("auth_user_disabled", tenant, subject, gate, reason, by)
	}
	return nil
}

// unbindSubject removes the user's memberships in gate-managed groups and
// retires its own ACL bindings. Local groups (no external_id) are left: an
// operator put the user there, and the user is disabled anyway. It reports
// whether anything changed.
func (s *Server) unbindSubject(ctx context.Context, tenant, subject string, allGroups bool) (bool, error) {
	changed := false
	groups, err := s.store.GroupsOfSubject(ctx, tenant, subject)
	if err != nil {
		return false, err
	}
	for _, name := range groups {
		if !allGroups {
			g, err := s.store.GroupGet(ctx, tenant, name)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return changed, err
			}
			if g.ExternalID == "" {
				continue
			}
		}
		ok, err := s.store.GroupMemberRemove(ctx, tenant, name, store.GroupMemberUser, subject)
		if err != nil {
			return changed, err
		}
		changed = changed || ok
	}
	bindings, err := s.store.AclBindingsFor(ctx, tenant, subject, nil, "")
	if err != nil {
		return changed, err
	}
	for _, b := range bindings {
		if b.SubjectKind != store.GroupMemberUser || b.SubjectID != subject || !b.RetiredAt.IsZero() {
			continue
		}
		ok, err := s.store.AclBindingRetire(ctx, tenant, b.BindingID)
		if err != nil {
			return changed, err
		}
		changed = changed || ok
	}
	return changed, nil
}

// AuthLifecycleSweep runs one tick of the removal policy: users dormant past
// dormant_after are removed, and every removal with a stage due advances by
// exactly one stage. Errors are logged per user and retried next tick.
func (s *Server) AuthLifecycleSweep(ctx context.Context, now time.Time) {
	if s.store == nil {
		return
	}
	// The external gates' replay cache: a jti whose assertion can no longer
	// verify has nothing left to guard.
	if n, err := s.store.AuthAssertionDeleteExpired(ctx, now); err != nil {
		slog.Error("auth-lifecycle sweep: assertion replay cache", "err", err)
	} else if n > 0 {
		slog.Info("auth-lifecycle sweep: dropped used assertion ids", "rows", n)
	}
	policy := s.cfg().ResolvedAuth.RemovalPolicy()
	if policy.DormantAfter > 0 {
		for i := 0; i < lifecycleBatches; i++ {
			users, err := s.store.AuthDormantCandidates(ctx, now.Add(-policy.DormantAfter), lifecycleBatch)
			if err != nil {
				slog.Error("auth-lifecycle sweep: dormant users", "err", err)
				break
			}
			for _, u := range users {
				if err := s.startRemoval(ctx, u.TenantID, u.Subject, "", store.RemovalReasonDormant, lifecycleActor); err != nil {
					slog.Error("auth-lifecycle sweep: removing a dormant user", "tenant", u.TenantID, "subject", u.Subject, "err", err)
				}
			}
			if len(users) < lifecycleBatch {
				break
			}
		}
	}
	for i := 0; i < lifecycleBatches; i++ {
		due, err := s.store.AuthRemovalDue(ctx, now, lifecycleBatch)
		if err != nil {
			slog.Error("auth-lifecycle sweep: due removals", "err", err)
			return
		}
		progressed := 0
		for _, r := range due {
			ok, err := s.advanceRemoval(ctx, r, now)
			if err != nil {
				slog.Error("auth-lifecycle sweep: advancing a removal", "tenant", r.TenantID, "subject", r.Subject, "err", err)
			}
			if ok {
				progressed++
			}
		}
		// A batch that made no progress (every row failing) would come back
		// identical; stop rather than spin on it.
		if len(due) < lifecycleBatch || progressed == 0 {
			return
		}
	}
}

// advanceRemoval performs r's one due stage. A user an operator re-enabled
// (or deleted) meanwhile ends the removal instead: nothing is unbound or
// erased under a user who is no longer disabled. Reports whether the row
// changed.
func (s *Server) advanceRemoval(ctx context.Context, r store.AuthRemovalRow, now time.Time) (bool, error) {
	u, err := s.store.UserGet(ctx, r.TenantID, r.Subject)
	if err != nil && !isNotFound(err) {
		return false, err
	}
	if err != nil || u.Status != "disabled" {
		if _, err := s.store.AuthRemovalDelete(ctx, r.TenantID, r.Subject); err != nil {
			return false, err
		}
		s.recordLifecycleEvent("auth_removal_cancelled", r.TenantID, r.Subject, r.Gate, "user_not_disabled", lifecycleActor)
		return true, nil
	}
	switch lifecycle.Next(removalTiming(r), now) {
	case lifecycle.StageUnbind:
		changed, err := s.unbindSubject(ctx, r.TenantID, r.Subject, false)
		if changed {
			s.aclChanged(ctx, r.TenantID, "removal_unbound", r.Subject)
		}
		if err != nil {
			return false, err
		}
		ok, err := s.store.AuthRemovalMarkUnbound(ctx, r.TenantID, r.Subject, now)
		if err != nil {
			return false, err
		}
		if ok {
			s.recordLifecycleEvent("auth_user_unbound", r.TenantID, r.Subject, r.Gate, r.Reason, lifecycleActor)
		}
		return ok, nil
	case lifecycle.StageErase:
		return s.eraseRemoved(ctx, r, now)
	}
	return false, nil
}

// eraseRemoved runs the RFC CF erasure for a removal whose erase_after came:
// the report first (what will go), then the execute. ErasureExecute writes
// erase_intent before anything is deleted and refuses when it cannot, so an
// erasure nobody could audit never happens; the removal then stays due.
func (s *Server) eraseRemoved(ctx context.Context, r store.AuthRemovalRow, now time.Time) (bool, error) {
	rep, err := s.ErasureReport(ctx, r.TenantID, r.Subject)
	if err != nil {
		return false, fmt.Errorf("erasure report: %w", err)
	}
	res, err := s.ErasureExecute(ctx, erasure.ExecuteRequest{
		Tenant: r.TenantID, Subject: r.Subject, DryRun: false, Confirm: r.Subject, SourceAddr: lifecycleActor,
	})
	if err != nil {
		return false, fmt.Errorf("erasure: %w", err)
	}
	ok, err := s.store.AuthRemovalMarkErased(ctx, r.TenantID, r.Subject, now)
	if err != nil {
		return false, err
	}
	if ok {
		s.recordLifecycleEvent("auth_user_erased", r.TenantID, r.Subject, r.Gate,
			fmt.Sprintf("%s; reported %d rows, %d plane errors", r.Reason, rep.Tier1.Total, len(res.Errors)), lifecycleActor)
	}
	return ok, nil
}

// errNoRemoval: the user has no removal to reactivate from.
var errNoRemoval = errors.New("the user is not being removed")

// reactivateRemoved re-enables a removed user at the §3.7a defaults: every
// group membership and every binding of its own goes, the access mode drops
// to the floor, the floor groups that exist in the tenant are joined, and only
// then is the user active again. A failure part-way leaves the user disabled.
func (s *Server) reactivateRemoved(ctx context.Context, tenant, subject, by string) error {
	r, err := s.store.AuthRemovalGet(ctx, tenant, subject)
	if err != nil {
		if isNotFound(err) {
			return errNoRemoval
		}
		return err
	}
	if err := s.resetRemovedToFloor(ctx, tenant, subject, by); err != nil {
		return err
	}
	active := "active"
	if err := s.store.UserUpdate(ctx, tenant, subject, store.UserPatch{Status: &active}); err != nil {
		return err
	}
	if _, err := s.store.AuthRemovalDelete(ctx, tenant, subject); err != nil {
		// The sweeper ends a removal whose user is active; log, don't fail.
		slog.Warn("auth: reactivated user's removal row not deleted", "tenant", tenant, "subject", subject, "err", err)
	}
	s.recordLifecycleEvent("auth_user_reactivated", tenant, subject, r.Gate, r.Reason, by)
	return nil
}

// resetRemovedToFloor is the §3.7a part of a re-enable: the user's own group
// memberships and bindings go, its access mode drops to the floor, and it
// joins the floor groups that exist. Its status is the caller's to set.
func (s *Server) resetRemovedToFloor(ctx context.Context, tenant, subject, by string) error {
	changed, err := s.unbindSubject(ctx, tenant, subject, true)
	if err != nil {
		return err
	}
	floor := s.cfg().ResolvedAuth.NewUserFloor()
	mode := floor.AccessMode
	if err := s.store.UserUpdate(ctx, tenant, subject, store.UserPatch{AccessMode: &mode}); err != nil {
		return err
	}
	for _, g := range floor.Groups {
		if _, err := s.store.GroupGet(ctx, tenant, g); err != nil {
			if isNotFound(err) {
				continue
			}
			return err
		}
		err := s.store.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: tenant, GroupName: g,
			MemberKind: store.GroupMemberUser, MemberID: subject, AddedBy: by})
		var conflict *store.ErrConflict
		if err != nil && !errors.As(err, &conflict) {
			return err
		}
		changed = true
	}
	if changed {
		s.aclChanged(ctx, tenant, "removal_reactivated", subject)
	}
	return nil
}

// removalRow and removalTiming convert between the store's removal row and
// the lifecycle package's timing, which stays below storage.
func removalRow(t lifecycle.Removal, tenant, subject, gate, reason, by string) store.AuthRemovalRow {
	return store.AuthRemovalRow{TenantID: tenant, Subject: subject, Gate: gate, Reason: reason, CreatedBy: by,
		DisabledAt: t.DisabledAt, UnbindDueAt: t.UnbindDueAt, EraseDueAt: t.EraseDueAt, UnboundAt: t.UnboundAt, ErasedAt: t.ErasedAt}
}

func removalTiming(r store.AuthRemovalRow) lifecycle.Removal {
	return lifecycle.Removal{DisabledAt: r.DisabledAt, UnbindDueAt: r.UnbindDueAt, EraseDueAt: r.EraseDueAt,
		UnboundAt: r.UnboundAt, ErasedAt: r.ErasedAt}
}
