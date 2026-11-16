package config

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/credential"
)

func pepperTestKey(b byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

// RFC GN: LOOMCYCLE_OPERATOR_TOKEN_PEPPER is legacy. Beside a secret key it
// is kept for lookups only; new hashes use the derived pepper.
func TestEnvTokenHasher_ExplicitPepperBesideTheSecretKeyIsLegacy(t *testing.T) {
	e := &Env{OperatorTokenPepper: "pep", SecretKey: pepperTestKey(1)}
	h, err := e.TokenHasher()
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := credential.DeriveTokenPepper(pepperTestKey(1))
	if h.Source() != auth.PepperDerived || h.Hash("lct_x") != auth.NewTokenHasher("", cur, "").Hash("lct_x") {
		t.Fatalf("source=%q: new hashes must use the pepper derived from LOOMCYCLE_SECRET_KEY", h.Source())
	}
	if !h.HasLegacy() || h.LegacyHash("lct_x") != auth.HashToken("pep", "lct_x") {
		t.Errorf("the explicit pepper must stay usable for lookups of its own rows")
	}
}

// Alone, the explicit pepper hashes exactly as before, so an instance with no
// secret key keeps its tokens.
func TestEnvTokenHasher_ExplicitPepperAloneIsUnchanged(t *testing.T) {
	h, err := (&Env{OperatorTokenPepper: "pep"}).TokenHasher()
	if err != nil {
		t.Fatal(err)
	}
	if h.Source() != auth.PepperExplicit || h.Hash("lct_x") != auth.HashToken("pep", "lct_x") || h.HasLegacy() {
		t.Errorf("source=%q: the explicit pepper alone must be used unchanged", h.Source())
	}
}

func TestEnvTokenHasher_DerivesFromTheSecretKeyAndThePreviousOne(t *testing.T) {
	e := &Env{SecretKey: pepperTestKey(1), SecretKeyPrevious: pepperTestKey(2)}
	h, err := e.TokenHasher()
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := credential.DeriveTokenPepper(pepperTestKey(1))
	prev, _ := credential.DeriveTokenPepper(pepperTestKey(2))
	want := auth.NewTokenHasher("", cur, prev)
	if h.Source() != auth.PepperDerived || h.Hash("lct_x") != want.Hash("lct_x") {
		t.Fatalf("source=%q: not the pepper derived from LOOMCYCLE_SECRET_KEY", h.Source())
	}
	if got := h.PreviousHashes("lct_x"); len(got) != 2 || got[0] != want.PreviousHashes("lct_x")[0] {
		t.Errorf("previous forms = %q, want those of the pepper LOOMCYCLE_SECRET_KEY_PREVIOUS derives", got)
	}
}

func TestEnvTokenHasher_MalformedSecretKeyIsAnError(t *testing.T) {
	if _, err := (&Env{SecretKey: "short"}).TokenHasher(); err == nil {
		t.Error("a malformed LOOMCYCLE_SECRET_KEY must not fall back to no pepper")
	}
	if _, err := (&Env{SecretKey: pepperTestKey(1), SecretKeyPrevious: "short"}).TokenHasher(); err == nil {
		t.Error("a malformed LOOMCYCLE_SECRET_KEY_PREVIOUS must be an error")
	}
}
