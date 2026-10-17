package cryptoops

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex fixture: %v", err)
	}
	return b
}

func mustGenerate(t *testing.T, k KeyType) []byte {
	t.Helper()
	m, err := Generate(k)
	if err != nil {
		t.Fatalf("Generate(%s): %v", k, err)
	}
	return m
}

func TestParseKeyType_AcceptsKnownRejectsUnknown(t *testing.T) {
	for _, k := range allKeyTypes {
		got, err := ParseKeyType(" " + strings.ToUpper(string(k)) + " ")
		if err != nil || got != k {
			t.Errorf("ParseKeyType(%q) = %q, %v", k, got, err)
		}
	}
	_, err := ParseKeyType("rsa-2048")
	if err == nil || !strings.Contains(err.Error(), "aes-256-gcm") || !strings.Contains(err.Error(), "totp") {
		t.Errorf("unknown type error should list the valid set, got %v", err)
	}
}

func TestKeyType_OpsMatchClassification(t *testing.T) {
	cases := []struct {
		k    KeyType
		ops  []string
		deny []string
	}{
		{AES256GCM, []string{"encrypt", "decrypt", "rewrap"}, []string{"sign", "mac", "totp"}},
		{XChaCha20Poly1305, []string{"encrypt", "decrypt", "rewrap"}, []string{"verify"}},
		{HMACSHA256, []string{"mac", "verify_mac", "sign", "verify"}, []string{"encrypt", "public_key"}},
		{HMACSHA512, []string{"mac", "verify_mac", "sign", "verify"}, []string{"decrypt"}},
		{Ed25519, []string{"sign", "verify", "public_key"}, []string{"mac", "encrypt"}},
		{ECDSAP256, []string{"sign", "verify", "public_key"}, []string{"totp"}},
		{TOTP, []string{"totp"}, []string{"sign", "encrypt"}},
	}
	for _, c := range cases {
		if got := c.k.Ops(); !slices.Equal(got, c.ops) {
			t.Errorf("%s.Ops() = %v, want %v", c.k, got, c.ops)
		}
		for _, op := range c.ops {
			if !c.k.Allows(op) {
				t.Errorf("%s should allow %s", c.k, op)
			}
		}
		for _, op := range c.deny {
			if c.k.Allows(op) {
				t.Errorf("%s should not allow %s", c.k, op)
			}
		}
		n := 0
		for _, b := range []bool{c.k.IsAEAD(), c.k.IsMAC(), c.k.IsSigning()} {
			if b {
				n++
			}
		}
		if want := map[bool]int{true: 0, false: 1}[c.k == TOTP]; n != want {
			t.Errorf("%s: %d classifications true, want %d", c.k, n, want)
		}
	}
	if KeyType("bogus").Ops() != nil || KeyType("bogus").Allows("encrypt") {
		t.Error("unknown type must support no ops")
	}
}

// --- Known-answer vectors --------------------------------------------------

// GCM spec (McGrew & Viega) test cases 13, 14 and 16 — the AES-256 vectors.
func TestSealWithNonce_AESGCMKnownAnswer(t *testing.T) {
	cases := []struct {
		name                     string
		key, iv, pt, aad, ct, tg string
	}{
		{"tc13", "0000000000000000000000000000000000000000000000000000000000000000", "000000000000000000000000", "", "", "", "530f8afbc74536b9a963b4f1c4cb738b"},
		{"tc14", "0000000000000000000000000000000000000000000000000000000000000000", "000000000000000000000000", "00000000000000000000000000000000", "", "cea7403d4d606b6e074ec5d3baf39d18", "d0d1c8a799996bf0265b98b5d48ab919"},
		{"tc16",
			"feffe9928665731c6d6a8f9467308308feffe9928665731c6d6a8f9467308308",
			"cafebabefacedbaddecaf888",
			"d9313225f88406e5a55909c5aff5269a86a7a9531534f7da2e4c303d8a318a721c3c0c95956809532fcf0e2449a6b525b16aedf5aa0de657ba637b39",
			"feedfacedeadbeeffeedfacedeadbeefabaddad2",
			"522dc1f099567d07f47f37a32a84427d643a8cdcbfe5c0c97598a2bd2555d1aa8cb08e48590dbb3da7b08b1056828838c5f61e6393ba7a0abcc9f662",
			"76fc6ece0f4e1768cddf8853bb2d551b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, iv, pt, aad := mustHex(t, c.key), mustHex(t, c.iv), mustHex(t, c.pt), mustHex(t, c.aad)
			got, err := sealWithNonce(AES256GCM, key, iv, pt, aad)
			if err != nil {
				t.Fatal(err)
			}
			want := append(append(append([]byte{}, iv...), mustHex(t, c.ct)...), mustHex(t, c.tg)...)
			if !bytes.Equal(got, want) {
				t.Fatalf("sealed = %x\nwant     %x", got, want)
			}
			// The output layout must be exactly what Open consumes.
			back, err := Open(AES256GCM, key, got, aad)
			if err != nil || !bytes.Equal(back, pt) {
				t.Fatalf("Open(KAT) = %x, %v", back, err)
			}
		})
	}
}

