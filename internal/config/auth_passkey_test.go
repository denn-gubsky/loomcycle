package config

import (
	"strings"
	"testing"
)

func TestPasskeyRP_IsThePublicURLsHostAndOrigin(t *testing.T) {
	for in, want := range map[string]AuthPasskeyRP{
		"https://LC.Example.com":        {ID: "lc.example.com", Origin: "https://lc.example.com"},
		"https://lc.example.com:8443/x": {ID: "lc.example.com", Origin: "https://lc.example.com:8443"},
		"http://localhost:8080":         {ID: "localhost", Origin: "http://localhost:8080"},
	} {
		got, err := passkeyRP(in)
		if err != nil || *got != want {
			t.Errorf("%s: %+v %v, want %+v", in, got, err, want)
		}
	}
	// WebAuthn needs a secure context and a domain name.
	for _, in := range []string{"", "http://lc.example.com", "https://192.168.0.77", "https://[::1]", "http://127.0.0.1:8080", "lc.example.com"} {
		if _, err := passkeyRP(in); err == nil {
			t.Errorf("%q: accepted as a relying party", in)
		}
	}
}

func passkeyConfig(publicURL string, gates map[string]AuthGateConfig) *Config {
	c := &Config{Auth: AuthConfig{Gates: gates}}
	c.Env.PublicURL = publicURL
	return c
}

func TestResolveAuth_APasskeyGateNeedsARelyingPartyAndTakesOnlyALabel(t *testing.T) {
	keys := map[string]AuthGateConfig{"keys": {Kind: AuthGateKindPasskey, Label: "Passkey"}}
	c := passkeyConfig("https://lc.example.com", keys)
	if err := resolveAuth(c); err != nil {
		t.Fatalf("a passkey gate: %v", err)
	}
	if rp := c.ResolvedAuth.Passkeys; rp == nil || rp.ID != "lc.example.com" {
		t.Fatalf("relying party = %+v", rp)
	}
	if g, ok := c.ResolvedAuth.PasskeyGate(); !ok || g.Name != "keys" {
		t.Fatalf("PasskeyGate = %+v %v", g, ok)
	}
	for _, url := range []string{"", "http://lc.example.com", "https://10.0.0.1"} {
		if err := resolveAuth(passkeyConfig(url, keys)); err == nil || !strings.Contains(err.Error(), "relying party") {
			t.Errorf("public URL %q: err = %v, want a relying-party refusal", url, err)
		}
	}
	two := map[string]AuthGateConfig{"a": {Kind: AuthGateKindPasskey}, "b": {Kind: AuthGateKindPasskey}}
	if err := resolveAuth(passkeyConfig("https://lc.example.com", two)); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Errorf("two passkey gates: %v", err)
	}
	trusted := map[string]AuthGateConfig{"keys": {Kind: AuthGateKindPasskey, Trusted: true}}
	if err := resolveAuth(passkeyConfig("https://lc.example.com", trusted)); err == nil {
		t.Error("a passkey gate accepted trusted")
	}
	// A passkey cannot be a strict deployment's only way in: it is
	// registered from another gate's session.
	strict := passkeyConfig("https://lc.example.com", keys)
	strict.Auth.Mode = AuthModeStrict
	if err := resolveAuth(strict); err == nil || !strings.Contains(err.Error(), "interactive gate") {
		t.Errorf("strict with only a passkey gate: %v", err)
	}
}

// A password gate offers passkeys when the public URL can be a relying
// party, and simply does not otherwise (it predates them).
func TestResolveAuth_APasswordGateOffersPasskeysOnlyWithARelyingParty(t *testing.T) {
	c := &Config{Auth: AuthConfig{Gates: map[string]AuthGateConfig{"local": {Kind: AuthGateKindPassword}}}}
	c.Env.PublicURL = "https://192.168.0.77"
	c.Env.SecretKey = "a-test-secret-key-that-is-long-enough-0123456789"
	if err := resolveAuth(c); err != nil {
		t.Fatalf("password gate on an IP: %v", err)
	}
	if c.ResolvedAuth.Passkeys != nil {
		t.Fatalf("passkeys offered on an IP address: %+v", c.ResolvedAuth.Passkeys)
	}
	c.Env.PublicURL = "https://lc.example.com"
	if err := resolveAuth(c); err != nil || c.ResolvedAuth.Passkeys == nil {
		t.Fatalf("password gate on a domain: %v %+v", err, c.ResolvedAuth.Passkeys)
	}
}
