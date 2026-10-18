package builtin

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/cryptoops"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// transitFake is a small Vault Transit that does the real cryptography, so a
// test proves what reaches the KMS and what comes back: AES-GCM with the
// associated data, ECDSA P-256, HMAC-SHA256 and RSA-OAEP-SHA256, each key with
// numbered versions.
type transitFake struct {
	t      *testing.T
	mu     sync.Mutex
	status int // when set, every call answers it
	calls  int
	keys   map[string]*transitKey
}

type transitKey struct {
	typ     string // aes256-gcm96 | ecdsa-p256 | hmac | rsa-2048
	aes     map[int][]byte
	ec      map[int]*ecdsa.PrivateKey
	rsa     map[int]*rsa.PrivateKey
	latest  int
	minDecr int
}

const transitToken = "s.transit-token"

func newTransitFake(t *testing.T) *transitFake {
	t.Helper()
	f := &transitFake{t: t, keys: map[string]*transitKey{}}
	f.keys["orders"] = &transitKey{typ: "aes256-gcm96", aes: map[int][]byte{1: randBytes(32)}, latest: 1, minDecr: 1}
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f.keys["signer"] = &transitKey{typ: "ecdsa-p256", ec: map[int]*ecdsa.PrivateKey{1: ec}, latest: 1, minDecr: 1}
	f.keys["macker"] = &transitKey{typ: "hmac", aes: map[int][]byte{1: randBytes(32)}, latest: 1, minDecr: 1}
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	f.keys["inbox"] = &transitKey{typ: "rsa-2048", rsa: map[int]*rsa.PrivateKey{1: rk}, latest: 1, minDecr: 1}
	return f
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// rotate adds a version to key name.
func (f *transitFake) rotate(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.keys[name]
	k.latest++
	switch {
	case k.aes != nil:
		k.aes[k.latest] = randBytes(32)
	case k.ec != nil:
		ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		k.ec[k.latest] = ec
	case k.rsa != nil:
		rk, _ := rsa.GenerateKey(rand.Reader, 2048)
		k.rsa[k.latest] = rk
	}
}

func (f *transitFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if r.Header.Get("X-Vault-Token") != transitToken {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"errors":["VAULT-INTERNAL-DETAIL"]}`))
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/transit/"), "/")
	if len(parts) < 2 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	endpoint, name := parts[0], parts[1]
	k := f.keys[name]
	if k == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var in map[string]string
	_ = json.NewDecoder(r.Body).Decode(&in)
	reply := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": data}) }
	bad := func() { w.WriteHeader(http.StatusBadRequest) }
	b64 := base64.StdEncoding
	val := func(field string) (int, []byte, bool) {
		rest, ok := strings.CutPrefix(in[field], "vault:v")
		if !ok {
			return 0, nil, false
		}
		num, body, ok := strings.Cut(rest, ":")
		v, err := strconv.Atoi(num)
		raw, err2 := b64.DecodeString(body)
		if !ok || err != nil || err2 != nil || v < k.minDecr || v > k.latest {
			return 0, nil, false
		}
		return v, raw, true
	}
	switch endpoint {
	case "keys":
		pubs := map[string]any{}
		for v := 1; v <= k.latest; v++ {
			var pub any
			switch {
			case k.ec != nil:
				pub = &k.ec[v].PublicKey
			case k.rsa != nil:
				pub = &k.rsa[v].PublicKey
			default:
				pubs[strconv.Itoa(v)] = 0
				continue
			}
			der, _ := x509.MarshalPKIXPublicKey(pub)
			pubs[strconv.Itoa(v)] = map[string]string{"public_key": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}
		}
		reply(map[string]any{"type": k.typ, "latest_version": k.latest, "min_decryption_version": k.minDecr, "keys": pubs})
	case "encrypt":
		pt, _ := b64.DecodeString(in["plaintext"])
		aad, _ := b64.DecodeString(in["associated_data"])
		g := gcmOf(k.aes[k.latest])
		nonce := randBytes(12)
		reply(map[string]string{"ciphertext": fmt.Sprintf("vault:v%d:%s", k.latest, b64.EncodeToString(g.Seal(nonce, nonce, pt, aad)))})
	case "decrypt":
		v, raw, ok := val("ciphertext")
		if !ok {
			bad()
			return
		}
		var pt []byte
		var err error
		if k.rsa != nil {
			pt, err = rsa.DecryptOAEP(sha256.New(), nil, k.rsa[v], raw, nil)
		} else {
			aad, _ := b64.DecodeString(in["associated_data"])
			if len(raw) < 12 {
				bad()
				return
			}
			pt, err = gcmOf(k.aes[v]).Open(nil, raw[:12], raw[12:], aad)
		}
		if err != nil {
			bad()
			return
		}
		reply(map[string]string{"plaintext": b64.EncodeToString(pt)})
	case "sign":
		data, _ := b64.DecodeString(in["input"])
		d := sha256.Sum256(data)
		sig, _ := ecdsa.SignASN1(rand.Reader, k.ec[k.latest], d[:])
		reply(map[string]string{"signature": fmt.Sprintf("vault:v%d:%s", k.latest, b64.EncodeToString(sig))})
	case "hmac":
		data, _ := b64.DecodeString(in["input"])
		m := hmac.New(sha256.New, k.aes[k.latest])
		m.Write(data)
		reply(map[string]string{"hmac": fmt.Sprintf("vault:v%d:%s", k.latest, b64.EncodeToString(m.Sum(nil)))})
	case "verify":
		v, mac, ok := val("hmac")
		if !ok {
			bad()
			return
		}
		data, _ := b64.DecodeString(in["input"])
		m := hmac.New(sha256.New, k.aes[v])
		m.Write(data)
		reply(map[string]bool{"valid": hmac.Equal(m.Sum(nil), mac)})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func gcmOf(key []byte) cipher.AEAD {
	b, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(b)
	return g
}

// kmsFixture is cryptoFixture with a transit fake reachable as an allowlisted
// Vault, and the Vault token stored as an ordinary tenant credential "vt".
func kmsFixture(t *testing.T, scopes ...string) (*Crypto, *CredentialDef, context.Context, *transitFake, string) {
	t.Helper()
	c, cd, ctx := cryptoFixture(t, scopes...)
	f := newTransitFake(t)
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	cfg := &config.Config{}
	cfg.Env.HTTPHostAllowlist = []string{u.Hostname()}
	access := &KMSAccess{Cfg: cfg, HTTP: srv.Client()}
	c.KMS, cd.KMS = access, access
	_, res := credExec(t, cd, ctx, `{"op":"create","scope":"tenant","name":"vt","value":"`+transitToken+`"}`)
	mustOK(t, "store the vault token", res)
	return c, cd, ctx, f, srv.URL
}

func vaultKeyBody(name, keyType, address, transitKey string) string {
	return fmt.Sprintf(`{"op":"create","usage":"crypto","scope":"tenant","name":%q,"key_type":%q,"backend":"vault",`+
		`"external":{"address":%q,"key":%q,"token":"$cred:vt"}}`, name, keyType, address, transitKey)
}

func TestCryptoKMS_AVaultKeyEncryptsAndDecryptsThroughTheKMS(t *testing.T) {
	c, cd, ctx, f, addr := kmsFixture(t, "tenant")
	out, res := credExec(t, cd, ctx, vaultKeyBody("orders", "aes-256-gcm", addr, "orders"))
	mustOK(t, "create", res)
	if out["backend"] != "inline" || out["key_backend"] != "vault" {
		t.Errorf("create meta = %v, want key_backend vault", out)
	}

	out, res = cryptoExec(t, c, ctx, `{"op":"encrypt","key":"orders","plaintext":"card 4111","context":"customer:42"}`)
	mustOK(t, "encrypt", res)
	ct := out["ciphertext"].(string)
	if !strings.HasPrefix(ct, "lcx1:kt:orders:1:") {
		t.Fatalf("ciphertext = %q, want an envelope naming version 1", ct)
	}
	out, res = cryptoExec(t, c, ctx, `{"op":"decrypt","ciphertext":"`+ct+`","context":"customer:42"}`)
	mustOK(t, "decrypt", res)
	if out["plaintext"] != "card 4111" {
		t.Errorf("plaintext = %v", out["plaintext"])
	}
	// The context reaches the KMS as associated data: another one does not open.
	_, res = cryptoExec(t, c, ctx, `{"op":"decrypt","ciphertext":"`+ct+`","context":"customer:43"}`)
	if !res.IsError || !strings.Contains(res.Text, "did not decrypt") {
		t.Errorf("decrypt with another context = %+v, want did not decrypt", res)
	}

	// After a rotation in the KMS, rewrap moves the value to the new version.
	f.rotate("orders")
	out, res = cryptoExec(t, c, ctx, `{"op":"rewrap","ciphertext":"`+ct+`","context":"customer:42"}`)
	mustOK(t, "rewrap", res)
	nct := out["ciphertext"].(string)
	if !strings.HasPrefix(nct, "lcx1:kt:orders:2:") || out["from_version"] != float64(1) {
		t.Errorf("rewrap = %v, want version 2 from 1", out)
	}
	out, res = cryptoExec(t, c, ctx, `{"op":"decrypt","ciphertext":"`+nct+`","context":"customer:42"}`)
	mustOK(t, "decrypt rewrapped", res)
	if out["plaintext"] != "card 4111" {
		t.Errorf("rewrapped plaintext = %v", out["plaintext"])
	}
}

// The envelope's name and scope are part of the associated data, so a body
// moved under another key's name (even one backed by the same KMS key) does
// not open.
func TestCryptoKMS_AnEnvelopeMovedToAnotherKeyNameDoesNotOpen(t *testing.T) {
	c, cd, ctx, _, addr := kmsFixture(t, "tenant")
	mustOK(t, "create a", second(credExec(t, cd, ctx, vaultKeyBody("a", "aes-256-gcm", addr, "orders"))))
	mustOK(t, "create b", second(credExec(t, cd, ctx, vaultKeyBody("b", "aes-256-gcm", addr, "orders"))))
	out, res := cryptoExec(t, c, ctx, `{"op":"encrypt","key":"a","plaintext":"x"}`)
	mustOK(t, "encrypt", res)
	moved := strings.Replace(out["ciphertext"].(string), "lcx1:kt:a:", "lcx1:kt:b:", 1)
	_, res = cryptoExec(t, c, ctx, `{"op":"decrypt","ciphertext":"`+moved+`"}`)
	if !res.IsError || !strings.Contains(res.Text, "did not decrypt") {
		t.Errorf("decrypt under b = %+v, want did not decrypt", res)
	}
}

func second[A, B any](_ A, b B) B { return b }

func TestCryptoKMS_SignVerifyAndJWSThroughTheKMS(t *testing.T) {
	c, cd, ctx, f, addr := kmsFixture(t, "tenant")
	mustOK(t, "create", second(credExec(t, cd, ctx, vaultKeyBody("signer", "ecdsa-p256", addr, "signer"))))

	out, res := cryptoExec(t, c, ctx, `{"op":"sign","key":"signer","data":"invoice 7"}`)
	mustOK(t, "sign", res)
	sig := out["signature"].(string)
	out, res = cryptoExec(t, c, ctx, `{"op":"verify","key":"signer","data":"invoice 7","signature":"`+sig+`"}`)
	mustOK(t, "verify", res)
	if out["valid"] != true {
		t.Errorf("verify = %v", out)
	}
	out, _ = cryptoExec(t, c, ctx, `{"op":"verify","key":"signer","data":"invoice 8","signature":"`+sig+`"}`)
	if out["valid"] != false {
		t.Errorf("verify of other data = %v, want false", out)
	}

	out, res = cryptoExec(t, c, ctx, `{"op":"sign","key":"signer","data":"{\"sub\":\"u1\"}","format":"jws"}`)
	mustOK(t, "sign jws", res)
	tok := out["signature"].(string)
	// The public key the KMS publishes verifies the token anywhere.
	out, res = cryptoExec(t, c, ctx, `{"op":"public_key","key":"signer"}`)
	mustOK(t, "public_key", res)
	pemKey := out["pem"].(string)
	if _, _, ok := cryptoops.VerifyJWS(cryptoops.ECDSAP256, nil, mustParsePub(t, pemKey), tok); !ok {
		t.Errorf("the KMS-signed JWS does not verify against the published public key")
	}
	// After a rotation, the old token still verifies (verify tries each
	// version the KMS still uses).
	f.rotate("signer")
	out, res = cryptoExec(t, c, ctx, `{"op":"verify","key":"signer","signature":"`+tok+`"}`)
	mustOK(t, "verify jws", res)
	if out["valid"] != true || out["key_version"] != float64(1) {
		t.Errorf("verify jws after rotation = %v, want valid at version 1", out)
	}
}

func mustParsePub(t *testing.T, pemKey string) any {
	t.Helper()
	b, _ := pem.Decode([]byte(pemKey))
	if b == nil {
		t.Fatal("no PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestCryptoKMS_MACAndAnRSAOAEPJWEThroughTheKMS(t *testing.T) {
	c, cd, ctx, f, addr := kmsFixture(t, "tenant")
	mustOK(t, "create mac", second(credExec(t, cd, ctx, vaultKeyBody("macker", "hmac-sha256", addr, "macker"))))
	out, res := cryptoExec(t, c, ctx, `{"op":"mac","key":"macker","data":"payload"}`)
	mustOK(t, "mac", res)
	mac := out["mac"].(string)
	out, res = cryptoExec(t, c, ctx, `{"op":"verify_mac","key":"macker","data":"payload","mac":"`+mac+`"}`)
	mustOK(t, "verify_mac", res)
	if out["valid"] != true {
		t.Errorf("verify_mac = %v", out)
	}

	mustOK(t, "create rsa", second(credExec(t, cd, ctx, vaultKeyBody("inbox", "rsa-oaep-2048", addr, "inbox"))))
	out, res = cryptoExec(t, c, ctx, `{"op":"encrypt","key":"inbox","plaintext":"for the inbox only"}`)
	mustOK(t, "encrypt to the inbox", res)
	tok := out["ciphertext"].(string)
	if out["format"] != "jwe" || cryptoops.JWEKid(tok) != "inbox.v1" {
		t.Fatalf("encrypt = %v, want a JWE with kid inbox.v1", out)
	}
	// The KMS rotates; the token names v1, and v1 unwraps it.
	f.rotate("inbox")
	out, res = cryptoExec(t, c, ctx, `{"op":"decrypt","key":"inbox","ciphertext":"`+tok+`"}`)
	mustOK(t, "decrypt", res)
	if out["plaintext"] != "for the inbox only" || out["key_version"] != float64(1) {
		t.Errorf("decrypt = %v", out)
	}
	// A tampered token is the same opaque failure as a wrong key.
	parts := strings.Split(tok, ".")
	parts[3] = base64.RawURLEncoding.EncodeToString(randBytes(12))
	_, res = cryptoExec(t, c, ctx, `{"op":"decrypt","key":"inbox","ciphertext":"`+strings.Join(parts, ".")+`"}`)
	if !res.IsError || !strings.Contains(res.Text, "did not decrypt") {
		t.Errorf("tampered JWE = %+v, want did not decrypt", res)
	}
}

func TestCryptoKMS_CreateChecksTheKeyInTheKMS(t *testing.T) {
	_, cd, ctx, _, addr := kmsFixture(t, "tenant")
	// The KMS key is AES; asking for an ECDSA key is refused.
	_, res := credExec(t, cd, ctx, vaultKeyBody("x", "ecdsa-p256", addr, "orders"))
	if !res.IsError || !strings.Contains(res.Text, "aes256-gcm96") {
		t.Errorf("type mismatch = %+v, want a refusal naming the KMS type", res)
	}
	// A key the KMS does not have.
	_, res = credExec(t, cd, ctx, vaultKeyBody("y", "aes-256-gcm", addr, "missing"))
	if !res.IsError || !strings.Contains(res.Text, "key not found") {
		t.Errorf("missing KMS key = %+v, want not found", res)
	}
	// Nothing was stored for either.
	out, _ := credExec(t, cd, ctx, `{"op":"list","scope":"tenant"}`)
	if n := len(out["credentials"].([]any)); n != 1 {
		t.Errorf("list has %d credentials, want only the vault token", n)
	}
}

func TestCryptoKMS_AVaultHostOffTheAllowlistIsRefused(t *testing.T) {
	c, cd, ctx, f, addr := kmsFixture(t, "tenant")
	// Off the list at create: refused, and the KMS is never called.
	saved := cd.KMS.Cfg.Env.HTTPHostAllowlist
	cd.KMS.Cfg.Env.HTTPHostAllowlist = nil
	_, res := credExec(t, cd, ctx, vaultKeyBody("orders", "aes-256-gcm", addr, "orders"))
	if !res.IsError || res.Error == nil || res.Error.Category != tools.CategoryPermission {
		t.Fatalf("create with an unlisted host = %+v, want a permission refusal", res)
	}
	if f.calls != 0 {
		t.Errorf("the KMS was called %d times for an unlisted host", f.calls)
	}
	// Listed at create, then taken off: refused at use, though a client was
	// already built and cached.
	cd.KMS.Cfg.Env.HTTPHostAllowlist = saved
	mustOK(t, "create", second(credExec(t, cd, ctx, vaultKeyBody("orders", "aes-256-gcm", addr, "orders"))))
	mustOK(t, "encrypt", second(cryptoExec(t, c, ctx, `{"op":"encrypt","key":"orders","plaintext":"x"}`)))
	cd.KMS.Cfg.Env.HTTPHostAllowlist = nil
	calls := f.calls
	_, res = cryptoExec(t, c, ctx, `{"op":"encrypt","key":"orders","plaintext":"x"}`)
	if !res.IsError || res.Error == nil || res.Error.Category != tools.CategoryPermission {
		t.Errorf("encrypt after the host left the list = %+v, want a permission refusal", res)
	}
	if f.calls != calls {
		t.Errorf("the KMS was called after its host left the list")
	}
}

// The token reference resolves in the key's own scope only: a tenant key
// cannot run on a credential only a user holds.
func TestCryptoKMS_KMSCredentialsResolveInTheKeysOwnScopeOnly(t *testing.T) {
	_, cd, ctx, _, addr := kmsFixture(t, "tenant")
	mustOK(t, "user token", second(credExec(t, cd, ctx, `{"op":"create","scope":"user","name":"utok","value":"`+transitToken+`"}`)))
	body := strings.Replace(vaultKeyBody("orders", "aes-256-gcm", addr, "orders"), "$cred:vt", "$cred:utok", 1)
	_, res := credExec(t, cd, ctx, body)
	if !res.IsError || !strings.Contains(res.Text, `no credential "utok" in the key's tenant scope`) {
		t.Errorf("a tenant key on a user's token = %+v, want refused", res)
	}
	// The same key in the user's own scope may use it.
	body = strings.Replace(body, `"scope":"tenant"`, `"scope":"user"`, 1)
	mustOK(t, "a user key on the user's token", second(credExec(t, cd, ctx, body)))
}

