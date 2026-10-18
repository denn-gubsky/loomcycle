package cryptoops

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"sync"
	"testing"
)

// RSA key generation is slow; the 2048-bit keys are made once per test binary.
var (
	rsaOnce      sync.Once
	rsaOAEPKey   []byte
	rsaPSSKey    []byte
	rsaGenFailed error
)

func rsaKeys(t *testing.T) (oaep, pss []byte) {
	t.Helper()
	rsaOnce.Do(func() {
		if rsaOAEPKey, rsaGenFailed = Generate(RSAOAEP2048); rsaGenFailed != nil {
			return
		}
		rsaPSSKey, rsaGenFailed = Generate(RSAPSS2048)
	})
	if rsaGenFailed != nil {
		t.Fatal(rsaGenFailed)
	}
	return rsaOAEPKey, rsaPSSKey
}

// RFC 7518 Appendix C: the Concat KDF example for ECDH-ES.
func TestConcatKDF_MatchesRFC7518AppendixC(t *testing.T) {
	z := []byte{158, 86, 217, 29, 129, 113, 53, 211, 114, 131, 66, 131, 191, 132, 38, 156,
		251, 49, 110, 163, 218, 128, 106, 72, 246, 218, 167, 121, 140, 254, 144, 196}
	got := concatKDFParties(z, "A128GCM", []byte("Alice"), []byte("Bob"), 16)
	if b64 := base64.RawURLEncoding.EncodeToString(got); b64 != "VqqN6vgjbSBcIijNcacQGg" {
		t.Errorf("concat KDF = %s, want VqqN6vgjbSBcIijNcacQGg", b64)
	}
}

func TestNewKeyTypes_OpsAndPredicates(t *testing.T) {
	for _, c := range []struct {
		k   KeyType
		ops []string
	}{
		{X25519, []string{OpEncrypt, OpDecrypt, OpPublicKey}},
		{RSAOAEP3072, []string{OpEncrypt, OpDecrypt, OpPublicKey}},
		{RSAPSS4096, []string{OpSign, OpVerify, OpPublicKey}},
	} {
		if strings.Join(c.k.Ops(), ",") != strings.Join(c.ops, ",") {
			t.Errorf("%s ops = %v, want %v", c.k, c.k.Ops(), c.ops)
		}
		if got, err := ParseKeyType(" " + strings.ToUpper(string(c.k)) + " "); err != nil || got != c.k {
			t.Errorf("ParseKeyType(%s) = %q %v", c.k, got, err)
		}
	}
	if RSAPSS2048.IsRecipient() || RSAOAEP2048.IsSigning() || X25519.IsAEAD() {
		t.Error("an RSA type crosses purpose, or x25519 claims AEAD")
	}
	if _, err := Derive(X25519, []byte("secret"), "n"); err == nil {
		t.Error("derived x25519 material")
	}
}

func TestRSAMaterial_SizeAndFormsAreEnforced(t *testing.T) {
	oaep, pss := rsaKeys(t)
	if err := ValidateMaterial(RSAOAEP2048, oaep); err != nil {
		t.Fatal(err)
	}
	if ValidateMaterial(RSAOAEP3072, oaep) == nil {
		t.Error("a 2048-bit key was accepted as rsa-oaep-3072")
	}
	// PKCS#1 PEM normalises to the same PKCS#8 bytes.
	key, _ := x509.ParsePKCS8PrivateKey(pss)
	p1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key.(*rsa.PrivateKey))}))
	m, err := ParseMaterial(RSAPSS2048, p1, "")
	if err != nil || !bytes.Equal(m, pss) {
		t.Errorf("PKCS#1 PEM → %v, equal %v", err, bytes.Equal(m, pss))
	}
	if m, err := ParseMaterial(RSAPSS2048, base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(key.(*rsa.PrivateKey))), "base64"); err != nil || !bytes.Equal(m, pss) {
		t.Errorf("PKCS#1 DER base64 → %v", err)
	}
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	der, _ := x509.MarshalPKCS8PrivateKey(small)
	if _, err := ParseMaterial(RSAPSS2048, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "pem"); err == nil {
		t.Error("a 1024-bit key was accepted")
	}
	if _, err := ParseMaterial(X25519, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "pem"); err == nil {
		t.Error("an RSA PEM was accepted as x25519")
	}
}

func TestX25519Material_ParsesFromPEMAndRaw(t *testing.T) {
	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	m, err := ParseMaterial(X25519, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "")
	if err != nil || !bytes.Equal(m, priv.Bytes()) {
		t.Fatalf("pem → %v", err)
	}
	if m, err := ParseMaterial(X25519, base64.StdEncoding.EncodeToString(priv.Bytes()), "base64"); err != nil || !bytes.Equal(m, priv.Bytes()) {
		t.Fatalf("base64 → %v", err)
	}
	if ValidateMaterial(X25519, []byte("short")) == nil {
		t.Error("short x25519 material accepted")
	}
}

