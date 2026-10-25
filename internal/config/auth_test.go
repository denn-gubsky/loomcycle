package config

import (
	"strings"
	"testing"
	"time"
)

func TestResolveAuth_DefaultsToTokenModeWithImplicitTokenGate(t *testing.T) {
	c := &Config{}
	if err := resolveAuth(c); err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	p := c.ResolvedAuth
	if p.ModeName() != AuthModeToken || p.SessionIdle() != 8*time.Hour || p.SessionAbsolute() != 168*time.Hour || p.ReauthWindow != 15*time.Minute {
		t.Errorf("policy = %+v", p)
	}
	g, ok := p.TokenGate()
	if !ok || g.Name != "token" || g.Kind != AuthGateKindToken {
		t.Errorf("token mode must offer the implicit token gate; got %+v, %v", g, ok)
	}
}

func TestResolveAuth_ZeroPolicyIsTokenMode(t *testing.T) {
	var p AuthPolicy
	if p.Strict() || p.ModeName() != AuthModeToken || p.SessionIdle() != DefaultSessionIdle {
		t.Errorf("zero policy = %+v", p)
	}
	if _, ok := p.TokenGate(); !ok {
		t.Error("zero policy must offer the token gate")
	}
}

func TestResolveAuth_StrictWithoutInteractiveGateFails(t *testing.T) {
	c := &Config{Auth: AuthConfig{Mode: "strict"}}
	err := resolveAuth(c)
	if err == nil || !strings.Contains(err.Error(), "interactive gate") {
		t.Fatalf("err = %v, want the lock-out refusal", err)
	}
	// The same refusal holds through the env default.
	c = &Config{Env: Env{AuthMode: "strict"}}
	if err := resolveAuth(c); err == nil {
		t.Fatal("LOOMCYCLE_AUTH_MODE=strict with no gate loaded")
	}
}

func TestResolveAuth_StrictListsOnlyConfiguredGates(t *testing.T) {
	c := &Config{Auth: AuthConfig{Mode: "strict", Gates: map[string]AuthGateConfig{"paste": {Kind: "token", Label: "Paste"}}}}
	if err := resolveAuth(c); err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	gates := c.ResolvedAuth.LoginGates()
	if len(gates) != 1 || gates[0] != (AuthGate{Name: "paste", Kind: "token", Label: "Paste"}) {
		t.Errorf("strict gates = %+v, want only the listed paste gate", gates)
	}
}

func TestResolveAuth_ConfiguringAGateNeverFlipsTheMode(t *testing.T) {
	c := &Config{Auth: AuthConfig{Gates: map[string]AuthGateConfig{"paste": {Kind: "token"}}}}
	if err := resolveAuth(c); err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if c.ResolvedAuth.Strict() {
		t.Error("listing a gate turned strict mode on (decision 1)")
	}
	if g, _ := c.ResolvedAuth.TokenGate(); g.Name != "paste" {
		t.Errorf("token gate = %+v, want the listed one to replace the implicit one", g)
	}
}

func TestResolveAuth_EnvIsTheDefaultAndConfigMayNotLowerIt(t *testing.T) {
	gates := map[string]AuthGateConfig{"paste": {Kind: "token"}}
	c := &Config{Env: Env{AuthMode: "strict"}, Auth: AuthConfig{Gates: gates}}
	if err := resolveAuth(c); err != nil || !c.ResolvedAuth.Strict() {
		t.Fatalf("env strict as default: err=%v policy=%+v", err, c.ResolvedAuth)
	}
	c = &Config{Env: Env{AuthMode: "strict"}, Auth: AuthConfig{Mode: "token", Gates: gates}}
	if err := resolveAuth(c); err == nil || !strings.Contains(err.Error(), "lower") {
		t.Errorf("config token under env strict: err = %v, want a refusal", err)
	}
	c = &Config{Env: Env{AuthMode: "token"}, Auth: AuthConfig{Mode: "strict", Gates: gates}}
	if err := resolveAuth(c); err != nil || !c.ResolvedAuth.Strict() {
		t.Errorf("config may raise the env default: err=%v policy=%+v", err, c.ResolvedAuth)
	}
}

func TestResolveAuth_RefusesBadValues(t *testing.T) {
	cases := map[string]AuthConfig{
		"open is not a mode":   {Mode: "open"},
		"unknown mode":         {Mode: "lax"},
		"later kind":           {Gates: map[string]AuthGateConfig{"google": {Kind: "oidc"}}},
		"unknown kind":         {Gates: map[string]AuthGateConfig{"x": {Kind: "magic"}}},
		"missing kind":         {Gates: map[string]AuthGateConfig{"x": {}}},
		"bad name":             {Gates: map[string]AuthGateConfig{"Bad Name": {Kind: "token"}}},
		"two token gates":      {Gates: map[string]AuthGateConfig{"a": {Kind: "token"}, "b": {Kind: "token"}}},
		"bad duration":         {Session: AuthSessionConfig{Idle: "soon"}},
		"negative duration":    {Session: AuthSessionConfig{Absolute: "-1h"}},
		"idle beyond absolute": {Session: AuthSessionConfig{Idle: "48h", Absolute: "24h"}},
		"zero reauth window":   {Session: AuthSessionConfig{ReauthWindow: "0s"}},
	}
	for name, ac := range cases {
		if err := resolveAuth(&Config{Auth: ac}); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	err := resolveAuth(&Config{Auth: AuthConfig{Gates: map[string]AuthGateConfig{"github": {Kind: "oauth"}}}})
	if err == nil || !strings.Contains(err.Error(), "not supported in this release") {
		t.Errorf("a later-phase kind must say it is not supported yet; got %v", err)
	}
}

func TestLoad_StrictWithoutGateFailsTheLoad(t *testing.T) {
	_, err := LoadLayers(Layer{Name: "auth.yaml", Data: []byte("auth:\n  mode: strict\n")})
	if err == nil || !strings.Contains(err.Error(), "interactive gate") {
		t.Fatalf("load err = %v, want the strict lock-out refusal", err)
	}
	cfg, err := LoadLayers(Layer{Name: "auth.yaml", Data: []byte("auth:\n  mode: strict\n  session: {idle: 1h}\n  gates:\n    paste: {kind: token}\n")})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.ResolvedAuth.Strict() || cfg.ResolvedAuth.SessionIdle() != time.Hour {
		t.Errorf("resolved = %+v", cfg.ResolvedAuth)
	}
}

func TestAuthModeLowered_OnlyStrictToTokenLowers(t *testing.T) {
	strict, token := AuthPolicy{Mode: AuthModeStrict}, AuthPolicy{Mode: AuthModeToken}
	if !AuthModeLowered(strict, token) || AuthModeLowered(token, strict) || AuthModeLowered(strict, strict) || AuthModeLowered(token, AuthPolicy{}) {
		t.Error("AuthModeLowered disagrees with strict > token")
	}
}
