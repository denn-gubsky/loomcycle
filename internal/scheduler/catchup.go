package scheduler

import (
	"context"
	"fmt"
	"time"

	cron "github.com/robfig/cron/v3"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// catch_up_max (RFC DZ). When a claim finds the schedule overdue by more than
// one slot — an outage, a pause, a run that outlived several slots under
// forbid — the missed slots are counted from next_run_at up to now:
//
//   - catch_up_max 0 (the default): fire once, for the newest missed slot, as
//     a live slot; the older ones are dropped and counted.
//   - catch_up_max N: keep the newest N, drop (and count) the older ones, and
//     drain the kept ones in slot order as catch-up runs.
//
// Nothing stores the kept list: each claim recomputes it from next_run_at with
// the same cron, so the claim's compare-and-set stays the only coordination.
// What is stored is catch_up_until, the backlog's newest slot, because a
// slot's KIND must outlive the claims: once the older kept slots have run, the
// last one is the only one still due and would otherwise read as live.

// maxCatchUpCeiling bounds catch_up_max at write time.
const maxCatchUpCeiling = 1000

// maxSlotScan bounds how many slots one claim counts. An every-minute cron
// over a 30-day outage is 43,200; past the bound the count stops and the
// claim moves on from now, so a years-long gap cannot stall a tick.
const maxSlotScan = 1_000_000

// slotPlan is what one claim takes and where it leaves next_run_at.
type slotPlan struct {
	slot time.Time // the slot this claim fires
	next time.Time // where next_run_at moves
	// catchUpUntil is the newest slot of the backlog this slot belongs to;
	// zero for a live slot.
	catchUpUntil time.Time
	// dropped counts the missed slots this claim does not run.
	dropped int
}

func (p slotPlan) catchUp() bool { return !p.catchUpUntil.IsZero() }

// planSlot works out what a claim of row takes, keeping at most keep missed
// slots (0 collapses them into one live slot).
func (s *Scheduler) planSlot(def scheduleDef, row store.ScheduleDueRow, now time.Time, keep int) (slotPlan, error) {
	expr, err := ResolveCron(def.Schedule, def.UserTierSchedules, def.UserTier)
	if err != nil {
		return slotPlan{}, err
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return slotPlan{}, fmt.Errorf("parse cron %q: %w", expr, err)
	}
	loc := time.UTC
	if def.Timezone != "" {
		l, err := time.LoadLocation(def.Timezone)
		if err != nil {
			return slotPlan{}, fmt.Errorf("load timezone %q: %w", def.Timezone, err)
		}
		loc = l
	}

	// The due slots: the listed one, then every cron time after it up to now.
	// Only the newest `window` are remembered.
	window := keep
	if window < 1 {
		window = 1
	}
	ring := make([]time.Time, window) // the newest `window` due slots, circular
	total := 0
	t := row.NextRunAt
	for !t.After(now) && total < maxSlotScan {
		ring[total%window] = t
		total++
		t = sched.Next(t.In(loc))
	}
	future := t
	if !future.After(now) {
		// The scan stopped at its bound: resume the cadence from now.
		future = sched.Next(now.In(loc))
	}
	if total == 0 {
		// Not due after all (a clock step between listing and now).
		return slotPlan{slot: row.NextRunAt, next: future}, nil
	}
	n := total
	if n > window {
		n = window
	}
	kept := make([]time.Time, n) // oldest first
	for i := range kept {
		kept[i] = ring[(total-n+i)%window]
	}
	newest := kept[n-1]
	inBacklog := !row.CatchUpUntil.IsZero() && !row.NextRunAt.After(row.CatchUpUntil)

	if keep == 0 || (total == 1 && !inBacklog) {
		// One slot due (the steady state), or no catch-up asked for: fire the
		// newest due slot as a live one.
		return slotPlan{slot: newest, next: future, dropped: total - 1}, nil
	}
	p := slotPlan{slot: kept[0], next: future, dropped: total - len(kept), catchUpUntil: newest}
	if len(kept) > 1 {
		p.next = kept[1]
	}
	if row.CatchUpUntil.After(p.catchUpUntil) {
		p.catchUpUntil = row.CatchUpUntil
	}
	return p, nil
}

// catchUpMustWait reports whether a catch-up slot has to wait for the
// schedule's running runs. It is asked BEFORE the claim, so a waiting slot
// leaves next_run_at where it is and is tried again next tick:
//
//   - forbid and replace: a catch-up slot waits for any running run, so the
//     backlog runs one at a time. Skipping would discard the very slots
//     catch_up_max asked to keep; replacing would cancel every catch-up run
//     but the last.
//   - allow: it starts while the schedule has fewer than catch_up_max running
//     runs, so catch_up_max is also how many run at once while catching up.
//
// A store fault waits too: unknown is not "nothing running".
func (s *Scheduler) catchUpMustWait(ctx context.Context, row store.ScheduleDueRow, def scheduleDef) bool {
	active, err := s.store.ScheduleActiveRunsList(ctx, row.DefID)
	if err != nil {
		s.logf("scheduler: schedule %q: list its running runs: %v — the catch-up slot waits", row.Name, err)
		return true
	}
	if def.ConcurrencyPolicy == policyAllow {
		return len(active) >= def.CatchUpMax
	}
	return len(active) > 0
}

// catchUpMetadata is the metadata a catch-up run carries on top of the def's:
// the slot it was fired for, so a job can tell "the 06:00 run, executed at
// 09:12" from a live one. Live runs get none, so their metadata — which some
// providers carry in the prompt — is unchanged.
func catchUpMetadata(md map[string]any, slot time.Time) map[string]any {
	out := make(map[string]any, len(md)+2)
	for k, v := range md {
		out[k] = v
	}
	out["slot_at"] = slot.UTC().Format(time.RFC3339)
	out["catch_up"] = true
	return out
}
