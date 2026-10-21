package auth

import (
	"strings"
	"testing"
)

func TestTokenHasher_ExplicitPepperKeepsTheUntaggedFormAndHasNoLegacy(t *testing.T) {
	h := NewTokenHasher("pep", "derived", "prev", true)
	if h.Source() != PepperExplicit {
		t.Fatalf("source = %q, want explicit", h.Source())
	}
	if got := h.Hash("lct_x"); got != HashToken("pep", "lct_x") {
		t.Errorf("explicit-pepper hash changed form: %q", got)
	}
	if h.HasLegacyCandidates() || h.LegacyHashes("lct_x") != nil {
		t.Error("an explicit pepper must not accept legacy hashes")
	}
	if !h.IsCurrent("anything") {
		t.Error("under an explicit pepper every stored hash is current")
	}
}

func TestTokenHasher_DerivedPepperTagsTheHashAndOrdersLegacyCandidates(t *testing.T) {
	h := NewTokenHasher("", "derived", "prev", true)
	if h.Source() != PepperDerived {
		t.Fatalf("source = %q, want derived", h.Source())
	}
	got := h.Hash("lct_x")
	// Computed outside Go: "p1:" + hex(SHA-256(pepper)[:8]) + ":" +
	// hex(HMAC-SHA256(key=pepper, msg=token)).
	const known = "p1:08f0be883564cbf2:6cdbeead4825a28fdf0eade839cac55e57188ae9c848ff099b3d8102eddba34b"
	if got != known {
		t.Fatalf("derived hash = %q, want %q", got, known)
	}
	if !IsTagged(got) || IsTagged(HashToken("derived", "lct_x")) {
		t.Error("IsTagged must tell the derived form from an untagged one")
	}
	if strings.Contains(got, "derived") {
		t.Error("the stored form must not contain the pepper")
	}
	if !h.IsCurrent(got) {
		t.Error("the current hash must read as current")
	}
	legacy := h.LegacyHashes("lct_x")
	want := []string{NewTokenHasher("", "prev", "", false).Hash("lct_x"), HashToken("", "lct_x")}
	if len(legacy) != len(want) || legacy[0] != want[0] || legacy[1] != want[1] {
		t.Fatalf("legacy candidates = %q, want %q", legacy, want)
	}
	for _, l := range legacy {
		if h.IsCurrent(l) {
			t.Errorf("legacy hash %q reads as current", l)
		}
	}
}

// Turning the unpeppered form off keeps the previous-key form: dropping it
// would make a later key rotation log every token out.
func TestTokenHasher_UnpepperedOffKeepsThePreviousKeyCandidate(t *testing.T) {
	h := NewTokenHasher("", "derived", "prev", false)
	got := h.LegacyHashes("lct_x")
	if len(got) != 1 || got[0] != NewTokenHasher("", "prev", "", false).Hash("lct_x") || h.AcceptsUnpeppered() {
		t.Errorf("legacy = %q, want only the previous-key hash", got)
	}
	if h := NewTokenHasher("", "derived", "", false); h.HasLegacyCandidates() {
		t.Error("no previous key and the unpeppered form off: no candidates")
	}
	if h.IsCurrent(HashToken("", "lct_x")) {
		t.Error("an unpeppered row is still not current with the fallback off")
	}
}

func TestTokenHasher_PreviousKeyEqualToCurrentIsNotACandidate(t *testing.T) {
	h := NewTokenHasher("", "same", "same", true)
	if got := h.LegacyHashes("lct_x"); len(got) != 1 || got[0] != HashToken("", "lct_x") {
		t.Errorf("legacy = %q, want only the unpeppered hash", got)
	}
}

func TestTokenHasher_ZeroValueIsUnpeppered(t *testing.T) {
	var h TokenHasher
	if h.Source() != PepperNone || h.Hash("lct_x") != HashToken("", "lct_x") {
		t.Error("the zero hasher must hash with no pepper")
	}
	none := NewTokenHasher("", "", "prev", true)
	if none.Source() != PepperNone || none.HasLegacyCandidates() {
		t.Error("no pepper and no secret key → none, without a fallback")
	}
}
