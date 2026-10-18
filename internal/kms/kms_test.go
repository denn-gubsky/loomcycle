package kms

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func secretsOf(m map[string]string) Secrets {
	return func(_ context.Context, ref string) (string, error) {
		v, ok := m[ref]
		if !ok {
			return "", errors.New("no such credential")
		}
		return v, nil
	}
}

func pubPEM(t *testing.T, pub crypto.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func ecdsaDERSign(t *testing.T, k *ecdsa.PrivateKey, digest []byte) []byte {
	t.Helper()
	sig, err := ecdsa.SignASN1(rand.Reader, k, digest)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func verifyRaw(pub *ecdsa.PublicKey, data, sig []byte) bool {
	d := sha256.Sum256(data)
	return len(sig) == 64 && ecdsa.Verify(pub, d[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}

// AWS's published SigV4 test-suite case "get-vanilla".
func TestSignV4_MatchesTheAWSTestSuiteGetVanilla(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	now, _ := time.Parse("20060102T150405Z", "20150830T123600Z")
	signV4(req, nil, "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "", "us-east-1", "service", now)
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n %s\nwant\n %s", got, want)
	}
}

func TestRefValidate_Shapes(t *testing.T) {
	good := []Ref{
		{Kind: KindVault, Address: "https://vault.example:8200", Key: "orders", TokenCred: "$cred:vault-token"},
		{Kind: KindVault, Address: "https://vault.example/proxy", Mount: "kv/transit", Key: "k", Namespace: "team/a", TokenCred: "$cred:t"},
		{Kind: KindAWS, Region: "eu-west-1", KeyID: "alias/orders", AccessKeyIDCred: "$cred:a", SecretAccessKeyCred: "$cred:s"},
		{Kind: KindAWS, Region: "us-east-1", KeyID: "arn:aws:kms:us-east-1:123456789012:key/abc", AccessKeyIDCred: "$cred:a", SecretAccessKeyCred: "$cred:s", SessionTokenCred: "$cred:st"},
		{Kind: KindGCP, Resource: "projects/p1/locations/global/keyRings/r/cryptoKeys/k", ServiceAccountCred: "$cred:sa"},
		{Kind: KindGCP, Resource: "projects/p1/locations/europe-west1/keyRings/r/cryptoKeys/k/cryptoKeyVersions/3", ServiceAccountCred: "$cred:sa"},
	}
	for i, r := range good {
		if err := r.Validate(false); err != nil {
			t.Errorf("good[%d]: %v", i, err)
		}
	}
	bad := map[string]Ref{
		"vault http":         {Kind: KindVault, Address: "http://vault:8200", Key: "k", TokenCred: "$cred:t"},
		"vault userinfo":     {Kind: KindVault, Address: "https://u:p@vault", Key: "k", TokenCred: "$cred:t"},
		"vault query":        {Kind: KindVault, Address: "https://vault/?x=1", Key: "k", TokenCred: "$cred:t"},
		"vault dot key":      {Kind: KindVault, Address: "https://vault", Key: "..", TokenCred: "$cred:t"},
		"vault mount climb":  {Kind: KindVault, Address: "https://vault", Mount: "a/../b", Key: "k", TokenCred: "$cred:t"},
		"vault raw token":    {Kind: KindVault, Address: "https://vault", Key: "k", TokenCred: "s.SECRETTOKEN"},
		"vault aws field":    {Kind: KindVault, Address: "https://vault", Key: "k", TokenCred: "$cred:t", Region: "us-east-1"},
		"aws bad region":     {Kind: KindAWS, Region: "evil.example.com/x", KeyID: "k", AccessKeyIDCred: "$cred:a", SecretAccessKeyCred: "$cred:s"},
		"aws arn region":     {Kind: KindAWS, Region: "us-east-1", KeyID: "arn:aws:kms:eu-west-1:1:key/k", AccessKeyIDCred: "$cred:a", SecretAccessKeyCred: "$cred:s"},
		"aws missing secret": {Kind: KindAWS, Region: "us-east-1", KeyID: "k", AccessKeyIDCred: "$cred:a"},
		"gcp resource":       {Kind: KindGCP, Resource: "projects/p/keyRings/r", ServiceAccountCred: "$cred:sa"},
		"gcp raw json":       {Kind: KindGCP, Resource: "projects/p1/locations/global/keyRings/r/cryptoKeys/k", ServiceAccountCred: "{}"},
		"unknown kind":       {Kind: "azure"},
	}
	for name, r := range bad {
		if err := r.Validate(false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (Ref{Kind: KindVault, Address: "http://vault:8200", Key: "k", TokenCred: "$cred:t"}).Validate(true); err != nil {
		t.Errorf("http with AllowHTTP: %v", err)
	}
	if _, err := New(Ref{Kind: KindAWS, Region: "us-east-1", KeyID: "k", AccessKeyIDCred: "$cred:a", SecretAccessKeyCred: "$cred:s"}, Ed25519, Options{Secrets: secretsOf(nil)}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ed25519 on AWS = %v, want ErrUnsupported", err)
	}
}

// ---- Vault -----------------------------------------------------------------

type fakeVault struct {
	t      *testing.T
	ec     *ecdsa.PrivateKey
	status int // when non-zero, every call answers this with a body holding a secret
}

func (f *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Vault-Token") != "s.vault-token" || r.Header.Get("X-Vault-Namespace") != "team" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"errors":["LEAKED-VAULT-DETAIL"]}`))
		return
	}
	var in map[string]string
	_ = json.NewDecoder(r.Body).Decode(&in)
	reply := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": data}) }
	switch {
	case r.URL.Path == "/v1/transit/encrypt/orders":
		sealed, _ := json.Marshal(in)
		reply(map[string]string{"ciphertext": "vault:v2:" + base64.StdEncoding.EncodeToString(sealed)})
	case r.URL.Path == "/v1/transit/decrypt/orders":
		_, rest, _ := parseVaultValue(in["ciphertext"])
		raw, _ := base64.StdEncoding.DecodeString(rest)
		var sealed map[string]string
		if json.Unmarshal(raw, &sealed) != nil || sealed["associated_data"] != in["associated_data"] {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reply(map[string]string{"plaintext": sealed["plaintext"]})
	case r.URL.Path == "/v1/transit/sign/orders/sha2-256":
		data, _ := base64.StdEncoding.DecodeString(in["input"])
		d := sha256.Sum256(data)
		reply(map[string]string{"signature": "vault:v1:" + base64.StdEncoding.EncodeToString(ecdsaDERSign(f.t, f.ec, d[:]))})
	case r.URL.Path == "/v1/transit/keys/orders" && r.Method == http.MethodGet:
		reply(map[string]any{"type": "ecdsa-p256", "latest_version": 1, "min_decryption_version": 1,
			"keys": map[string]any{"1": map[string]string{"public_key": pubPEM(f.t, &f.ec.PublicKey)}}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func vaultFor(t *testing.T, f *fakeVault, keyType string) Client {
	t.Helper()
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Ref{Kind: KindVault, Address: srv.URL, Key: "orders", Namespace: "team", TokenCred: "$cred:vt"}, keyType,
		Options{HTTPClient: srv.Client(), Secrets: secretsOf(map[string]string{"$cred:vt": "s.vault-token"})})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVault_EncryptDecryptCarriesTheAssociatedData(t *testing.T) {
	c := vaultFor(t, &fakeVault{t: t}, AES256GCM)
	ctx := context.Background()
	body, v, err := c.Encrypt(ctx, []byte("card"), []byte("rec:1"))
	if err != nil || v != 2 || !strings.HasPrefix(string(body), "vault:v2:") {
		t.Fatalf("encrypt = %q v%d %v", body, v, err)
	}
	pt, _, err := c.Decrypt(ctx, body, []byte("rec:1"))
	if err != nil || string(pt) != "card" {
		t.Fatalf("decrypt = %q %v", pt, err)
	}
	if _, _, err := c.Decrypt(ctx, body, []byte("rec:2")); !errors.Is(err, ErrDecrypt) {
		t.Errorf("wrong associated data = %v, want ErrDecrypt", err)
	}
	if _, _, err := c.Sign(ctx, []byte("x")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("sign with an AES key = %v, want ErrUnsupported", err)
	}
}

func TestVault_ECDSASignaturesAreRawAndVerify(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c := vaultFor(t, &fakeVault{t: t, ec: ec}, ECDSAP256)
	ctx := context.Background()
	sig, _, err := c.Sign(ctx, []byte("report"))
	if err != nil || !verifyRaw(&ec.PublicKey, []byte("report"), sig) {
		t.Fatalf("sign = %d bytes %v; want a 64-byte r||s that verifies", len(sig), err)
	}
	ok, v, err := c.Verify(ctx, []byte("report"), sig, 0)
	if err != nil || !ok || v != 1 {
		t.Errorf("verify = %v v%d %v", ok, v, err)
	}
	if ok, _, _ := c.Verify(ctx, []byte("other"), sig, 0); ok {
		t.Error("a signature verified over other data")
	}
	info, err := c.Describe(ctx)
	if err != nil || info.KeyType != ECDSAP256 {
		t.Errorf("describe = %+v %v", info, err)
	}
}

func TestVault_ErrorsAreClassifiedAndCarryNoBodyOrToken(t *testing.T) {
	for status, want := range map[int]error{403: ErrDenied, 500: ErrUnavailable, 404: ErrNotFound} {
		c := vaultFor(t, &fakeVault{t: t, status: status}, AES256GCM)
		_, _, err := c.Encrypt(context.Background(), []byte("x"), nil)
		if !errors.Is(err, want) {
			t.Errorf("HTTP %d = %v, want %v", status, err, want)
		}
		if s := err.Error(); strings.Contains(s, "LEAKED") || strings.Contains(s, "s.vault-token") {
			t.Errorf("HTTP %d error leaks: %s", status, s)
		}
	}
}

// ---- AWS -------------------------------------------------------------------

type fakeAWS struct {
	t   *testing.T
	ec  *ecdsa.PrivateKey
	mac []byte
}

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIATEST/") || !strings.Contains(auth, "/kms/aws4_request") ||
		r.Header.Get("Content-Type") != "application/x-amz-json-1.1" || r.Header.Get("X-Amz-Security-Token") != "session" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"__type":"com.amazon.coral.service#UnrecognizedClientException","message":"LEAKED-AWS-DETAIL"}`))
		return
	}
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	fail := func(typ string) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"__type":"` + typ + `","message":"LEAKED-AWS-DETAIL"}`))
	}
	switch strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "TrentService.") {
	case "Encrypt":
		sealed, _ := json.Marshal(map[string]any{"pt": in["Plaintext"], "ctx": in["EncryptionContext"]})
		reply(map[string]string{"CiphertextBlob": base64.StdEncoding.EncodeToString(sealed)})
	case "Decrypt":
		raw, _ := base64.StdEncoding.DecodeString(in["CiphertextBlob"].(string))
		var sealed map[string]any
		_ = json.Unmarshal(raw, &sealed)
		a, _ := json.Marshal(sealed["ctx"])
		b, _ := json.Marshal(in["EncryptionContext"])
		if in["KeyId"] != "alias/orders" || !bytes.Equal(a, b) {
			fail("InvalidCiphertextException")
			return
		}
		reply(map[string]any{"Plaintext": sealed["pt"]})
	case "Sign":
		if in["MessageType"] != "DIGEST" || in["SigningAlgorithm"] != "ECDSA_SHA_256" {
			fail("ValidationException")
			return
		}
		d, _ := base64.StdEncoding.DecodeString(in["Message"].(string))
		reply(map[string]string{"Signature": base64.StdEncoding.EncodeToString(ecdsaDERSign(f.t, f.ec, d))})
	case "GetPublicKey":
		der, _ := x509.MarshalPKIXPublicKey(&f.ec.PublicKey)
		reply(map[string]string{"PublicKey": base64.StdEncoding.EncodeToString(der), "KeySpec": "ECC_NIST_P256", "KeyUsage": "SIGN_VERIFY"})
	case "VerifyMac":
		got, _ := base64.StdEncoding.DecodeString(in["Mac"].(string))
		if !bytes.Equal(got, f.mac) {
			fail("KMSInvalidMacException")
			return
		}
		reply(map[string]bool{"MacValid": true})
	case "DescribeKey":
		reply(map[string]any{"KeyMetadata": map[string]any{"KeySpec": "SYMMETRIC_DEFAULT", "KeyUsage": "ENCRYPT_DECRYPT", "KeyState": "Enabled", "Enabled": true}})
	default:
		fail("AccessDeniedException")
	}
}

func awsFor(t *testing.T, f *fakeAWS, keyType string) Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Ref{Kind: KindAWS, Region: "eu-west-1", KeyID: "alias/orders", AccessKeyIDCred: "$cred:a", SecretAccessKeyCred: "$cred:s", SessionTokenCred: "$cred:t"},
		keyType, Options{AWSEndpoint: srv.URL, Secrets: secretsOf(map[string]string{"$cred:a": "AKIATEST", "$cred:s": "secret-key", "$cred:t": "session"})})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAWS_EncryptionContextBindsTheAssociatedData(t *testing.T) {
	c := awsFor(t, &fakeAWS{t: t}, AES256GCM)
	ctx := context.Background()
	body, v, err := c.Encrypt(ctx, []byte("card"), []byte("rec:1"))
	if err != nil || v != 0 {
		t.Fatalf("encrypt = %v v%d", err, v)
	}
	if pt, _, err := c.Decrypt(ctx, body, []byte("rec:1")); err != nil || string(pt) != "card" {
		t.Fatalf("decrypt = %q %v", pt, err)
	}
	_, _, err = c.Decrypt(ctx, body, []byte("rec:2"))
	if !errors.Is(err, ErrDecrypt) || strings.Contains(err.Error(), "LEAKED") {
		t.Errorf("wrong context = %v, want an opaque ErrDecrypt", err)
	}
	if info, err := c.Describe(ctx); err != nil || info.KeyType != AES256GCM || !info.Enabled {
		t.Errorf("describe = %+v %v", info, err)
	}
	if _, err := c.(*awsClient).base.describeResult(KeyInfo{KeyType: HMACSHA256}); !errors.Is(err, ErrMismatch) {
		t.Errorf("a mismatched type = %v, want ErrMismatch", err)
	}
}

func TestAWS_ECDSAIsConvertedAndRSARefusesAssociatedData(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c := awsFor(t, &fakeAWS{t: t, ec: ec}, ECDSAP256)
	ctx := context.Background()
	sig, _, err := c.Sign(ctx, []byte("report"))
	if err != nil || !verifyRaw(&ec.PublicKey, []byte("report"), sig) {
		t.Fatalf("sign = %v; want a raw signature that verifies", err)
	}
	if ok, _, err := c.Verify(ctx, []byte("report"), sig, 0); err != nil || !ok {
		t.Errorf("verify = %v %v", ok, err)
	}
	der, _ := rawToDER(sig)
	if back, err := derToRaw(der); err != nil || !bytes.Equal(back, sig) {
		t.Error("raw → DER → raw is not the identity")
	}
	r := awsFor(t, &fakeAWS{t: t}, RSAOAEP2048)
	if _, _, err := r.Encrypt(ctx, []byte("x"), []byte("aad")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("RSA with aad = %v, want ErrUnsupported", err)
	}
}

func TestAWS_VerifyMacAndDeniedCredentials(t *testing.T) {
	c := awsFor(t, &fakeAWS{t: t, mac: []byte("right")}, HMACSHA256)
	ctx := context.Background()
	if ok, _, err := c.VerifyMAC(ctx, []byte("d"), []byte("right"), 0); err != nil || !ok {
		t.Errorf("right mac = %v %v", ok, err)
	}
	if ok, _, err := c.VerifyMAC(ctx, []byte("d"), []byte("wrong"), 0); err != nil || ok {
		t.Errorf("wrong mac = %v %v, want false and no error", ok, err)
	}
	srv := httptest.NewServer(&fakeAWS{t: t})
	t.Cleanup(srv.Close)
	bad, _ := New(Ref{Kind: KindAWS, Region: "eu-west-1", KeyID: "alias/orders", AccessKeyIDCred: "$cred:a", SecretAccessKeyCred: "$cred:s"},
		AES256GCM, Options{AWSEndpoint: srv.URL, Secrets: secretsOf(map[string]string{"$cred:a": "AKIAOTHER", "$cred:s": "secret-key"})})
	_, _, err := bad.Encrypt(ctx, []byte("x"), nil)
	if !errors.Is(err, ErrDenied) || strings.Contains(err.Error(), "LEAKED") || strings.Contains(err.Error(), "secret-key") {
		t.Errorf("bad credentials = %v, want an ErrDenied that leaks nothing", err)
	}
}

// ---- Google Cloud KMS ------------------------------------------------------

const gcpKey = "projects/p1/locations/global/keyRings/r/cryptoKeys/k"

type fakeGCP struct {
	t       *testing.T
	saPub   *rsa.PublicKey
	ec      *ecdsa.PrivateKey
	rsaKey  *rsa.PrivateKey
	tokenN  atomic.Int32
	tokenAt string
}

func (f *fakeGCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	if r.URL.Path == "/token" {
		f.tokenN.Add(1)
		_ = r.ParseForm()
		parts := strings.Split(r.PostForm.Get("assertion"), ".")
		if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || len(parts) != 3 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		d := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		_ = json.Unmarshal(cb, &claims)
		if rsa.VerifyPKCS1v15(f.saPub, crypto.SHA256, d[:], sig) != nil || claims["iss"] != "svc@p1.iam.gserviceaccount.com" ||
			claims["scope"] != gcpScope || claims["aud"] != f.tokenAt {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reply(map[string]any{"access_token": "ya29.token", "expires_in": 3600})
		return
	}
	if r.Header.Get("Authorization") != "Bearer ya29.token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	switch {
	case path == gcpKey+":encrypt":
		sealed, _ := json.Marshal(in)
		reply(map[string]string{"name": gcpKey + "/cryptoKeyVersions/3", "ciphertext": base64.StdEncoding.EncodeToString(sealed)})
	case path == gcpKey+":decrypt":
		raw, _ := base64.StdEncoding.DecodeString(in["ciphertext"].(string))
		var sealed map[string]any
		_ = json.Unmarshal(raw, &sealed)
		if sealed["additionalAuthenticatedData"] != in["additionalAuthenticatedData"] {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reply(map[string]any{"plaintext": sealed["plaintext"]})
	case path == gcpKey+"/cryptoKeyVersions":
		reply(map[string]any{"cryptoKeyVersions": []map[string]string{
			{"name": gcpKey + "/cryptoKeyVersions/1", "state": "ENABLED"},
			{"name": gcpKey + "/cryptoKeyVersions/2", "state": "ENABLED"},
		}})
	case path == gcpKey+"/cryptoKeyVersions/2:asymmetricSign":
		d, _ := base64.StdEncoding.DecodeString(in["digest"].(map[string]any)["sha256"].(string))
		reply(map[string]string{"signature": base64.StdEncoding.EncodeToString(ecdsaDERSign(f.t, f.ec, d))})
	case path == gcpKey+"/cryptoKeyVersions/2/publicKey" && f.ec != nil:
		reply(map[string]string{"pem": pubPEM(f.t, &f.ec.PublicKey)})
	case path == gcpKey+"/cryptoKeyVersions/2/publicKey" && f.rsaKey != nil:
		reply(map[string]string{"pem": pubPEM(f.t, &f.rsaKey.PublicKey)})
	case path == gcpKey+"/cryptoKeyVersions/2:asymmetricDecrypt":
		ct, _ := base64.StdEncoding.DecodeString(in["ciphertext"].(string))
		pt, err := rsa.DecryptOAEP(sha256.New(), nil, f.rsaKey, ct, nil)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reply(map[string]string{"plaintext": base64.StdEncoding.EncodeToString(pt)})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func gcpFor(t *testing.T, f *fakeGCP, keyType string, now func() time.Time) Client {
	t.Helper()
	sa, _ := rsa.GenerateKey(rand.Reader, 2048)
	f.saPub = &sa.PublicKey
	der, _ := x509.MarshalPKCS8PrivateKey(sa)
	saJSON, _ := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "svc@p1.iam.gserviceaccount.com", "private_key_id": "kid1",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":   "https://attacker.example/token", // must be ignored
	})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.tokenAt = srv.URL + "/token"
	c, err := New(Ref{Kind: KindGCP, Resource: gcpKey, ServiceAccountCred: "$cred:sa"}, keyType,
		Options{GCPEndpoint: srv.URL, GCPTokenURL: f.tokenAt, Now: now, Secrets: secretsOf(map[string]string{"$cred:sa": string(saJSON)})})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGCP_TokenIsMintedOnceAndRefreshedNearExpiry(t *testing.T) {
	f := &fakeGCP{t: t}
	now := time.Unix(1_800_000_000, 0)
	c := gcpFor(t, f, AES256GCM, func() time.Time { return now })
	ctx := context.Background()
	body, v, err := c.Encrypt(ctx, []byte("card"), []byte("rec:1"))
	if err != nil || v != 3 {
		t.Fatalf("encrypt = v%d %v", v, err)
	}
	if pt, _, err := c.Decrypt(ctx, body, []byte("rec:1")); err != nil || string(pt) != "card" {
		t.Fatalf("decrypt = %q %v", pt, err)
	}
	if _, _, err := c.Decrypt(ctx, body, []byte("rec:2")); !errors.Is(err, ErrDecrypt) {
		t.Errorf("wrong aad = %v, want ErrDecrypt", err)
	}
	if n := f.tokenN.Load(); n != 1 {
		t.Errorf("token requests = %d for three calls, want 1", n)
	}
	now = now.Add(59*time.Minute + 30*time.Second) // inside the refresh margin
	if _, _, err := c.Encrypt(ctx, []byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	if n := f.tokenN.Load(); n != 2 {
		t.Errorf("token requests after near-expiry = %d, want 2", n)
	}
}

func TestGCP_SignVerifyAndRSAOAEPUseTheNewestEnabledVersion(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c := gcpFor(t, &fakeGCP{t: t, ec: ec}, ECDSAP256, nil)
	ctx := context.Background()
	sig, v, err := c.Sign(ctx, []byte("report"))
	if err != nil || v != 2 || !verifyRaw(&ec.PublicKey, []byte("report"), sig) {
		t.Fatalf("sign = v%d %v", v, err)
	}
	if ok, v, err := c.Verify(ctx, []byte("report"), sig, 0); err != nil || !ok || v != 2 {
		t.Errorf("verify = %v v%d %v", ok, v, err)
	}

	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	r := gcpFor(t, &fakeGCP{t: t, rsaKey: rk}, RSAOAEP2048, nil)
	body, v, err := r.Encrypt(ctx, []byte("wrapped"), nil)
	if err != nil || v != 2 || !strings.HasPrefix(string(body), "v2:") {
		t.Fatalf("rsa encrypt = %q v%d %v", body[:min(4, len(body))], v, err)
	}
	if pt, v, err := r.Decrypt(ctx, body, nil); err != nil || string(pt) != "wrapped" || v != 2 {
		t.Errorf("rsa decrypt = %q v%d %v", pt, v, err)
	}
	if _, _, err := r.Decrypt(ctx, []byte("garbage"), nil); !errors.Is(err, ErrDecrypt) {
		t.Errorf("an unversioned body = %v, want ErrDecrypt", err)
	}
}

func TestNew_DisablesRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	c, _ := New(Ref{Kind: KindVault, Address: redirector.URL, Key: "k", TokenCred: "$cred:t"}, AES256GCM,
		Options{AllowHTTP: true, Secrets: secretsOf(map[string]string{"$cred:t": "s.token"})})
	_, _, err := c.Encrypt(context.Background(), []byte("x"), nil)
	if !errors.Is(err, ErrUnavailable) || hits.Load() != 0 {
		t.Errorf("redirect = %v, target hit %d times; want refused and never followed", err, hits.Load())
	}
}
