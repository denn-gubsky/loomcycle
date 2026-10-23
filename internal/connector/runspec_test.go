package connector

import (
	"encoding/json"
	"strings"
	"testing"
)

// The caller's access-control origin is stripped from a served record, at
// the top and inside a sub-run's offered parent record; every other key
// survives, and a record without it is served byte for byte.
func TestServedRunSpec_StripsTheACLOriginAtEveryLevel(t *testing.T) {
	stored := `{"sampling":{"temperature":0.2},"acl_origin":{"tenant_id":"acme","subject":"dev","class":"member"},` +
		`"spawn":{"volumes":{"active":false},"offered_overrides":{"run_id":"r_p","record":{"acl_origin":{"subject":"dev"},"routing":{"tier":"low"}}}}}`
	got := string(ServedRunSpec(json.RawMessage(stored)))
	if strings.Contains(got, "acl_origin") || strings.Contains(got, `"dev"`) {
		t.Errorf("served spec still names the caller: %s", got)
	}
	for _, keep := range []string{`"temperature":0.2`, `"run_id":"r_p"`, `"tier":"low"`, `"active":false`} {
		if !strings.Contains(got, keep) {
			t.Errorf("served spec lost %s: %s", keep, got)
		}
	}
	plain := `{"sampling":{"temperature":0.2},  "hosts":{"has_list":true}}`
	if got := string(ServedRunSpec(json.RawMessage(plain))); got != plain {
		t.Errorf("a record with no origin changed: %s", got)
	}
	for _, odd := range []string{"", `null`, `"acl_origin"`} {
		if got := string(ServedRunSpec(json.RawMessage(odd))); got != odd {
			t.Errorf("ServedRunSpec(%q) = %q, want it unchanged", odd, got)
		}
	}
}
