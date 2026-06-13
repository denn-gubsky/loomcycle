// Package scheduler is the v1.x RFC E sweeper runtime. The Scheduler
// struct owns a goroutine that ticks at a configurable interval (default
// 30s), queries the store for due schedules, and fires each one via
// runner.RunOnce — the same seam HTTP + gRPC drive their interactive
// runs through. Reusing RunOnce means per-user fairness, retry policy,
// transcripts, and OTEL all work for scheduled runs without duplication.
//
// File map:
//
//	cron.go      — robfig/cron/v3 wrapper for next-fire computation
//	runinput.go  — mergedScheduleDef → runner.RunInput conversion
//	dispatch.go  — on_complete hook execution (channel.publish + memory.set + mcp.call)
//	scheduler.go — Scheduler struct + sweeper goroutine + per-fire orchestration
//
// What this package does NOT own:
//   - The substrate CRUD surface (lives in internal/tools/builtin/scheduledef.go)
//   - The HTTP admin endpoint (lives in internal/api/http/substrate_admin.go)
//   - Persistence (lives in internal/store/{sqlite,postgres})
//
// Single-replica only in v1.0. Cluster mode (advisory-lock per def)
// lands in v0.12+ once the multi-replica HA infrastructure ships.
package scheduler

import (
	"fmt"
	"sort"
	"time"

	"github.com/robfig/cron/v3"
)

// NextFireAfter computes the next time the cron expression fires
// strictly AFTER the supplied moment, interpreted in the given timezone.
// Empty tz defaults to UTC. Returns an error on invalid cron syntax
// or unknown timezone — both should have been caught at config-load /
// fork-time validation, but the sweeper re-validates defensively
// because the store doesn't enforce semantic constraints on the
// JSON-blob Definition field.
func NextFireAfter(cronExpr, tz string, after time.Time) (time.Time, error) {
	if cronExpr == "" {
		return time.Time{}, fmt.Errorf("cron expression is empty")
	}
	sched, err := cron.ParseStandard(cronExpr)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse cron %q: %w", cronExpr, err)
	}
	loc := time.UTC
	if tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return time.Time{}, fmt.Errorf("load timezone %q: %w", tz, err)
		}
		loc = l
	}
	return sched.Next(after.In(loc)), nil
}

// ResolveCron picks the cron expression to use for a (def, tier)
// combination. The substrate-write side enforces mutual exclusion
// between `schedule` (single cron) and `user_tier_schedules` (per-tier
// map), so exactly one of these branches resolves:
//
//   - def.Schedule non-empty → that's the cron, tier ignored
//   - def.UserTierSchedules has an entry for `tier` → use it
//   - tier empty AND no Schedule → error (missing fork-time selection)
//
// The returned cron expression goes into NextFireAfter.
func ResolveCron(schedule string, userTierSchedules map[string]string, tier string) (string, error) {
	if schedule != "" {
		return schedule, nil
	}
	if len(userTierSchedules) == 0 {
		return "", fmt.Errorf("def has neither `schedule` nor `user_tier_schedules`")
	}
	if tier == "" {
		// Single-tier defs would be authored with explicit `schedule:`.
		// Reaching here means the def is a template that needs a tier
		// pick — which would normally happen at fork time. If a fork
		// landed without picking, the scheduler can't fire it.
		return "", fmt.Errorf("def has `user_tier_schedules` but no tier supplied (fork-time pick missing)")
	}
	expr, ok := userTierSchedules[tier]
	if !ok {
		// List the available tiers in the error so operators see what's
		// valid rather than a generic "not found." Sort so the message is
		// deterministic — Go randomises map iteration order, so without this
		// the "(have: ...)" list shuffled between identical failures.
		avail := make([]string, 0, len(userTierSchedules))
		for k := range userTierSchedules {
			avail = append(avail, k)
		}
		sort.Strings(avail)
		return "", fmt.Errorf("tier %q not in user_tier_schedules (have: %v)", tier, avail)
	}
	return expr, nil
}
