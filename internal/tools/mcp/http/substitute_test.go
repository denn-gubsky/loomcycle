package http

import (
	"strings"
	"testing"
)

func TestSubstituteRunVars_BearerPresent(t *testing.T) {
	got, drop := substituteRunVars("Bearer ${run.user_bearer}", "tok123abc")
	if got != "Bearer tok123abc" {
		t.Errorf("got = %q, want %q", got, "Bearer tok123abc")
	}
	if drop {
		t.Errorf("drop = true, want false")
	}
}

func TestSubstituteRunVars_FallbackUsed(t *testing.T) {
	got, drop := substituteRunVars("Bearer ${run.user_bearer:-static-fallback}", "")
	if got != "Bearer static-fallback" {
		t.Errorf("got = %q, want %q", got, "Bearer static-fallback")
	}
	if drop {
		t.Errorf("drop = true, want false")
	}
}

func TestSubstituteRunVars_BearerWinsOverFallback(t *testing.T) {
	got, drop := substituteRunVars("Bearer ${run.user_bearer:-static-fallback}", "real-token")
	if got != "Bearer real-token" {
		t.Errorf("got = %q, want %q", got, "Bearer real-token")
	}
	if drop {
		t.Errorf("drop = true, want false")
	}
}

func TestSubstituteRunVars_NoBearerNoFallback(t *testing.T) {
	got, drop := substituteRunVars("Bearer ${run.user_bearer}", "")
	if !drop {
		t.Errorf("drop = false, want true (bare token with empty bearer)")
	}
	// got value is irrelevant when drop=true (caller discards the header),
	// but for hygiene the literal "${...}" placeholder must not survive
	// — otherwise a caller that ignores drop would ship the placeholder
	// downstream.
	if strings.Contains(got, "${run.") {
		t.Errorf("got = %q, must not contain unresolved placeholder", got)
	}
}

func TestSubstituteRunVars_MultipleOccurrences(t *testing.T) {
	got, drop := substituteRunVars("${run.user_bearer}:${run.user_bearer}", "abc")
	if got != "abc:abc" {
		t.Errorf("got = %q, want %q", got, "abc:abc")
	}
	if drop {
		t.Errorf("drop = true, want false")
	}
}

func TestSubstituteRunVars_NoToken(t *testing.T) {
	// Pure no-op when value contains no ${run.*} template.
	got, drop := substituteRunVars("Bearer static-value", "any-bearer")
	if got != "Bearer static-value" {
		t.Errorf("got = %q, want unchanged", got)
	}
	if drop {
		t.Errorf("drop = true, want false")
	}
}

// TestSubstituteRunVars_MixedBareAndFallback covers the edge case where
// one header value contains both a bare and a fallback-bearing token.
// Empty bearer: the bare one drops the header (per loomcycle plan §3),
// the fallback resolves, drop is true.
func TestSubstituteRunVars_MixedBareAndFallback(t *testing.T) {
	got, drop := substituteRunVars("${run.user_bearer}|${run.user_bearer:-fb}", "")
	if !drop {
		t.Errorf("drop = false, want true (at least one bare token unresolved)")
	}
	if strings.Contains(got, "${run.") {
		t.Errorf("got = %q, must not contain unresolved placeholder", got)
	}
	// Pin the full expected shape: bare token → empty, fallback → "fb".
	// Without this, the test would pass even if the fallback branch were
	// silently broken (returning empty too).
	if got != "|fb" {
		t.Errorf("got = %q, want %q (bare-token empty + fallback resolved)", got, "|fb")
	}
}

func TestTokenPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "(empty)"},
		{"abc", "abc…"},
		{"abcd", "abcd…"},
		{"abcdefgh", "abcd…"},
		{"eyJhbGciOiJIUzI1NiJ9.payload.sig", "eyJh…"},
	}
	for _, tc := range cases {
		if got := tokenPrefix(tc.in); got != tc.want {
			t.Errorf("tokenPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---- RFC F per-tool credentials substitution ----------------------

func TestSubstituteCredentialRefs_KeyPresent(t *testing.T) {
	got, drop, missing := substituteCredentialRefs(
		"Bearer ${run.credentials.jobs}",
		map[string]string{"jobs": "xoxb-jobs-tok"},
	)
	if got != "Bearer xoxb-jobs-tok" {
		t.Errorf("got = %q, want %q", got, "Bearer xoxb-jobs-tok")
	}
	if drop {
		t.Errorf("drop = true, want false")
	}
	if len(missing) > 0 {
		t.Errorf("missing = %v, want empty", missing)
	}
}

func TestSubstituteCredentialRefs_FallbackUsed(t *testing.T) {
	got, drop, missing := substituteCredentialRefs(
		"Bearer ${run.credentials.jobs:-static-fallback}",
		map[string]string{}, // jobs absent
	)
	if got != "Bearer static-fallback" {
		t.Errorf("got = %q, want %q", got, "Bearer static-fallback")
	}
	if drop {
		t.Errorf("drop = true, want false")
	}
	if len(missing) > 0 {
		t.Errorf("missing = %v, want empty", missing)
	}
}

func TestSubstituteCredentialRefs_ValueWinsOverFallback(t *testing.T) {
	got, drop, _ := substituteCredentialRefs(
		"Bearer ${run.credentials.jobs:-static-fallback}",
		map[string]string{"jobs": "real-jobs-tok"},
	)
	if got != "Bearer real-jobs-tok" {
		t.Errorf("got = %q, want %q", got, "Bearer real-jobs-tok")
	}
	if drop {
		t.Errorf("drop = true, want false")
	}
}

func TestSubstituteCredentialRefs_MissingKeyDrops(t *testing.T) {
	got, drop, missing := substituteCredentialRefs(
		"Bearer ${run.credentials.jobs}",
		map[string]string{}, // jobs absent
	)
	if !drop {
		t.Errorf("drop = false, want true")
	}
	if strings.Contains(got, "${run.") {
		t.Errorf("got = %q, must not contain unresolved placeholder", got)
	}
	if len(missing) != 1 || missing[0] != "jobs" {
		t.Errorf("missing = %v, want [jobs]", missing)
	}
}

func TestSubstituteCredentialRefs_EmptyValueTreatedAsMissing(t *testing.T) {
	// Empty string credential value matches the "absent" behaviour —
	// otherwise an operator who accidentally rotated to "" would silently
	// ship empty Authorization headers downstream. Loud failure is better.
	got, drop, missing := substituteCredentialRefs(
		"Bearer ${run.credentials.jobs}",
		map[string]string{"jobs": ""},
	)
	if !drop {
		t.Errorf("drop = false, want true (empty value treated as missing)")
	}
	if strings.Contains(got, "${run.") {
		t.Errorf("got = %q, must not contain unresolved placeholder", got)
	}
	if len(missing) != 1 {
		t.Errorf("missing = %v, want [jobs]", missing)
	}
}

func TestSubstituteCredentialRefs_MultipleKeys(t *testing.T) {
	// Single header value with two different credential references —
	// the substrate substitutes each from the same map. Exercises the
	// realistic case where one mcp_servers.foo.headers maps multiple
	// fields to multiple credentials (e.g. App-Token + Channel-Token).
	got, drop, _ := substituteCredentialRefs(
		"App ${run.credentials.app}; Channel ${run.credentials.chan}",
		map[string]string{"app": "A1", "chan": "C1"},
	)
	if got != "App A1; Channel C1" {
		t.Errorf("got = %q, want %q", got, "App A1; Channel C1")
	}
	if drop {
		t.Errorf("drop = true, want false")
	}
}

func TestSubstituteCredentialRefs_MixedPresentAndMissing(t *testing.T) {
	// When ANY bare ref in the value is missing, drop=true wins for the
	// whole header — half-credentialed requests are never desirable
	// (they'd fail at the upstream auth layer with a confusing 401).
	_, drop, missing := substituteCredentialRefs(
		"App ${run.credentials.app}; Channel ${run.credentials.chan}",
		map[string]string{"app": "A1"}, // chan absent
	)
	if !drop {
		t.Errorf("drop = false, want true")
	}
	if len(missing) != 1 || missing[0] != "chan" {
		t.Errorf("missing = %v, want [chan]", missing)
	}
}

func TestSubstituteCredentialRefs_NoRefIsNoOp(t *testing.T) {
	// Header without any ${run.credentials.*} ref passes through
	// unchanged. The map is irrelevant in this case.
	got, drop, missing := substituteCredentialRefs(
		"Authorization: Bearer literal-token",
		map[string]string{"unused": "x"},
	)
	if got != "Authorization: Bearer literal-token" {
		t.Errorf("got = %q, want unchanged", got)
	}
	if drop || len(missing) > 0 {
		t.Errorf("drop=%v missing=%v, want false/empty for ref-less input", drop, missing)
	}
}

func TestSubstituteCredentialRefs_InvalidKeyCharIgnored(t *testing.T) {
	// The regex requires [a-zA-Z0-9_-]{1,64}; an invalid expression
	// like ${run.credentials.foo bar} fails to match and survives as
	// a literal in the header. Wire-layer validation prevents this
	// shape from reaching the substrate; this test pins the regex's
	// strict charset so an operator typo never silently runs.
	got, drop, _ := substituteCredentialRefs(
		"Bearer ${run.credentials.foo bar}",
		map[string]string{"foo bar": "shouldnt-match"},
	)
	// Literal survives — regex didn't engage.
	if !strings.Contains(got, "${run.credentials.foo bar}") {
		t.Errorf("got = %q, expected literal to pass through", got)
	}
	// drop stays false because the regex didn't engage.
	if drop {
		t.Errorf("drop = true, want false for non-matching expression")
	}
}

func TestSubstituteRunIDs(t *testing.T) {
	// Both identifiers resolve.
	got := substituteRunIDs("run=${run.root_run_id} tenant=${run.tenant_id}", "r-123", "acme")
	if got != "run=r-123 tenant=acme" {
		t.Errorf("got = %q", got)
	}
	// Unresolved (empty run) → empty substitution, NOT a dropped/literal token.
	got = substituteRunIDs("X-Loom-Root-Run=[${run.root_run_id}]", "", "")
	if got != "X-Loom-Root-Run=[]" {
		t.Errorf("empty root_run_id should substitute to empty, got %q", got)
	}
	// No token → unchanged (and the fast-path returns it verbatim).
	if got := substituteRunIDs("Bearer abc", "r-1", "t-1"); got != "Bearer abc" {
		t.Errorf("no-token passthrough failed: %q", got)
	}
	// Disjoint from the bearer/credential tokens — leaves them alone.
	if got := substituteRunIDs("${run.user_bearer}", "r", "t"); got != "${run.user_bearer}" {
		t.Errorf("must not touch ${run.user_bearer}: %q", got)
	}
}
