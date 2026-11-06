package scheduler

import "testing"

// An $oauth: reference resolves by scope like $cred:, so a schedule that may
// not act as its user must see it named.
func TestNamesStoredCredential_DetectsOAuthRef(t *testing.T) {
	if !namesStoredCredential(map[string]string{"peer": "Bearer $oauth:docs"}) {
		t.Error("an $oauth: reference was not seen")
	}
	if namesStoredCredential(map[string]string{"peer": "a literal"}) {
		t.Error("a literal was taken for a reference")
	}
}
