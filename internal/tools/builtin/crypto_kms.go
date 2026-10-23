package builtin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/cryptoops"
	"github.com/denn-gubsky/loomcycle/internal/kms"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Crypto keys held in an external KMS (Vault Transit, AWS KMS, Google Cloud
// KMS). The credential row stores only a reference to the key; every op is a
// call to the KMS, so the material is never in this process at all.
//
// The reference names the credentials that reach the KMS as $cred: names.
// They resolve in the key's own bucket (its tenant, scope and scope id) and
// nowhere else, so a user's key never runs on the tenant's KMS credentials,
// nor the reverse. Such keys are made only outside a run: an agent choosing a
// KMS reference would choose whose credentials its calls use.

// KMSAccess is how the Crypto and CredentialDef tools reach an external KMS.
// One value is shared by both. Nil on a tool: external keys are refused.
type KMSAccess struct {
	// Cfg supplies the Vault host floor: a Vault address is dialled only when
	// its host is on LOOMCYCLE_HTTP_HOST_ALLOWLIST or
	// LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST, checked at create and at each use.
	Cfg *config.Config
	// HTTP sends Vault requests: an SSRF-guarded client. AWS and Google
	// endpoints are fixed, and use the kms package's own client.
	HTTP *http.Client
	// AllowHTTP admits a plain http:// Vault address
	// (LOOMCYCLE_KMS_VAULT_ALLOW_HTTP, for a dev server).
	AllowHTTP bool
	// Endpoint overrides, for tests only.
	AWSEndpoint, GCPEndpoint, GCPTokenURL string

	mu      sync.Mutex
	clients map[[32]byte]kms.Client
}

// maxKMSClients bounds the client cache. A client holds cached auth tokens
// and public keys; past the bound the cache starts over.
const maxKMSClients = 256

// kmsKinds are the backends a crypto key may name.
func kmsKind(backend string) bool {
	switch kms.Kind(backend) {
	case kms.KindVault, kms.KindAWS, kms.KindGCP:
		return true
	}
	return false
}

// errKMSHostNotListed: a Vault address whose host the operator has not
// allowlisted.
var errKMSHostNotListed = errors.New("the Vault host is not on the operator's host allowlist (LOOMCYCLE_HTTP_HOST_ALLOWLIST or LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST)")

// checkRef validates a reference and its Vault host.
func (a *KMSAccess) checkRef(ref kms.Ref) error {
	if err := ref.Validate(a.AllowHTTP); err != nil {
		return err
	}
	if ref.Kind == kms.KindVault && !remotePeerHostListed(a.Cfg, ref.Address) {
		return errKMSHostNotListed
	}
	return nil
}

// newClient builds a client for the key stored under id (id.Name is the key's
// name), whose $cred: references resolve in id's bucket only.
func (a *KMSAccess) newClient(eng *credential.Engine, id credential.Identity, keyType string, ref kms.Ref) (kms.Client, error) {
	if err := a.checkRef(ref); err != nil {
		return nil, err
	}
	secrets := func(ctx context.Context, credRef string) (string, error) {
		name, ok := credential.ParseRef(credRef)
		if !ok {
			return "", fmt.Errorf("%q is not a $cred: reference", credRef)
		}
		v, found, err := eng.ResolveKMSSecret(ctx, credential.Identity{TenantID: id.TenantID, Scope: id.Scope, ScopeID: id.ScopeID, Name: name})
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("no credential %q in the key's %s scope", name, id.Scope)
		}
		return v, nil
	}
	return kms.New(ref, keyType, kms.Options{
		HTTPClient: a.HTTP, Secrets: secrets, AllowHTTP: a.AllowHTTP,
		AWSEndpoint: a.AWSEndpoint, GCPEndpoint: a.GCPEndpoint, GCPTokenURL: a.GCPTokenURL,
	})
}

