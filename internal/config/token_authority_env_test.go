package config

import "testing"

// RFC EY §8: per-user tokens are minted frozen unless the operator sets
// LOOMCYCLE_TOKEN_AUTHORITY_INHERIT=1 exactly; any other value leaves the
// default off, like the other =1 switches.
func TestLoad_TokenAuthorityInheritIsOffUnlessSetToOne(t *testing.T) {
	for value, want := range map[string]bool{"": false, "0": false, "true": false, "1": true} {
		t.Setenv("LOOMCYCLE_TOKEN_AUTHORITY_INHERIT", value)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load (%q): %v", value, err)
		}
		if cfg.Env.TokenAuthorityInherit != want {
			t.Errorf("LOOMCYCLE_TOKEN_AUTHORITY_INHERIT=%q → %v, want %v", value, cfg.Env.TokenAuthorityInherit, want)
		}
	}
}
