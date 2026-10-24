// Package lifecycle is the RFC FA §3.7c removal policy as pure decisions: when
// each stage of a removed user's removal is due, which stage comes next, and
// which users an authoritative gate's sync no longer asserts. The HTTP server
// carries the decisions out (internal/api/http/authgate_lifecycle.go); keeping
// them here keeps the "a stage never skips" rule in one testable place.
package lifecycle

import (
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Policy is a removal's timing, fixed when the removal starts.
type Policy struct {
	UnbindAfter time.Duration
	// EraseAfter is measured from the disable; zero means only an operator
	// erases.
	EraseAfter time.Duration
}

// Start is the removal row for a user disabled at `at`.
func (p Policy) Start(tenant, subject, gate, reason, by string, at time.Time) store.AuthRemovalRow {
	r := store.AuthRemovalRow{TenantID: tenant, Subject: subject, Gate: gate, Reason: reason, CreatedBy: by,
		DisabledAt: at, UnbindDueAt: at.Add(p.UnbindAfter)}
	if p.EraseAfter > 0 {
		r.EraseDueAt = at.Add(p.EraseAfter)
		if r.EraseDueAt.Before(r.UnbindDueAt) {
			r.EraseDueAt = r.UnbindDueAt
		}
	}
	return r
}

// Stage is a step after the disable.
type Stage string

const (
	// StageNone: nothing is due.
	StageNone Stage = ""
	// StageUnbind: remove gate-managed memberships and the user's bindings.
	StageUnbind Stage = "unbind"
	// StageErase: run the subject erasure.
	StageErase Stage = "erase"
)

// Next is the one stage due for r at now. The erase is never next while the
// unbind has not happened, whatever the clock says, and never at all without
// an EraseDueAt (an operator erases).
func Next(r store.AuthRemovalRow, now time.Time) Stage {
	switch {
	case r.UnboundAt.IsZero():
		if !now.Before(r.UnbindDueAt) {
			return StageUnbind
		}
	case r.ErasedAt.IsZero() && !r.EraseDueAt.IsZero() && !now.Before(r.EraseDueAt):
		return StageErase
	}
	return StageNone
}

// Missing returns the identities at a gate whose external subject the gate's
// complete sync no longer lists: the users it says are gone. Sorted by
// (tenant, subject). An identity the gate lists stays, wherever it is.
func Missing(existing []store.UserIdentityRow, asserted []string) []store.UserIdentityRow {
	keep := make(map[string]bool, len(asserted))
	for _, s := range asserted {
		keep[s] = true
	}
	var out []store.UserIdentityRow
	for _, id := range existing {
		if !keep[id.ExternalSubject] {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID < out[j].TenantID
		}
		return out[i].Subject < out[j].Subject
	})
	return out
}
