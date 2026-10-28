package config

import (
	"strings"
	"testing"
)

func strictWithUserTokens(mode, v string) *Config {
	return &Config{Auth: AuthConfig{Mode: mode, Strict: AuthStrictConfig{UserTokens: v},
		Gates: map[string]AuthGateConfig{"paste": {Kind: AuthGateKindToken}}}}
}

func TestResolveAuth_GateOnlyLoadsInStrictModeOnly(t *testing.T) {
	c := strictWithUserTokens(AuthModeStrict, UserTokensGateOnly)
	if err := resolveAuth(c); err != nil {
		t.Fatalf("strict + gate_only: %v", err)
	}
	if !c.ResolvedAuth.UserTokensGateOnly {
		t.Fatal("gate_only did not reach the resolved policy")
	}
	// In token mode it would protect nothing, so it does not load.
	if err := resolveAuth(strictWithUserTokens(AuthModeToken, UserTokensGateOnly)); err == nil ||
		!strings.Contains(err.Error(), "only in auth.mode: strict") {
		t.Fatalf("token mode + gate_only: err = %v, want a refusal", err)
	}
	for _, v := range []string{"", UserTokensAny} {
		c := strictWithUserTokens(AuthModeStrict, v)
		if err := resolveAuth(c); err != nil || c.ResolvedAuth.UserTokensGateOnly {
			t.Fatalf("user_tokens %q: err %v, gate_only %v", v, err, c.ResolvedAuth.UserTokensGateOnly)
		}
	}
	if err := resolveAuth(strictWithUserTokens(AuthModeStrict, "sometimes")); err == nil {
		t.Fatal("an unknown user_tokens value must fail the load")
	}
}
