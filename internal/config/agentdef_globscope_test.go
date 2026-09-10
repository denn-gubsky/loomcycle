package config

import (
	"strings"
	"testing"
)

// Decision 7's guard rail. A bare wildcard grants every name at that depth,
// which is so close to `any` that writing it is almost certainly a mistake —
// and an operator who means "everything" should say `any`, where it reads as
// the grant it is.
func TestValidateAgentDefScope_BareWildcardIsRefused(t *testing.T) {
	for _, sc := range []string{"named:*", "named:**"} {
		if err := validateAgentDefScope(sc); err == nil {
			t.Errorf("%q accepted; it grants every agent name", sc)
		}
	}
	// A SCOPED pattern is the point of the feature and must stay accepted.
	for _, sc := range []string{"named:sdlc/*", "named:sdlc/**", "named:sdlc/review/*", "named:coder"} {
		if err := validateAgentDefScope(sc); err != nil {
			t.Errorf("%q rejected: %v", sc, err)
		}
	}
}

// A pattern the matcher can never match is refused at authoring time rather
// than granted-and-silent. Each case below is a grant an operator would
// reasonably write and then wait forever for.
func TestValidateAgentDefScope_UnmatchablePatternsAreRefused(t *testing.T) {
	cases := []struct {
		scope string
		want  string
	}{
		// `**` means "all remaining segments", so nothing can follow it.
		{"named:sdlc/**/review", "must be last"},
		// A wildcard is a whole segment; `sdlc*` reads like a prefix match and
		// is not one — it would match only an agent literally named `sdlc*`,
		// which agents.ValidateName forbids.
		{"named:sdlc*", "whole segment"},
		{"named:sdlc/rev*", "whole segment"},
		{"named:*sdlc", "whole segment"},
		{"named:a/**b/c", "whole segment"},
	}
	for _, c := range cases {
		err := validateAgentDefScope(c.scope)
		if err == nil {
			t.Errorf("%q accepted; it can never match a valid agent name", c.scope)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q refused with %q, want it to mention %q", c.scope, err, c.want)
		}
	}
}
