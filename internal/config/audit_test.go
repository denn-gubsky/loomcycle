package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// A deployment whose posture turns the audit stream on, or that asks for
// audit retention, needs LOOMCYCLE_SECRET_KEY to sign the chain; config load
// says so (so `loomcycle validate` does), as it does for a password gate.
func TestValidate_TheAuditStreamNeedsTheSecretKey(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml string
		env  map[string]string
		want string
	}{
		"a gate":               {yaml: "auth:\n  gates:\n    paste: { kind: token }\n", want: "LOOMCYCLE_SECRET_KEY"},
		"strict mode":          {yaml: "auth:\n  mode: strict\n  gates:\n    paste: { kind: token }\n", want: "LOOMCYCLE_SECRET_KEY"},
		"an enforcing ACL":     {env: map[string]string{"LOOMCYCLE_ACL": "enforce"}, want: "LOOMCYCLE_SECRET_KEY"},
		"asked for":            {env: map[string]string{"LOOMCYCLE_AUDIT_STORE": "1"}, want: "LOOMCYCLE_SECRET_KEY"},
		"retention":            {env: map[string]string{"LOOMCYCLE_AUDIT_RETENTION_DAYS": "30"}, want: "LOOMCYCLE_AUDIT_RETENTION_DAYS"},
		"single operator":      {},
		"a gate and a key":     {yaml: "auth:\n  gates:\n    paste: { kind: token }\n", env: map[string]string{"LOOMCYCLE_SECRET_KEY": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}},
		"retention with a key": {env: map[string]string{"LOOMCYCLE_AUDIT_RETENTION_DAYS": "30", "LOOMCYCLE_SECRET_KEY": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}},
	} {
		t.Run(name, func(t *testing.T) {
			for _, k := range []string{"LOOMCYCLE_AUTH_MODE", "LOOMCYCLE_ACL", "LOOMCYCLE_AUDIT_STORE",
				"LOOMCYCLE_AUDIT_RETENTION_DAYS", "LOOMCYCLE_SECRET_KEY", "LOOMCYCLE_SECRET_KEY_PREVIOUS"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			path := filepath.Join(t.TempDir(), "loomcycle.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Load: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("Load = %v; want an error naming %s", err, tc.want)
			}
		})
	}
}
