package config

import (
	"strings"
	"testing"
)

// An unknown crypto_scopes value is refused at load, naming the value and every
// scope the loader accepts. Dropped silently instead, `crypto_scopes: [tennant]`
// would boot an agent the operator believes holds tenant keys and that holds
// none.
func TestLoad_RefusesAnUnknownCryptoScope(t *testing.T) {
	_, err := loadYAMLString(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  signer:
    model: claude-sonnet-4-6
    tools: [Crypto]
    crypto_scopes: [user, tennant]
`)
	if err == nil {
		t.Fatal("expected an error for the unknown crypto scope")
	}
	for _, want := range []string{`"signer"`, "crypto_scopes[1]", `"tennant"`, "agent", "user", "tenant", "run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestLoad_AcceptsEveryCryptoScopeAndDenyAll(t *testing.T) {
	for _, list := range []string{"[agent, user, tenant, run]", `["-*"]`} {
		cfg, err := loadYAMLString(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  signer:
    model: claude-sonnet-4-6
    tools: [Crypto]
    crypto_scopes: `+list+`
`)
		if err != nil {
			t.Fatalf("crypto_scopes: %s refused: %v", list, err)
		}
		if len(cfg.Agents["signer"].CryptoScopes) == 0 {
			t.Errorf("crypto_scopes: %s did not reach the agent", list)
		}
	}
}

func TestValidateCryptoScopes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scopes  []string
		wantErr string
	}{
		{"unset", nil, ""},
		{"every scope", []string{"agent", "user", "tenant", "run"}, ""},
		{"deny-all alone", []string{"-*"}, ""},
		{"unknown", []string{"global"}, `unknown scope "global"`},
		// A list that both grants and denies says two things.
		{"deny-all beside a scope", []string{"user", "-*"}, "must be the only entry"},
		{"empty string", []string{""}, "unknown scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCryptoScopes(tc.scopes)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// Both halves of the grant are advised on at boot, and each says what actually
// happens: the tool without scopes still runs its keyless ops (so "every call
// refuses" would be false), and scopes without the tool reach nothing.
func TestAgentGateWarnings_Crypto(t *testing.T) {
	for _, tc := range []struct {
		name       string
		def        AgentDef
		wantPhrase string // "" = no crypto advisory
	}{
		{"tool without scopes", AgentDef{Tools: []string{"Crypto"}}, "only the keyless Crypto ops will work"},
		{"scopes without tool", AgentDef{Tools: []string{"Read"}, CryptoScopes: []string{"user"}}, "crypto_scopes is set but Crypto is not in tools"},
		{"tool with scopes", AgentDef{Tools: []string{"Crypto"}, CryptoScopes: []string{"agent"}}, ""},
		// An explicit deny-all with the tool is the operator saying keyless-only.
		{"tool with deny-all", AgentDef{Tools: []string{"Crypto"}, CryptoScopes: []string{"-*"}}, ""},
		// A deny-all without the tool grants nothing, so there is nothing inert.
		{"deny-all without tool", AgentDef{CryptoScopes: []string{"-*"}}, ""},
		{"neither", AgentDef{Tools: []string{"Read"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(agentGateWarnings("a", tc.def), "\n")
			if tc.wantPhrase == "" {
				if strings.Contains(got, "rypto") {
					t.Fatalf("unexpected crypto advisory:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantPhrase) {
				t.Fatalf("advisory does not say %q:\n%s", tc.wantPhrase, got)
			}
		})
	}
}

// crypto_scopes is a per-op gate inside a multi-op tool: an unset one leaves the
// keyless ops working, so a run-start "capability inert" event — which says
// every call is refused — must not fire for it.
func TestInertToolGrants_CryptoIsNotReported(t *testing.T) {
	if got := InertToolGrants(AgentDef{Tools: []string{"Crypto"}}); len(got) != 0 {
		t.Fatalf("Crypto reported as inert: %+v", got)
	}
}

// A yaml override that only grants crypto_scopes is the shape of an overlay
// meant to widen an existing agent, like the other grant-only declarations.
func TestLooksLikeAnOverrideOnly_CountsCryptoScopes(t *testing.T) {
	if !looksLikeAnOverrideOnly(AgentDef{CryptoScopes: []string{"user"}}) {
		t.Error("a crypto_scopes-only declaration was not recognised as an override")
	}
}