// client returns the (cached) client for a resolved external key. The cache
// key covers the key's identity, type and whole reference, so an edited or
// recreated key gets a new client.
func (a *KMSAccess) client(eng *credential.Engine, tenant string, k credential.CryptoKey) (kms.Client, error) {
	ref, err := parseStoredRef(k.External)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	for _, part := range []string{tenant, k.Scope, k.ScopeID, k.Name, k.KeyType, k.External.Kind} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	h.Write(k.External.Ref)
	var key [32]byte
	copy(key[:], h.Sum(nil))
	// The Vault host floor is checked at every use, not only when the client
	// was built: the operator may have taken the host off the list since.
	if ref.Kind == kms.KindVault && !remotePeerHostListed(a.Cfg, ref.Address) {
		return nil, errKMSHostNotListed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.clients[key]; ok {
		return c, nil
	}
	c, err := a.newClient(eng, credential.Identity{TenantID: tenant, Scope: k.Scope, ScopeID: k.ScopeID, Name: k.Name}, k.KeyType, ref)
	if err != nil {
		return nil, err
	}
	if a.clients == nil || len(a.clients) >= maxKMSClients {
		a.clients = map[[32]byte]kms.Client{}
	}
	a.clients[key] = c
	return c, nil
}

// parseStoredRef reads a stored reference. It was checked when stored, and is
// sealed with its kind, so a failure here is a damaged row.
func parseStoredRef(ext *credential.ExternalKey) (kms.Ref, error) {
	var ref kms.Ref
	dec := json.NewDecoder(bytes.NewReader(ext.Ref))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ref); err != nil || string(ref.Kind) != ext.Kind {
		return kms.Ref{}, errors.New("the stored KMS reference cannot be read")
	}
	return ref, nil
}

// kmsFailure turns a KMS error into the model's answer. A KMS error's text
// names the KMS, the op and at most an HTTP status (never a response body or
// a token); a credential resolver's names the credential, never its value.
func kmsFailure(op string, err error) tools.Result {
	msg := op + ": " + err.Error()
	switch {
	case errors.Is(err, kms.ErrDecrypt):
		return didNotDecrypt()
	case errors.Is(err, errKMSHostNotListed):
		return errPermission(msg, "Ask the operator to allowlist the Vault host.")
	case errors.Is(err, kms.ErrDenied):
		return errPermission(msg, "Ask the key's owner to check the KMS credentials and the key's policy.")
	case errors.Is(err, kms.ErrUnavailable):
		return errTransient(msg, "Try again later.")
	case errors.Is(err, kms.ErrNotFound):
		return errNotFound(msg, "Check that the key (and version) still exists in the KMS.")
	case errors.Is(err, kms.ErrUnsupported), errors.Is(err, kms.ErrMismatch):
		return errValidation(msg, "")
	case errors.Is(err, kms.ErrRejected):
		return errBusiness(msg, "Check the key is enabled in the KMS and the input is within its limits.")
	case credential.HeldButUnusable(err):
		return errBusiness(msg, "Ask the key's owner to fix the credential its KMS reference names.")
	}
	return errBusiness(msg, "Ask the key's owner to check the key's KMS reference.")
}

// extKid names an external key's version in a JWE/JWS header; the bare name
// when the KMS has no versions (AWS).
func extKid(k *cryptoKey, v int) string {
	if v > 0 {
		return fmt.Sprintf("%s.v%d", k.name, v)
	}
	return k.name
}

// extVersion is the version a decrypt or verify names: the call's version,
// else the one a "<name>.v<n>" kid names, else 0 (the KMS's choice).
func extVersion(k *cryptoKey, ref keyRef, kid string) int {
	if ref.Version != 0 {
		return ref.Version
	}
	if n, ok := kidVersion(k.name, kid); ok {
		return n
	}
	return 0
}

