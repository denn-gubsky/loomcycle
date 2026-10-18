package kms

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// gcpClient talks to Google Cloud KMS's REST API, authenticated with an OAuth2
// access token it mints from a service-account key (a signed JWT exchanged at
// Google's token endpoint) and caches until shortly before it expires.
//
// Cloud KMS names asymmetric and MAC operations by key VERSION, and has no
// encrypt call for an RSA key (anyone with the public key encrypts). So an
// RSA-OAEP body carries the version it was made with ("v<N>:" + ciphertext),
// and an unpinned Ref uses the newest enabled version.
type gcpClient struct {
	base
	endpoint, tokenURL string
	cryptoKey          string // projects/…/cryptoKeys/<k>
	pinned             int    // a version the Ref pins, or 0
	saCred             string

	mu      sync.Mutex
	token   string
	expires time.Time
	pubs    map[int]crypto.PublicKey
}

const (
	gcpScope       = "https://www.googleapis.com/auth/cloudkms"
	gcpTokenURL    = "https://oauth2.googleapis.com/token"
	gcpEndpoint    = "https://cloudkms.googleapis.com"
	gcpTokenMargin = time.Minute
)

func newGCP(b base, r Ref, endpoint, tokenURL string) *gcpClient {
	if endpoint == "" {
		endpoint = gcpEndpoint
	}
	// The token endpoint is fixed, never taken from the service-account JSON:
	// a credential naming its own token_uri would have this process send a
	// signed assertion wherever it points.
	if tokenURL == "" {
		tokenURL = gcpTokenURL
	}
	key, ver, _ := strings.Cut(r.Resource, "/cryptoKeyVersions/")
	n, _ := strconv.Atoi(ver)
	return &gcpClient{base: b, endpoint: strings.TrimRight(endpoint, "/"), tokenURL: tokenURL, cryptoKey: key, pinned: n,
		saCred: r.ServiceAccountCred, pubs: map[int]crypto.PublicKey{}}
}

