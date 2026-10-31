package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func passwordGateConfig(mode string, g AuthGateConfig) *Config {
	g.Kind = AuthGateKindPassword
	c := &Config{Auth: AuthConfig{Mode: mode, Gates: map[string]AuthGateConfig{"local": g}}}
	c.Env.SecretKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	c.Env.PublicURL = "https://lc.example"
	return c
}

func TestResolveAuth_PasswordGateDefaults(t *testing.T) {
	c := passwordGateConfig("", AuthGateConfig{})
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	g := c.ResolvedAuth.Gates[0]
	pw := g.Password
	if g.Kind != AuthGateKindPassword || pw == nil || g.Label != "local" {
		t.Fatalf("gate = %+v", g)
	}
	if pw.RequireMFA || pw.InviteDefaultTTL != 168*time.Hour || pw.InviteMaxTTL != 720*time.Hour || pw.ResetTTL != time.Hour ||
		pw.MaxFailures != 10 || pw.Lockout != 15*time.Minute || pw.Trusted || pw.ManagerMayRecoverOwners || pw.NewUserGroups != nil {
		t.Fatalf("token-mode defaults = %+v", pw)
	}
	// D3: MFA is required by default in strict mode, and a password gate is
	// an interactive gate strict mode accepts on its own.
	c = passwordGateConfig("strict", AuthGateConfig{})
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	if !c.ResolvedAuth.Gates[0].Password.RequireMFA {
		t.Fatal("strict mode must default require_mfa to true")
	}
	off := false
	c = passwordGateConfig("strict", AuthGateConfig{RequireMFA: &off})
	if err := resolveAuth(c); err != nil || c.ResolvedAuth.Gates[0].Password.RequireMFA {
		t.Fatalf("explicit require_mfa: false: %v", err)
	}
}

// `loomcycle validate` fails a password gate that could not issue a link or
// key a hash, rather than the first invite failing at run time.
func TestResolveAuth_PasswordGateNeedsTheSecretKeyAndThePublicURL(t *testing.T) {
	c := passwordGateConfig("", AuthGateConfig{})
	c.Env.PublicURL = ""
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_PUBLIC_URL") {
		t.Fatalf("no public URL: %v", err)
	}
	c = passwordGateConfig("", AuthGateConfig{})
	c.Env.SecretKey = ""
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_SECRET_KEY") {
		t.Fatalf("no secret key: %v", err)
	}
}

