package password

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
)

func testPepper(b byte) []byte { return []byte(strings.Repeat(string(rune('a'+b)), 32)) }

func mustHasher(t *testing.T, cur, prev []byte) *Hasher {
	t.Helper()
	h, err := NewHasher(cur, prev)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHasher_RefusesToRunWithoutAPepper(t *testing.T) {
	if _, err := NewHasher(nil, nil); !errors.Is(err, ErrNoPepper) {
		t.Fatalf("err = %v, want ErrNoPepper", err)
	}
}

func TestHasher_VerifiesItsOwnHashAndRefusesAnotherPassword(t *testing.T) {
	ctx := context.Background()
	h := mustHasher(t, testPepper(1), nil)
	phc, kid, err := h.Hash(ctx, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("phc = %q, want the OWASP-minimum argon2id parameters", phc)
	}
	if !strings.HasPrefix(kid, "pw1:") {
		t.Fatalf("kid = %q, want pw1: tag", kid)
	}
	ok, rehash, err := h.Verify(ctx, "correct horse battery staple", phc, kid)
	if err != nil || !ok || rehash {
		t.Fatalf("Verify(right) = (%v, %v, %v), want (true, false, nil)", ok, rehash, err)
	}
	if ok, _, _ := h.Verify(ctx, "correct horse battery stapler", phc, kid); ok {
		t.Fatal("a different password verified")
	}
	phc2, _, _ := h.Hash(ctx, "correct horse battery staple")
	if phc2 == phc {
		t.Fatal("two hashes of one password are equal: the salt is not random")
	}
}

// A dump of user_credentials alone verifies nothing: the stored digest is
// the peppered HMAC of the argon2 output, so a hasher with another pepper
// (an attacker without LOOMCYCLE_SECRET_KEY) cannot check a guess, even with
// the right password and the row's own kid.
func TestHasher_AStoredHashDoesNotVerifyWithoutThePepper(t *testing.T) {
	ctx := context.Background()
	h := mustHasher(t, testPepper(1), nil)
	phc, kid, _ := h.Hash(ctx, "correct horse battery staple")
	other := mustHasher(t, testPepper(2), nil)
	other.cur.kid = kid // the attacker copies the id; the key is what it lacks
	if ok, _, _ := other.Verify(ctx, "correct horse battery staple", phc, kid); ok {
		t.Fatal("the hash verified under a different pepper")
	}
}

func TestHasher_PreviousPepperVerifiesAndAsksForARehash(t *testing.T) {
	ctx := context.Background()
	old := mustHasher(t, testPepper(1), nil)
	phc, kid, _ := old.Hash(ctx, "correct horse battery staple")
	rotated := mustHasher(t, testPepper(2), testPepper(1))
	ok, rehash, err := rotated.Verify(ctx, "correct horse battery staple", phc, kid)
	if err != nil || !ok || !rehash {
		t.Fatalf("Verify after rotation = (%v, %v, %v), want (true, true, nil)", ok, rehash, err)
	}
	if ok, _, _ := mustHasher(t, testPepper(2), nil).Verify(ctx, "correct horse battery staple", phc, kid); ok {
		t.Fatal("a hash under a rotated-out pepper still verified")
	}
}

func TestHasher_OtherParametersVerifyAndAskForARehash(t *testing.T) {
	ctx := context.Background()
	h := mustHasher(t, testPepper(1), nil)
	h.params = Params{MemoryKiB: 8192, Time: 1, Threads: 1}
	phc, kid, _ := h.Hash(ctx, "correct horse battery staple")
	h.params = DefaultParams
	ok, rehash, _ := h.Verify(ctx, "correct horse battery staple", phc, kid)
	if !ok || !rehash {
		t.Fatalf("Verify = (%v, %v), want (true, true)", ok, rehash)
	}
}

// A tampered row must not be able to make a login allocate gigabytes.
func TestHasher_RefusesAStoredHashWithOutOfBoundsParameters(t *testing.T) {
	ctx := context.Background()
	h := mustHasher(t, testPepper(1), nil)
	phc, kid, _ := h.Hash(ctx, "correct horse battery staple")
	huge := strings.Replace(phc, "m=19456", "m=4194304", 1)
	if ok, _, err := h.Verify(ctx, "correct horse battery staple", huge, kid); ok || err != nil {
		t.Fatalf("Verify(huge m) = (%v, %v), want (false, nil)", ok, err)
	}
	for _, bad := range []string{"", "$argon2i$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAA", "plaintext"} {
		if ok, _, err := h.Verify(ctx, "x", bad, kid); ok || err != nil {
			t.Errorf("Verify(%q) = (%v, %v), want (false, nil)", bad, ok, err)
		}
	}
}

func TestHasher_RecoveryMACsCoverThePreviousPepper(t *testing.T) {
	old := mustHasher(t, testPepper(1), nil)
	stored := old.MAC(RecoveryPurpose, "abcdefghij")
	rotated := mustHasher(t, testPepper(2), testPepper(1))
	macs := rotated.MACs(RecoveryPurpose, "abcdefghij")
	if len(macs) != 2 || macs[1] != stored || macs[0] == stored {
		t.Fatalf("MACs = %v, want [current, previous=%s]", macs, stored)
	}
	if old.MAC("other", "abcdefghij") == stored {
		t.Fatal("purposes share a MAC key")
	}
}

func mustPolicy(t *testing.T, extra string) *Policy {
	t.Helper()
	var r io.Reader // a nil *strings.Reader would be a non-nil io.Reader
	if extra != "" {
		r = strings.NewReader(extra)
	}
	p, err := NewPolicy(r)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The embedded list is the SecLists top-10k, unmodified: pin its digest so a
// silent edit (or a truncated copy) fails here.
func TestPolicy_BuiltinListIsTheSecListsTop10k(t *testing.T) {
	sum := sha256.Sum256([]byte(builtinBreached))
	const want = "68782d6a4a19a4768d5f15dd66bd534e7a33055cc755411e33f16d18c50fdcce"
	if hex.EncodeToString(sum[:]) != want {
		t.Fatalf("breached list digest = %x, want %s", sum, want)
	}
	if n := mustPolicy(t, "").Size(); n < 9900 {
		t.Fatalf("policy refuses %d entries, want ~10000", n)
	}
}

func TestPolicy_LengthDependsOnTheSecondFactor(t *testing.T) {
	p := mustPolicy(t, "")
	var pe *PolicyError
	if err := p.Check("kq8#vZ2!mw", false); !errors.As(err, &pe) || !errors.Is(err, ErrTooShort) || pe.MinLength != 15 {
		t.Fatalf("sole factor 10 chars: err = %v, want too short (15)", err)
	}
	if err := p.Check("kq8#vZ2!mw", true); err != nil {
		t.Fatalf("with MFA 10 chars: err = %v, want nil", err)
	}
	if err := p.Check("kq8#vZ2", true); !errors.Is(err, ErrTooShort) {
		t.Fatalf("with MFA 7 chars: err = %v, want too short (8)", err)
	}
	// Length is counted in characters, not bytes: 15 Cyrillic letters pass.
	if err := p.Check(strings.Repeat("ж", 15), false); err != nil {
		t.Fatalf("15 multi-byte chars: %v", err)
	}
	if err := p.Check(strings.Repeat("x", MaxLength+1), false); !errors.Is(err, ErrTooLong) {
		t.Fatalf("over max: %v", err)
	}
}

// No composition rules: a long lowercase passphrase is fine; a common
// password is refused whatever its case, and so are an operator's entries.
func TestPolicy_RefusesBreachedAndOperatorListedPasswordsCaseInsensitively(t *testing.T) {
	p := mustPolicy(t, "# ours\nCorrectHorseBatteryStaple\n")
	if err := p.Check("onlylowercaselettershere", false); err != nil {
		t.Fatalf("passphrase: %v", err)
	}
	if err := p.Check("PASSWORD", true); !errors.Is(err, ErrBreached) {
		t.Fatalf("PASSWORD: %v, want breached (the list has password)", err)
	}
	if err := p.Check("correcthorsebatterystaple", false); !errors.Is(err, ErrBreached) {
		t.Fatalf("operator entry: %v, want breached", err)
	}
	if err := mustPolicy(t, "").Check("correcthorsebatterystaple", false); err != nil {
		t.Fatalf("without the operator list: %v", err)
	}
}

func TestPolicy_RefusesThePersonsOwnLogin(t *testing.T) {
	p := mustPolicy(t, "")
	if err := p.Check("Alice.Example@Mail.Test", true, "alice.example@mail.test"); !errors.Is(err, ErrContextual) {
		t.Fatalf("err = %v, want contextual", err)
	}
}

func TestNormalize_IsNFKCAndRefusesInvalidUTF8(t *testing.T) {
	// U+FB01 (ﬁ) is NFKC "fi"; the full-width A is "A".
	got, err := Normalize("ﬁＡ")
	if err != nil || got != "fiA" {
		t.Fatalf("Normalize = (%q, %v), want fiA", got, err)
	}
	if _, err := Normalize("\xff\xfe"); !errors.Is(err, ErrInvalidUTF) {
		t.Fatalf("invalid UTF-8: %v", err)
	}
	if got, _ := Normalize("  spaced  "); got != "  spaced  " {
		t.Fatalf("Normalize trimmed a password: %q", got)
	}
}

// Logins collide case-insensitively and across Unicode compatibility forms
// (RFC FA security row): two that read the same normalise to one string.
func TestNormalizeLogin_CollidesCaseAndCompatibilityForms(t *testing.T) {
	a, err := NormalizeLogin("  Alice@Example.COM ")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"alice@example.com", "ALICE@EXAMPLE.COM", "Ａｌｉｃｅ@example.com"} {
		b, err := NormalizeLogin(v)
		if err != nil || b != a {
			t.Errorf("NormalizeLogin(%q) = (%q, %v), want %q", v, b, err, a)
		}
	}
	// German sharp s folds to "ss" under full case folding.
	s1, _ := NormalizeLogin("straße")
	s2, _ := NormalizeLogin("STRASSE")
	if s1 != s2 {
		t.Errorf("straße=%q STRASSE=%q, want equal", s1, s2)
	}
}

func TestNormalizeLogin_RefusesInvisibleSpacedAndOversizedNames(t *testing.T) {
	for _, bad := range []string{"", "a", "al ice", "al\tice", "al​ice", "al‮ice", "ctl\x00x", strings.Repeat("a", 255), "\xff\xfe"} {
		if got, err := NormalizeLogin(bad); !errors.Is(err, ErrInvalidLogin) {
			t.Errorf("NormalizeLogin(%q) = (%q, %v), want ErrInvalidLogin", bad, got, err)
		}
	}
}

func TestNewSubject_IsGatePrefixedAndRandom(t *testing.T) {
	a, _ := NewSubject("local")
	b, _ := NewSubject("local")
	if !strings.HasPrefix(a, "local:") || len(a) != len("local:")+16 || a == b {
		t.Fatalf("subjects %q %q", a, b)
	}
}

func TestCodes_AreSingleShapeAndStoredOnlyAsTheirHash(t *testing.T) {
	code, hash, err := NewCode(InvitePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if !WellFormedCode(InvitePrefix, code) || WellFormedCode(ResetPrefix, code) {
		t.Fatalf("WellFormedCode wrong for %q", InvitePrefix)
	}
	if hash != HashCode(code) || strings.Contains(hash, code[len(InvitePrefix):]) || len(hash) != 64 {
		t.Fatalf("hash %q is not the SHA-256 of the code", hash)
	}
	for _, junk := range []string{"", InvitePrefix, InvitePrefix + "short", InvitePrefix + strings.Repeat("A", 100)} {
		if WellFormedCode(InvitePrefix, junk) {
			t.Errorf("WellFormedCode(%q) = true", junk)
		}
	}
}

// RFC 6238 Appendix B, SHA-1 secret "12345678901234567890", truncated to the
// six digits authenticator apps show.
func TestTOTPCode_MatchesRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		if got := TOTPCode(secret, TOTPStep(time.Unix(unix, 0))); got != want {
			t.Errorf("TOTP(%d) = %s, want %s", unix, got, want)
		}
	}
}

