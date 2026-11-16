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
	// hex(HMAC-SHA256(key=pepper, msg)), msg = the token for p1 and
	// "w1" 0x00 + the hex text of SHA-256(token) for w1.
	const (
		knownCurrent = "p1:08f0be883564cbf2:6cdbeead4825a28fdf0eade839cac55e57188ae9c848ff099b3d8102eddba34b"
		knownWrapped = "w1:08f0be883564cbf2:453da591a2e0290fe76244d4aae0999699eefafea7e885354da40f5b276dcfee"
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
	// Domain separation: a bearer equal to the token's unpeppered hash must
	// not produce the wrapped form's MAC under the current form.
	if mac := func(x string) string { return x[strings.LastIndex(x, ":")+1:] }; mac(h.Hash(HashToken("", "lct_x"))) == mac(knownWrapped) {
		t.Error("the current and wrapped forms share a MAC")
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

// UsablePrefixes and Form agree on which stored hashes can authenticate.
func TestTokenHasher_UsablePrefixesMatchTheFormsALookupReaches(t *testing.T) {
	matches := func(h TokenHasher, stored string) bool {
		inc, exc := h.UsablePrefixes()
		ok := len(inc) == 0
		for _, p := range inc {
			ok = ok || strings.HasPrefix(stored, p)
		}
		for _, p := range exc {
			ok = ok && !strings.HasPrefix(stored, p)
		}
		return ok
	}
	derived := NewTokenHasher("", "derived", "prev")
	explicit := NewTokenHasher("pep", "", "")
	stored := []string{
		derived.Hash("lct_x"), derived.WrappedHash("lct_x"),
		NewTokenHasher("", "prev", "").Hash("lct_x"), NewTokenHasher("", "prev", "").WrappedHash("lct_x"),
		NewTokenHasher("", "other", "").Hash("lct_x"), HashToken("", "lct_x"), HashToken("pep", "lct_x"),
	}
	for _, st := range stored {
		f := derived.Form(st)
		if want := f == FormCurrent || f == FormWrapped || f == FormPrevious; matches(derived, st) != want {
			t.Errorf("derived: %q usable=%v, form %d", st, !want, f)
		}
		if want := !strings.HasPrefix(st, "p1:") && !strings.HasPrefix(st, "w1:"); matches(explicit, st) != want {
			t.Errorf("explicit: %q usable=%v, want %v", st, !want, want)
		}
	}
}

// A legacy explicit pepper kept beside a derived one finds its own rows, never
// mints with them, and never matches an unpeppered row.
func TestTokenHasher_LegacyPepperIsForLookupsOnly(t *testing.T) {
	h := NewTokenHasher("", "derived", "").WithLegacy("pep")
	if !h.HasLegacy() {
		t.Fatal("WithLegacy on a derived hasher must keep the pepper")
	}
	if got, want := h.LegacyHash("lct_x"), HashToken("pep", "lct_x"); got != want {
		t.Errorf("LegacyHash = %q, want the explicit pepper's hash %q", got, want)
	}
	if h.Hash("lct_x") != NewTokenHasher("", "derived", "").Hash("lct_x") {
		t.Error("a mint must use the derived pepper, not the legacy one")
	}
	if h.LegacyHash("lct_x") == HashToken("", "lct_x") {
		t.Error("the legacy form must differ from the unpeppered one")
	}
	for _, other := range []TokenHasher{NewTokenHasher("pep", "", ""), NewTokenHasher("", "", "")} {
		if other.WithLegacy("pep2").HasLegacy() {
			t.Errorf("WithLegacy on a %s hasher must be a no-op", other.Source())
		}
	}
}

// With a legacy pepper an untagged row may authenticate, so the usable
// prefixes cover every untagged (lowercase hex) hash, and still no tagged
// hash under another key.
func TestTokenHasher_LegacyPepperCountsUntaggedRowsAsUsable(t *testing.T) {
	usable := func(h TokenHasher, stored string) bool {
		include, exclude := h.UsablePrefixes()
		for _, p := range exclude {
			if strings.HasPrefix(stored, p) {
				return false
			}
		}
		if len(include) == 0 {
			return true
		}
		for _, p := range include {
			if strings.HasPrefix(stored, p) {
				return true
			}
		}
		return false
	}
	plain := NewTokenHasher("", "derived", "")
	legacy := plain.WithLegacy("pep")
	untagged := HashToken("pep", "lct_x")
	other := NewTokenHasher("", "other", "").Hash("lct_x")
	if usable(plain, untagged) {
		t.Error("without a legacy pepper an untagged row cannot authenticate")
	}
	if !usable(legacy, untagged) {
		t.Error("with a legacy pepper an untagged row is usable")
	}
	if !usable(legacy, legacy.Hash("lct_x")) {
		t.Error("a current row must stay usable")
	}
	if usable(legacy, other) {
		t.Error("a row under another key must stay unusable")
	}
}