// execExternal runs a keyed op on a key an external KMS holds. The op is
// already allowed for the key type. The associated data is the held key's,
// with version 0: the KMS's ciphertext carries its own version.
func (c *Crypto) execExternal(ctx context.Context, k *cryptoKey, tenant string, in cryptoInput, env *cryptoops.Envelope, ref keyRef, ev *audit.Event) tools.Result {
	cl := k.ext
	aad := func() []byte { return cryptoops.AAD(tenant, k.source, k.name, 0, in.Context) }
	isJWE := cryptoops.IsJWE(strings.TrimSpace(in.Ciphertext))
	oaep := strings.HasPrefix(string(k.keyType), "rsa-oaep-")

	switch in.Op {
	case "encrypt":
		pt, res, ok := decodeInput(in.Plaintext, in.InputEncoding, "plaintext")
		if !ok {
			return res
		}
		if oaep {
			// As with a held rsa-oaep key: a JWE to the key's public half.
			if in.Format != "" && in.Format != "jwe" {
				return errValidation(fmt.Sprintf("encrypt: a %s key encrypts only as jwe", k.keyType), "Drop format, or use format jwe.")
			}
			if in.Context != "" {
				return errValidation("encrypt: a JWE carries no context", "Drop context.")
			}
			pub, v, err := cl.PublicKey(ctx, 0)
			if err != nil {
				return kmsFailure("encrypt", err)
			}
			tok, err := cryptoops.EncryptJWETo(k.keyType, pub, pt, extKid(k, v))
			if err != nil {
				return errTransient("encrypt failed", "Try again.")
			}
			ev.CryptoVersion = v
			return cryptoJSON(map[string]any{"ciphertext": tok, "format": "jwe", "key_version": v})
		}
		body, v, err := cl.Encrypt(ctx, pt, aad())
		if err != nil {
			return kmsFailure("encrypt", err)
		}
		ev.CryptoVersion = v
		switch in.Format {
		case "", "envelope":
			e := cryptoops.Envelope{Source: k.source, Name: k.name, Version: v, Body: body}
			return cryptoJSON(map[string]any{"ciphertext": e.String(), "key_version": v})
		case "raw":
			return cryptoJSON(map[string]any{"ciphertext": base64.StdEncoding.EncodeToString(body), "key_version": v})
		case "jwe":
			return errValidation("encrypt: a key held in a KMS has no jwe form", "Use the default envelope.")
		}
		return errValidation(fmt.Sprintf("encrypt: format %q", in.Format), "Use envelope or raw.")

	case "decrypt", "rewrap":
		if in.Ciphertext == "" {
			return errValidation(in.Op+": ciphertext is required", "")
		}
		if isJWE && in.Context != "" {
			return errValidation(in.Op+": a JWE carries no context", "Drop context.")
		}
		if oaep {
			if !isJWE || env != nil {
				return didNotDecrypt()
			}
			tok := strings.TrimSpace(in.Ciphertext)
			v := extVersion(k, ref, cryptoops.JWEKid(tok))
			used := v
			pt, _, ok, err := cryptoops.DecryptJWEUnwrap(k.keyType, tok, func(ek []byte) ([]byte, error) {
				cek, uv, err := cl.DecryptOAEP(ctx, ek, v)
				if errors.Is(err, kms.ErrDecrypt) {
					return nil, nil // opaque, as a wrong key is
				}
				used = uv
				return cek, err
			})
			if err != nil {
				return kmsFailure(in.Op, err)
			}
			if !ok {
				return didNotDecrypt()
			}
			ev.CryptoVersion = used
			// Only decrypt gets here: a recipient key has no rewrap.
			return decryptOutput(ctx, in, pt, used)
		}
		if isJWE {
			return didNotDecrypt()
		}
		var body []byte
		if env != nil {
			body = env.Body
		} else {
			b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.Ciphertext))
			if err != nil {
				return didNotDecrypt()
			}
			body = b
		}
		if in.Op == "decrypt" {
			pt, v, err := cl.Decrypt(ctx, body, aad())
			if err != nil {
				return kmsFailure("decrypt", err)
			}
			if v == 0 && env != nil {
				v = env.Version
			}
			ev.CryptoVersion = v
			return decryptOutput(ctx, in, pt, v)
		}
		nb, v, err := cl.Rewrap(ctx, body, aad())
		if err != nil {
			return kmsFailure("rewrap", err)
		}
		ev.CryptoVersion = v
		out := map[string]any{"key_version": v}
		if env != nil {
			out["from_version"] = env.Version
			out["ciphertext"] = cryptoops.Envelope{Source: k.source, Name: k.name, Version: v, Body: nb}.String()
		} else {
			out["ciphertext"] = base64.StdEncoding.EncodeToString(nb)
		}
		return cryptoJSON(out)

	case "mac":
		data, res, ok := decodeInput(in.Data, in.InputEncoding, "data")
		if !ok {
			return res
		}
		sum, v, err := cl.MAC(ctx, data)
		if err != nil {
			return kmsFailure("mac", err)
		}
		out, res, ok := encodeBytes(sum, in.OutputEncoding)
		if !ok {
			return res
		}
		ev.CryptoVersion = v
		return cryptoJSON(map[string]any{"mac": out, "key_version": v})

	case "verify_mac":
		data, res, ok := decodeInput(in.Data, in.InputEncoding, "data")
		if !ok {
			return res
		}
		want, ok := decodeHexOrBase64(in.MAC)
		if !ok {
			return errValidation("verify_mac: mac must be hex or base64", "")
		}
		valid, v, err := cl.VerifyMAC(ctx, data, want, ref.Version)
		if err != nil {
			return kmsFailure("verify_mac", err)
		}
		if !valid {
			return cryptoJSON(map[string]any{"valid": false})
		}
		return cryptoJSON(map[string]any{"valid": true, "key_version": v})

	case "sign":
		data, res, ok := decodeInput(in.Data, in.InputEncoding, "data")
		if !ok {
			return res
		}
		format := in.Format
		if format == "" {
			format = "raw"
			if k.keyType.IsMAC() {
				format = "jws"
			}
		}
		switch format {
		case "jws":
			// The header is signed, and the version is known only once the KMS
			// has signed, so the kid is the bare name: verify tries each
			// version the KMS still uses.
			var v int
			tok, err := cryptoops.SignJWSWith(k.keyType, data, k.name, func(si []byte) ([]byte, error) {
				var (
					sig []byte
					err error
				)
				if k.keyType.IsMAC() {
					sig, v, err = cl.MAC(ctx, si)
				} else {
					sig, v, err = cl.Sign(ctx, si)
				}
				return sig, err
			})
			if err != nil {
				return kmsFailure("sign", err)
			}
			ev.CryptoVersion = v
			return cryptoJSON(map[string]any{"signature": tok, "format": "jws", "key_version": v})
		case "raw":
			if k.keyType.IsMAC() {
				return errValidation("a MAC key signs only as jws", "Use format jws, or op mac for a bare MAC.")
			}
			sig, v, err := cl.Sign(ctx, data)
			if err != nil {
				return kmsFailure("sign", err)
			}
			ev.CryptoVersion = v
			return cryptoJSON(map[string]any{"signature": base64.StdEncoding.EncodeToString(sig), "format": "raw", "key_version": v})
		}
		return errValidation(fmt.Sprintf("sign: format %q", format), "Use raw or jws.")

	case "verify":
		if in.Signature == "" {
			return errValidation("verify: signature is required", "")
		}
		if strings.Count(in.Signature, ".") == 2 && in.Data == "" {
			si, payload, sig, kid, ok := cryptoops.ParseJWS(k.keyType, in.Signature)
			if !ok {
				return cryptoJSON(map[string]any{"valid": false})
			}
			var (
				valid bool
				v     int
				err   error
			)
			if k.keyType.IsMAC() {
				valid, v, err = cl.VerifyMAC(ctx, si, sig, extVersion(k, ref, kid))
			} else {
				valid, v, err = cl.Verify(ctx, si, sig, extVersion(k, ref, kid))
			}
			if err != nil {
				return kmsFailure("verify", err)
			}
			if !valid {
				return cryptoJSON(map[string]any{"valid": false})
			}
			return cryptoJSON(map[string]any{"valid": true, "key_version": v, "kid": kid, "payload": string(payload)})
		}
		if k.keyType.IsMAC() {
			return errValidation("a MAC key verifies a jws token, or use verify_mac", "")
		}
		data, res, ok := decodeInput(in.Data, in.InputEncoding, "data")
		if !ok {
			return res
		}
		sig, ok := decodeHexOrBase64(in.Signature)
		if !ok {
			return cryptoJSON(map[string]any{"valid": false})
		}
		valid, v, err := cl.Verify(ctx, data, sig, ref.Version)
		if err != nil {
			return kmsFailure("verify", err)
		}
		if !valid {
			return cryptoJSON(map[string]any{"valid": false})
		}
		return cryptoJSON(map[string]any{"valid": true, "key_version": v})

	case "public_key":
		pub, v, err := cl.PublicKey(ctx, ref.Version)
		if err != nil {
			return kmsFailure("public_key", err)
		}
		ev.CryptoVersion = v
		return publicKeyResult(k, pub, v, extKid(k, v))
	}
	return errValidation(fmt.Sprintf("%s is not available for a key held in a KMS", in.Op), "")
}