func TestCryptoKMS_AnExternalKeyIsManagedInTheKMS(t *testing.T) {
	_, cd, ctx, _, addr := kmsFixture(t, "tenant")
	mustOK(t, "create", second(credExec(t, cd, ctx, vaultKeyBody("orders", "aes-256-gcm", addr, "orders"))))
	for _, body := range []string{
		`{"op":"rotate","scope":"tenant","name":"orders"}`,
		`{"op":"set_min_version","scope":"tenant","name":"orders","min_version":2}`,
		`{"op":"delete_version","scope":"tenant","name":"orders","version":1}`,
		`{"op":"export_key","scope":"tenant","name":"orders","passphrase":"a long enough passphrase"}`,
	} {
		_, res := credExec(t, cd, ctx, body)
		if !res.IsError || !strings.Contains(res.Text, "external KMS") {
			t.Errorf("%s = %+v, want refused as held in an external KMS", body, res)
		}
	}
	// delete removes the reference (the key stays in the KMS).
	out, res := credExec(t, cd, ctx, `{"op":"delete","scope":"tenant","name":"orders"}`)
	mustOK(t, "delete", res)
	if out["deleted"] != true {
		t.Errorf("delete = %v", out)
	}
}

func TestCryptoKMS_ExternalKeysAreNotMadeInsideARun(t *testing.T) {
	_, cd, ctx, f, addr := kmsFixture(t, "tenant")
	runCtx := tools.WithRunID(ctx, "run-1")
	body := strings.Replace(vaultKeyBody("mine", "aes-256-gcm", addr, "orders"), `"scope":"tenant"`, `"scope":"agent"`, 1)
	_, res := credExec(t, cd, runCtx, body)
	if !res.IsError || res.Error == nil || res.Error.Category != tools.CategoryPermission {
		t.Errorf("create inside a run = %+v, want a permission refusal", res)
	}
	if f.calls != 0 {
		t.Errorf("the KMS was called from inside a run")
	}
}

func TestCryptoKMS_KMSFailuresNeverCarryTheResponseBody(t *testing.T) {
	c, cd, ctx, f, addr := kmsFixture(t, "tenant")
	mustOK(t, "create", second(credExec(t, cd, ctx, vaultKeyBody("orders", "aes-256-gcm", addr, "orders"))))
	for status, cat := range map[int]tools.ErrorCategory{
		http.StatusServiceUnavailable: tools.CategoryTransient,
		http.StatusForbidden:          tools.CategoryPermission,
	} {
		f.mu.Lock()
		f.status = status
		f.mu.Unlock()
		_, res := cryptoExec(t, c, ctx, `{"op":"encrypt","key":"orders","plaintext":"x"}`)
		if !res.IsError || res.Error == nil || res.Error.Category != cat {
			t.Errorf("HTTP %d = %+v, want category %s", status, res, cat)
		}
		if strings.Contains(res.Text, "VAULT-INTERNAL-DETAIL") || strings.Contains(res.Text, transitToken) {
			t.Errorf("HTTP %d leaked the response or the token: %s", status, res.Text)
		}
	}
}
