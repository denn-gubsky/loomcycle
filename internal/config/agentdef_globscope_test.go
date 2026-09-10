package config

import "testing"

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