// RFC 4231 test case 2 (key "Jefe" — shorter than MAC accepts, so via macRaw).
func TestMACRaw_RFC4231KnownAnswer(t *testing.T) {
	data := []byte("what do ya want for nothing?")
	cases := []struct {
		k    KeyType
		want string
	}{
		{HMACSHA256, "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"},
		{HMACSHA512, "164b7a7bfcf819e2e395fbe73b56e0a387bd64222e831fd610270cd7ea2505549758bf75c05a994a6d034f65f8f0e6fdcaeab1a34d4a6b4b636e070a38bce737"},
	}
	for _, c := range cases {
		got, err := macRaw(c.k, []byte("Jefe"), data)
		if err != nil || hex.EncodeToString(got) != c.want {
			t.Errorf("%s = %x, %v; want %s", c.k, got, err, c.want)
		}
	}
	if _, err := MAC(HMACSHA256, []byte("Jefe"), data); err == nil {
		t.Error("MAC must refuse a 4-byte key")
	}
}

// RFC 8032 §7.1 TEST 1.
func TestSign_Ed25519RFC8032KnownAnswer(t *testing.T) {
	seed := mustHex(t, "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	wantPub := mustHex(t, "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
	wantSig := mustHex(t, "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b")
	sig, err := Sign(Ed25519, seed, nil)
	if err != nil || !bytes.Equal(sig, wantSig) {
		t.Fatalf("sig = %x, %v", sig, err)
	}
	pub, err := PublicKeyOf(Ed25519, seed)
	if err != nil || !bytes.Equal(pub.(ed25519.PublicKey), wantPub) {
		t.Fatalf("pub = %x, %v", pub, err)
	}
	if !Verify(Ed25519, pub, nil, sig) {
		t.Fatal("KAT signature did not verify")
	}
}

// RFC 6238 Appendix B, SHA-1 rows.
func TestTOTPCode_RFC6238KnownAnswer(t *testing.T) {
	seed := []byte("12345678901234567890")
	cases := []struct {
		unix int64
		code string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, c := range cases {
		code, until, err := TOTPCode(seed, time.Unix(c.unix, 0), 8, 30)
		if err != nil || code != c.code {
			t.Errorf("t=%d: code %q, %v; want %q", c.unix, code, err, c.code)
		}
		if want := (c.unix/30 + 1) * 30; until.Unix() != want {
			t.Errorf("t=%d: validUntil %d, want %d", c.unix, until.Unix(), want)
		}
	}
}

func TestTOTPCode_DefaultsAndBounds(t *testing.T) {
	seed := []byte("12345678901234567890")
	code, _, err := TOTPCode(seed, time.Unix(59, 0), 0, 0)
	if err != nil || code != "287082" { // 6-digit truncation of the 8-digit KAT
		t.Fatalf("default code = %q, %v", code, err)
	}
	bad := []struct {
		name           string
		seed           []byte
		t              time.Time
		digits, period int
	}{
		{"digits 5", seed, time.Unix(59, 0), 5, 30},
		{"digits 9", seed, time.Unix(59, 0), 9, 30},
		{"negative period", seed, time.Unix(59, 0), 6, -1},
		{"short seed", seed[:9], time.Unix(59, 0), 6, 30},
		{"pre-epoch", seed, time.Unix(-1, 0), 6, 30},
	}
	for _, b := range bad {
		if _, _, err := TOTPCode(b.seed, b.t, b.digits, b.period); err == nil {
			t.Errorf("%s: want error", b.name)
		}
	}
}

func TestHash_KnownAnswersAndUnknownAlg(t *testing.T) {
	cases := []struct{ alg, want string }{
		{"sha3-256", "3a985da74fe225b2045c172d6bd390bd855f086e3e9d525b46bfe24511431532"},
		{"sha256", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
	}
	for _, c := range cases {
		got, err := Hash(c.alg, []byte("abc"))
		if err != nil || hex.EncodeToString(got) != c.want {
			t.Errorf("%s(abc) = %x, %v", c.alg, got, err)
		}
	}
	for alg, n := range map[string]int{"sha512": 64, "sha3-512": 64} {
		got, err := Hash(alg, []byte("abc"))
		if err != nil || len(got) != n {
			t.Errorf("%s: len %d, %v", alg, len(got), err)
		}
	}
	for _, alg := range []string{"md5", "sha1", "SHA256", ""} {
		if _, err := Hash(alg, nil); err == nil {
			t.Errorf("Hash(%q) should fail", alg)
		}
	}
}

// --- AEAD ------------------------------------------------------------------

func TestSealOpen_RoundTripAndFailClosed(t *testing.T) {
	for _, k := range []KeyType{AES256GCM, XChaCha20Poly1305} {
		t.Run(string(k), func(t *testing.T) {
			key := mustGenerate(t, k)
			other := mustGenerate(t, k)
			pt := []byte("attack at dawn")
			aad := AAD("tenant-a", "k", "orders", 3, "ctx")
			sealed, err := Seal(k, key, pt, aad)
			if err != nil {
				t.Fatal(err)
			}
			wantNonce := map[KeyType]int{AES256GCM: 12, XChaCha20Poly1305: 24}[k]
			if len(sealed) != wantNonce+len(pt)+16 {
				t.Fatalf("sealed length %d, want nonce %d + pt + 16", len(sealed), wantNonce)
			}
			got, err := Open(k, key, sealed, aad)
			if err != nil || !bytes.Equal(got, pt) {
				t.Fatalf("Open = %q, %v", got, err)
			}
			again, _ := Seal(k, key, pt, aad)
			if bytes.Equal(again, sealed) {
				t.Fatal("two Seals of the same plaintext must differ (fresh nonce)")
			}

			flipped := bytes.Clone(sealed)
			flipped[len(flipped)/2] ^= 0x01
			bad := []struct {
				name        string
				key, sealed []byte
				aad         []byte
			}{
				{"wrong key", other, sealed, aad},
				{"wrong aad", key, sealed, AAD("tenant-b", "k", "orders", 3, "ctx")},
				{"nil aad", key, sealed, nil},
				{"flipped byte", key, flipped, aad},
				{"truncated tag", key, sealed[:len(sealed)-1], aad},
				{"nonce only", key, sealed[:wantNonce], aad},
				{"empty", key, nil, aad},
				{"short key", key[:16], sealed, aad},
			}
			for _, b := range bad {
				if out, err := Open(k, b.key, b.sealed, b.aad); !errors.Is(err, ErrDecrypt) || out != nil {
					t.Errorf("%s: Open = %q, %v; want ErrDecrypt", b.name, out, err)
				}
			}
		})
	}
	// A non-AEAD type is still just ErrDecrypt — no hint about why.
	if _, err := Open(HMACSHA256, make([]byte, 32), make([]byte, 64), nil); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open(non-AEAD) = %v", err)
	}
}

func TestSeal_RejectsWrongKeySizeAndType(t *testing.T) {
	// aes.NewCipher accepts 16 bytes; Seal must not downgrade to AES-128.
	if _, err := Seal(AES256GCM, make([]byte, 16), []byte("x"), nil); err == nil {
		t.Error("Seal with a 16-byte AES key must fail")
	}
	if _, err := Seal(Ed25519, make([]byte, 32), []byte("x"), nil); err == nil {
		t.Error("Seal with a signing type must fail")
	}
}

func TestAAD_LayoutIsStableAndFieldSensitive(t *testing.T) {
	got := AAD("t1", "k", "orders", 2, "invoice:7")
	want := "loomcycle/crypto/v1\x1ft1\x1fk\x1forders\x1f2\x1finvoice:7"
	if string(got) != want {
		t.Fatalf("AAD = %q, want %q", got, want)
	}
	base := string(got)
	for _, v := range [][]byte{
		AAD("t2", "k", "orders", 2, "invoice:7"),
		AAD("t1", "c", "orders", 2, "invoice:7"),
		AAD("t1", "k", "orders2", 2, "invoice:7"),
		AAD("t1", "k", "orders", 3, "invoice:7"),
		AAD("t1", "k", "orders", 2, ""),
	} {
		if string(v) == base {
			t.Errorf("AAD did not change for %q", v)
		}
	}
}

// --- Envelope --------------------------------------------------------------

func TestEnvelope_RoundTrip(t *testing.T) {
	cases := []Envelope{
		{Source: "kt", Name: "orders-key_1", Version: 7, Body: []byte{0xfb, 0xff, 0x00, 0x01}},
		{Source: "c", Name: "x", Version: 0, Body: bytes.Repeat([]byte{0xab}, 40)},
		{Source: "r", Name: strings.Repeat("a", 128), Version: 123456789, Body: []byte{1}},
	}
	for _, e := range cases {
		s := e.String()
		got, err := ParseEnvelope(s)
		if err != nil {
			t.Fatalf("ParseEnvelope(%q): %v", s, err)
		}
		if got.Source != e.Source || got.Name != e.Name || got.Version != e.Version || !bytes.Equal(got.Body, e.Body) {
			t.Errorf("round trip %q -> %+v", s, got)
		}
		if strings.ContainsAny(s[strings.LastIndex(s, ":")+1:], "+/=") {
			t.Errorf("body must be unpadded base64url: %q", s)
		}
	}
}

func TestParseEnvelope_RejectsMalformed(t *testing.T) {
	body := base64.RawURLEncoding.EncodeToString([]byte("some sealed bytes"))
	bad := map[string]string{
		"bad prefix":       "lcx2:k:name:1:" + body,
		"upper prefix":     "LCX1:k:name:1:" + body,
		"bad source":       "lcx1:x:name:1:" + body,
		"empty source":     "lcx1::name:1:" + body,
		"bad name chars":   "lcx1:kt:na.me:1:" + body,
		"empty name":       "lcx1:kt::1:" + body,
		"long name":        "lcx1:kt:" + strings.Repeat("a", 129) + ":1:" + body,
		"negative version": "lcx1:kt:name:-1:" + body,
		"plus version":     "lcx1:kt:name:+1:" + body,
		"alpha version":    "lcx1:kt:name:v1:" + body,
		"leading zero":     "lcx1:kt:name:01:" + body,
		"empty version":    "lcx1:kt:name::" + body,
		"huge version":     "lcx1:kt:name:9999999999:" + body,
		"padded body":      "lcx1:kt:name:1:" + base64.URLEncoding.EncodeToString([]byte("ab")),
		"std alphabet":     "lcx1:kt:name:1:" + base64.RawStdEncoding.EncodeToString([]byte{0xfb, 0xff}),
		"bad base64":       "lcx1:kt:name:1:!!!!",
		"empty body":       "lcx1:kt:name:1:",
		"extra field":      "lcx1:kt:name:1:" + body + ":x",
		"missing field":    "lcx1:kt:name:" + body,
		"whitespace":       " lcx1:kt:name:1:" + body,
		"empty":            "",
	}
	for name, s := range bad {
		if e, err := ParseEnvelope(s); err == nil {
			t.Errorf("%s: ParseEnvelope(%q) = %+v, want error", name, s, e)
		}
	}
}

// --- Material --------------------------------------------------------------

func TestGenerate_ProducesValidMaterial(t *testing.T) {
	sizes := map[KeyType]int{AES256GCM: 32, XChaCha20Poly1305: 32, HMACSHA256: 32, HMACSHA512: 64, Ed25519: 32, ECDSAP256: 32, TOTP: 20}
	for k, n := range sizes {
		m := mustGenerate(t, k)
		if len(m) != n {
			t.Errorf("Generate(%s) len %d, want %d", k, len(m), n)
		}
		if err := ValidateMaterial(k, m); err != nil {
			t.Errorf("Generate(%s) fails its own validation: %v", k, err)
		}
	}
	if _, err := Generate("bogus"); err == nil {
		t.Error("Generate(unknown) should fail")
	}
}

func TestValidateMaterial_RejectsWrongShape(t *testing.T) {
	n := elliptic.P256().Params().N.Bytes() // the curve order: one past the largest valid scalar
	bad := []struct {
		k KeyType
		m []byte
	}{
		{AES256GCM, make([]byte, 31)},
		{AES256GCM, make([]byte, 33)},
		{XChaCha20Poly1305, make([]byte, 16)},
		{HMACSHA256, make([]byte, 31)},
		{HMACSHA512, make([]byte, 16)},
		{Ed25519, make([]byte, 64)}, // a full private key, not the seed
		{ECDSAP256, make([]byte, 32)},
		{ECDSAP256, n},
		{TOTP, make([]byte, 9)},
		{"bogus", make([]byte, 32)},
	}
	for _, b := range bad {
		if err := ValidateMaterial(b.k, b.m); err == nil {
			t.Errorf("ValidateMaterial(%s, %d bytes) should fail", b.k, len(b.m))
		}
	}
	if err := ValidateMaterial(HMACSHA256, make([]byte, 100)); err != nil {
		t.Errorf("long HMAC key should be fine: %v", err)
	}
}

func pemOf(t *testing.T, typ string, der []byte) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

func TestParseMaterial_Encodings(t *testing.T) {
	key := bytes.Repeat([]byte{0xfb}, 32) // 0xfb exercises the +/ vs -_ alphabets
	totpSeed := []byte("12345678901234567890")
	totpB32 := base32.StdEncoding.EncodeToString(totpSeed) // GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ

	edPriv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	edDER, _ := x509.MarshalPKCS8PrivateKey(edPriv)
	ecPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecRaw, _ := ecPriv.Bytes()
	ecPKCS8, _ := x509.MarshalPKCS8PrivateKey(ecPriv)
	ecSEC1, _ := x509.MarshalECPrivateKey(ecPriv)

	ok := []struct {
		name     string
		k        KeyType
		value    string
		encoding string
		want     []byte
	}{
		{"auto base64 std", AES256GCM, base64.StdEncoding.EncodeToString(key), "", key},
		{"auto base64 url raw", XChaCha20Poly1305, base64.RawURLEncoding.EncodeToString(key), "", key},
		{"auto base64 hmac trimmed", HMACSHA256, "  " + base64.URLEncoding.EncodeToString(key) + "\n", "", key},
		{"explicit base64", HMACSHA512, base64.StdEncoding.EncodeToString(append(key, key...)), "base64", append(key, key...)},
		{"hex", AES256GCM, hex.EncodeToString(key), "hex", key},
		{"raw", HMACSHA256, strings.Repeat("s", 32), "raw", []byte(strings.Repeat("s", 32))},
		{"base32 explicit", TOTP, totpB32, "base32", totpSeed},
		{"auto base32 lower spaced unpadded", TOTP, "gezd gnbv gy3t qojq gezd gnbv gy3t qojq", "", totpSeed},
		{"base32 16 chars padded", TOTP, "JBSWY3DPEHPK3PXP", "", mustB32(t, "JBSWY3DPEHPK3PXP")},
		{"ed25519 pkcs8 pem", Ed25519, pemOf(t, "PRIVATE KEY", edDER), "", edPriv.Seed()},
		{"ed25519 hex seed", Ed25519, hex.EncodeToString(edPriv.Seed()), "hex", edPriv.Seed()},
		{"ecdsa pkcs8 pem", ECDSAP256, pemOf(t, "PRIVATE KEY", ecPKCS8), "pem", ecRaw},
		{"ecdsa sec1 pem", ECDSAP256, pemOf(t, "EC PRIVATE KEY", ecSEC1), "", ecRaw},
		{"ecdsa base64 scalar", ECDSAP256, base64.StdEncoding.EncodeToString(ecRaw), "base64", ecRaw},
	}
	for _, c := range ok {
		got, err := ParseMaterial(c.k, c.value, c.encoding)
		if err != nil || !bytes.Equal(got, c.want) {
			t.Errorf("%s: got %x, %v", c.name, got, err)
		}
	}

	rsaPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaDER, _ := x509.MarshalPKCS8PrivateKey(rsaPriv)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p384DER, _ := x509.MarshalPKCS8PrivateKey(p384)
	p384SEC1, _ := x509.MarshalECPrivateKey(p384)
	ecPub, _ := x509.MarshalPKIXPublicKey(&ecPriv.PublicKey)

	bad := []struct {
		name     string
		k        KeyType
		value    string
		encoding string
	}{
		{"aes 31 bytes", AES256GCM, base64.StdEncoding.EncodeToString(key[:31]), ""},
		{"aes 33 bytes hex", AES256GCM, hex.EncodeToString(append(key, 1)), "hex"},
		{"hmac short raw", HMACSHA256, "short", "raw"},
		{"not base64", AES256GCM, "not*base64*at*all", ""},
		{"not hex", AES256GCM, "zz" + hex.EncodeToString(key[1:]), "hex"},
		{"totp short", TOTP, "GEZDGNBV", ""},
		{"totp not base32", TOTP, "1111 8888 0000 9999", ""},
		{"rsa pkcs8 as ed25519", Ed25519, pemOf(t, "PRIVATE KEY", rsaDER), ""},
		{"rsa pkcs8 as ecdsa", ECDSAP256, pemOf(t, "PRIVATE KEY", rsaDER), ""},
		{"rsa block type", ECDSAP256, pemOf(t, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaPriv)), ""},
		{"p384 pkcs8", ECDSAP256, pemOf(t, "PRIVATE KEY", p384DER), ""},
		{"p384 sec1", ECDSAP256, pemOf(t, "EC PRIVATE KEY", p384SEC1), ""},
		{"ecdsa pem as ed25519", Ed25519, pemOf(t, "PRIVATE KEY", ecPKCS8), ""},
		{"sec1 block as ed25519", Ed25519, pemOf(t, "EC PRIVATE KEY", ecSEC1), ""},
		{"public key pem", ECDSAP256, pemOf(t, "PUBLIC KEY", ecPub), ""},
		{"two pem blocks", Ed25519, pemOf(t, "PRIVATE KEY", edDER) + pemOf(t, "PRIVATE KEY", edDER), ""},
		{"not pem", Ed25519, "hello", ""},
		{"pem for aead", AES256GCM, pemOf(t, "PRIVATE KEY", edDER), "pem"},
		{"unknown encoding", AES256GCM, base64.StdEncoding.EncodeToString(key), "base58"},
		{"unknown type", "bogus", base64.StdEncoding.EncodeToString(key), ""},
	}
	for _, c := range bad {
		if got, err := ParseMaterial(c.k, c.value, c.encoding); err == nil {
			t.Errorf("%s: got %x, want error", c.name, got)
		}
	}
}

func mustB32(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseMaterial_ErrorNeverEchoesValue(t *testing.T) {
	secret := "SUPERSECRETVALUE-not-base64!"
	_, err := ParseMaterial(AES256GCM, secret, "")
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("err = %v", err)
	}
}

func TestDerive_DeterministicAndNameSeparated(t *testing.T) {
	secret := []byte("an ordinary credential value")
	for k, n := range map[KeyType]int{AES256GCM: 32, XChaCha20Poly1305: 32, HMACSHA256: 32, HMACSHA512: 64} {
		a1, err := Derive(k, secret, "a")
		if err != nil || len(a1) != n {
			t.Fatalf("Derive(%s) = %d bytes, %v", k, len(a1), err)
		}
		a2, _ := Derive(k, secret, "a")
		b, _ := Derive(k, secret, "b")
		if !bytes.Equal(a1, a2) {
			t.Errorf("%s: Derive not deterministic", k)
		}
		if bytes.Equal(a1, b) {
			t.Errorf("%s: different names gave the same key", k)
		}
		if err := ValidateMaterial(k, a1); err != nil {
			t.Errorf("%s: derived material invalid: %v", k, err)
		}
	}
	x, _ := Derive(AES256GCM, secret, "a")
	y, _ := Derive(XChaCha20Poly1305, secret, "a")
	if bytes.Equal(x, y) {
		t.Error("different types under the same name must derive different keys")
	}
	for _, k := range []KeyType{Ed25519, ECDSAP256, TOTP, "bogus"} {
		if _, err := Derive(k, secret, "a"); err == nil {
			t.Errorf("Derive(%s) should fail", k)
		}
	}
	if _, err := Derive(AES256GCM, nil, "a"); err == nil {
		t.Error("Derive with an empty secret should fail")
	}
}

// --- MAC / signatures --------------------------------------------------------

func TestVerifyMAC_RightAndWrong(t *testing.T) {
	for _, k := range []KeyType{HMACSHA256, HMACSHA512} {
		key := mustGenerate(t, k)
		data := []byte("payload")
		mac, err := MAC(k, key, data)
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyMAC(k, key, data, mac) {
			t.Errorf("%s: right MAC rejected", k)
		}
		flipped := bytes.Clone(mac)
		flipped[0] ^= 1
		for name, m := range map[string][]byte{"flipped": flipped, "truncated": mac[:len(mac)-1], "empty": nil} {
			if VerifyMAC(k, key, data, m) {
				t.Errorf("%s: %s MAC accepted", k, name)
			}
		}
		if VerifyMAC(k, key, []byte("other"), mac) || VerifyMAC(k, mustGenerate(t, k), data, mac) {
			t.Errorf("%s: MAC accepted for wrong data/key", k)
		}
	}
	if _, err := MAC(AES256GCM, make([]byte, 32), nil); err == nil {
		t.Error("MAC with an AEAD type must fail")
	}
}

func TestSignVerify_RoundTripEachSigningType(t *testing.T) {
	for _, k := range []KeyType{Ed25519, ECDSAP256} {
		t.Run(string(k), func(t *testing.T) {
			key := mustGenerate(t, k)
			pub, err := PublicKeyOf(k, key)
			if err != nil {
				t.Fatal(err)
			}
			data := []byte("sign me")
			sig, err := Sign(k, key, data)
			if err != nil || len(sig) != 64 {
				t.Fatalf("Sign = %d bytes, %v", len(sig), err)
			}
			if !Verify(k, pub, data, sig) {
				t.Fatal("valid signature rejected")
			}
			flipped := bytes.Clone(sig)
			flipped[10] ^= 1
			otherPub, _ := PublicKeyOf(k, mustGenerate(t, k))
			if Verify(k, pub, []byte("other"), sig) || Verify(k, pub, data, flipped) ||
				Verify(k, otherPub, data, sig) || Verify(k, pub, data, sig[:63]) {
				t.Fatal("bad signature accepted")
			}
		})
	}
	// Cross-type: an Ed25519 public key handed to an ECDSA verify is false, not a panic.
	edPub, _ := PublicKeyOf(Ed25519, mustGenerate(t, Ed25519))
	if Verify(ECDSAP256, edPub, nil, make([]byte, 64)) || Verify(HMACSHA256, edPub, nil, nil) {
		t.Error("cross-type verify must be false")
	}
	if _, err := Sign(HMACSHA256, make([]byte, 32), nil); err == nil {
		t.Error("raw Sign with a MAC type must fail")
	}
}

func TestPublicKey_PEMAndJWKRoundTrip(t *testing.T) {
	for _, k := range []KeyType{Ed25519, ECDSAP256} {
		t.Run(string(k), func(t *testing.T) {
			key := mustGenerate(t, k)
			pub, _ := PublicKeyOf(k, key)
			sig, _ := Sign(k, key, []byte("m"))

			p, err := PublicKeyPEM(pub)
			if err != nil || !strings.HasPrefix(p, "-----BEGIN PUBLIC KEY-----") {
				t.Fatalf("PEM = %q, %v", p, err)
			}
			kt, back, err := ParsePublicKey(p)
			if err != nil || kt != k || !Verify(k, back, []byte("m"), sig) {
				t.Fatalf("ParsePublicKey(PEM) = %s, %v", kt, err)
			}

			jwk, err := PublicKeyJWK(pub, "key-1")
			if err != nil || jwk["kid"] != "key-1" {
				t.Fatalf("JWK = %v, %v", jwk, err)
			}
			wantKty := map[KeyType]string{Ed25519: "OKP", ECDSAP256: "EC"}[k]
			if jwk["kty"] != wantKty {
				t.Errorf("kty = %v, want %s", jwk["kty"], wantKty)
			}
			js, _ := json.Marshal(jwk)
			kt, back, err = ParsePublicKey(string(js))
			if err != nil || kt != k || !Verify(k, back, []byte("m"), sig) {
				t.Fatalf("ParsePublicKey(JWK) = %s, %v", kt, err)
			}
			if j2, _ := PublicKeyJWK(pub, ""); j2["kid"] != nil {
				t.Error("empty kid must be omitted")
			}
		})
	}
}

func TestParsePublicKey_RejectsUnsupported(t *testing.T) {
	rsaPriv, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsaPub, _ := x509.MarshalPKIXPublicKey(&rsaPriv.PublicKey)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p384Pub, _ := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	edPriv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	edDER, _ := x509.MarshalPKCS8PrivateKey(edPriv)
	x := base64.RawURLEncoding.EncodeToString(edPriv.Public().(ed25519.PublicKey))
	bad := map[string]string{
		"rsa pem":         pemOf(t, "PUBLIC KEY", rsaPub),
		"p384 pem":        pemOf(t, "PUBLIC KEY", p384Pub),
		"private pem":     pemOf(t, "PRIVATE KEY", edDER),
		"garbage":         "not a key",
		"jwk rsa":         `{"kty":"RSA","n":"AQAB","e":"AQAB"}`,
		"jwk private":     `{"kty":"OKP","crv":"Ed25519","x":"` + x + `","d":"` + x + `"}`,
		"jwk short x":     `{"kty":"OKP","crv":"Ed25519","x":"AAAA"}`,
		"jwk p384":        `{"kty":"EC","crv":"P-384","x":"` + x + `","y":"` + x + `"}`,
		"jwk off curve":   `{"kty":"EC","crv":"P-256","x":"` + x + `","y":"` + x + `"}`,
		"jwk bad json":    `{"kty":`,
		"jwk wrong crv":   `{"kty":"OKP","crv":"X25519","x":"` + x + `"}`,
		"pem trailing":    pemOf(t, "PUBLIC KEY", p384Pub) + "junk",
		"jwk padded b64x": `{"kty":"OKP","crv":"Ed25519","x":"` + x + `="}`,
	}
	for name, s := range bad {
		if kt, _, err := ParsePublicKey(s); err == nil {
			t.Errorf("%s: accepted as %s", name, kt)
		}
	}
	if _, err := PublicKeyPEM(&rsaPriv.PublicKey); err == nil {
		t.Error("PublicKeyPEM(RSA) should fail")
	}
	if _, err := PublicKeyJWK(&p384.PublicKey, ""); err == nil {
		t.Error("PublicKeyJWK(P-384) should fail")
	}
}

// --- JWS -----------------------------------------------------------------------

func jwsPart(t *testing.T, token string, i int) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[i])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestJWS_SignVerifyEachAlg(t *testing.T) {
	cases := []struct {
		k   KeyType
		alg string
	}{
		{HMACSHA256, "HS256"}, {HMACSHA512, "HS512"}, {Ed25519, "EdDSA"}, {ECDSAP256, "ES256"},
	}
	payload := []byte(`{"sub":"agent","n":1}`)
	for _, c := range cases {
		t.Run(c.alg, func(t *testing.T) {
			key := mustGenerate(t, c.k)
			var pub any
			vkey := key
			if c.k.IsSigning() {
				pub, _ = PublicKeyOf(c.k, key)
				vkey = nil
			}
			tok, err := SignJWS(c.k, key, payload, "kid-9")
			if err != nil {
				t.Fatal(err)
			}
			if h := string(jwsPart(t, tok, 0)); h != `{"alg":"`+c.alg+`","kid":"kid-9"}` {
				t.Errorf("header = %s", h)
			}
			got, kid, ok := VerifyJWS(c.k, vkey, pub, tok)
			if !ok || kid != "kid-9" || !bytes.Equal(got, payload) {
				t.Fatalf("VerifyJWS = %q, %q, %v", got, kid, ok)
			}
			if c.k.IsSigning() { // pub omitted: derived from the private key
				if _, _, ok := VerifyJWS(c.k, key, nil, tok); !ok {
					t.Error("verify via private key material failed")
				}
			}

			jwt, _ := SignJWT(c.k, key, payload, "")
			if h := string(jwsPart(t, jwt, 0)); h != `{"alg":"`+c.alg+`","typ":"JWT"}` {
				t.Errorf("JWT header = %s", h)
			}
			if _, kid, ok := VerifyJWS(c.k, vkey, pub, jwt); !ok || kid != "" {
				t.Errorf("JWT verify = %q, %v", kid, ok)
			}

			parts := strings.Split(tok, ".")
			tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"root","n":1}`)) + "." + parts[2]
			if p, _, ok := VerifyJWS(c.k, vkey, pub, tampered); ok || p != nil {
				t.Error("tampered payload accepted")
			}
		})
	}
}

func TestVerifyJWS_RejectsAlgConfusionAndMalformed(t *testing.T) {
	hsKey := mustGenerate(t, HMACSHA256)
	edKey := mustGenerate(t, Ed25519)
	edPub, _ := PublicKeyOf(Ed25519, edKey)
	b64 := base64.RawURLEncoding.EncodeToString
	payload := b64([]byte(`{"a":1}`))

	// forge builds a token with an arbitrary header, HMAC-signed with hsKey.
	forge := func(header string) string {
		in := b64([]byte(header)) + "." + payload
		mac, _ := MAC(HMACSHA256, hsKey, []byte(in))
		return in + "." + b64(mac)
	}
	if _, _, ok := VerifyJWS(HMACSHA256, hsKey, nil, forge(`{"alg":"HS256"}`)); !ok {
		t.Fatal("control: a well-formed forged-by-us HS256 token must verify")
	}

	hsTok, _ := SignJWS(HMACSHA256, hsKey, []byte("x"), "")
	edTok, _ := SignJWS(Ed25519, edKey, []byte("x"), "")
	cases := []struct {
		name  string
		k     KeyType
		key   []byte
		pub   any
		token string
	}{
		{"alg none unsigned", HMACSHA256, hsKey, nil, b64([]byte(`{"alg":"none"}`)) + "." + payload + "."},
		{"alg none with mac", HMACSHA256, hsKey, nil, forge(`{"alg":"none"}`)},
		{"alg HS512 on HS256 key", HMACSHA256, hsKey, nil, forge(`{"alg":"HS512"}`)},
		{"alg lowercase", HMACSHA256, hsKey, nil, forge(`{"alg":"hs256"}`)},
		{"alg missing", HMACSHA256, hsKey, nil, forge(`{"kid":"a"}`)},
		{"alg not string", HMACSHA256, hsKey, nil, forge(`{"alg":256}`)},
		{"crit present", HMACSHA256, hsKey, nil, forge(`{"alg":"HS256","crit":["b64"],"b64":false}`)},
		{"kid not string", HMACSHA256, hsKey, nil, forge(`{"alg":"HS256","kid":5}`)},
		{"header not object", HMACSHA256, hsKey, nil, forge(`["HS256"]`)},
		{"HS256 token, Ed25519 type", Ed25519, nil, edPub, hsTok},
		{"HS256 token, Ed25519 type, pub bytes as key", Ed25519, []byte(edPub.(ed25519.PublicKey)), nil, hsTok},
		{"EdDSA token, HS256 type", HMACSHA256, hsKey, nil, edTok},
		{"EdDSA token, ES256 type", ECDSAP256, nil, edPub, edTok},
		{"wrong hmac key", HMACSHA256, mustGenerate(t, HMACSHA256), nil, hsTok},
		{"two parts", HMACSHA256, hsKey, nil, strings.Join(strings.Split(hsTok, ".")[:2], ".")},
		{"four parts", HMACSHA256, hsKey, nil, hsTok + ".x"},
		{"padded signature", HMACSHA256, hsKey, nil, hsTok + "="},
		{"empty", HMACSHA256, hsKey, nil, ""},
		{"unsupported type", AES256GCM, make([]byte, 32), nil, hsTok},
	}
	for _, c := range cases {
		if p, _, ok := VerifyJWS(c.k, c.key, c.pub, c.token); ok || p != nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	if _, err := SignJWS(TOTP, mustGenerate(t, TOTP), nil, ""); err == nil {
		t.Error("SignJWS with TOTP must fail")
	}
}

// --- Random ------------------------------------------------------------------

func TestRandom_LengthsBoundsAndUUIDFormat(t *testing.T) {
	for _, n := range []int{1, 16, 4096} {
		b, err := RandomBytes(n)
		if err != nil || len(b) != n {
			t.Errorf("RandomBytes(%d) = %d, %v", n, len(b), err)
		}
		tok, err := RandomToken(n)
		if err != nil || base64.RawURLEncoding.DecodedLen(len(tok)) != n || strings.ContainsAny(tok, "+/=") {
			t.Errorf("RandomToken(%d) = %q, %v", n, tok, err)
		}
	}
	for _, n := range []int{0, -1, 4097} {
		if _, err := RandomBytes(n); err == nil {
			t.Errorf("RandomBytes(%d) should fail", n)
		}
		if _, err := RandomToken(n); err == nil {
			t.Errorf("RandomToken(%d) should fail", n)
		}
	}
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for range 100 {
		u, err := UUIDv4()
		if err != nil || !re.MatchString(u) {
			t.Fatalf("UUIDv4 = %q, %v", u, err)
		}
		if seen[u] {
			t.Fatalf("duplicate UUID %s", u)
		}
		seen[u] = true
	}
}

// A crypto key's envelope names its scope, so a decrypt opens the key of the
// scope that sealed. Each scope has one source, and a bare "k" is not one.
func TestKeySource_NamesTheScope(t *testing.T) {
	for _, scope := range []string{"tenant", "user", "agent"} {
		src := KeySource(scope)
		if src == "" || KeyScope(src) != scope {
			t.Errorf("scope %q -> source %q -> scope %q", scope, src, KeyScope(src))
		}
		env := Envelope{Source: src, Name: "k1", Version: 1, Body: []byte{1, 2, 3}}
		if got, err := ParseEnvelope(env.String()); err != nil || got.Source != src {
			t.Errorf("%s envelope round trip: %+v %v", scope, got, err)
		}
	}
	if KeySource("run") != "" || KeyScope(SourceCredential) != "" {
		t.Error("a non-key scope or source mapped to a key")
	}
	if _, err := ParseEnvelope("lcx1:k:k1:1:AQID"); err == nil {
		t.Error("an envelope with the bare source k parsed")
	}
}
