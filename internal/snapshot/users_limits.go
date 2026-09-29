package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/limits"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The users and token_limits sections (RFC DP §4.1, §4.2).
//
// Both follow the definition-section rule that the LIVE row stands: a row the
// target already has on the same key is left alone and not counted, so a
// re-restore writes nothing and an old snapshot cannot roll back a newer user
// status or budget. The one exception is the month-to-date usage carry, a
// monotonic counter that keeps the larger of the live and the incoming value —
// equally idempotent, and it can only raise a counter, never lower it.

// RestoreRefresh is what the caller's post-restore refresh must push into the
// in-process caches: the rows the restore actually wrote, not the whole
// section, because a row left alone is already what the caches hold. It never
// reaches a transport.
type RestoreRefresh struct {
	// TokenLimits are the budget rows inserted by this restore.
	TokenLimits []store.TokenLimitRow
	// UsageCarried is how much each (tenant, user) carry GREW in this restore.
	UsageCarried []CarriedUsage
}

// CarriedUsage is growth in one (tenant, user) usage carry for Month.
type CarriedUsage struct {
	Month    time.Time
	TenantID string
	UserID   string
	Tokens   int64
}

// captureUsers reads every tenant's user rows.
func captureUsers(ctx context.Context, s store.Store, out *UsersSection) error {
	out.Version = SectionVersion
	// "" is every tenant — the super-admin read. A snapshot spans tenants.
	rows, err := s.UserList(ctx, "")
	if err != nil {
		return fmt.Errorf("snapshot users: %w", err)
	}
	out.Entries = make([]UserEntry, 0, len(rows))
	for _, r := range rows {
		out.Entries = append(out.Entries, UserEntry{
			TenantID:    r.TenantID,
			Subject:     r.Subject,
			DisplayName: r.DisplayName,
			AccessMode:  r.AccessMode,
			Status:      r.Status,
			CreatedAt:   r.CreatedAt,
			CreatedBy:   r.CreatedBy,
		})
	}
	return nil
}

// captureTokenLimits reads every budget row and the month-to-date usage for
// the capture's UTC month. The usage is limits.MonthToDate — the same function
// the tracker seeds from — so what travels is exactly what the source was
// enforcing, including usage it had itself been carried from an earlier hop.
func captureTokenLimits(ctx context.Context, s store.Store, capturedAt time.Time, out *TokenLimitsSection) error {
	out.Version = SectionVersion
	rows, err := s.TokenLimitsAll(ctx)
	if err != nil {
		return fmt.Errorf("snapshot token_limits: %w", err)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.TenantID != b.TenantID {
			return a.TenantID < b.TenantID
		}
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		return a.ScopeID < b.ScopeID
	})
	out.Entries = make([]TokenLimitEntry, 0, len(rows))
	for _, r := range rows {
		out.Entries = append(out.Entries, TokenLimitEntry{
			TenantID:  r.TenantID,
			Scope:     r.Scope,
			ScopeID:   r.ScopeID,
			SoftLimit: r.SoftLimit,
			HardLimit: r.HardLimit,
			UpdatedAt: r.UpdatedAt,
			UpdatedBy: r.UpdatedBy,
		})
	}

	month := limits.MonthStart(capturedAt)
	mtd, err := limits.MonthToDate(ctx, s, month)
	if err != nil {
		return fmt.Errorf("snapshot token_limits usage_mtd: %w", err)
	}
	if len(mtd) == 0 {
		return nil
	}
	block := &UsageMTDBlock{Month: month, Entries: make([]UsageMTDEntry, 0, len(mtd))}
	for k, tok := range mtd {
		block.Entries = append(block.Entries, UsageMTDEntry{TenantID: k.TenantID, UserID: k.UserID, Tokens: tok})
	}
	sort.Slice(block.Entries, func(i, j int) bool {
		a, b := block.Entries[i], block.Entries[j]
		if a.TenantID != b.TenantID {
			return a.TenantID < b.TenantID
		}
		return a.UserID < b.UserID
	})
	out.UsageMTD = block
	return nil
}

// restoreUsers creates each user the target does not have. An existing user
// keeps its access_mode, status and display name; when the snapshot's row is
// stricter the operator is told, and tightens the live row by hand if the
// snapshot is right.
func restoreUsers(ctx context.Context, s store.Store, sec *UsersSection, result *RestoreResult) {
	for _, e := range sec.Entries {
		// UserCreate enum-validates access_mode and status, so a crafted entry
		// cannot store a mode the wire layer would refuse.
		err := s.UserCreate(ctx, store.UserRow{
			TenantID:    e.TenantID,
			Subject:     e.Subject,
			DisplayName: e.DisplayName,
			AccessMode:  e.AccessMode,
			Status:      e.Status,
			CreatedAt:   e.CreatedAt,
			CreatedBy:   e.CreatedBy,
		})
		var conflict *store.ErrConflict
		switch {
		case err == nil:
			result.UsersRestored++
		case errors.As(err, &conflict):
			live, gerr := s.UserGet(ctx, e.TenantID, e.Subject)
			if gerr != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("user %s/%s: exists here but could not be read to compare: %v", e.TenantID, e.Subject, gerr))
				continue
			}
			result.Warnings = append(result.Warnings, stricterUserWarnings(e, live)...)
		default:
			result.Warnings = append(result.Warnings, fmt.Sprintf("user %s/%s: %v", e.TenantID, e.Subject, err))
		}
	}
}

