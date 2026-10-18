package kms

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// vaultClient talks to a Vault Transit secrets engine over its HTTP API.
// Ciphertexts are Vault's own "vault:v<N>:<base64>" strings, kept verbatim as
// the body so Vault can read its version back on decrypt.
type vaultClient struct {
	base
	address, mount, key, namespace, tokenCred string
}

func newVault(b base, r Ref) *vaultClient {
	mount := r.Mount
	if mount == "" {
		mount = defaultVaultMount
	}
	return &vaultClient{base: b, address: strings.TrimRight(r.Address, "/"), mount: mount, key: r.Key, namespace: r.Namespace, tokenCred: r.TokenCred}
}

// call POSTs (or GETs, when in is nil) <address>/v1/<mount>/<path>/<key>[/suffix]
// and decodes the response's "data" object into out.
func (c *vaultClient) call(ctx context.Context, op, method, endpoint, suffix string, in, out any, decryptish bool) error {
	token, err := c.secret(ctx, c.tokenCred)
	if err != nil {
		return err
	}
	u := c.address + "/v1/" + c.mount + "/" + endpoint + "/" + url.PathEscape(c.key)
	if suffix != "" {
		u += "/" + suffix
	}
	var body []byte
	if in != nil {
		if body, err = json.Marshal(in); err != nil {
			return err
		}
		defer clear(body)
	}
	req, err := newJSONRequest(method, u, body, "application/json")
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", token)
	if c.namespace != "" {
		req.Header.Set("X-Vault-Namespace", c.namespace)
	}
	status, resp, err := c.send(ctx, op, req)
	if err != nil {
		return err
	}
	defer clear(resp)
	if status < 200 || status > 299 {
		return errf(c.kind, op, classifyStatus(status, decryptish), status, "")
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp, &env); err != nil || len(env.Data) == 0 || json.Unmarshal(env.Data, out) != nil {
		return errf(c.kind, op, ErrUnavailable, status, "malformed response")
	}
	return nil
}

