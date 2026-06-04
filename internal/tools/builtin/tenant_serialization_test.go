package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestScheduleDef_TenantInContentHash pins the serialization back-compat that
// the locked RFC N design requires of the def-content TenantID field: it is
// `omitempty`, so a def with NO tenant marshals IDENTICALLY to a pre-change
// def (no `tenant_id` key in the persisted JSONB / snapshot / content image),
// while setting the tenant changes the serialized bytes. ScheduleDef has no
// content_sha256 op (see scheduledef.go) — the serialized image IS the
// content identity that matters for round-trip, so byte-equality is the
// testable analog of "the content hash is unchanged for existing defs".
func TestScheduleDef_TenantInContentHash(t *testing.T) {
	noTenant := mergedScheduleDef{Agent: "digest"}
	withTenant := mergedScheduleDef{Agent: "digest", TenantID: "acme"}

	noBytes, err := json.Marshal(noTenant)
	if err != nil {
		t.Fatal(err)
	}
	withBytes, err := json.Marshal(withTenant)
	if err != nil {
		t.Fatal(err)
	}

	// omitempty back-compat: an absent tenant must not emit the key at all,
	// so an existing def's serialized image (and any hash over it) is unchanged.
	if strings.Contains(string(noBytes), "tenant_id") {
		t.Errorf("empty TenantID must be omitted (omitempty) so existing defs serialize identically; got %s", noBytes)
	}
	// Setting the tenant is a genuinely different def → different bytes.
	if string(noBytes) == string(withBytes) {
		t.Errorf("setting TenantID must change the serialized content; both produced %s", noBytes)
	}
	if !strings.Contains(string(withBytes), `"tenant_id":"acme"`) {
		t.Errorf("set TenantID must serialize as tenant_id; got %s", withBytes)
	}
}