// stricterUserWarnings names each field where the snapshot's user is more
// confined than the live one. A looser snapshot row says nothing: the live
// row is already the stricter of the two.
func stricterUserWarnings(snap UserEntry, live store.UserRow) []string {
	var out []string
	if snap.AccessMode == "isolated" && live.AccessMode != "isolated" {
		out = append(out, fmt.Sprintf(
			"user %s/%s: the snapshot has access_mode=%s but this instance has %s; the live row was kept — set it to %s by hand if the snapshot is right",
			snap.TenantID, snap.Subject, snap.AccessMode, live.AccessMode, snap.AccessMode))
	}
	if snap.Status == "disabled" && live.Status != "disabled" {
		out = append(out, fmt.Sprintf(
			"user %s/%s: the snapshot has status=%s but this instance has %s; the live row was kept — set it to %s by hand if the snapshot is right",
			snap.TenantID, snap.Subject, snap.Status, live.Status, snap.Status))
	}
	return out
}

// restoreTokenLimits inserts each budget the target does not have, then
// persists the month-to-date usage carry. now is the target's clock.
func restoreTokenLimits(ctx context.Context, s store.Store, sec *TokenLimitsSection, now time.Time, result *RestoreResult) {
	for _, e := range sec.Entries {
		if msg := invalidTokenLimit(e); msg != "" {
			result.Warnings = append(result.Warnings, fmt.Sprintf("token_limit %s/%s/%s: %s; skipped", e.TenantID, e.Scope, e.ScopeID, msg))
			continue
		}
		row := store.TokenLimitRow{
			TenantID:  e.TenantID,
			Scope:     e.Scope,
			ScopeID:   e.ScopeID,
			SoftLimit: e.SoftLimit,
			HardLimit: e.HardLimit,
			UpdatedAt: e.UpdatedAt,
			UpdatedBy: e.UpdatedBy,
		}
		inserted, err := s.SnapshotRestoreTokenLimit(ctx, row)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("token_limit %s/%s/%s: %v", e.TenantID, e.Scope, e.ScopeID, err))
			continue
		}
		if inserted {
			result.TokenLimitsRestored++
			result.Refresh.TokenLimits = append(result.Refresh.TokenLimits, row)
		}
	}
	if sec.UsageMTD != nil {
		restoreUsageCarry(ctx, s, sec.UsageMTD, now, result)
	}
}

// invalidTokenLimit applies the rules the budget write surface enforces, so a
// hand-edited envelope cannot store a row no author could have: a negative
// ceiling would refuse every run in its scope. "" means valid.
func invalidTokenLimit(e TokenLimitEntry) string {
	if (e.SoftLimit != nil && *e.SoftLimit < 0) || (e.HardLimit != nil && *e.HardLimit < 0) {
		return "a negative ceiling"
	}
	switch e.Scope {
	case "operator":
		if e.TenantID != "" || e.ScopeID != "" {
			return "an operator budget names a tenant or subject"
		}
	case "tenant":
		if e.ScopeID != "" {
			return "a tenant budget names a subject"
		}
	case "user":
		if e.ScopeID == "" {
			return "a user budget names no subject"
		}
	default:
		return fmt.Sprintf("unknown scope %q", e.Scope)
	}
	return ""
}

// restoreUsageCarry persists the carried month-to-date usage. The carry is
// dropped when the target is in a different UTC month than the capture: the
// budget window has rolled over, as the tracker's own counters do.
//
// Known over-count, accepted: the carry is ADDED to the target's own ledger.
// That is right for a migration, where the two ledgers hold disjoint spend,
// and double-counts when a snapshot is restored back into an instance whose
// ledger already holds the same calls (a restore in place, or ping-pong
// between two instances). Budgets are advisory, and over-counting refuses runs
// earlier, never later — the fail-safe direction.
func restoreUsageCarry(ctx context.Context, s store.Store, b *UsageMTDBlock, now time.Time, result *RestoreResult) {
	cur := limits.MonthStart(now)
	if !b.Month.Equal(cur) {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"token_limits: usage for %s not carried: the budget window has rolled over (this instance is in %s)",
			b.Month.UTC().Format("2006-01"), cur.Format("2006-01")))
		return
	}
	for _, e := range b.Entries {
		if e.Tokens < 0 {
			result.Warnings = append(result.Warnings, fmt.Sprintf("token_limits usage %s/%s: negative total %d; skipped", e.TenantID, e.UserID, e.Tokens))
			continue
		}
		grew, err := s.UsageCarryRaise(ctx, store.UsageCarryRow{
			TenantID: e.TenantID, UserID: e.UserID, Month: cur, Tokens: e.Tokens,
		})
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("token_limits usage %s/%s: %v", e.TenantID, e.UserID, err))
			continue
		}
		if grew > 0 {
			result.UsageCarryRestored++
			result.Refresh.UsageCarried = append(result.Refresh.UsageCarried, CarriedUsage{
				Month: cur, TenantID: e.TenantID, UserID: e.UserID, Tokens: grew,
			})
		}
	}
}