func TestRSAPSS_SignVerifyAndPS256(t *testing.T) {
	_, pss := rsaKeys(t)
	sig, err := Sign(RSAPSS2048, pss, []byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := PublicKeyOf(RSAPSS2048, pss)
	if err != nil {
		t.Fatal(err)
	}
	if !Verify(RSAPSS2048, pub, []byte("data"), sig) || Verify(RSAPSS2048, pub, []byte("datA"), sig) {
		t.Error("PSS verify wrong")
	}
	if Verify(RSAPSS3072, pub, []byte("data"), sig) {
		t.Error("a 2048-bit signature verified as rsa-pss-3072")
	}
	tok, err := SignJWS(RSAPSS2048, pss, []byte(`{"a":1}`), "k.v1")
	if err != nil || !strings.HasPrefix(tok, base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"PS256"`))) {
		t.Fatalf("SignJWS = %q %v", tok, err)
	}
	if body, kid, ok := VerifyJWS(RSAPSS2048, nil, pub, tok); !ok || kid != "k.v1" || string(body) != `{"a":1}` {
		t.Errorf("VerifyJWS = %s %s %v", body, kid, ok)
	}
	// The header cannot choose RS256 or none for a PSS key.
	parts := strings.Split(tok, ".")
	for _, alg := range []string{"RS256", "none", "ES256"} {
		h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"` + alg + `","kid":"k.v1"}`))
		if _, _, ok := VerifyJWS(RSAPSS2048, nil, pub, h+"."+parts[1]+"."+parts[2]); ok {
			t.Errorf("alg %s accepted", alg)
		}
	}
}

func TestPublicKeys_RoundTripThroughPEMAndJWKByPurpose(t *testing.T) {
	oaep, pss := rsaKeys(t)
	x, _ := Generate(X25519)
	for _, c := range []struct {
		k         KeyType
		key       []byte
		recipient bool
	}{{X25519, x, true}, {RSAOAEP2048, oaep, true}, {RSAPSS2048, pss, false}} {
		pub, err := PublicKeyOf(c.k, c.key)
		if err != nil {
			t.Fatal(err)
		}
		p, err := PublicKeyPEM(pub)
		if err != nil {
			t.Fatal(err)
		}
		jwk, err := PublicKeyJWK(pub, "kid1")
		if err != nil {
			t.Fatal(err)
		}
		jb, _ := json.Marshal(jwk)
		for _, s := range []string{p, string(jb)} {
			var kt KeyType
			if c.recipient {
				kt, _, err = ParseRecipientKey(s)
			} else {
				kt, _, err = ParsePublicKey(s)
			}
			if err != nil || kt != c.k {
				t.Errorf("%s: parsed %q as %s %v", c.k, s[:20], kt, err)
			}
		}
	}
	// Purpose is enforced: an Ed25519 key is not a recipient, an X25519 key cannot verify.
	ed, _ := Generate(Ed25519)
	edPub, _ := PublicKeyOf(Ed25519, ed)
	edPEM, _ := PublicKeyPEM(edPub)
	if _, _, err := ParseRecipientKey(edPEM); err == nil {
		t.Error("an Ed25519 key was accepted as a recipient")
	}
	xPub, _ := PublicKeyOf(X25519, x)
	xPEM, _ := PublicKeyPEM(xPub)
	if _, _, err := ParsePublicKey(xPEM); err == nil {
		t.Error("an X25519 key was accepted to verify")
	}
	if _, _, err := ParseRecipientKey(`{"kty":"RSA","n":"AQAB","e":"AQAB"}`); err == nil {
		t.Error("a tiny RSA JWK was accepted")
	}
}

func TestJWE_RoundTripsForEveryAlg(t *testing.T) {
	oaep, _ := rsaKeys(t)
	x, _ := Generate(X25519)
	aesKey, _ := Generate(AES256GCM)

	tok, err := EncryptJWEDirect(aesKey, []byte("hello dir"), "k.v1")
	if err != nil {
		t.Fatal(err)
	}
	if pt, kid, ok := DecryptJWE(AES256GCM, aesKey, tok); !ok || string(pt) != "hello dir" || kid != "k.v1" {
		t.Errorf("dir: %q %q %v", pt, kid, ok)
	}
	for _, c := range []struct {
		k   KeyType
		key []byte
	}{{X25519, x}, {RSAOAEP2048, oaep}} {
		pub, _ := PublicKeyOf(c.k, c.key)
		tok, err := EncryptJWETo(c.k, pub, []byte("hello "+string(c.k)), "")
		if err != nil {
			t.Fatal(err)
		}
		if !IsJWE(tok) {
			t.Errorf("%s: not a JWE shape", c.k)
		}
		if pt, _, ok := DecryptJWE(c.k, c.key, tok); !ok || string(pt) != "hello "+string(c.k) {
			t.Errorf("%s: %q %v", c.k, pt, ok)
		}
	}
	// A recipient type refuses a public key of another type.
	edKey, _ := Generate(Ed25519)
	edPub, _ := PublicKeyOf(Ed25519, edKey)
	if _, err := EncryptJWETo(X25519, edPub, []byte("x"), ""); err == nil {
		t.Error("encrypted to an Ed25519 key")
	}
	if _, err := EncryptJWEDirect(make([]byte, 16), []byte("x"), ""); err == nil {
		t.Error("dir with a 16-byte key")
	}
}