// ---- CredentialDef: create a key held in an external KMS ------------------

// credentialExternalInput is create's `external` object. Credential fields
// are $cred:<name> references, resolved in the new key's own scope.
type credentialExternalInput struct {
	// vault
	Address   string `json:"address"`
	Mount     string `json:"mount"`
	Namespace string `json:"namespace"`
	Token     string `json:"token"`
	// vault: the Transit key name; gcp_kms: the CryptoKey resource name.
	Key string `json:"key"`
	// aws_kms
	Region          string `json:"region"`
	KeyID           string `json:"key_id"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	// gcp_kms
	ServiceAccount string `json:"service_account"`
}

// externalRef reads create's external object into a kms.Ref for backend. A
// field of another backend is carried over, so Validate refuses it rather
// than it being dropped.
func externalRef(backend string, raw json.RawMessage) (kms.Ref, error) {
	if len(raw) == 0 {
		return kms.Ref{}, errors.New("external is required for a key held in a KMS")
	}
	var in credentialExternalInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return kms.Ref{}, fmt.Errorf("external: %v", err)
	}
	ref := kms.Ref{
		Kind: kms.Kind(backend), Address: in.Address, Mount: in.Mount, Namespace: in.Namespace, TokenCred: in.Token,
		Region: in.Region, KeyID: in.KeyID, AccessKeyIDCred: in.AccessKeyID, SecretAccessKeyCred: in.SecretAccessKey,
		SessionTokenCred: in.SessionToken, ServiceAccountCred: in.ServiceAccount,
	}
	if ref.Kind == kms.KindGCP {
		ref.Resource = in.Key
	} else {
		ref.Key = in.Key
	}
	return ref, nil
}

// refuseUnusableCredRefs asks the caller's access-control set `use` of each
// $cred:<name> the external reference names: creating the key sends those
// credentials to the KMS (to describe the key) and stores them as the key's
// way in, so a caller denied a credential must not reach it through a key
// (RFC EZ §1.5 seam 5). A field that is not a $cred: reference is left to
// the reference's own validation; a malformed external object too.
func refuseUnusableCredRefs(ctx context.Context, id credential.Identity, backend string, raw json.RawMessage) *tools.Result {
	acl := tools.CallerACL(ctx)
	if acl == nil {
		return nil
	}
	ref, err := externalRef(backend, raw)
	if err != nil {
		return nil
	}
	for _, field := range []string{ref.TokenCred, ref.AccessKeyIDCred, ref.SecretAccessKeyCred, ref.SessionTokenCred, ref.ServiceAccountCred} {
		name, ok := credential.ParseRef(field)
		if !ok {
			continue
		}
		res := credentialACLResource(id.Scope, id.ScopeID, name)
		if !tools.ACLAllowsInstance(ctx, acl, "use", res) {
			r := errPermission(fmt.Sprintf("create: the caller's access-control rules do not allow use on %s:%s, which external names", res.Type, res.Name),
				"Name a credential the caller may use.")
			return &r
		}
	}
	return nil
}

// execCreateExternal stores a reference to a key an external KMS holds, after
// asking the KMS that the key exists, is enabled and is of key_type.
func (c *CredentialDef) execCreateExternal(ctx context.Context, id credential.Identity, in credentialDefInput) (tools.Result, error) {
	if callInRun(ctx) {
		return errPermission("create: a key held in an external KMS is added by an operator or its user, outside a run",
			"Use op generate for a key in agent scope."), nil
	}
	if refused := refuseUnusableCredRefs(ctx, id, in.Backend, in.External); refused != nil {
		return *refused, nil
	}
	if c.KMS == nil {
		return errBusiness("create: keys held in an external KMS are not available on this deployment", ""), nil
	}
	if in.Value != "" || in.KeyEncoding != "" {
		return errValidation("create: a key held in a KMS takes no value or key_encoding", "Its material stays in the KMS; name it with external."), nil
	}
	if in.AllowedURLs != nil || in.Username != nil {
		return errValidation("create: allowed_urls and username do not apply to a crypto key", "A crypto key is never sent anywhere; drop them."), nil
	}
	if !credNameRe.MatchString(in.Name) {
		return errResult("create: name must match ^[A-Za-z0-9_-]{1,128}$"), nil
	}
	kt, err := cryptoops.ParseKeyType(in.KeyType)
	if err != nil {
		return errValidation("create: "+err.Error(), "A crypto key needs key_type."), nil
	}
	ref, err := externalRef(in.Backend, in.External)
	if err != nil {
		return errValidation("create: "+err.Error(), "See Context op=help topic=credentials for each backend's fields."), nil
	}
	expires, res, ok := parseExpiry("create", in.ExpiresAt)
	if !ok {
		return res, nil
	}
	if !c.Engine.InlineEnabled() {
		return errResult("create: crypto keys need the inline credential store — the operator must set LOOMCYCLE_SECRET_KEY"), nil
	}
	if err := c.KMS.checkRef(ref); err != nil {
		if errors.Is(err, errKMSHostNotListed) {
			return errPermission("create: "+err.Error(), "Ask the operator to allowlist the Vault host."), nil
		}
		return errValidation("create: "+err.Error(), ""), nil
	}
	cl, err := c.KMS.newClient(c.Engine, id, string(kt), ref)
	if err != nil {
		return errValidation("create: "+err.Error(), ""), nil
	}
	info, err := cl.Describe(ctx)
	if err != nil {
		if errors.Is(err, kms.ErrMismatch) {
			return errValidation("create: "+err.Error(), "Set key_type to the KMS key's type."), nil
		}
		return kmsFailure("create", err), nil
	}
	if !info.Enabled {
		return errBusiness(fmt.Sprintf("create: the KMS reports the key (%s) as disabled", info.Native), "Enable it in the KMS first."), nil
	}
	if res, ok := c.refuseShadow(ctx, "crypto_key_create", id); !ok {
		return res, nil
	}
	stored, err := json.Marshal(ref)
	if err != nil {
		return errTransient("create: could not store the reference", "Try again."), nil
	}
	m, err := c.Engine.PutExternalCryptoKey(ctx, id, string(kt), credential.ExternalKey{Kind: string(ref.Kind), Ref: stored}, expires)
	c.recordKey(ctx, "crypto_key_create", id, string(kt), info.Version, err)
	if err != nil {
		return keyOpFailure("crypto_key_create", id.Name, id.Scope, err), nil
	}
	return jsonResult(credentialMeta(m, "stored"))
}
