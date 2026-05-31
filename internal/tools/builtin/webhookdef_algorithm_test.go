package builtin

import "testing"

// TestValidateWebhookDef_RejectsUnsupportedAlgorithm pins the never-silently-
// degrade fix: auth.algorithm is carried through every Def site but the
// receiver only implements SHA-256, so a non-sha256 value must be refused at
// validation rather than accepted-then-ignored (which would 401 the sender's
// valid signatures with no diagnostic). Regression-grade: pre-fix
// validateWebhookDef had no Algorithm branch and accepted any value.
func TestValidateWebhookDef_RejectsUnsupportedAlgorithm(t *testing.T) {
	base := func(alg string) mergedWebhookDef {
		return mergedWebhookDef{
			Delivery: "spawn",
			Agent:    "responder",
			Auth: mergedWebhookAuth{
				Kind:             "hmac",
				SigningSecretEnv: "WH_SECRET",
				Algorithm:        alg,
			},
		}
	}
	for _, alg := range []string{"sha512", "sha1", "md5", "garbage"} {
		if err := validateWebhookDef(base(alg)); err == nil {
			t.Errorf("algorithm %q was accepted; want a validation error", alg)
		}
	}
	for _, alg := range []string{"", "sha256", "SHA256", " sha256 "} {
		if err := validateWebhookDef(base(alg)); err != nil {
			t.Errorf("algorithm %q rejected (%v); want accepted", alg, err)
		}
	}
}
