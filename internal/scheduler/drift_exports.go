package scheduler

import "reflect"

// ScheduleDefJSONTagsForDrift returns the set of JSON tag names on
// the package-private scheduleDef struct (the sweeper-read shape).
// Test-only export: lets the builtin package's drift test pin
// scheduler↔builtin parity without a circular dep on scheduler from
// builtin (scheduler doesn't import builtin, so the import direction
// is safe). Each tag is the bare name with `,omitempty` stripped.
//
// Why exported: builtin.mergedScheduleDef is package-private, so the
// drift test that compares the two shapes must live in the builtin
// package; that test needs to see scheduleDef's tags from outside
// the scheduler package. This helper is the minimum surface to make
// that test trivially possible.
//
// Why this matters: the v1.x RFC E code review caught a missing
// UserTier field on mergedScheduleDef — visible only because the
// substrate write side had no field for the fork-time tier pick that
// the sweeper-read side already expected. A scheduler↔builtin drift
// test using this helper would have failed immediately when the
// field was added on one side only.
func ScheduleDefJSONTagsForDrift() map[string]bool {
	t := reflect.TypeOf(scheduleDef{})
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
