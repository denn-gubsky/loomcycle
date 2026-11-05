package config

import "testing"

// The store-backed audit stream is required exactly where someone other than
// the single operator will ask who did what: a gate, strict mode, or an ACL
// mode other than compat.
func TestAuditStoreRequired_FollowsThePosture(t *testing.T) {
	gate := []AuthGate{{Name: "paste", Kind: AuthGateKindToken}}
	for name, tc := range map[string]struct {
		c    Config
		want bool
	}{
		"single operator":  {Config{}, false},
		"compat ACL":       {Config{Env: Env{ACLMode: "compat"}}, false},
		"a gate":           {Config{ResolvedAuth: AuthPolicy{Gates: gate}}, true},
		"strict":           {Config{ResolvedAuth: AuthPolicy{Mode: AuthModeStrict}}, true},
		"shadow ACL":       {Config{Env: Env{ACLMode: "shadow"}}, true},
		"enforce ACL":      {Config{Env: Env{ACLMode: " Enforce "}}, true},
		"asked for, alone": {Config{Env: Env{AuditStore: true}}, false},
	} {
		if got := tc.c.AuditStoreRequired(); got != tc.want {
			t.Errorf("%s: AuditStoreRequired = %v, want %v", name, got, tc.want)
		}
	}
	if c := (Config{Env: Env{AuditStore: true}}); !c.AuditStoreEnabled() {
		t.Error("LOOMCYCLE_AUDIT_STORE=1 alone: AuditStoreEnabled = false")
	}
}

// erase_after needs a durable record of the erasure; with a gate configured
// the store-backed stream is that record, so no audit file is needed.
func TestResolveRemoval_EraseAfterAcceptsTheStoreStream(t *testing.T) {
	c := removalCfg(AuthOnRemovedConfig{EraseAfter: "30d"}, "")
	c.Auth.Gates = map[string]AuthGateConfig{"paste": {Kind: AuthGateKindToken}}
	if err := resolveAuth(c); err != nil {
		t.Fatalf("erase_after with a gate (the stream on): %v", err)
	}
	c = removalCfg(AuthOnRemovedConfig{EraseAfter: "30d"}, "")
	c.Env.AuditStore = true
	if err := resolveAuth(c); err != nil {
		t.Fatalf("erase_after with LOOMCYCLE_AUDIT_STORE=1: %v", err)
	}
}