func TestResolveAuth_PasswordGateRefusesBadValues(t *testing.T) {
	tenant := "tenant"
	for name, g := range map[string]AuthGateConfig{
		"default over max":       {Invites: AuthGateInvitesConfig{DefaultTTL: "200h", MaxTTL: "100h"}},
		"max over 30d":           {Invites: AuthGateInvitesConfig{MaxTTL: "800h"}},
		"reset over 24h":         {Resets: AuthGateResetsConfig{TTL: "25h"}},
		"foreign http return":    {AllowedReturnTo: []string{"http://site.example/"}},
		"relative return":        {AllowedReturnTo: []string{"/welcome"}},
		"zero lockout":           {Password: AuthGatePasswordConfig{LockoutMinutes: -1}},
		"missing denylist":       {Password: AuthGatePasswordConfig{DenylistFile: "/nonexistent/denylist.txt"}},
		"bad new_user mode":      {NewUser: AuthGateNewUserConfig{AccessMode: "admin"}},
		"tenant floor untrusted": {NewUser: AuthGateNewUserConfig{AccessMode: tenant}},
	} {
		if err := resolveAuth(passwordGateConfig("", g)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	list := filepath.Join(t.TempDir(), "deny.txt")
	if err := os.WriteFile(list, []byte("hunter2hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := AuthGateConfig{Trusted: true, AllowedReturnTo: []string{"https://site.example/welcome", "http://localhost:3000/"},
		Password: AuthGatePasswordConfig{DenylistFile: list, MaxFailures: 5, LockoutMinutes: 30},
		NewUser:  AuthGateNewUserConfig{AccessMode: tenant, Groups: &[]string{"staff"}}}
	c := passwordGateConfig("", ok)
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	pw := c.ResolvedAuth.Gates[0].Password
	if pw.MaxFailures != 5 || pw.Lockout != 30*time.Minute || pw.DenylistFile != list || len(pw.NewUserGroups) != 1 || len(pw.AllowedReturnTo) != 2 {
		t.Fatalf("resolved = %+v", pw)
	}
}

// The password gate's keys on another kind are a mistake the operator should
// hear about, not silently dropped.
func TestResolveAuth_PasswordKeysOnATokenGateAreRefused(t *testing.T) {
	c := &Config{Auth: AuthConfig{Gates: map[string]AuthGateConfig{"paste": {Kind: "token", Trusted: true}}}}
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "only to kind: password") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveAuth_TrustedProxiesAndClientIPHeader(t *testing.T) {
	c := &Config{Auth: AuthConfig{TrustedProxies: []string{"172.18.0.0/16", "127.0.0.1"}, ClientIPHeader: "cf-connecting-ip"}}
	if err := resolveAuth(c); err != nil {
		t.Fatal(err)
	}
	p := c.ResolvedAuth
	if len(p.TrustedProxies) != 2 || p.TrustedProxies[1].Bits() != 32 || p.ClientIPHeader != "Cf-Connecting-Ip" {
		t.Fatalf("resolved = %+v", p)
	}
	for name, a := range map[string]AuthConfig{
		"header without proxies": {ClientIPHeader: "CF-Connecting-IP"},
		"bad cidr":               {TrustedProxies: []string{"not-a-network"}},
		"bad header name":        {TrustedProxies: []string{"10.0.0.0/8"}, ClientIPHeader: "X Bad: header"},
	} {
		if err := resolveAuth(&Config{Auth: a}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// new_user.status decides what a website's invite creates: active by default,
// pending to hold the person for an operator's approval. Any other value is
// refused, and the key on a gate that is not a password gate is too.
func TestResolveAuth_PasswordGateNewUserStatus(t *testing.T) {
	for in, want := range map[string]string{"": "active", "active": "active", "pending": "pending"} {
		c := passwordGateConfig("", AuthGateConfig{NewUser: AuthGateNewUserConfig{Status: in}})
		if err := resolveAuth(c); err != nil {
			t.Fatalf("status %q: %v", in, err)
		}
		if got := c.ResolvedAuth.Gates[0].Password.NewUserStatus; got != want {
			t.Errorf("status %q resolved to %q, want %q", in, got, want)
		}
	}
	if err := resolveAuth(passwordGateConfig("", AuthGateConfig{NewUser: AuthGateNewUserConfig{Status: "disabled"}})); err == nil ||
		!strings.Contains(err.Error(), "new_user.status") {
		t.Fatalf("status disabled: %v", err)
	}
	c := &Config{Auth: AuthConfig{Gates: map[string]AuthGateConfig{"paste": {Kind: "token", NewUser: AuthGateNewUserConfig{Status: "pending"}}}}}
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "only to kind: password") {
		t.Fatalf("status on a token gate: %v", err)
	}
}

// A gate key this version does not know fails the load instead of being
// dropped: a config written for a newer runtime (new_user.status: pending on
// a runtime that cannot hold anyone for approval) must not start fail-open.
// Keys of the gate kinds inlined into the entry (OAuth, external, device)
// are still known.
func TestLoad_UnknownGateKeysFailTheLoad(t *testing.T) {
	t.Setenv("LOOMCYCLE_SECRET_KEY", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	t.Setenv("LOOMCYCLE_PUBLIC_URL", "https://lc.example")
	for name, gate := range map[string]string{
		"gate level":     "kind: password\n      require_approval: true",
		"new_user block": "kind: password\n      new_user: { access_mode: isolated, approval: pending }",
	} {
		_, err := loadYAMLString(t, "auth:\n  gates:\n    local:\n      "+gate+"\n")
		if err == nil || !strings.Contains(err.Error(), "unknown key") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	_, err := loadYAMLString(t, "auth:\n  gates:\n    gh:\n      kind: oauth\n      preset: github\n      client_id: x\n      client_secret_env: LOOMCYCLE_GATE_GH_SECRET\n      allow_any_account: true\n")
	if err != nil && strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("an inlined OAuth key was refused: %v", err)
	}
	if _, err := loadYAMLString(t, "auth:\n  gates:\n    local:\n      kind: password\n      new_user: { access_mode: isolated, status: pending }\n"); err != nil {
		t.Fatalf("known keys refused: %v", err)
	}
}
