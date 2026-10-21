package auth

import (
	"strings"
	"testing"
)

func TestTokenHasher_ExplicitPepperKeepsTheUntaggedFormAndHasNoOlderForms(t *testing.T) {
	h := NewTokenHasher("pep", "derived", "prev")
	if h.Source() != PepperExplicit {
		t.Fatalf("source = %q, want explicit", h.Source())
	}
	if got := h.Hash("lct_x"); got != HashToken("pep", "lct_x") {
		t.Errorf("explicit-pepper hash changed form: %q", got)
	}
	if h.HasPrevious() || h.PreviousHashes("lct_x") != nil || h.WrappedHash("lct_x") != "" || h.Wrap("x") != "" {
		t.Error("an explicit pepper must have no older forms")
	}
	if h.Form(HashToken("", "lct_x")) != FormCurrent {
		t.Error("under an explicit pepper every stored hash is current")
	}
}

func TestTokenHasher_DerivedPepperTagsBothForms(t *testing.T) {
	h := NewTokenHasher("", "derived", "")
	if h.Source() != PepperDerived {
		t.Fatalf("source = %q, want derived", h.Source())
	}
	// Both computed outside Go: tag + ":" + hex(SHA-256(pepper)[:8]) + ":" +
	// hex(HMAC-SHA256(key=pepper, msg)), msg = the token for p1 and the
	// hex text of SHA-256(token) for w1.
	const (
		knownCurrent = "p1:08f0be883564cbf2:6cdbeead4825a28fdf0eade839cac55e57188ae9c848ff099b3d8102eddba34b"
		knownWrapped = "w1:08f0be883564cbf2:939be6b7e5d7d92d717bb5b694384e9435a6389efb3383636d294ab3c3f82e85"
	)
	if got := h.Hash("lct_x"); got != knownCurrent {
		t.Fatalf("current hash = %q, want %q", got, knownCurrent)
	}
	if got := h.Wrap(HashToken("", "lct_x")); got != knownWrapped {
		t.Fatalf("wrapped hash = %q, want %q", got, knownWrapped)
	}
	if got := h.WrappedHash("lct_x"); got != knownWrapped {
		t.Errorf("WrappedHash = %q, want the wrap of the unpeppered hash %q", got, knownWrapped)
	}
	if strings.Contains(knownCurrent+knownWrapped, "derived") {
		t.Error("the stored forms must not contain the pepper")
	}
	for stored, want := range map[string]TokenHashForm{
		knownCurrent:              FormCurrent,
		knownWrapped:              FormWrapped,
		HashToken("", "lct_x"):    FormUntagged,
		HashToken("pep", "lct_x"): FormUntagged,
		NewTokenHasher("", "other", "").Hash("lct_x"):        FormOtherKey,
		NewTokenHasher("", "other", "").WrappedHash("lct_x"): FormOtherKey,
	} {
		if got := h.Form(stored); got != want {
			t.Errorf("Form(%q) = %d, want %d", stored, got, want)
		}
	}
	if h.HasPrevious() || h.PreviousHashes("lct_x") != nil {
		t.Error("no previous key: no previous forms")
	}
}

// A key rotation keeps both forms under the previous key's pepper verifiable,
// and recognisable as previous.
func TestTokenHasher_PreviousKeyFormsAreBothTriedAndRecognised(t *testing.T) {
	h := NewTokenHasher("", "derived", "prev")
	old := NewTokenHasher("", "prev", "")
	want := []string{old.Hash("lct_x"), old.WrappedHash("lct_x")}
	got := h.PreviousHashes("lct_x")
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("previous forms = %q, want %q", got, want)
	}
	for _, p := range got {
		if f := h.Form(p); f != FormPrevious {
			t.Errorf("Form(%q) = %d, want previous", p, f)
		}
	}
	if h.Form(h.Hash("lct_x")) != FormCurrent {
		t.Error("the current form read as something else with a previous key set")
	}
}

func TestTokenHasher_PreviousKeyEqualToCurrentIsNotPrevious(t *testing.T) {
	if h := NewTokenHasher("", "same", "same"); h.HasPrevious() {
		t.Error("a previous key equal to the current one is not a previous key")
	}
}

func TestTokenHasher_ZeroValueIsUnpeppered(t *testing.T) {
	var h TokenHasher
	if h.Source() != PepperNone || h.Hash("lct_x") != HashToken("", "lct_x") {
		t.Error("the zero hasher must hash with no pepper")
	}
	none := NewTokenHasher("", "", "prev")
	if none.Source() != PepperNone || none.HasPrevious() || none.WrappedHash("lct_x") != "" {
		t.Error("no pepper and no secret key → none, with no older forms")
	}
}