func TestVerifyTOTP_AcceptsOneStepOfSkewAndNoMore(t *testing.T) {
	secret, _ := NewTOTPSecret()
	now := time.Unix(1_800_000_000, 0)
	cur := TOTPStep(now)
	for _, d := range []int64{-1, 0, 1} {
		step, ok := VerifyTOTP(secret, TOTPCode(secret, cur+d), now)
		if !ok || step != cur+d {
			t.Errorf("skew %d: (%d, %v), want (%d, true)", d, step, ok, cur+d)
		}
	}
	for _, d := range []int64{-2, 2} {
		if _, ok := VerifyTOTP(secret, TOTPCode(secret, cur+d), now); ok {
			t.Errorf("skew %d accepted", d)
		}
	}
	if _, ok := VerifyTOTP(secret, "12345", now); ok {
		t.Error("a 5-digit code was accepted")
	}
}

func TestTOTPSeal_OpensOnlyForTheSameUser(t *testing.T) {
	s, err := credential.NewSealer("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=", "")
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := NewTOTPSecret()
	sealed, err := SealTOTP(s, "acme", "local:abc", secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, TOTPSecretText(secret)) {
		t.Fatal("sealed value contains the secret")
	}
	got, err := OpenTOTP(s, "acme", "local:abc", sealed)
	if err != nil || string(got) != string(secret) {
		t.Fatalf("OpenTOTP = (%x, %v)", got, err)
	}
	if _, err := OpenTOTP(s, "acme", "local:other", sealed); err == nil {
		t.Fatal("a secret copied to another user's row opened")
	}
	if _, err := OpenTOTP(s, "other", "local:abc", sealed); err == nil {
		t.Fatal("a secret copied to another tenant opened")
	}
}