// accessToken returns a cached token, or mints one from the service account.
func (c *gcpClient) accessToken(ctx context.Context, op string) (string, error) {
	c.mu.Lock()
	if c.token != "" && c.now().Before(c.expires.Add(-gcpTokenMargin)) {
		t := c.token
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()

	raw, err := c.secret(ctx, c.saCred)
	if err != nil {
		return "", err
	}
	var sa struct {
		ClientEmail  string `json:"client_email"`
		PrivateKey   string `json:"private_key"`
		PrivateKeyID string `json:"private_key_id"`
	}
	if json.Unmarshal([]byte(raw), &sa) != nil || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return "", errf(c.kind, op, ErrDenied, 0, "the service account credential is not a service-account key JSON")
	}
	key, err := parseRSAPrivatePEM(sa.PrivateKey)
	if err != nil {
		return "", errf(c.kind, op, ErrDenied, 0, "the service account key is not a usable RSA private key")
	}
	now := c.now()
	assertion, err := signJWTRS256(key, sa.PrivateKeyID, map[string]any{
		"iss": sa.ClientEmail, "scope": gcpScope, "aud": c.tokenURL,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", errf(c.kind, op, ErrDenied, 0, "signing the token request failed")
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}.Encode()
	req, err := newJSONRequest(http.MethodPost, c.tokenURL, []byte(form), "application/x-www-form-urlencoded")
	if err != nil {
		return "", err
	}
	status, resp, err := c.send(ctx, op, req)
	if err != nil {
		return "", err
	}
	defer clear(resp)
	if status < 200 || status > 299 {
		sentinel := classifyStatus(status, false)
		if status == http.StatusBadRequest {
			sentinel = ErrDenied // invalid_grant: the key is revoked or wrong
		}
		return "", errf(c.kind, op, sentinel, status, "token exchange failed")
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if json.Unmarshal(resp, &tok) != nil || tok.AccessToken == "" || tok.ExpiresIn <= 0 {
		return "", errf(c.kind, op, ErrUnavailable, status, "malformed token response")
	}
	c.mu.Lock()
	c.token, c.expires = tok.AccessToken, now.Add(time.Duration(tok.ExpiresIn)*time.Second)
	c.mu.Unlock()
	return tok.AccessToken, nil
}

// call sends method to <endpoint>/v1/<path> with a JSON body (nil for GET).
func (c *gcpClient) call(ctx context.Context, op, method, path string, in, out any, decryptish bool) error {
	token, err := c.accessToken(ctx, op)
	if err != nil {
		return err
	}
	var body []byte
	if in != nil {
		if body, err = json.Marshal(in); err != nil {
			return err
		}
		defer clear(body)
	}
	req, err := newJSONRequest(method, c.endpoint+"/v1/"+path, body, "application/json")
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	status, resp, err := c.send(ctx, op, req)
	if err != nil {
		return err
	}
	defer clear(resp)
	if status < 200 || status > 299 {
		return errf(c.kind, op, classifyStatus(status, decryptish), status, "")
	}
	if out != nil && json.Unmarshal(resp, out) != nil {
		return errf(c.kind, op, ErrUnavailable, status, "malformed response")
	}
	return nil
}

func (c *gcpClient) versionName(v int) string {
	return c.cryptoKey + "/cryptoKeyVersions/" + strconv.Itoa(v)
}

// versionOf reads the version number from a cryptoKeyVersions resource name.
func versionOf(name string) int {
	_, v, ok := strings.Cut(name, "/cryptoKeyVersions/")
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(v)
	return n
}

// currentVersion is the pinned version, or the newest enabled one.
func (c *gcpClient) currentVersion(ctx context.Context, op string) (int, error) {
	if c.pinned != 0 {
		return c.pinned, nil
	}
	vs, err := c.enabledVersions(ctx, op)
	if err != nil {
		return 0, err
	}
	if len(vs) == 0 {
		return 0, errf(c.kind, op, ErrRejected, 0, "the key has no enabled version")
	}
	return vs[0], nil
}

// enabledVersions lists the key's enabled versions, newest first, bounded.
func (c *gcpClient) enabledVersions(ctx context.Context, op string) ([]int, error) {
	var out struct {
		CryptoKeyVersions []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"cryptoKeyVersions"`
	}
	path := c.cryptoKey + "/cryptoKeyVersions?pageSize=100&filter=" + url.QueryEscape("state=ENABLED")
	if err := c.call(ctx, op, http.MethodGet, path, nil, &out, false); err != nil {
		return nil, err
	}
	var vs []int
	for _, kv := range out.CryptoKeyVersions {
		if n := versionOf(kv.Name); n > 0 && kv.State == "ENABLED" {
			vs = append(vs, n)
		}
	}
	// Newest first.
	for i := 1; i < len(vs); i++ {
		for j := i; j > 0 && vs[j] > vs[j-1]; j-- {
			vs[j], vs[j-1] = vs[j-1], vs[j]
		}
	}
	if len(vs) > maxVersionTries {
		vs = vs[:maxVersionTries]
	}
	return vs, nil
}

func (c *gcpClient) Encrypt(ctx context.Context, plaintext, aad []byte) ([]byte, int, error) {
	const op = "encrypt"
	if err := c.need(op, isEncrypting(c.keyType)); err != nil {
		return nil, 0, err
	}
	if isRSAOAEP(c.keyType) {
		if err := c.noAAD(op, aad); err != nil {
			return nil, 0, err
		}
		pub, v, err := c.PublicKey(ctx, 0)
		if err != nil {
			return nil, 0, err
		}
		ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub.(*rsa.PublicKey), plaintext, nil)
		if err != nil {
			return nil, 0, errf(c.kind, op, ErrRejected, 0, "the plaintext is too long for this RSA key")
		}
		return append([]byte("v"+strconv.Itoa(v)+":"), ct...), v, nil
	}
	in := map[string]string{"plaintext": b64(plaintext)}
	if len(aad) > 0 {
		in["additionalAuthenticatedData"] = b64(aad)
	}
	var out struct {
		Name       string `json:"name"`
		Ciphertext string `json:"ciphertext"`
	}
	// Symmetric encryption goes to the key (its primary version) even when the
	// Ref pins one: Cloud KMS encrypts only with the primary.
	if err := c.call(ctx, op, http.MethodPost, c.cryptoKey+":encrypt", in, &out, false); err != nil {
		return nil, 0, err
	}
	body, err := base64.StdEncoding.DecodeString(out.Ciphertext)
	if err != nil || len(body) == 0 {
		return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed ciphertext in response")
	}
	return body, versionOf(out.Name), nil
}

func (c *gcpClient) Decrypt(ctx context.Context, body, aad []byte) ([]byte, int, error) {
	const op = "decrypt"
	if err := c.need(op, isEncrypting(c.keyType)); err != nil {
		return nil, 0, err
	}
	if isRSAOAEP(c.keyType) {
		if err := c.noAAD(op, aad); err != nil {
			return nil, 0, err
		}
		v, ct, ok := splitVersioned(body)
		if !ok {
			return nil, 0, errf(c.kind, op, ErrDecrypt, 0, "")
		}
		var out struct {
			Plaintext string `json:"plaintext"`
		}
		if err := c.call(ctx, op, http.MethodPost, c.versionName(v)+":asymmetricDecrypt", map[string]string{"ciphertext": b64(ct)}, &out, true); err != nil {
			return nil, 0, err
		}
		pt, err := base64.StdEncoding.DecodeString(out.Plaintext)
		if err != nil {
			return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed plaintext in response")
		}
		return pt, v, nil
	}
	in := map[string]string{"ciphertext": b64(body)}
	if len(aad) > 0 {
		in["additionalAuthenticatedData"] = b64(aad)
	}
	var out struct {
		Plaintext string `json:"plaintext"`
	}
	if err := c.call(ctx, op, http.MethodPost, c.cryptoKey+":decrypt", in, &out, true); err != nil {
		return nil, 0, err
	}
	pt, err := base64.StdEncoding.DecodeString(out.Plaintext)
	if err != nil {
		return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed plaintext in response")
	}
	return pt, 0, nil
}

// splitVersioned reads an RSA-OAEP body: "v<N>:" + ciphertext.
func splitVersioned(body []byte) (int, []byte, bool) {
	s := string(body)
	if !strings.HasPrefix(s, "v") {
		return 0, nil, false
	}
	num, _, ok := strings.Cut(s[1:], ":")
	if !ok {
		return 0, nil, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 || strconv.Itoa(n) != num {
		return 0, nil, false
	}
	return n, body[len(num)+2:], true
}

// Rewrap has no Cloud KMS call: it decrypts and encrypts again, the plaintext
// held only for the round trip and cleared after.
func (c *gcpClient) Rewrap(ctx context.Context, body, aad []byte) ([]byte, int, error) {
	pt, _, err := c.Decrypt(ctx, body, aad)
	if err != nil {
		return nil, 0, err
	}
	defer clear(pt)
	return c.Encrypt(ctx, pt, aad)
}

func (c *gcpClient) MAC(ctx context.Context, data []byte) ([]byte, int, error) {
	const op = "mac"
	if err := c.need(op, isMAC(c.keyType)); err != nil {
		return nil, 0, err
	}
	v, err := c.currentVersion(ctx, op)
	if err != nil {
		return nil, 0, err
	}
	var out struct {
		Mac string `json:"mac"`
	}
	if err := c.call(ctx, op, http.MethodPost, c.versionName(v)+":macSign", map[string]string{"data": b64(data)}, &out, false); err != nil {
		return nil, 0, err
	}
	mac, err := base64.StdEncoding.DecodeString(out.Mac)
	if err != nil || len(mac) == 0 {
		return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed mac in response")
	}
	return mac, v, nil
}

func (c *gcpClient) VerifyMAC(ctx context.Context, data, mac []byte, version int) (bool, int, error) {
	const op = "verify_mac"
	if err := c.need(op, isMAC(c.keyType)); err != nil {
		return false, 0, err
	}
	versions := []int{version}
	if version == 0 {
		vs, err := c.candidateVersions(ctx, op)
		if err != nil {
			return false, 0, err
		}
		versions = vs
	}
	for _, v := range versions {
		var out struct {
			Success bool `json:"success"`
		}
		err := c.call(ctx, op, http.MethodPost, c.versionName(v)+":macVerify", map[string]string{"data": b64(data), "mac": b64(mac)}, &out, true)
		switch {
		case errors.Is(err, ErrDecrypt):
			continue
		case err != nil:
			return false, 0, err
		case out.Success:
			return true, v, nil
		}
	}
	return false, 0, nil
}

// candidateVersions are the versions an unversioned verify tries: the pinned
// one, or every enabled one newest first.
func (c *gcpClient) candidateVersions(ctx context.Context, op string) ([]int, error) {
	if c.pinned != 0 {
		return []int{c.pinned}, nil
	}
	return c.enabledVersions(ctx, op)
}

func (c *gcpClient) Sign(ctx context.Context, data []byte) ([]byte, int, error) {
	const op = "sign"
	if err := c.need(op, isSigning(c.keyType)); err != nil {
		return nil, 0, err
	}
	v, err := c.currentVersion(ctx, op)
	if err != nil {
		return nil, 0, err
	}
	d := sha256.Sum256(data)
	var out struct {
		Signature string `json:"signature"`
	}
	in := map[string]any{"digest": map[string]string{"sha256": b64(d[:])}}
	if err := c.call(ctx, op, http.MethodPost, c.versionName(v)+":asymmetricSign", in, &out, false); err != nil {
		return nil, 0, err
	}
	sig, err := base64.StdEncoding.DecodeString(out.Signature)
	if err != nil || len(sig) == 0 {
		return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed signature in response")
	}
	if c.keyType == ECDSAP256 {
		if sig, err = derToRaw(sig); err != nil {
			return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed signature in response")
		}
	}
	return sig, v, nil
}

// Verify checks locally against each candidate version's public key.
func (c *gcpClient) Verify(ctx context.Context, data, sig []byte, version int) (bool, int, error) {
	const op = "verify"
	if err := c.need(op, isSigning(c.keyType)); err != nil {
		return false, 0, err
	}
	versions := []int{version}
	if version == 0 {
		vs, err := c.candidateVersions(ctx, op)
		if err != nil {
			return false, 0, err
		}
		versions = vs
	}
	for _, v := range versions {
		pub, _, err := c.PublicKey(ctx, v)
		if err != nil {
			continue
		}
		if verifyLocal(c.keyType, pub, data, sig) {
			return true, v, nil
		}
	}
	return false, 0, nil
}

func (c *gcpClient) PublicKey(ctx context.Context, version int) (crypto.PublicKey, int, error) {
	const op = "public_key"
	if err := c.need(op, hasPublicKey(c.keyType)); err != nil {
		return nil, 0, err
	}
	if version == 0 {
		v, err := c.currentVersion(ctx, op)
		if err != nil {
			return nil, 0, err
		}
		version = v
	}
	c.mu.Lock()
	cached := c.pubs[version]
	c.mu.Unlock()
	if cached != nil {
		return cached, version, nil
	}
	var out struct {
		Pem string `json:"pem"`
	}
	if err := c.call(ctx, op, http.MethodGet, c.versionName(version)+"/publicKey", nil, &out, false); err != nil {
		return nil, 0, err
	}
	pub, err := parsePublicPEM(out.Pem)
	if err != nil || !checkPublic(c.keyType, pub) {
		return nil, 0, errf(c.kind, op, ErrMismatch, 0, "public key is not a "+c.keyType+" key")
	}
	c.mu.Lock()
	c.pubs[version] = pub
	c.mu.Unlock()
	return pub, version, nil
}

func (c *gcpClient) Describe(ctx context.Context) (KeyInfo, error) {
	var out struct {
		Purpose string `json:"purpose"`
		Primary struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"primary"`
		VersionTemplate struct {
			Algorithm string `json:"algorithm"`
		} `json:"versionTemplate"`
	}
	if err := c.call(ctx, "describe", http.MethodGet, c.cryptoKey, nil, &out, false); err != nil {
		return KeyInfo{}, err
	}
	info := KeyInfo{KeyType: gcpKeyType(out.VersionTemplate.Algorithm), Native: out.VersionTemplate.Algorithm, Enabled: true}
	if out.Primary.Name != "" {
		info.Version, info.Enabled = versionOf(out.Primary.Name), out.Primary.State == "ENABLED"
	}
	return c.describeResult(info)
}

// gcpKeyType maps a Cloud KMS algorithm onto loomcycle's type.
func gcpKeyType(alg string) string {
	switch alg {
	case "GOOGLE_SYMMETRIC_ENCRYPTION":
		return AES256GCM
	case "HMAC_SHA256":
		return HMACSHA256
	case "HMAC_SHA512":
		return HMACSHA512
	case "EC_SIGN_P256_SHA256":
		return ECDSAP256
	}
	for _, bits := range []string{"2048", "3072", "4096"} {
		switch alg {
		case "RSA_SIGN_PSS_" + bits + "_SHA256":
			return "rsa-pss-" + bits
		case "RSA_DECRYPT_OAEP_" + bits + "_SHA256":
			return "rsa-oaep-" + bits
		}
	}
	return ""
}

// parseRSAPrivatePEM reads a PKCS#8 (or PKCS#1) RSA private key PEM, as a
// service-account key JSON carries it.
func parseRSAPrivatePEM(s string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, errors.New("not an RSA key")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// signJWTRS256 makes a compact RS256 JWT over claims.
func signJWTRS256(key *rsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if kid != "" {
		header["kid"] = kid
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString
	input := enc(hb) + "." + enc(cb)
	d := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
	if err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	return input + "." + enc(sig), nil
}
