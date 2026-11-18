package config

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC EZ P5: acl_modes raises a tenant's mode above LOOMCYCLE_ACL, never
// lowers it, ordered by what each mode refuses (shadow < compat < enforce).
func TestResolveACLModes_AnEntryMayOnlyBeStricter(t *testing.T) {
	for _, tc := range []struct {
		deployment string
		modes      map[string]string
		want       map[string]authz.Mode
		wantErr    string
	}{
		{"", map[string]string{"acme": "enforce"}, map[string]authz.Mode{"acme": authz.ModeEnforce}, ""},
		{"compat", map[string]string{"acme": "Compat"}, map[string]authz.Mode{"acme": authz.ModeCompat}, ""},
		{"shadow", map[string]string{"acme": "compat", "beta": "enforce"},
			map[string]authz.Mode{"acme": authz.ModeCompat, "beta": authz.ModeEnforce}, ""},
		// Shadow honours no deny, so it is below compat, not between compat
		// and enforce: a tenant cannot drop its denies through the map.
		{"compat", map[string]string{"acme": "shadow"}, nil, "refuses less than the deployment's LOOMCYCLE_ACL=compat"},
		{"enforce", map[string]string{"acme": "compat"}, nil, "refuses less than the deployment's LOOMCYCLE_ACL=enforce"},
		{"", map[string]string{"acme": "strict"}, nil, `acl_modes.acme: "strict" is not one of`},
		{"", map[string]string{"acme": ""}, nil, "acl_modes.acme: a mode is required"},
		{"", map[string]string{"": "enforce"}, nil, `acl_modes: "" is not a tenant id`},
		{"", map[string]string{" acme": "enforce"}, nil, `acl_modes: " acme" is not a tenant id`},
		{"bogus", map[string]string{"acme": "enforce"}, nil, "LOOMCYCLE_ACL"},
	} {
		c := &Config{Env: Env{ACLMode: tc.deployment}, ACLModes: tc.modes}
		err := ResolveACLModes(c)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("LOOMCYCLE_ACL=%q %v: err = %v, want %q", tc.deployment, tc.modes, err, tc.wantErr)
			}
			if c.ResolvedACLModes != nil {
				t.Errorf("LOOMCYCLE_ACL=%q %v: a refused map was kept: %v", tc.deployment, tc.modes, c.ResolvedACLModes)
			}
			continue
		}
		if err != nil {
			t.Errorf("LOOMCYCLE_ACL=%q %v: %v", tc.deployment, tc.modes, err)
			continue
		}
		if len(c.ResolvedACLModes) != len(tc.want) {
			t.Errorf("LOOMCYCLE_ACL=%q %v: resolved %v, want %v", tc.deployment, tc.modes, c.ResolvedACLModes, tc.want)
		}
		for k, v := range tc.want {
			if c.ResolvedACLModes[k] != v {
				t.Errorf("LOOMCYCLE_ACL=%q %v: %s = %s, want %s", tc.deployment, tc.modes, k, c.ResolvedACLModes[k], v)
			}
		}
	}
	c := &Config{}
	if err := ResolveACLModes(c); err != nil || c.ResolvedACLModes != nil {
		t.Errorf("no acl_modes: %v %v, want nothing resolved", err, c.ResolvedACLModes)
	}
}

// A tenant raised above compat makes the audit stream mandatory, as
// LOOMCYCLE_ACL above compat does, and the reason names the entry.
func TestResolveACLModes_ATenantAboveCompatRequiresTheAuditStream(t *testing.T) {
	c := &Config{ACLModes: map[string]string{"beta": "compat", "acme": "enforce"}}
	if got := c.AuditRequiredReason(0); got != "acl_modes.acme is enforce" {
		t.Errorf("reason = %q, want acl_modes.acme is enforce", got)
	}
	if !c.auditStoreOn(false) {
		t.Error("auditStoreOn: a tenant on enforce must turn the store-backed stream on")
	}
	c = &Config{ACLModes: map[string]string{"beta": "compat"}}
	if got := c.AuditRequiredReason(0); got != "" {
		t.Errorf("a compat entry under a compat deployment: reason = %q, want none", got)
	}
}

// The yaml key reaches the resolved map through Load, and a lowering entry
// fails the load (so `loomcycle validate` and a reload refuse it too).
func TestLoad_ACLModesAreValidatedAtLoad(t *testing.T) {
	// Shadow requires the audit stream, which signs with a derived key.
	t.Setenv("LOOMCYCLE_SECRET_KEY", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	t.Setenv("LOOMCYCLE_ACL", "shadow")
	c, err := loadYAMLString(t, "acl_modes:\n  acme: compat\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.ResolvedACLModes["acme"] != authz.ModeCompat {
		t.Errorf("resolved = %v, want acme compat", c.ResolvedACLModes)
	}
	t.Setenv("LOOMCYCLE_ACL", "enforce")
	if _, err := loadYAMLString(t, "acl_modes:\n  acme: compat\n"); err == nil || !strings.Contains(err.Error(), "acl_modes.acme") {
		t.Errorf("a lowering entry: err = %v, want a refusal naming acl_modes.acme", err)
	}
}
