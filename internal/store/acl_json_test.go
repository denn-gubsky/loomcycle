package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The ACL rows' times read back from the store in the local zone; on the
// wire they are UTC, as the rest of the API writes its times. A zone that is
// not UTC makes the difference visible on any machine.
func TestACLRows_MarshalTimesInUTC(t *testing.T) {
	zone := time.FixedZone("UTC+3", 3*3600)
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, zone) // 09:00Z
	for name, v := range map[string]any{
		"AclDefRow":      AclDefRow{Name: "l", CreatedAt: at, UpdatedAt: at, RetiredAt: at},
		"AclBindingRow":  AclBindingRow{BindingID: "b", ACLName: "l", SubjectKind: "user", CreatedAt: at, ExpiresAt: at, RetiredAt: at},
		"GroupRow":       GroupRow{Name: "g", CreatedAt: at, UpdatedAt: at},
		"GroupMemberRow": GroupMemberRow{GroupName: "g", MemberKind: "user", MemberID: "u", AddedAt: at},
	} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(raw), "+03:00") || !strings.Contains(string(raw), `"2026-10-23T09:00:00Z"`) {
			t.Errorf("%s = %s, want every time in UTC", name, raw)
		}
	}
	// An unset time stays out, as before.
	raw, _ := json.Marshal(AclBindingRow{BindingID: "b", ACLName: "l", SubjectKind: "user", CreatedAt: at})
	if strings.Contains(string(raw), "expires_at") || strings.Contains(string(raw), "retired_at") {
		t.Errorf("unset times written: %s", raw)
	}
}
