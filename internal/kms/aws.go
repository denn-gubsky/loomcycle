package kms

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// awsClient talks to AWS KMS's JSON 1.1 API, signing each request with SigV4.
// AWS keys have no versions loomcycle can name (rotation keeps the old
// material behind the same key id), so every returned version is 0.
type awsClient struct {
	base
	endpoint, region, keyID         string
	akidCred, secretCred, tokenCred string

	mu  sync.Mutex
	pub crypto.PublicKey // cached GetPublicKey result
}

func newAWS(b base, r Ref, endpoint string) *awsClient {
	if endpoint == "" {
		endpoint = "https://kms." + r.Region + ".amazonaws.com"
	}
	return &awsClient{base: b, endpoint: strings.TrimRight(endpoint, "/"), region: r.Region, keyID: r.KeyID,
		akidCred: r.AccessKeyIDCred, secretCred: r.SecretAccessKeyCred, tokenCred: r.SessionTokenCred}
}

// awsContextKey carries the associated data in an encryption context, which
// AWS binds into the ciphertext exactly as an AEAD binds its associated data.
const awsContextKey = "loomcycle-aad"

// call POSTs one KMS action and decodes the response into out. decryptish
// marks decrypt and verify calls, where an invalid-input answer means "does
// not open" rather than a malformed request.
func (c *awsClient) call(ctx context.Context, op, action string, in, out any, decryptish bool) error {
	akid, err := c.secret(ctx, c.akidCred)
	if err != nil {
		return err
	}
	secret, err := c.secret(ctx, c.secretCred)
	if err != nil {
		return err
	}
	var token string
	if c.tokenCred != "" {
		if token, err = c.secret(ctx, c.tokenCred); err != nil {
			return err
		}
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	defer clear(body)
	req, err := newJSONRequest(http.MethodPost, c.endpoint+"/", body, "application/x-amz-json-1.1")
	if err != nil {
		return err
	}
	req.Header.Set("X-Amz-Target", "TrentService."+action)
	signV4(req, body, akid, secret, token, c.region, "kms", c.now().UTC())
	status, resp, err := c.send(ctx, op, req)
	if err != nil {
		return err
	}
	defer clear(resp)
	if status < 200 || status > 299 {
		return errf(c.kind, op, awsErrorSentinel(status, resp, decryptish), status, awsErrorType(resp))
	}
	if out != nil && json.Unmarshal(resp, out) != nil {
		return errf(c.kind, op, ErrUnavailable, status, "malformed response")
	}
	return nil
}

// awsErrorType is the error's type name (e.g. "InvalidCiphertextException"),
// which is fixed text and safe to report; the message is not used.
func awsErrorType(resp []byte) string {
	var e struct {
		Type string `json:"__type"`
	}
	_ = json.Unmarshal(resp, &e)
	if i := strings.LastIndexByte(e.Type, '#'); i >= 0 {
		e.Type = e.Type[i+1:]
	}
	if len(e.Type) > 64 || strings.ContainsAny(e.Type, " \n") {
		return ""
	}
	return e.Type
}

func awsErrorSentinel(status int, resp []byte, decryptish bool) error {
	switch awsErrorType(resp) {
	case "AccessDeniedException", "UnrecognizedClientException", "InvalidSignatureException",
		"ExpiredTokenException", "IncompleteSignature", "MissingAuthenticationToken", "InvalidClientTokenId":
		return ErrDenied
	case "NotFoundException":
		return ErrNotFound
	case "InvalidCiphertextException", "IncorrectKeyException", "KMSInvalidSignatureException", "KMSInvalidMacException":
		return ErrDecrypt
	case "ThrottlingException", "KMSInternalException", "DependencyTimeoutException", "LimitExceededException":
		return ErrUnavailable
	case "DisabledException", "KMSInvalidStateException", "InvalidKeyUsageException", "UnsupportedOperationException":
		return ErrRejected
	}
	return classifyStatus(status, decryptish)
}

func (c *awsClient) encAlg() string {
	if isRSAOAEP(c.keyType) {
		return "RSAES_OAEP_SHA_256"
	}
	return "SYMMETRIC_DEFAULT"
}

func (c *awsClient) encContext(aad []byte) map[string]string {
	if len(aad) == 0 {
		return nil
	}
	return map[string]string{awsContextKey: b64(aad)}
}

func (c *awsClient) Encrypt(ctx context.Context, plaintext, aad []byte) ([]byte, int, error) {
	const op = "encrypt"
	if err := c.need(op, isEncrypting(c.keyType)); err != nil {
		return nil, 0, err
	}
	if isRSAOAEP(c.keyType) {
		if err := c.noAAD(op, aad); err != nil {
			return nil, 0, err
		}
	}
	in := map[string]any{"KeyId": c.keyID, "Plaintext": b64(plaintext), "EncryptionAlgorithm": c.encAlg()}
	if ec := c.encContext(aad); ec != nil {
		in["EncryptionContext"] = ec
	}
	var out struct {
		CiphertextBlob string
	}
	if err := c.call(ctx, op, "Encrypt", in, &out, false); err != nil {
		return nil, 0, err
	}
	body, err := base64.StdEncoding.DecodeString(out.CiphertextBlob)
	if err != nil || len(body) == 0 {
		return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed ciphertext in response")
	}
	return body, 0, nil
}

func (c *awsClient) Decrypt(ctx context.Context, body, aad []byte) ([]byte, int, error) {
	const op = "decrypt"
	if err := c.need(op, isEncrypting(c.keyType)); err != nil {
		return nil, 0, err
	}
	if isRSAOAEP(c.keyType) {
		if err := c.noAAD(op, aad); err != nil {
			return nil, 0, err
		}
	}
	// KeyId is always sent, so a ciphertext of another key in the account is
	// refused rather than opened with that key.
	in := map[string]any{"KeyId": c.keyID, "CiphertextBlob": b64(body), "EncryptionAlgorithm": c.encAlg()}
	if ec := c.encContext(aad); ec != nil {
		in["EncryptionContext"] = ec
	}
	var out struct {
		Plaintext string
	}
	if err := c.call(ctx, op, "Decrypt", in, &out, true); err != nil {
		return nil, 0, err
	}
	pt, err := base64.StdEncoding.DecodeString(out.Plaintext)
	if err != nil {
		return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed plaintext in response")
	}
	return pt, 0, nil
}

// Rewrap uses ReEncrypt, which keeps the plaintext inside KMS, under the same
// key and context: after a rotation the result is under the newest material.
func (c *awsClient) Rewrap(ctx context.Context, body, aad []byte) ([]byte, int, error) {
	const op = "rewrap"
	if err := c.need(op, isEncrypting(c.keyType)); err != nil {
		return nil, 0, err
	}
	if isRSAOAEP(c.keyType) {
		if err := c.noAAD(op, aad); err != nil {
			return nil, 0, err
		}
	}
	in := map[string]any{
		"CiphertextBlob": b64(body), "SourceKeyId": c.keyID, "DestinationKeyId": c.keyID,
		"SourceEncryptionAlgorithm": c.encAlg(), "DestinationEncryptionAlgorithm": c.encAlg(),
	}
	if ec := c.encContext(aad); ec != nil {
		in["SourceEncryptionContext"], in["DestinationEncryptionContext"] = ec, ec
	}
	var out struct {
		CiphertextBlob string
	}
	if err := c.call(ctx, op, "ReEncrypt", in, &out, true); err != nil {
		return nil, 0, err
	}
	nb, err := base64.StdEncoding.DecodeString(out.CiphertextBlob)
	if err != nil || len(nb) == 0 {
		return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed ciphertext in response")
	}
	return nb, 0, nil
}

func (c *awsClient) macAlg() string {
	if c.keyType == HMACSHA512 {
		return "HMAC_SHA_512"
	}
	return "HMAC_SHA_256"
}

func (c *awsClient) MAC(ctx context.Context, data []byte) ([]byte, int, error) {
	const op = "mac"
	if err := c.need(op, isMAC(c.keyType)); err != nil {
		return nil, 0, err
	}
	var out struct {
		Mac string
	}
	if err := c.call(ctx, op, "GenerateMac", map[string]any{"KeyId": c.keyID, "Message": b64(data), "MacAlgorithm": c.macAlg()}, &out, false); err != nil {
		return nil, 0, err
	}
	mac, err := base64.StdEncoding.DecodeString(out.Mac)
	if err != nil || len(mac) == 0 {
		return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed mac in response")
	}
	return mac, 0, nil
}

func (c *awsClient) VerifyMAC(ctx context.Context, data, mac []byte, _ int) (bool, int, error) {
	const op = "verify_mac"
	if err := c.need(op, isMAC(c.keyType)); err != nil {
		return false, 0, err
	}
	var out struct {
		MacValid bool
	}
	err := c.call(ctx, op, "VerifyMac", map[string]any{"KeyId": c.keyID, "Message": b64(data), "Mac": b64(mac), "MacAlgorithm": c.macAlg()}, &out, true)
	switch {
	case err == nil:
		return out.MacValid, 0, nil
	case errors.Is(err, ErrDecrypt): // KMSInvalidMacException: not a match
		return false, 0, nil
	}
	return false, 0, err
}

func (c *awsClient) sigAlg() string {
	if c.keyType == ECDSAP256 {
		return "ECDSA_SHA_256"
	}
	return "RSASSA_PSS_SHA_256"
}

// Sign sends the SHA-256 digest (MessageType DIGEST): it signs data of any
// size in one call, and is what ECDSA_SHA_256 and RSASSA_PSS_SHA_256 sign
// anyway. An ECDSA signature comes back as DER and is returned as r||s.
func (c *awsClient) Sign(ctx context.Context, data []byte) ([]byte, int, error) {
	const op = "sign"
	if err := c.need(op, isSigning(c.keyType)); err != nil {
		return nil, 0, err
	}
	d := sha256.Sum256(data)
	var out struct {
		Signature string
	}
	if err := c.call(ctx, op, "Sign", map[string]any{"KeyId": c.keyID, "Message": b64(d[:]), "MessageType": "DIGEST", "SigningAlgorithm": c.sigAlg()}, &out, false); err != nil {
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
	return sig, 0, nil
}

// Verify checks locally against the key's public half (one cached
// GetPublicKey), so a verify costs no KMS call after the first.
func (c *awsClient) Verify(ctx context.Context, data, sig []byte, _ int) (bool, int, error) {
	const op = "verify"
	if err := c.need(op, isSigning(c.keyType)); err != nil {
		return false, 0, err
	}
	pub, _, err := c.PublicKey(ctx, 0)
	if err != nil {
		return false, 0, err
	}
	return verifyLocal(c.keyType, pub, data, sig), 0, nil
}

func (c *awsClient) PublicKey(ctx context.Context, _ int) (crypto.PublicKey, int, error) {
	const op = "public_key"
	if err := c.need(op, hasPublicKey(c.keyType)); err != nil {
		return nil, 0, err
	}
	c.mu.Lock()
	cached := c.pub
	c.mu.Unlock()
	if cached != nil {
		return cached, 0, nil
	}
	var out struct {
		PublicKey string
	}
	if err := c.call(ctx, op, "GetPublicKey", map[string]any{"KeyId": c.keyID}, &out, false); err != nil {
		return nil, 0, err
	}
	der, err := base64.StdEncoding.DecodeString(out.PublicKey)
	if err != nil {
		return nil, 0, errf(c.kind, op, ErrUnavailable, 0, "malformed public key in response")
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil || !checkPublic(c.keyType, pub) {
		return nil, 0, errf(c.kind, op, ErrMismatch, 0, "public key is not a "+c.keyType+" key")
	}
	c.mu.Lock()
	c.pub = pub
	c.mu.Unlock()
	return pub, 0, nil
}

func (c *awsClient) Describe(ctx context.Context) (KeyInfo, error) {
	var out struct {
		KeyMetadata struct {
			KeySpec  string
			KeyUsage string
			KeyState string
			Enabled  bool
		}
	}
	if err := c.call(ctx, "describe", "DescribeKey", map[string]any{"KeyId": c.keyID}, &out, false); err != nil {
		return KeyInfo{}, err
	}
	m := out.KeyMetadata
	info := KeyInfo{KeyType: awsKeyType(m.KeySpec, m.KeyUsage), Native: m.KeySpec + "/" + m.KeyUsage, Enabled: m.Enabled && m.KeyState == "Enabled"}
	return c.describeResult(info)
}

// awsKeyType maps a KeySpec and KeyUsage onto loomcycle's type.
func awsKeyType(spec, usage string) string {
	switch {
	case spec == "SYMMETRIC_DEFAULT" && usage == "ENCRYPT_DECRYPT":
		return AES256GCM
	case spec == "HMAC_256" && usage == "GENERATE_VERIFY_MAC":
		return HMACSHA256
	case spec == "HMAC_512" && usage == "GENERATE_VERIFY_MAC":
		return HMACSHA512
	case spec == "ECC_NIST_P256" && usage == "SIGN_VERIFY":
		return ECDSAP256
	}
	bits := map[string]string{"RSA_2048": "2048", "RSA_3072": "3072", "RSA_4096": "4096"}[spec]
	switch {
	case bits != "" && usage == "SIGN_VERIFY":
		return "rsa-pss-" + bits
	case bits != "" && usage == "ENCRYPT_DECRYPT":
		return "rsa-oaep-" + bits
	}
	return ""
}

// signV4 signs req with AWS Signature Version 4 (header form), for one
// region and service. payload is the exact request body. Host, X-Amz-Date and
// (when token is set) X-Amz-Security-Token are added and signed with every
// other header already on req.
func signV4(req *http.Request, payload []byte, akid, secret, token, region, service string, now time.Time) {
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	if token != "" {
		req.Header.Set("X-Amz-Security-Token", token)
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	headers := map[string]string{"host": host}
	for k, vs := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(trimAll(vs), ",")
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	sum := sha256.Sum256(payload)
	canonical := strings.Join([]string{req.Method, path, req.URL.RawQuery, canonHeaders.String(), signed, hex.EncodeToString(sum[:])}, "\n")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	creq := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(creq[:])

	k := hmacSHA256([]byte("AWS4"+secret), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+akid+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// trimAll trims each header value and collapses inner runs of spaces, as the
// canonical form requires.
func trimAll(vs []string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = strings.Join(strings.Fields(v), " ")
	}
	return out
}
