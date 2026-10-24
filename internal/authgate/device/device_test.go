package device

import (
	"strings"
	"testing"
)

func TestNewUserCode_DrawsOnlyUnambiguousLetters(t *testing.T) {
	seen := map[byte]int{}
	for i := 0; i < 2000; i++ {
		c, err := NewUserCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(c) != UserCodeLen {
			t.Fatalf("code %q has length %d", c, len(c))
		}
		for j := 0; j < len(c); j++ {
			if strings.IndexByte(UserCodeAlphabet, c[j]) < 0 {
				t.Fatalf("code %q has %q, outside the alphabet", c, c[j])
			}
			seen[c[j]]++
		}
	}
	// Every letter appears: a broken sampler (always index 0, a short
	// modulus) would leave most unused.
	if len(seen) != len(UserCodeAlphabet) {
		t.Fatalf("only %d of %d letters drawn in 16000 samples", len(seen), len(UserCodeAlphabet))
	}
	for _, bad := range "AEIOUL01" {
		if strings.ContainsRune(UserCodeAlphabet, bad) {
			t.Fatalf("alphabet contains the ambiguous or vowel character %q", bad)
		}
	}
}

func TestUserCode_SpaceIsAtLeastTenBillion(t *testing.T) {
	space := 1.0
	for i := 0; i < UserCodeLen; i++ {
		space *= float64(len(UserCodeAlphabet))
	}
	if space < 1e10 {
		t.Fatalf("user code space %.3g is below 1e10", space)
	}
}

func TestNormalizeUserCode_IgnoresCaseAndSeparators(t *testing.T) {
	for _, in := range []string{"BCDF-GHJK", "bcdf-ghjk", " bcdf ghjk ", "BcDfGhJk", "bcdf_ghjk", "BC-DF-GH-JK"} {
		got, ok := NormalizeUserCode(in)
		if !ok || got != "BCDFGHJK" {
			t.Fatalf("NormalizeUserCode(%q) = %q, %v; want BCDFGHJK", in, got, ok)
		}
	}
}

func TestNormalizeUserCode_RefusesAmbiguousOrMalformedInput(t *testing.T) {
	for _, in := range []string{
		"", "BCDF-GHJ", "BCDF-GHJKM", // wrong length
		"BCDF-GHJ0", "BCDF-GHJO", "BCDF-GHJ1", "BCDF-GHJL", "BCDF-GHJI", // not in the alphabet: never guessed
		"BCDF-GHJÄ", "BCDF/GHJK", strings.Repeat("B", 100),
	} {
		if got, ok := NormalizeUserCode(in); ok {
			t.Fatalf("NormalizeUserCode(%q) = %q, accepted", in, got)
		}
	}
}

func TestFormatUserCode_GroupsInFours(t *testing.T) {
	if got := FormatUserCode("BCDFGHJK"); got != "BCDF-GHJK" {
		t.Fatalf("got %q", got)
	}
}

func TestDeviceCode_StoredFormIsAHashThatDoesNotEqualTheCode(t *testing.T) {
	code, hash, err := NewDeviceCode()
	if err != nil {
		t.Fatal(err)
	}
	if !WellFormedDeviceCode(code) {
		t.Fatalf("fresh code %q is not well formed", code)
	}
	if hash == code || strings.Contains(hash, code[len(DeviceCodePrefix):]) || HashDeviceCode(code) != hash {
		t.Fatal("stored form must be the code's hash, not the code")
	}
	other, _, _ := NewDeviceCode()
	if other == code {
		t.Fatal("two device codes collided")
	}
	for _, bad := range []string{"", "lcd_", "lcd_short", "xyz_" + code[4:], code + "AA"} {
		if WellFormedDeviceCode(bad) {
			t.Fatalf("%q accepted as a device code", bad)
		}
	}
}

func TestValidClientID_AcceptsShortLabelsOnly(t *testing.T) {
	for _, ok := range []string{"loomcycle-cli", "loomcycle-mcp", "a", "client:1.2"} {
		if !ValidClientID(ok) {
			t.Fatalf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "has space", "<script>", strings.Repeat("a", 65), "näme"} {
		if ValidClientID(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
}
