package config

import "testing"

// TestIsSecretEnvName_DSN: a *_DSN name (which embeds the DB password) is now
// classified secret so the transcript redactor masks its value.
func TestIsSecretEnvName_DSN(t *testing.T) {
	secret := []string{"LOOMCYCLE_PG_DSN", "PG_DSN", "SOME_DSN"}
	for _, n := range secret {
		if !IsSecretEnvName(n) {
			t.Errorf("IsSecretEnvName(%q) = false, want true (DSN embeds a DB password)", n)
		}
	}
	for _, n := range []string{"LOOMCYCLE_LISTEN_ADDR", "LOOMCYCLE_BASE_URL"} {
		if IsSecretEnvName(n) {
			t.Errorf("IsSecretEnvName(%q) = true, want false", n)
		}
	}
}

// TestResolvePrincipals_DeniesTokenEnvInterpolation is the regression for the
// per-principal bearer exfil: a declared principal's token_env passed
// ExpandEnvAllowed (LOOMCYCLE_*) and wasn't a built-in infra secret, so
// ${LOOMCYCLE_..._TOKEN} stayed interpolatable into an MCPServerDef field and
// could exfil the bearer. resolvePrincipals must add it to the expand denylist.
func TestResolvePrincipals_DeniesTokenEnvInterpolation(t *testing.T) {
	const envName = "LOOMCYCLE_TESTPRIN_TOKEN"
	t.Setenv(envName, "prin_secret_value")
	// The fix mutates the process-global expand denylist; keep the test isolated.
	defer delete(expandDenyNames, envName)

	// Sanity: before resolution the name IS interpolatable (LOOMCYCLE_* allowed).
	if got := expandEnv("${" + envName + "}"); got != "prin_secret_value" {
		t.Fatalf("precondition: expandEnv = %q, want it to expand before the deny is applied", got)
	}

	c := &Config{
		Principals: map[string]PrincipalDef{
			"alice": {Subject: "alice", TokenEnv: envName, Scopes: []string{"runs:read"}},
		},
	}
	if err := resolvePrincipals(c); err != nil {
		t.Fatalf("resolvePrincipals: %v", err)
	}

	got := expandEnv("${" + envName + "}")
	if got == "prin_secret_value" {
		t.Errorf("token_env %s was interpolated (bearer exfil not prevented)", envName)
	}
	if got != "${"+envName+"}" {
		t.Errorf("expandEnv(${%s}) = %q, want the literal (interpolation denied)", envName, got)
	}
}
