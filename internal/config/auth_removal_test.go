package config

import (
	"strings"
	"testing"
	"time"
)

func removalCfg(on AuthOnRemovedConfig, dormant string) *Config {
	return &Config{Auth: AuthConfig{Lifecycle: AuthLifecycleConfig{OnRemoved: on, DormantAfter: dormant}}}
}

func TestResolveRemoval_DefaultsAreUnbindAfterAWeekAndEraseNever(t *testing.T) {
	c := removalCfg(AuthOnRemovedConfig{}, "")
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	r := c.ResolvedAuth.RemovalPolicy()
	if r.UnbindAfter != 7*24*time.Hour || r.EraseAfter != 0 || r.DormantAfter != 0 || r.Reactivate != ReactivateOperator {
		t.Fatalf("defaults = %+v", r)
	}
	// A policy built without validate (tests) gets the same defaults.
	if z := (AuthPolicy{}).RemovalPolicy(); z.UnbindAfter != DefaultUnbindAfter || z.Reactivate != ReactivateOperator {
		t.Fatalf("zero policy = %+v", z)
	}
}

func TestResolveRemoval_ParsesDaysDurationsAndNever(t *testing.T) {
	c := removalCfg(AuthOnRemovedConfig{Disable: "at_once", UnbindAfter: "3d", EraseAfter: "720h"}, "90d")
	c.Env.AuditLogPath = "/tmp/audit.jsonl"
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	r := c.ResolvedAuth.RemovalPolicy()
	if r.UnbindAfter != 72*time.Hour || r.EraseAfter != 720*time.Hour || r.DormantAfter != 90*24*time.Hour {
		t.Fatalf("policy = %+v", r)
	}
	c = removalCfg(AuthOnRemovedConfig{EraseAfter: "never"}, "never")
	if err := resolveAuth(c); err != nil || c.ResolvedAuth.Removal.EraseAfter != 0 || c.ResolvedAuth.Removal.DormantAfter != 0 {
		t.Fatalf("never: %+v %v", c.ResolvedAuth.Removal, err)
	}
}

// Each removal key refuses what it cannot honour, at load.
func TestResolveRemoval_RefusesWhatItCannotHonour(t *testing.T) {
	for name, tc := range map[string]struct {
		on      AuthOnRemovedConfig
		dormant string
		audit   bool
		want    string
	}{
		"disable later":            {on: AuthOnRemovedConfig{Disable: "after_7d"}, want: "at_once"},
		"unbind never":             {on: AuthOnRemovedConfig{UnbindAfter: "never"}, want: "never is not allowed"},
		"unbind garbage":           {on: AuthOnRemovedConfig{UnbindAfter: "soon"}, want: "not a duration"},
		"erase before unbind":      {on: AuthOnRemovedConfig{UnbindAfter: "7d", EraseAfter: "1d"}, audit: true, want: "never skip"},
		"erase with no audit sink": {on: AuthOnRemovedConfig{EraseAfter: "30d"}, want: "LOOMCYCLE_AUDIT_LOG_PATH"},
		"erase zero":               {on: AuthOnRemovedConfig{EraseAfter: "0s"}, audit: true, want: "positive"},
		// A typo for 7d must not erase a revoked user within the hour.
		"erase too soon":     {on: AuthOnRemovedConfig{UnbindAfter: "0s", EraseAfter: "7m"}, audit: true, want: "at least"},
		"reactivate auto":    {on: AuthOnRemovedConfig{Reactivate: "auto"}, want: "not supported yet"},
		"reactivate unknown": {on: AuthOnRemovedConfig{Reactivate: "sometimes"}, want: "want operator"},
		"dormant too short":  {dormant: "90m", want: "at least"},
		"dormant garbage":    {dormant: "quarterly", want: "not a duration"},
	} {
		t.Run(name, func(t *testing.T) {
			c := removalCfg(tc.on, tc.dormant)
			if tc.audit {
				c.Env.AuditLogPath = "/tmp/audit.jsonl"
			}
			err := resolveAuth(c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
