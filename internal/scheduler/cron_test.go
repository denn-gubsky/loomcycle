package scheduler

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// TestScheduleDef_DriftDetection_VsLookupSubstrateScheduleDef pins
// JSON-tag parity between scheduler.scheduleDef (the sweeper-read
// shape) and lookup.SubstrateScheduleDef (the canonical
// substrate-read adapter). The third mirror — builtin's
// mergedScheduleDef — is locked against lookup.SubstrateScheduleDef
// by a sister test in internal/tools/builtin. Together, the two
// drift tests cover the three-way mirror so a field added to ANY
// side without the matching additions on the others fails CI.
//
// One field is intentionally non-mirrored: `user_credentials` lives
// only on the substrate-write side (builtin.mergedScheduleDef +
// scheduler.scheduleDef both need to read it). lookup.Substrate
// ScheduleDef omits it because the lookup-side returns a
// config.ScheduledRun which has no credentials field — credentials
// flow into RunInput at the scheduler's fire seam, not through the
// config layer.
//
// `user_tier` is similarly scheduler-only — it's a sweeper-side
// pick (or a fork-time override stored in the fork's definition).
// lookup.SubstrateScheduleDef doesn't carry it because the
// substrate-write side hasn't standardised the field name yet.
func TestScheduleDef_DriftDetection_VsLookupSubstrateScheduleDef(t *testing.T) {
	exempt := map[string]bool{
		"user_credentials": true, // sweeper-side only — see commentary above
		"user_tier":        true, // sweeper-side only — see commentary above
	}
	schedTags := jsonTagsOf(reflect.TypeOf(scheduleDef{}))
	substrateTags := jsonTagsOf(reflect.TypeOf(lookup.SubstrateScheduleDef{}))

	for tag := range schedTags {
		if exempt[tag] {
			continue
		}
		if !substrateTags[tag] {
			t.Errorf("scheduler.scheduleDef has json tag %q but lookup.SubstrateScheduleDef does not — mirror it OR add %q to the exempt set with a justifying comment",
				tag, tag)
		}
	}
	for tag := range substrateTags {
		if !schedTags[tag] {
			t.Errorf("lookup.SubstrateScheduleDef has json tag %q but scheduler.scheduleDef does not — add it on the scheduler-read side so the sweeper can decode the field",
				tag)
		}
	}
}

func jsonTagsOf(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		for j, c := range tag {
			if c == ',' {
				tag = tag[:j]
				break
			}
		}
		out[tag] = true
	}
	return out
}

func TestNextFireAfter_DailyAt6AMUTC(t *testing.T) {
	// 2026-05-28 10:00 UTC → next fire of "0 6 * * *" should be
	// 2026-05-29 06:00 UTC.
	now := time.Date(2026, 5, 28, 10, 0, 0, 0, time.UTC)
	next, err := NextFireAfter("0 6 * * *", "", now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 5, 29, 6, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
}

func TestNextFireAfter_RespectsTimezone(t *testing.T) {
	// "0 6 * * *" in Europe/Berlin = 06:00 Berlin = 04:00 UTC (summer).
	// 2026-05-28 10:00 UTC → next is 2026-05-29 04:00 UTC.
	now := time.Date(2026, 5, 28, 10, 0, 0, 0, time.UTC)
	next, err := NextFireAfter("0 6 * * *", "Europe/Berlin", now)
	if err != nil {
		t.Fatal(err)
	}
	if next.UTC().Hour() != 4 {
		t.Errorf("expected next.UTC().Hour() == 4 (06:00 Berlin = 04:00 UTC); got hour %d (%v)",
			next.UTC().Hour(), next.UTC())
	}
}

func TestNextFireAfter_InvalidCron(t *testing.T) {
	_, err := NextFireAfter("not a cron", "", time.Now())
	if err == nil {
		t.Fatal("expected error for invalid cron")
	}
	if !strings.Contains(err.Error(), "parse cron") {
		t.Errorf("error should mention parse cron; got %v", err)
	}
}

func TestNextFireAfter_InvalidTimezone(t *testing.T) {
	_, err := NextFireAfter("0 * * * *", "Made/Up", time.Now())
	if err == nil {
		t.Fatal("expected error for invalid tz")
	}
}

func TestResolveCron_ExplicitSchedule(t *testing.T) {
	expr, err := ResolveCron("0 9 * * 1", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if expr != "0 9 * * 1" {
		t.Errorf("expr = %q, want '0 9 * * 1'", expr)
	}
}

func TestResolveCron_TierPick(t *testing.T) {
	tiers := map[string]string{
		"low":  "0 6 1,11,21 * *",
		"high": "0 6 * * *",
	}
	expr, err := ResolveCron("", tiers, "high")
	if err != nil {
		t.Fatal(err)
	}
	if expr != "0 6 * * *" {
		t.Errorf("expr = %q, want '0 6 * * *' (high tier)", expr)
	}
}

func TestResolveCron_UnknownTier(t *testing.T) {
	tiers := map[string]string{"low": "0 6 * * *"}
	_, err := ResolveCron("", tiers, "premium")
	if err == nil {
		t.Fatal("expected error for unknown tier")
	}
}

func TestResolveCron_NoScheduleNoTiers(t *testing.T) {
	_, err := ResolveCron("", nil, "")
	if err == nil {
		t.Fatal("expected error when neither schedule nor tier_schedules is set")
	}
}

func TestResolveCron_TiersButNoTierPicked(t *testing.T) {
	tiers := map[string]string{"low": "0 6 * * *"}
	_, err := ResolveCron("", tiers, "")
	if err == nil {
		t.Fatal("expected error when tiers exist but no tier supplied")
	}
}