// parseVaultValue splits "vault:v<N>:<rest>".
func parseVaultValue(s string) (version int, rest string, ok bool) {
	tail, found := strings.CutPrefix(s, "vault:v")
	if !found {
		return 0, "", false
	}
	num, rest, found := strings.Cut(tail, ":")
	if !found {
		return 0, "", false
	}
	v, err := strconv.Atoi(num)
	if err != nil || v < 1 || strconv.Itoa(v) != num {
		return 0, "", false
	}
	return v, rest, true
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func (c *vaultClient) hmacAlg() string {
	if c.keyType == HMACSHA512 {
		return "sha2-512"
	}
	return "sha2-256"
}

func (c *vaultClient) Encrypt(ctx context.Context, plaintext, aad []byte) ([]byte, int, error) {
	const op = "encrypt"
	if err := c.need(op, isEncrypting(c.keyType)); err != nil {
		return nil, 0, err
	}
	if isRSAOAEP(c.keyType) {
		if err := c.noAAD(op, aad); err != nil {
			return nil, 0, err
		}
	}
	in := map[string]string{"plaintext": b64(plaintext)}
	if len(aad) > 0 {
		in["associated_data"] = b64(aad)
	}
	var out struct {
		Ciphertext string `json:"ciphertext"`
	}
	if err := c.call(ctx, op, http.MethodPost, "encrypt", "", in, &out, false); err != nil {
		return nil, 0, err
	}
	v, _, ok := parseVaultValue(out.Ciphertext)
	if !ok {
		return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed ciphertext in response")
	}
	return []byte(out.Ciphertext), v, nil
}

func (c *vaultClient) Decrypt(ctx context.Context, body, aad []byte) ([]byte, int, error) {
	const op = "decrypt"
	if err := c.need(op, isEncrypting(c.keyType)); err != nil {
		return nil, 0, err
	}
	if isRSAOAEP(c.keyType) {
		if err := c.noAAD(op, aad); err != nil {
			return nil, 0, err
		}
	}
	v, _, ok := parseVaultValue(string(body))
	if !ok {
		return nil, 0, errf(c.kind, op, ErrDecrypt, 0, "")
	}
	in := map[string]string{"ciphertext": string(body)}
	if len(aad) > 0 {
		in["associated_data"] = b64(aad)
	}
	var out struct {
		Plaintext string `json:"plaintext"`
	}
	if err := c.call(ctx, op, http.MethodPost, "decrypt", "", in, &out, true); err != nil {
		return nil, 0, err
	}
	pt, err := base64.StdEncoding.DecodeString(out.Plaintext)
	if err != nil {
		return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed plaintext in response")
	}
	return pt, v, nil
}

// Rewrap uses Transit's rewrap endpoint, which keeps the plaintext inside
// Vault. That endpoint takes no associated_data, so an AAD-bound ciphertext is
// rewrapped as decrypt-then-encrypt here, the plaintext cleared after use.
func (c *vaultClient) Rewrap(ctx context.Context, body, aad []byte) ([]byte, int, error) {
	const op = "rewrap"
	if err := c.need(op, isEncrypting(c.keyType)); err != nil {
		return nil, 0, err
	}
	if len(aad) > 0 {
		if isRSAOAEP(c.keyType) {
			return nil, 0, c.noAAD(op, aad)
		}
		pt, _, err := c.Decrypt(ctx, body, aad)
		if err != nil {
			return nil, 0, err
		}
		defer clear(pt)
		return c.Encrypt(ctx, pt, aad)
	}
	if _, _, ok := parseVaultValue(string(body)); !ok {
		return nil, 0, errf(c.kind, op, ErrDecrypt, 0, "")
	}
	var out struct {
		Ciphertext string `json:"ciphertext"`
	}
	if err := c.call(ctx, op, http.MethodPost, "rewrap", "", map[string]string{"ciphertext": string(body)}, &out, true); err != nil {
		return nil, 0, err
	}
	v, _, ok := parseVaultValue(out.Ciphertext)
	if !ok {
		return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed ciphertext in response")
	}
	return []byte(out.Ciphertext), v, nil
}

func (c *vaultClient) MAC(ctx context.Context, data []byte) ([]byte, int, error) {
	const op = "mac"
	if err := c.need(op, isMAC(c.keyType)); err != nil {
		return nil, 0, err
	}
	var out struct {
		HMAC string `json:"hmac"`
	}
	if err := c.call(ctx, op, http.MethodPost, "hmac", c.hmacAlg(), map[string]string{"input": b64(data)}, &out, false); err != nil {
		return nil, 0, err
	}
	v, rest, ok := parseVaultValue(out.HMAC)
	mac, err := base64.StdEncoding.DecodeString(rest)
	if !ok || err != nil {
		return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed hmac in response")
	}
	return mac, v, nil
}

// VerifyMAC asks Vault (it holds the key) whether mac is data's HMAC under a
// version, newest usable version first when none is named.
func (c *vaultClient) VerifyMAC(ctx context.Context, data, mac []byte, version int) (bool, int, error) {
	const op = "verify_mac"
	if err := c.need(op, isMAC(c.keyType)); err != nil {
		return false, 0, err
	}
	versions := []int{version}
	if version == 0 {
		k, err := c.readKey(ctx, op)
		if err != nil {
			return false, 0, err
		}
		versions = k.usableVersions()
	}
	for _, v := range versions {
		in := map[string]string{"input": b64(data), "hmac": "vault:v" + strconv.Itoa(v) + ":" + b64(mac)}
		var out struct {
			Valid bool `json:"valid"`
		}
		err := c.call(ctx, op, http.MethodPost, "verify", c.hmacAlg(), in, &out, true)
		switch {
		case errors.Is(err, ErrDecrypt):
			continue // Vault refused this version (e.g. below min_decryption_version)
		case err != nil:
			return false, 0, err
		case out.Valid:
			return true, v, nil
		}
	}
	return false, 0, nil
}

func (c *vaultClient) Sign(ctx context.Context, data []byte) ([]byte, int, error) {
	const op = "sign"
	if err := c.need(op, isSigning(c.keyType)); err != nil {
		return nil, 0, err
	}
	in := map[string]string{"input": b64(data)}
	suffix := "sha2-256"
	switch {
	case c.keyType == Ed25519:
		suffix = "" // Ed25519 signs the input itself; Vault ignores a hash choice
	case isRSAPSS(c.keyType):
		in["signature_algorithm"] = "pss"
		in["salt_length"] = "hash" // match AWS and GCP: salt = 32 bytes
	}
	// ECDSA keeps Vault's default asn1 marshaling (base64 DER) and is
	// converted to r||s here, rather than relying on jws marshaling.
	var out struct {
		Signature string `json:"signature"`
	}
	if err := c.call(ctx, op, http.MethodPost, "sign", suffix, in, &out, false); err != nil {
		return nil, 0, err
	}
	v, rest, ok := parseVaultValue(out.Signature)
	sig, err := base64.StdEncoding.DecodeString(rest)
	if !ok || err != nil {
		return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed signature in response")
	}
	if c.keyType == ECDSAP256 {
		if sig, err = derToRaw(sig); err != nil {
			return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed signature in response")
		}
	}
	return sig, v, nil
}

// Verify checks locally against the public keys Vault publishes for each
// version: Transit exposes them, and a local check needs one round trip
// however many versions are tried.
func (c *vaultClient) Verify(ctx context.Context, data, sig []byte, version int) (bool, int, error) {
	const op = "verify"
	if err := c.need(op, isSigning(c.keyType)); err != nil {
		return false, 0, err
	}
	k, err := c.readKey(ctx, op)
	if err != nil {
		return false, 0, err
	}
	versions := []int{version}
	if version == 0 {
		versions = k.usableVersions()
	}
	for _, v := range versions {
		pub, err := k.publicKey(v)
		if err != nil {
			continue
		}
		if verifyLocal(c.keyType, pub, data, sig) {
			return true, v, nil
		}
	}
	return false, 0, nil
}

func (c *vaultClient) PublicKey(ctx context.Context, version int) (crypto.PublicKey, int, error) {
	const op = "public_key"
	if err := c.need(op, hasPublicKey(c.keyType)); err != nil {
		return nil, 0, err
	}
	k, err := c.readKey(ctx, op)
	if err != nil {
		return nil, 0, err
	}
	if version == 0 {
		version = k.LatestVersion
	}
	pub, err := k.publicKey(version)
	if err != nil {
		return nil, 0, errf(c.kind, op, ErrNotFound, 0, "no public key for version "+strconv.Itoa(version))
	}
	if !checkPublic(c.keyType, pub) {
		return nil, 0, errf(c.kind, op, ErrMismatch, 0, "public key is not a "+c.keyType+" key")
	}
	return pub, version, nil
}

func (c *vaultClient) Describe(ctx context.Context) (KeyInfo, error) {
	k, err := c.readKey(ctx, "describe")
	if err != nil {
		return KeyInfo{}, err
	}
	info := KeyInfo{KeyType: c.mapVaultType(k.Type), Native: k.Type, Version: k.LatestVersion, Enabled: true}
	if k.Derived {
		// A derived key needs a per-call context loomcycle never sends.
		info.Ops = opsFor(info.KeyType)
		return info, errf(c.kind, "describe", ErrUnsupported, 0, "derived (context) keys are not supported")
	}
	return c.describeResult(info)
}

// mapVaultType maps Transit's key type onto ours. Vault doesn't separate an
// RSA key's padding (one key does PSS signing and OAEP encryption) or an HMAC
// key's hash (chosen per call), so for those the requested type decides.
func (c *vaultClient) mapVaultType(t string) string {
	switch t {
	case "aes256-gcm96":
		return AES256GCM
	case "ecdsa-p256":
		return ECDSAP256
	case "ed25519":
		return Ed25519
	case "hmac":
		if isMAC(c.keyType) {
			return c.keyType
		}
		return HMACSHA256
	case "rsa-2048", "rsa-3072", "rsa-4096":
		bits := strings.TrimPrefix(t, "rsa-")
		if isRSAOAEP(c.keyType) {
			return "rsa-oaep-" + bits
		}
		return "rsa-pss-" + bits
	}
	return ""
}

// vaultKey is the subset of GET <mount>/keys/<key> this package reads.
type vaultKey struct {
	Type                 string                     `json:"type"`
	LatestVersion        int                        `json:"latest_version"`
	MinDecryptionVersion int                        `json:"min_decryption_version"`
	Derived              bool                       `json:"derived"`
	Keys                 map[string]json.RawMessage `json:"keys"`
}

func (c *vaultClient) readKey(ctx context.Context, op string) (*vaultKey, error) {
	var k vaultKey
	if err := c.call(ctx, op, http.MethodGet, "keys", "", nil, &k, false); err != nil {
		return nil, err
	}
	if k.LatestVersion < 1 {
		return nil, errf(c.kind, op, ErrUnavailable, 0, "malformed key metadata")
	}
	return &k, nil
}

// usableVersions lists latest down to min_decryption_version, newest first,
// bounded by maxVersionTries.
func (k *vaultKey) usableVersions() []int {
	floor := max(k.MinDecryptionVersion, 1)
	var out []int
	for v := k.LatestVersion; v >= floor && len(out) < maxVersionTries; v-- {
		out = append(out, v)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

// publicKey reads version v's public key. Transit publishes ECDSA and RSA
// keys as PKIX PEM and Ed25519 keys as base64 of the raw 32 bytes.
func (k *vaultKey) publicKey(v int) (crypto.PublicKey, error) {
	raw, ok := k.Keys[strconv.Itoa(v)]
	if !ok {
		return nil, errors.New("no such version")
	}
	var entry struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil || entry.PublicKey == "" {
		return nil, errors.New("no public key")
	}
	if strings.HasPrefix(strings.TrimSpace(entry.PublicKey), "-----BEGIN") {
		return parsePublicPEM(entry.PublicKey)
	}
	b, err := base64.StdEncoding.DecodeString(entry.PublicKey)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("unrecognised public key encoding")
	}
	return ed25519.PublicKey(b), nil
}
