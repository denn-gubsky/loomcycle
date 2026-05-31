package a2a

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/a2a/sign"
)

// ecKeyPEM generates a fresh P-256 key and returns it plus its PKCS#8 PEM
// encoding, the shape the signing-key env var holds.
func ecKeyPEM(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// TestSignCardIfConfigured_AllowlistedKeySignsAndVerifies asserts that a
// card whose sign_with_key_env names an ALLOWLISTED var holding a usable
// key is served SIGNED, and the signature verifies via the self-contained
// (embedded-jwk) path the outbound client uses.
func TestSignCardIfConfigured_AllowlistedKeySignsAndVerifies(t *testing.T) {
	key, pemStr := ecKeyPEM(t)
	const envName = "LOOMCYCLE_A2A_SIGNING_KEY"
	t.Setenv(envName, pemStr)

	generated := buildAgentCard(fixtureCard(), "https://agents.example", "", false, true)
	cardCfg := fixtureCard()
	cardCfg.SignWithKeyEnv = envName
	allowlist := map[string]bool{envName: true}

	var traced []string
	signCardIfConfigured(generated, cardCfg, allowlist, func(f string, a ...any) {
		traced = append(traced, f)
	})

	if len(generated.Signatures) != 1 {
		t.Fatalf("card has %d signatures, want 1 (should be signed)", len(generated.Signatures))
	}
	if err := sign.VerifyCardSelfContained(generated); err != nil {
		t.Fatalf("served signature does not verify: %v", err)
	}
	// Sanity: the signature really is from this key (self-contained verify
	// already proves tamper-evidence; this proves key provenance).
	if err := sign.VerifyCard(generated, &key.PublicKey); err != nil {
		t.Fatalf("served signature does not verify against the signing key: %v", err)
	}
	if len(traced) != 0 {
		t.Errorf("happy-path signing emitted unexpected trace lines: %v", traced)
	}
}

// TestSignCardIfConfigured_UnallowlistedKeyServesUnsignedWithTrace asserts
// the trust-boundary floor: a sign_with_key_env naming a var NOT on the
// operator allowlist leaves the card unsigned and emits a tracing line —
// serving never fails, and a substrate-authored card cannot exfiltrate an
// arbitrary env var into a signature.
func TestSignCardIfConfigured_UnallowlistedKeyServesUnsignedWithTrace(t *testing.T) {
	_, pemStr := ecKeyPEM(t)
	const envName = "SOME_OTHER_KEY"
	t.Setenv(envName, pemStr)

	generated := buildAgentCard(fixtureCard(), "https://agents.example", "", false, true)
	cardCfg := fixtureCard()
	cardCfg.SignWithKeyEnv = envName
	allowlist := map[string]bool{} // envName NOT allowlisted

	var traced []string
	signCardIfConfigured(generated, cardCfg, allowlist, func(f string, a ...any) {
		traced = append(traced, f)
	})

	if len(generated.Signatures) != 0 {
		t.Fatalf("card was signed despite the key env not being allowlisted")
	}
	if len(traced) != 1 || !strings.Contains(traced[0], "not in allowlist") {
		t.Fatalf("trace = %v, want one 'not in allowlist' line", traced)
	}
}

// TestSignCardIfConfigured_UnsetEnvServesUnsignedWithTrace asserts an
// allowlisted-but-unset key var leaves the card unsigned + traced rather
// than failing.
func TestSignCardIfConfigured_UnsetEnvServesUnsignedWithTrace(t *testing.T) {
	const envName = "LOOMCYCLE_A2A_SIGNING_KEY"
	t.Setenv(envName, "") // allowlisted but empty

	generated := buildAgentCard(fixtureCard(), "https://agents.example", "", false, true)
	cardCfg := fixtureCard()
	cardCfg.SignWithKeyEnv = envName
	allowlist := map[string]bool{envName: true}

	var traced []string
	signCardIfConfigured(generated, cardCfg, allowlist, func(f string, a ...any) {
		traced = append(traced, f)
	})

	if len(generated.Signatures) != 0 {
		t.Fatalf("card was signed despite the key env being unset")
	}
	if len(traced) != 1 || !strings.Contains(traced[0], "unset") {
		t.Fatalf("trace = %v, want one 'unset' line", traced)
	}
}

// TestSignCardIfConfigured_NoKeyConfiguredServesUnsignedSilently asserts a
// card with no sign_with_key_env is served unsigned with NO trace line
// (unsigned by design is not a warnable condition).
func TestSignCardIfConfigured_NoKeyConfiguredServesUnsignedSilently(t *testing.T) {
	generated := buildAgentCard(fixtureCard(), "https://agents.example", "", false, true)
	cardCfg := fixtureCard() // SignWithKeyEnv == ""

	var traced []string
	signCardIfConfigured(generated, cardCfg, map[string]bool{}, func(f string, a ...any) {
		traced = append(traced, f)
	})

	if len(generated.Signatures) != 0 {
		t.Fatalf("card was signed with no key configured")
	}
	if len(traced) != 0 {
		t.Errorf("unsigned-by-design should be silent; got trace %v", traced)
	}
}

// TestSignCardIfConfigured_MalformedKeyServesUnsignedWithTrace asserts a
// malformed key env value is tolerated: card served unsigned + traced,
// never a 500.
func TestSignCardIfConfigured_MalformedKeyServesUnsignedWithTrace(t *testing.T) {
	const envName = "LOOMCYCLE_A2A_SIGNING_KEY"
	t.Setenv(envName, "-----BEGIN PRIVATE KEY-----\nnot a key\n-----END PRIVATE KEY-----")

	generated := buildAgentCard(fixtureCard(), "https://agents.example", "", false, true)
	cardCfg := fixtureCard()
	cardCfg.SignWithKeyEnv = envName

	var traced []string
	signCardIfConfigured(generated, cardCfg, map[string]bool{envName: true}, func(f string, a ...any) {
		traced = append(traced, f)
	})

	if len(generated.Signatures) != 0 {
		t.Fatalf("card was signed with a malformed key")
	}
	if len(traced) != 1 || !strings.Contains(traced[0], "failed to parse") {
		t.Fatalf("trace = %v, want one 'failed to parse' line", traced)
	}
}
