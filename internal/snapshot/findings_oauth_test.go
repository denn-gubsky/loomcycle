package snapshot

import "testing"

// An $oauth: reference is a reference, not a literal credential.
func TestSnapshotFindings_OAuthRefIsNotALiteral(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"Authorization", "Bearer $oauth:docs"},
		{"X-Api-Key", "$oauth:docs"},
	} {
		if got := headerDetector(tc.name, tc.value); got != "" {
			t.Errorf("headerDetector(%q, %q) = %q, want no finding", tc.name, tc.value, got)
		}
	}
	if got := envDetector("DOCS_TOKEN", "$oauth:docs"); got != "" {
		t.Errorf("envDetector = %q, want no finding", got)
	}
}