func TestDecryptJWE_RefusesTamperingAndSteering(t *testing.T) {
	x, _ := Generate(X25519)
	other, _ := Generate(X25519)
	aesKey, _ := Generate(AES256GCM)
	pub, _ := PublicKeyOf(X25519, x)
	tok, _ := EncryptJWETo(X25519, pub, []byte("secret"), "k.v1")
	parts := strings.Split(tok, ".")
	hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
	reheader := func(mutate func(map[string]any)) string {
		var h map[string]any
		_ = json.Unmarshal(hdr, &h)
		mutate(h)
		b, _ := json.Marshal(h)
		return base64.RawURLEncoding.EncodeToString(b) + "." + strings.Join(parts[1:], ".")
	}
	flip := func(i int) string {
		p := append([]string(nil), parts...)
		b, _ := base64.RawURLEncoding.DecodeString(p[i])
		if len(b) == 0 {
			b = []byte{1}
		} else {
			b[0] ^= 1
		}
		p[i] = base64.RawURLEncoding.EncodeToString(b)
		return strings.Join(p, ".")
	}
	cases := map[string]struct {
		k   KeyType
		key []byte
		tok string
	}{
		"wrong key":     {X25519, other, tok},
		"key type swap": {AES256GCM, aesKey, tok},
		"alg dir":       {X25519, x, reheader(func(h map[string]any) { h["alg"] = "dir" })},
		"enc A128GCM":   {X25519, x, reheader(func(h map[string]any) { h["enc"] = "A128GCM" })},
		"zip":           {X25519, x, reheader(func(h map[string]any) { h["zip"] = "DEF" })},
		"crit":          {X25519, x, reheader(func(h map[string]any) { h["crit"] = []string{"exp"} })},
		"kid changed":   {X25519, x, reheader(func(h map[string]any) { h["kid"] = "k.v2" })},
		"encrypted key": {X25519, x, flip(1)},
		"iv":            {X25519, x, flip(2)},
		"ciphertext":    {X25519, x, flip(3)},
		"tag":           {X25519, x, flip(4)},
		"four parts":    {X25519, x, strings.Join(parts[:4], ".")},
		"padded base64": {X25519, x, parts[0] + "=." + strings.Join(parts[1:], ".")},
		"low-order epk": {X25519, x, reheader(func(h map[string]any) {
			h["epk"].(map[string]any)["x"] = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		})},
		"epk wrong kind": {X25519, x, reheader(func(h map[string]any) { h["epk"].(map[string]any)["crv"] = "Ed25519" })},
	}
	for name, c := range cases {
		if pt, _, ok := DecryptJWE(c.k, c.key, c.tok); ok || pt != nil {
			t.Errorf("%s: opened (%q)", name, pt)
		}
	}
	if JWEKid(tok) != "k.v1" || JWEKid("nope") != "" {
		t.Error("JWEKid")
	}
}

func TestPasswords_HashVerifyAndBounds(t *testing.T) {
	h, err := HashPassword([]byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("hash %q", h)
	}
	if ok, err := VerifyPassword([]byte("correct horse battery staple"), h); err != nil || !ok {
		t.Errorf("verify right = %v %v", ok, err)
	}
	if ok, err := VerifyPassword([]byte("wrong"), h); err != nil || ok {
		t.Errorf("verify wrong = %v %v", ok, err)
	}
	h2, _ := HashPassword([]byte("correct horse battery staple"))
	if h2 == h {
		t.Error("two hashes of one password are equal: the salt is not random")
	}
	f := strings.Split(h, "$")
	for name, bad := range map[string]string{
		"huge memory":   "$argon2id$v=19$m=1048576,t=2,p=1$" + f[4] + "$" + f[5],
		"huge time":     "$argon2id$v=19$m=19456,t=99,p=1$" + f[4] + "$" + f[5],
		"many threads":  "$argon2id$v=19$m=19456,t=2,p=16$" + f[4] + "$" + f[5],
		"argon2i":       "$argon2i$v=19$m=19456,t=2,p=1$" + f[4] + "$" + f[5],
		"old version":   "$argon2id$v=16$m=19456,t=2,p=1$" + f[4] + "$" + f[5],
		"short salt":    "$argon2id$v=19$m=19456,t=2,p=1$AAAA$" + f[5],
		"not phc":       "plain",
		"leading zeros": "$argon2id$v=19$m=019456,t=2,p=1$" + f[4] + "$" + f[5],
	} {
		if _, err := VerifyPassword([]byte("x"), bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := HashPassword(make([]byte, MaxPasswordBytes+1)); err == nil {
		t.Error("an over-long password was hashed")
	}
	if _, err := HashPassword(nil); err == nil {
		t.Error("an empty password was hashed")
	}
}