func TestTOTPURI_CarriesTheSecretAndIssuer(t *testing.T) {
	uri := TOTPURI("loomcycle", "alice@example.com", []byte("12345678901234567890"))
	if !strings.HasPrefix(uri, "otpauth://totp/loomcycle:alice@example.com?") || !strings.Contains(uri, "secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ") {
		t.Fatalf("uri = %s", uri)
	}
}

func TestRecoveryCodes_AreDistinctAndNormaliseTheWayTheyAreTyped(t *testing.T) {
	codes, err := NewRecoveryCodes(RecoveryCodeCount)
	if err != nil || len(codes) != 10 {
		t.Fatal(codes, err)
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 11 || c[5] != '-' || seen[c] || !LooksLikeRecoveryCode(c) {
			t.Fatalf("bad or repeated code %q", c)
		}
		seen[c] = true
	}
	if NormalizeRecoveryCode("ABCDE-fghij") != "abcdefghij" || NormalizeRecoveryCode("abcde fghij") != "abcdefghij" {
		t.Fatal("normalisation")
	}
	if LooksLikeRecoveryCode("123456") {
		t.Fatal("a TOTP code looks like a recovery code")
	}
}

func TestThrottle_AccountBacksOffThenLocksAtMaxFailures(t *testing.T) {
	th := Throttle{MaxFailures: 10, Lockout: 15 * time.Minute}
	want := map[int]time.Duration{1: 0, 3: 0, 4: time.Second, 5: 2 * time.Second, 9: 32 * time.Second, 10: 15 * time.Minute, 11: 15 * time.Minute}
	for n, d := range want {
		if got := th.AccountLockFor(n); got != d {
			t.Errorf("AccountLockFor(%d) = %s, want %s", n, got, d)
		}
	}
	if th.SourceBlockFor(SourceFree) != 0 || th.SourceBlockFor(SourceFree+1) != time.Second || th.SourceBlockFor(1000) != 15*time.Minute {
		t.Error("source curve")
	}
	if th.CodeBlockFor(CodeMaxFailures-1) != 0 || th.CodeBlockFor(CodeMaxFailures) != 15*time.Minute {
		t.Error("code block")
	}
}
