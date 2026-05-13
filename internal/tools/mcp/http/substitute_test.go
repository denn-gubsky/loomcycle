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
