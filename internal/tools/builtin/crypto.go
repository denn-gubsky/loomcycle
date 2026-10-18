package builtin

import (
	"context"
	"crypto"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/cryptoops"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Crypto is the in-run cryptography tool (RFC EP): encrypt, decrypt, sign,
// verify, MAC, TOTP, hash and random, with keys the RUNTIME holds. The model
// names a key; the tool opens it here, uses it and returns only the result.
// Key material never reaches a tool result, a transcript, a log line or an
// audit record.
//
// A key comes from one of three places, and every one is gated by the run's
// crypto_scopes (tools.CryptoPolicy):
//   - a crypto-key credential, named by string: versioned, its type sealed in;
//   - an ordinary stored credential used as key material ({"cred": name});
//   - a credential the run's caller passed on the run ({"run": name}).
//
// Keyless ops (hash, random) need no scope.
type Crypto struct {
	// Engine resolves stored keys. Nil: every keyed op on a stored key refuses
	// (run credentials and keyless ops still work).
	Engine *credential.Engine
	// Audit records every keyed op. Nil: NopSink.
	Audit audit.Sink
	// MaxBytes caps each data argument (default 1 MiB).
	MaxBytes int
	// MaxOpsPerRun caps the ops one run may make (default 100).
	MaxOpsPerRun int

	budget runBudget
	stats  cryptoStats
}

// CryptoToolName is the in-band tool name (tools:[Crypto]).
const CryptoToolName = "Crypto"

// CryptoPlaintextField is encrypt's input carrying the plaintext. It is masked
// out of the persisted tool call: the event is stored before the tool runs,
// so the mask, not a later registration, is what keeps it out.
const CryptoPlaintextField = "plaintext"

const (
	defaultCryptoMaxBytes     = 1 << 20
	defaultCryptoMaxOpsPerRun = 100
)

func (c *Crypto) Name() string { return CryptoToolName }

func (c *Crypto) Description() string {
	return "Encrypt, decrypt, sign, verify, MAC, one-time codes, hashing and random values, with keys the runtime holds " +
		"(see Context op=help topic=Crypto). You name a key; its material is never shown to you. A key is a crypto key " +
		"stored with CredentialDef (key), an ordinary stored credential used as key material (cred), or a credential your " +
		"run's caller passed (run); which of those your run may use is its crypto scope. Ops: " +
		"encrypt, decrypt, rewrap, mac, verify_mac, sign, verify, public_key, totp, hash, random. Read the op's help " +
		"(Context op=help topic=Crypto.<op>) before your first call."
}

const cryptoInputSchema = `{
  "type": "object",
  "properties": {
    "op":         {"type": "string", "enum": ["encrypt","decrypt","rewrap","mac","verify_mac","sign","verify","public_key","totp","hash","random"], "description": "The operation."},
    "key":        {"type": "string", "description": "A crypto key stored with CredentialDef, by name. Its type and versions are stored with it. Give exactly one of key, cred, run (decrypt and rewrap of an envelope may omit all three)."},
    "cred":       {"type": "string", "description": "An ordinary stored credential, by name, used as key material. Needs key_type."},
    "run":        {"type": "string", "description": "A credential your run's caller passed with the run, by name, used as key material. Needs key_type."},
    "key_scope":  {"type": "string", "enum": ["agent","user","tenant"], "description": "key and cred only: which scope's key to use. Needed only when more than one scope your run may use holds one of that name; an envelope already records it."},
    "key_type":   {"type": "string", "enum": ["aes-256-gcm","xchacha20-poly1305","hmac-sha256","hmac-sha512","ed25519","ecdsa-p256","totp"], "description": "cred and run only: what kind of key the value is."},
    "key_encoding": {"type": "string", "enum": ["raw","base64","hex","base32","pem"], "description": "cred and run only: how the value is written (default by key_type: base64 for AEAD and HMAC, base32 for totp, pem for ed25519 and ecdsa-p256)."},
    "derive":     {"type": "boolean", "description": "cred and run only: true derives an AEAD or HMAC key from any secret instead of reading the value as a key."},
    "version":    {"type": "integer", "description": "key only: a version of the crypto key, for public_key or a raw ciphertext. Default: the current version (decrypt of raw ciphertext tries every usable version)."},
    "plaintext":  {"type": "string", "description": "encrypt: what to encrypt. Text, or base64 bytes with input_encoding base64. Masked from the stored transcript."},
    "ciphertext": {"type": "string", "description": "decrypt, rewrap: what encrypt returned."},
    "data":       {"type": "string", "description": "mac, verify_mac, sign, verify, hash: the data. Text, or base64 bytes with input_encoding base64."},
    "input_encoding":  {"type": "string", "enum": ["text","base64"], "description": "How plaintext / data is written (default text)."},
    "output_encoding": {"type": "string", "enum": ["hex","base64","text"], "description": "decrypt: text (default when the result is valid UTF-8) or base64. mac, hash, random bytes: hex (default) or base64."},
    "context":    {"type": "string", "description": "encrypt, decrypt, rewrap: optional associated data. A ciphertext made with a context decrypts only with the same context. Use it to tie a value to the record or field it belongs to."},
    "format":     {"type": "string", "enum": ["envelope","raw","jws"], "description": "encrypt: envelope (default, self-describing) or raw (base64 of nonce, ciphertext and tag). sign: raw (default for signing keys) or jws (the only form for a MAC key)."},
    "sensitive":  {"type": "boolean", "description": "decrypt: true keeps the plaintext out of the stored transcript. You still read it in this turn."},
    "mac":        {"type": "string", "description": "verify_mac: the MAC to check, hex or base64."},
    "signature":  {"type": "string", "description": "verify: a raw signature (base64) or a JWS token (pass the token here and leave data empty)."},
    "public_key": {"type": "string", "description": "verify: a PEM or JWK public key, in place of key, for a signer whose key this deployment does not hold."},
    "digits":     {"type": "integer", "description": "totp: code length, 6 to 8 (default 6)."},
    "period":     {"type": "integer", "description": "totp: seconds per code (default 30)."},
    "algorithm":  {"type": "string", "enum": ["sha256","sha512","sha3-256","sha3-512"], "description": "hash: the algorithm (default sha256)."},
    "kind":       {"type": "string", "enum": ["bytes","token","uuid"], "description": "random: what to make (default token)."},
    "length":     {"type": "integer", "description": "random: number of random bytes for bytes and token (default 32, at most 4096)."}
  },
  "required": ["op"]
}`

func (c *Crypto) InputSchema() json.RawMessage { return json.RawMessage(cryptoInputSchema) }

type cryptoInput struct {
	Op             string `json:"op"`
	Key            string `json:"key"`
	Cred           string `json:"cred"`
	Run            string `json:"run"`
	KeyScope       string `json:"key_scope"`
	KeyType        string `json:"key_type"`
	KeyEncoding    string `json:"key_encoding"`
	Derive         bool   `json:"derive"`
	Version        int    `json:"version"`
	Plaintext      string `json:"plaintext"`
	Ciphertext     string `json:"ciphertext"`
	Data           string `json:"data"`
	InputEncoding  string `json:"input_encoding"`
	OutputEncoding string `json:"output_encoding"`
	Context        string `json:"context"`
	Format         string `json:"format"`
	Sensitive      bool   `json:"sensitive"`
	MAC            string `json:"mac"`
	Signature      string `json:"signature"`
	PublicKey      string `json:"public_key"`
	Digits         int    `json:"digits"`
	Period         int    `json:"period"`
	Algorithm      string `json:"algorithm"`
	Kind           string `json:"kind"`
	Length         int    `json:"length"`
}

// keyRef is the key the call names: exactly one of Name (a crypto key), Cred,
// Run, with the fields that qualify it.
type keyRef struct {
	Name     string
	Cred     string
	Run      string
	Scope    string // Name or Cred: the scope pinned by key_scope or the envelope
	Version  int
	KeyType  string
	Encoding string
	Derive   bool
}

var cryptoKeyNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Key sources, as the envelope and the AAD write them. A stored key's source
// names its kind and scope (cryptoops.KeySource / CredSource), so a
// ciphertext is bound to the scope whose key sealed it.
const srcRun = cryptoops.SourceRun

// isKeySource reports whether src is a crypto key's (any scope): the only
// kind with versions.
func isKeySource(src string) bool { return cryptoops.KeyScope(src) != "" }

func sourceWord(src string) string {
	switch {
	case cryptoops.CredScope(src) != "":
		return "cred"
	case src == srcRun:
		return "run"
	}
	return "key"
}

// cryptoKey is a resolved key: its material by version, and where it came from.
type cryptoKey struct {
	source   string // kt/ku/ka (crypto key) | ct/cu/ca (cred) | srcRun
	name     string
	scope    string // agent | user | tenant | run
	keyType  cryptoops.KeyType
	current  int            // 0 for an unversioned (cred / run) key
	min      int            // lowest usable version
	versions map[int][]byte // version → material (only usable versions)
}

func (k *cryptoKey) material(v int) ([]byte, bool) {
	m, ok := k.versions[v]
	return m, ok
}

// sortedVersions lists the key's usable versions, newest first.
func (k *cryptoKey) sortedVersions() []int {
	out := make([]int, 0, len(k.versions))
	for v := range k.versions {
		out = append(out, v)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

func (c *Crypto) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var in cryptoInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errValidation("invalid input JSON: "+err.Error(), "Send a JSON object with an op (Context op=help topic=Crypto)."), nil
	}
	if in.Op == "" {
		return errValidation("missing required field: op", "Name one of: encrypt, decrypt, rewrap, mac, verify_mac, sign, verify, public_key, totp, hash, random."), nil
	}
	if res, ok := c.checkSizes(in); !ok {
		return res, nil
	}
	if res, ok := c.spend(ctx); !ok {
		return res, nil
	}
	var (
		res tools.Result
		ev  *audit.Event
	)
	switch in.Op {
	case "hash":
		res = c.execHash(in)
	case "random":
		res = c.execRandom(in)
	case "encrypt", "decrypt", "rewrap", "mac", "verify_mac", "sign", "verify", "public_key", "totp":
		res, ev = c.execKeyed(ctx, in)
	default:
		res = errValidation(fmt.Sprintf("unknown op %q", in.Op), "Use one of: encrypt, decrypt, rewrap, mac, verify_mac, sign, verify, public_key, totp, hash, random.")
	}
	// The op label is the model's input: count an unknown one under one name,
	// so the metric's series stay bounded by the op list.
	label := in.Op
	if !cryptoOps[label] {
		label = "unknown"
	}
	c.stats.count(label, res.IsError)
	if ev != nil {
		c.record(ctx, ev, res.IsError)
	}
	return res, nil
}

// cryptoOps is the op set, for the metric label.
var cryptoOps = map[string]bool{
	"encrypt": true, "decrypt": true, "rewrap": true, "mac": true, "verify_mac": true, "sign": true,
	"verify": true, "public_key": true, "totp": true, "hash": true, "random": true,
}

// Caps on the small arguments, independent of the data cap.
const (
	cryptoMaxContextBytes   = 4096
	cryptoMaxMACBytes       = 1024
	cryptoMaxPublicKeyBytes = 16384
)

// checkSizes refuses an argument past its cap before anything is decoded.
// Every string argument has one: the data arguments the operator's cap (with
// room for their base64 or envelope text), the rest a fixed one.
func (c *Crypto) checkSizes(in cryptoInput) (tools.Result, bool) {
	limit := c.MaxBytes
	if limit <= 0 {
		limit = defaultCryptoMaxBytes
	}
	dataCap := limit/3*4 + 1024 // base64 of a value at the cap, plus an envelope header
	for _, f := range []struct {
		name string
		v    string
		max  int
	}{
		{"plaintext", in.Plaintext, dataCap},
		{"ciphertext", in.Ciphertext, dataCap},
		{"data", in.Data, dataCap},
		{"signature", in.Signature, dataCap},
		{"context", in.Context, cryptoMaxContextBytes},
		{"mac", in.MAC, cryptoMaxMACBytes},
		{"public_key", in.PublicKey, cryptoMaxPublicKeyBytes},
	} {
		if len(f.v) > f.max {
			return errValidation(fmt.Sprintf("%s is %d bytes, over its %d-byte limit", f.name, len(f.v), f.max),
				"Split the data, or ask the operator to raise LOOMCYCLE_CRYPTO_MAX_BYTES."), false
		}
	}
	return tools.Result{}, true
}

// spend takes one op from the run's budget.
func (c *Crypto) spend(ctx context.Context) (tools.Result, bool) {
	limit := c.MaxOpsPerRun
	if limit <= 0 {
		limit = defaultCryptoMaxOpsPerRun
	}
	runID := tools.RunID(ctx)
	if runID == "" && tools.IsSubstrateOperator(ctx) {
		// The operator API: no run, so no run's budget. The token's rate bounds it.
		return tools.Result{}, true
	}
	if runID == "" && tools.HasRunIdentity(ctx) {
		// A run's call that carries no run id still spends from a budget: its
		// agent's, so the cap is never skipped for a run.
		runID = "agent:" + tools.RunIdentity(ctx).AgentID
	}
	if runID == "" {
		return tools.Result{}, true // not a run's call (tests, the operator API)
	}
	if !c.budget.take(runID, limit) {
		return errBusiness(fmt.Sprintf("this run has used its %d Crypto operations", limit),
			"Finish with what you have; a later run gets a new budget, or the operator can raise LOOMCYCLE_CRYPTO_MAX_OPS_PER_RUN."), false
	}
	return tools.Result{}, true
}

// ---- keyless ops -----------------------------------------------------------

func (c *Crypto) execHash(in cryptoInput) tools.Result {
	alg := in.Algorithm
	if alg == "" {
		alg = "sha256"
	}
	data, res, ok := decodeInput(in.Data, in.InputEncoding, "data")
	if !ok {
		return res
	}
	sum, err := cryptoops.Hash(alg, data)
	if err != nil {
		return errValidation("hash: "+err.Error(), "Use sha256, sha512, sha3-256 or sha3-512.")
	}
	out, res, ok := encodeBytes(sum, in.OutputEncoding)
	if !ok {
		return res
	}
	return cryptoJSON(map[string]any{"algorithm": alg, "digest": out})
}

func (c *Crypto) execRandom(in cryptoInput) tools.Result {
	n := in.Length
	if n == 0 {
		n = 32
	}
	switch in.Kind {
	case "", "token":
		tok, err := cryptoops.RandomToken(n)
		if err != nil {
			return errValidation("random: "+err.Error(), "Use a length from 1 to 4096.")
		}
		return cryptoJSON(map[string]any{"kind": "token", "value": tok})
	case "bytes":
		b, err := cryptoops.RandomBytes(n)
		if err != nil {
			return errValidation("random: "+err.Error(), "Use a length from 1 to 4096.")
		}
		out, res, ok := encodeBytes(b, in.OutputEncoding)
		if !ok {
			return res
		}
		return cryptoJSON(map[string]any{"kind": "bytes", "value": out})
	case "uuid":
		u, err := cryptoops.UUIDv4()
		if err != nil {
			return errTransient("random: "+err.Error(), "Try again.")
		}
		return cryptoJSON(map[string]any{"kind": "uuid", "value": u})
	default:
		return errValidation(fmt.Sprintf("random: unknown kind %q", in.Kind), "Use bytes, token or uuid.")
	}
}

// ---- keyed ops -------------------------------------------------------------

func (c *Crypto) execKeyed(ctx context.Context, in cryptoInput) (tools.Result, *audit.Event) {
	ident := tools.RunIdentity(ctx)
	ev := &audit.Event{
		Action:        "crypto_op",
		ActorTenant:   ident.TenantID,
		ActorSubject:  ident.UserID,
		TargetTenant:  ident.TenantID,
		CryptoOp:      in.Op,
		CryptoRunID:   tools.RunID(ctx),
		CryptoAgent:   tools.AgentName(ctx),
		CryptoBytesIn: len(in.Plaintext) + len(in.Ciphertext) + len(in.Data) + len(in.Signature),
	}
	// The actor is who asked, the target the tenant whose key was used. They
	// differ on the operator API, where an admin may use another tenant's keys,
	// so the actor comes from the authenticated principal when there is one.
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		ev.ActorTenant, ev.ActorSubject, ev.ActorTokenSuffix = p.TenantID, p.Subject, p.TokenSuffix
	}

	// decrypt and rewrap read the key from an envelope when one is given.
	var env *cryptoops.Envelope
	if (in.Op == "decrypt" || in.Op == "rewrap") && strings.HasPrefix(in.Ciphertext, "lcx1:") {
		e, err := cryptoops.ParseEnvelope(in.Ciphertext)
		if err != nil {
			return errValidation("ciphertext is not a valid envelope", "Pass the ciphertext exactly as encrypt returned it."), ev
		}
		env = &e
	}

	// verify against a public key the caller supplies needs no stored key.
	if in.Op == "verify" && in.PublicKey != "" {
		if in.Key != "" || in.Cred != "" || in.Run != "" {
			return errValidation("verify: give a key or public_key, not both", "Drop one of them."), ev
		}
		return c.verifyWithPublicKey(in), ev
	}

	ref, res, ok := parseKeyRef(keyRef{
		Name: in.Key, Cred: in.Cred, Run: in.Run, Scope: in.KeyScope, Version: in.Version,
		KeyType: in.KeyType, Encoding: in.KeyEncoding, Derive: in.Derive,
	}, env)
	if !ok {
		return res, ev
	}
	ev.TargetName = refName(ref)
	key, res, ok := c.resolveKey(ctx, ref, env)
	if !ok {
		return res, ev
	}
	ev.CryptoSource = sourceWord(key.source)
	ev.CryptoScope = key.scope
	ev.CryptoKeyType = string(key.keyType)
	ev.CryptoVersion = key.current
	if !key.keyType.Allows(in.Op) {
		return errValidation(fmt.Sprintf("%s: a %s key cannot do %s (it can: %s)", in.Op, key.keyType, in.Op, strings.Join(key.keyType.Ops(), ", ")),
			"Use a key of a type that does this operation."), ev
	}
	tenant := ident.TenantID
	switch in.Op {
	case "encrypt":
		return c.encrypt(key, tenant, in, ev), ev
	case "decrypt":
		return c.decrypt(ctx, key, tenant, in, env, ref, ev), ev
	case "rewrap":
		return c.rewrap(key, tenant, in, env, ref, ev), ev
	case "mac":
		return c.mac(key, in), ev
	case "verify_mac":
		return c.verifyMAC(key, in), ev
	case "sign":
		return c.sign(key, in), ev
	case "verify":
		return c.verify(key, in), ev
	case "public_key":
		return c.publicKey(key, ref), ev
	case "totp":
		return c.totp(key, in), ev
	}
	return errValidation("unknown op", ""), ev
}

// parseKeyRef checks the key the call names. An envelope supplies the key for
// a stored crypto key; for a cred or run key the caller still names it (its
// type is not in the envelope), and it must agree with the envelope.
func parseKeyRef(ref keyRef, env *cryptoops.Envelope) (keyRef, tools.Result, bool) {
	n := 0
	for _, v := range []string{ref.Name, ref.Cred, ref.Run} {
		if v != "" {
			n++
		}
	}
	if n == 0 && env != nil {
		if !isKeySource(env.Source) {
			return ref, errValidation("this ciphertext was made with a "+sourceWord(env.Source)+" key; name it again",
				"Pass "+sourceWord(env.Source)+": \""+env.Name+"\" with its key_type."), false
		}
		ref.Name, ref.Version, n = env.Name, env.Version, 1
	}
	if n != 1 {
		return ref, errValidation("name exactly one of key, cred, run", "key names a crypto key; cred an ordinary stored credential; run a credential the run's caller passed."), false
	}
	if ref.Name != "" && (ref.KeyType != "" || ref.Encoding != "" || ref.Derive) {
		return ref, errValidation("a crypto key's type is stored with it; key_type, key_encoding and derive apply only to cred and run", "Pass the key by name only."), false
	}
	if ref.Scope != "" {
		if ref.Run != "" {
			return ref, errValidation("key_scope applies only to key and cred", "Drop key_scope: a run key has no scope."), false
		}
		if cryptoops.KeySource(ref.Scope) == "" {
			return ref, errValidation(fmt.Sprintf("key_scope %q", ref.Scope), "Use agent, user or tenant."), false
		}
	}
	if !cryptoKeyNameRe.MatchString(refName(ref)) {
		return ref, errValidation("key name must match ^[A-Za-z0-9_-]{1,128}$", ""), false
	}
	if env != nil {
		// An envelope names its key and, for a stored one, its scope: the
		// call opens that scope's key and no other.
		if ref.Scope == "" {
			switch {
			case ref.Name != "":
				ref.Scope = cryptoops.KeyScope(env.Source)
			case ref.Cred != "":
				ref.Scope = cryptoops.CredScope(env.Source)
			}
		}
		if refSource(ref) != env.Source || refName(ref) != env.Name {
			// The same opaque answer a wrong key gets.
			return ref, didNotDecrypt(), false
		}
		if isKeySource(env.Source) {
			ref.Version = env.Version
		}
	}
	return ref, tools.Result{}, true
}

func refName(r keyRef) string {
	switch {
	case r.Cred != "":
		return r.Cred
	case r.Run != "":
		return r.Run
	}
	return r.Name
}

// refSource is the source the ref names; for a stored key not pinned to a
// scope, "" (its scope is known once it resolves).
func refSource(r keyRef) string {
	switch {
	case r.Cred != "":
		return cryptoops.CredSource(r.Scope)
	case r.Run != "":
		return srcRun
	}
	return cryptoops.KeySource(r.Scope)
}

// pinScopes narrows scopes to the one a ref pins (key_scope, or the
// envelope's), refusing a pin the run may not use. Unpinned: scopes as is.
func pinScopes(ref keyRef, scopes credential.Scopes) (credential.Scopes, bool) {
	if ref.Scope == "" {
		return scopes, true
	}
	pinned := credential.Scopes{Agent: ref.Scope == "agent", User: ref.Scope == "user", Tenant: ref.Scope == "tenant"}
	if (pinned.Agent && !scopes.Agent) || (pinned.User && !scopes.User) || (pinned.Tenant && !scopes.Tenant) {
		return credential.Scopes{}, false
	}
	return pinned, true
}

// resolveKey opens the key for this run, within its crypto scopes.
func (c *Crypto) resolveKey(ctx context.Context, ref keyRef, env *cryptoops.Envelope) (*cryptoKey, tools.Result, bool) {
	policy := tools.CryptoPolicy(ctx)
	ident := tools.RunIdentity(ctx)
	scopes := credential.Scopes{
		Agent:  policy.Allows("agent"),
		User:   policy.Allows("user") && !ident.NoUserScope(),
		Tenant: policy.Allows("tenant"),
	}
	userID := ident.UserID
	if !scopes.User {
		userID = ""
	}
	agentName := tools.AgentName(ctx)
	offRun := tools.IsSubstrateOperator(ctx) && tools.RunID(ctx) == ""
	noScope := func(which string) tools.Result {
		if offRun {
			// The operator API: there is no agent and no run whose keys these
			// would be, and no crypto_scopes to grant.
			return errPermission(which+" keys are not reachable outside a run",
				"Outside a run you use your tenant's keys and your own user keys (key_scope tenant or user).")
		}
		return errPermission("this run may not use "+which+" keys", "An operator grants key scopes with the agent's crypto_scopes (agent, user, tenant, run).")
	}
	notFound := func(name string) tools.Result {
		return errNotFound(fmt.Sprintf("no key %q this run may use", name), "Check the name with CredentialDef op=list, and the run's crypto scopes with Context op=self.")
	}

	if ref.Run != "" {
		if !policy.Allows("run") {
			return nil, noScope("run"), false
		}
		val, ok := ident.UserCredentials[ref.Run]
		if !ok || val == "" {
			return nil, notFound(ref.Run), false
		}
		return keyFromSecret(srcRun, ref, "run", []byte(val))
	}

	// A stored key: a crypto key (key) or an ordinary credential (cred).
	scopes, ok := pinScopes(ref, scopes)
	if !ok {
		if env != nil {
			return nil, didNotDecrypt(), false
		}
		return nil, noScope(ref.Scope), false
	}
	if !scopes.Agent && !scopes.User && !scopes.Tenant {
		return nil, noScope("stored"), false
	}

	if ref.Cred != "" {
		if c.Engine == nil {
			return nil, errBusiness("no credential store is configured", "Use a run credential, or ask the operator to configure LOOMCYCLE_SECRET_KEY."), false
		}
		r, found, err := c.Engine.ResolveKeyMaterial(ctx, ident.TenantID, agentName, userID, ref.Cred, scopes)
		if err != nil {
			return nil, keyLookupFailure(ref.Cred, err), false
		}
		if !found {
			return nil, notFound(ref.Cred), false
		}
		return keyFromSecret(cryptoops.CredSource(r.Scope), ref, r.Scope, []byte(r.Value))
	}

	// A crypto-key credential.
	if c.Engine == nil {
		return nil, errBusiness("no credential store is configured", "Ask the operator to configure LOOMCYCLE_SECRET_KEY."), false
	}
	k, found, err := c.Engine.ResolveCryptoKey(ctx, ident.TenantID, agentName, userID, ref.Name, scopes)
	if err != nil {
		if env != nil {
			return nil, didNotDecrypt(), false
		}
		return nil, keyLookupFailure(ref.Name, err), false
	}
	if !found {
		if env != nil {
			return nil, didNotDecrypt(), false
		}
		return nil, notFound(ref.Name), false
	}
	kt, err := cryptoops.ParseKeyType(k.KeyType)
	if err != nil {
		return nil, errBusiness(fmt.Sprintf("key %q has a type this build does not know", ref.Name), ""), false
	}
	return &cryptoKey{source: cryptoops.KeySource(k.Scope), name: ref.Name, scope: k.Scope, keyType: kt, current: k.Current, min: k.MinVersion, versions: k.Versions}, tools.Result{}, true
}

// keyFromSecret turns a cred or run secret into key material of the type the
// caller named.
func keyFromSecret(src string, ref keyRef, scope string, secret []byte) (*cryptoKey, tools.Result, bool) {
	if ref.KeyType == "" {
		return nil, errValidation("key_type is required for a "+sourceWord(src)+" key", "Add key_type: aes-256-gcm, xchacha20-poly1305, hmac-sha256, hmac-sha512, ed25519, ecdsa-p256 or totp."), false
	}
	kt, err := cryptoops.ParseKeyType(ref.KeyType)
	if err != nil {
		return nil, errValidation(err.Error(), ""), false
	}
	var m []byte
	if ref.Derive {
		if ref.Encoding != "" {
			return nil, errValidation("derive and key_encoding cannot be combined", "Drop key_encoding: derive reads the secret as written."), false
		}
		m, err = cryptoops.Derive(kt, secret, refName(ref))
	} else {
		m, err = cryptoops.ParseMaterial(kt, string(secret), ref.Encoding)
	}
	if err != nil {
		// Never quote the secret: say only that it does not fit.
		return nil, errValidation(fmt.Sprintf("%s %q is not a valid %s key", sourceWord(src), refName(ref), kt),
			"Name the encoding it is stored in, or pass derive: true to derive a key from it (AEAD and HMAC types only)."), false
	}
	return &cryptoKey{source: src, name: refName(ref), scope: scope, keyType: kt, versions: map[int][]byte{0: m}}, tools.Result{}, true
}

// keyLookupFailure turns an engine refusal into the model's answer. None of
// these texts carries the value.
func keyLookupFailure(name string, err error) tools.Result {
	switch {
	case errors.Is(err, credential.ErrCryptoKey), errors.Is(err, credential.ErrUseKeyName):
		return errValidation(fmt.Sprintf("%q is a crypto key", name), "Name it with key: \""+name+"\".")
	case errors.Is(err, credential.ErrAmbiguousKey):
		return errValidation(fmt.Sprintf("more than one scope you may use holds %q", name),
			"Name the one you mean with key_scope: agent, user or tenant (CredentialDef op=list shows each one's scope).")
	case errors.Is(err, credential.ErrNotCryptoKey):
		return errValidation(fmt.Sprintf("%q is an ordinary credential, not a crypto key", name), "Use it as key material: cred: \""+name+"\" with its key_type.")
	case errors.Is(err, credential.ErrRestricted):
		return errBusiness(fmt.Sprintf("%q is restricted to where it may be sent, so it cannot be used as a key", name), "Use a crypto key, or a credential without allowed_urls.")
	case errors.Is(err, credential.ErrExpired):
		return errBusiness(fmt.Sprintf("key %q has expired", name), "Ask its owner to rotate it.")
	case errors.Is(err, credential.ErrUnopenable):
		return errBusiness(fmt.Sprintf("key %q is stored but cannot be opened", name), "Ask the operator to check LOOMCYCLE_SECRET_KEY.")
	}
	return errTransient("key lookup failed", "Try again.")
}

func didNotDecrypt() tools.Result {
	return errValidation("did not decrypt", "Check the ciphertext, the key and the context are the ones it was made with.")
}

func (c *Crypto) encrypt(k *cryptoKey, tenant string, in cryptoInput, ev *audit.Event) tools.Result {
	pt, res, ok := decodeInput(in.Plaintext, in.InputEncoding, "plaintext")
	if !ok {
		return res
	}
	v := k.current
	m, ok := k.material(v)
	if !ok {
		return errBusiness("the key's current version is not usable", "")
	}
	body, err := cryptoops.Seal(k.keyType, m, pt, cryptoops.AAD(tenant, k.source, k.name, v, in.Context))
	if err != nil {
		return errTransient("encrypt failed", "Try again.")
	}
	ev.CryptoVersion = v
	switch in.Format {
	case "", "envelope":
		env := cryptoops.Envelope{Source: k.source, Name: k.name, Version: v, Body: body}
		return cryptoJSON(map[string]any{"ciphertext": env.String(), "key_version": v})
	case "raw":
		return cryptoJSON(map[string]any{"ciphertext": base64.StdEncoding.EncodeToString(body), "key_version": v})
	default:
		return errValidation(fmt.Sprintf("encrypt: format %q", in.Format), "Use envelope or raw.")
	}
}

// open decrypts a ciphertext (envelope or raw) and reports the version used.
func (c *Crypto) open(k *cryptoKey, tenant string, in cryptoInput, env *cryptoops.Envelope, ref keyRef) ([]byte, int, bool) {
	var (
		body     []byte
		versions []int
	)
	if env != nil {
		body = env.Body
		versions = []int{env.Version}
	} else {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.Ciphertext))
		if err != nil {
			return nil, 0, false
		}
		body = b
		if ref.Version != 0 || !isKeySource(k.source) {
			versions = []int{ref.Version}
		} else {
			versions = k.sortedVersions() // raw: try each usable version, newest first
		}
	}
	for _, v := range versions {
		m, ok := k.material(v)
		if !ok {
			continue
		}
		if pt, err := cryptoops.Open(k.keyType, m, body, cryptoops.AAD(tenant, k.source, k.name, v, in.Context)); err == nil {
			return pt, v, true
		}
	}
	return nil, 0, false
}

func (c *Crypto) decrypt(ctx context.Context, k *cryptoKey, tenant string, in cryptoInput, env *cryptoops.Envelope, ref keyRef, ev *audit.Event) tools.Result {
	if in.Ciphertext == "" {
		return errValidation("decrypt: ciphertext is required", "")
	}
	pt, v, ok := c.open(k, tenant, in, env, ref)
	if !ok {
		return didNotDecrypt()
	}
	ev.CryptoVersion = v
	if in.Sensitive {
		tools.MarkContentNotRetained(ctx)
	}
	out := map[string]any{"key_version": v}
	switch in.OutputEncoding {
	case "base64":
		out["plaintext"], out["encoding"] = base64.StdEncoding.EncodeToString(pt), "base64"
	case "", "text":
		if utf8.Valid(pt) {
			out["plaintext"], out["encoding"] = string(pt), "text"
		} else if in.OutputEncoding == "text" {
			return errValidation("decrypt: the plaintext is not valid UTF-8 text", "Ask for output_encoding base64.")
		} else {
			out["plaintext"], out["encoding"] = base64.StdEncoding.EncodeToString(pt), "base64"
		}
	default:
		return errValidation(fmt.Sprintf("decrypt: output_encoding %q", in.OutputEncoding), "Use text or base64.")
	}
	return cryptoJSON(out)
}

func (c *Crypto) rewrap(k *cryptoKey, tenant string, in cryptoInput, env *cryptoops.Envelope, ref keyRef, ev *audit.Event) tools.Result {
	if !isKeySource(k.source) {
		return errValidation("rewrap works on a crypto key's ciphertext", "A cred or run key has no versions to move between.")
	}
	pt, from, ok := c.open(k, tenant, in, env, ref)
	if !ok {
		return didNotDecrypt()
	}
	m, ok := k.material(k.current)
	if !ok {
		return errBusiness("the key's current version is not usable", "")
	}
	body, err := cryptoops.Seal(k.keyType, m, pt, cryptoops.AAD(tenant, k.source, k.name, k.current, in.Context))
	if err != nil {
		return errTransient("rewrap failed", "Try again.")
	}
	ev.CryptoVersion = k.current
	ct := base64.StdEncoding.EncodeToString(body)
	if env != nil {
		ct = cryptoops.Envelope{Source: k.source, Name: k.name, Version: k.current, Body: body}.String()
	}
	return cryptoJSON(map[string]any{"ciphertext": ct, "from_version": from, "key_version": k.current})
}

func (c *Crypto) mac(k *cryptoKey, in cryptoInput) tools.Result {
	data, res, ok := decodeInput(in.Data, in.InputEncoding, "data")
	if !ok {
		return res
	}
	m, _ := k.material(k.current)
	sum, err := cryptoops.MAC(k.keyType, m, data)
	if err != nil {
		return errTransient("mac failed", "Try again.")
	}
	out, res, ok := encodeBytes(sum, in.OutputEncoding)
	if !ok {
		return res
	}
	return cryptoJSON(map[string]any{"mac": out, "key_version": k.current})
}

func (c *Crypto) verifyMAC(k *cryptoKey, in cryptoInput) tools.Result {
	data, res, ok := decodeInput(in.Data, in.InputEncoding, "data")
	if !ok {
		return res
	}
	want, ok := decodeHexOrBase64(in.MAC)
	if !ok {
		return errValidation("verify_mac: mac must be hex or base64", "")
	}
	for _, v := range k.sortedVersions() {
		m, _ := k.material(v)
		if cryptoops.VerifyMAC(k.keyType, m, data, want) {
			return cryptoJSON(map[string]any{"valid": true, "key_version": v})
		}
	}
	return cryptoJSON(map[string]any{"valid": false})
}

func (c *Crypto) sign(k *cryptoKey, in cryptoInput) tools.Result {
	data, res, ok := decodeInput(in.Data, in.InputEncoding, "data")
	if !ok {
		return res
	}
	m, _ := k.material(k.current)
	format := in.Format
	if format == "" {
		format = "raw"
		if k.keyType.IsMAC() {
			format = "jws"
		}
	}
	switch format {
	case "jws":
		tok, err := cryptoops.SignJWS(k.keyType, m, data, kidOf(k, k.current))
		if err != nil {
			return errTransient("sign failed", "Try again.")
		}
		return cryptoJSON(map[string]any{"signature": tok, "format": "jws", "key_version": k.current})
	case "raw":
		if k.keyType.IsMAC() {
			return errValidation("a MAC key signs only as jws", "Use format jws, or op mac for a bare MAC.")
		}
		sig, err := cryptoops.Sign(k.keyType, m, data)
		if err != nil {
			return errTransient("sign failed", "Try again.")
		}
		return cryptoJSON(map[string]any{"signature": base64.StdEncoding.EncodeToString(sig), "format": "raw", "key_version": k.current})
	default:
		return errValidation(fmt.Sprintf("sign: format %q", format), "Use raw or jws.")
	}
}

// kidOf names a key version in a JWS header: "<name>.v<version>" for a
// crypto key, the bare name for an unversioned one.
func kidOf(k *cryptoKey, v int) string {
	if isKeySource(k.source) {
		return fmt.Sprintf("%s.v%d", k.name, v)
	}
	return k.name
}

func (c *Crypto) verify(k *cryptoKey, in cryptoInput) tools.Result {
	if in.Signature == "" {
		return errValidation("verify: signature is required", "")
	}
	isJWS := strings.Count(in.Signature, ".") == 2 && in.Data == ""
	for _, v := range k.sortedVersions() {
		m, _ := k.material(v)
		var pub crypto.PublicKey
		if k.keyType.IsSigning() {
			p, err := cryptoops.PublicKeyOf(k.keyType, m)
			if err != nil {
				continue
			}
			pub = p
		}
		if isJWS {
			secret := m
			if k.keyType.IsSigning() {
				secret = nil
			}
			if payload, kid, ok := cryptoops.VerifyJWS(k.keyType, secret, pub, in.Signature); ok {
				return cryptoJSON(map[string]any{"valid": true, "key_version": v, "kid": kid, "payload": string(payload)})
			}
			continue
		}
		if !k.keyType.IsSigning() {
			return errValidation("a MAC key verifies a jws token, or use verify_mac", "")
		}
		data, res, ok := decodeInput(in.Data, in.InputEncoding, "data")
		if !ok {
			return res
		}
		sig, ok := decodeHexOrBase64(in.Signature)
		if ok && cryptoops.Verify(k.keyType, pub, data, sig) {
			return cryptoJSON(map[string]any{"valid": true, "key_version": v})
		}
	}
	return cryptoJSON(map[string]any{"valid": false})
}

func (c *Crypto) verifyWithPublicKey(in cryptoInput) tools.Result {
	kt, pub, err := cryptoops.ParsePublicKey(in.PublicKey)
	if err != nil {
		return errValidation("verify: public_key is not a PEM or JWK Ed25519 / P-256 public key", "")
	}
	if strings.Count(in.Signature, ".") == 2 && in.Data == "" {
		payload, kid, ok := cryptoops.VerifyJWS(kt, nil, pub, in.Signature)
		if !ok {
			return cryptoJSON(map[string]any{"valid": false})
		}
		return cryptoJSON(map[string]any{"valid": true, "kid": kid, "payload": string(payload)})
	}
	data, res, ok := decodeInput(in.Data, in.InputEncoding, "data")
	if !ok {
		return res
	}
	sig, ok := decodeHexOrBase64(in.Signature)
	return cryptoJSON(map[string]any{"valid": ok && cryptoops.Verify(kt, pub, data, sig)})
}

func (c *Crypto) publicKey(k *cryptoKey, ref keyRef) tools.Result {
	v := k.current
	if ref.Version != 0 {
		v = ref.Version
	}
	m, ok := k.material(v)
	if !ok {
		return errNotFound(fmt.Sprintf("key %q has no usable version %d", k.name, v), "Omit version for the current one.")
	}
	pub, err := cryptoops.PublicKeyOf(k.keyType, m)
	if err != nil {
		return errTransient("public_key failed", "")
	}
	pemStr, err := cryptoops.PublicKeyPEM(pub)
	if err != nil {
		return errTransient("public_key failed", "")
	}
	jwk, err := cryptoops.PublicKeyJWK(pub, kidOf(k, v))
	if err != nil {
		return errTransient("public_key failed", "")
	}
	return cryptoJSON(map[string]any{"pem": pemStr, "jwk": jwk, "key_version": v})
}

func (c *Crypto) totp(k *cryptoKey, in cryptoInput) tools.Result {
	m, _ := k.material(k.current)
	code, until, err := cryptoops.TOTPCode(m, time.Now(), in.Digits, in.Period)
	if err != nil {
		return errValidation("totp: "+err.Error(), "digits is 6 to 8, period a positive number of seconds.")
	}
	return cryptoJSON(map[string]any{"code": code, "valid_until": until.UTC().Format(time.RFC3339)})
}

// ---- helpers ---------------------------------------------------------------

func decodeInput(v, enc, field string) ([]byte, tools.Result, bool) {
	switch enc {
	case "", "text":
		return []byte(v), tools.Result{}, true
	case "base64":
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, errValidation(field+" is not valid base64", ""), false
		}
		return b, tools.Result{}, true
	}
	return nil, errValidation(fmt.Sprintf("input_encoding %q", enc), "Use text or base64."), false
}

func encodeBytes(b []byte, enc string) (string, tools.Result, bool) {
	switch enc {
	case "", "hex":
		return hex.EncodeToString(b), tools.Result{}, true
	case "base64":
		return base64.StdEncoding.EncodeToString(b), tools.Result{}, true
	}
	return "", errValidation(fmt.Sprintf("output_encoding %q", enc), "Use hex or base64."), false
}

func decodeHexOrBase64(s string) ([]byte, bool) {
	s = strings.TrimSpace(s)
	if b, err := hex.DecodeString(s); err == nil && len(s)%2 == 0 {
		return b, true
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, true
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, true
	}
	return nil, false
}

func cryptoJSON(v any) tools.Result {
	b, err := json.Marshal(v)
	if err != nil {
		return errTransient("could not encode the result", "")
	}
	return tools.Result{Text: string(b)}
}

// record writes the audit record of one keyed op.
func (c *Crypto) record(ctx context.Context, ev *audit.Event, failed bool) {
	if c.Audit == nil {
		return
	}
	ev.CryptoOutcome = "ok"
	if failed {
		ev.CryptoOutcome = "refused"
	}
	_ = c.Audit.Record(*ev) // best effort: audit never blocks the op
}

// ---- per-run budget --------------------------------------------------------

// runBudget counts the ops each run has made. Entries untouched for a day are
// dropped, so the map is bounded by the runs active in that window.
type runBudget struct {
	mu   sync.Mutex
	runs map[string]*budgetEntry
	gc   time.Time
}

type budgetEntry struct {
	n    int
	seen time.Time
}

func (b *runBudget) take(runID string, limit int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.runs == nil {
		b.runs = map[string]*budgetEntry{}
	}
	if now.Sub(b.gc) > time.Hour {
		for id, e := range b.runs {
			if now.Sub(e.seen) > 24*time.Hour {
				delete(b.runs, id)
			}
		}
		b.gc = now
	}
	e := b.runs[runID]
	if e == nil {
		e = &budgetEntry{}
		b.runs[runID] = e
	}
	e.seen = now
	if e.n >= limit {
		return false
	}
	e.n++
	return true
}

// ---- metrics ---------------------------------------------------------------

// cryptoStats counts ops by name and outcome for /metrics.
type cryptoStats struct {
	mu sync.Mutex
	m  map[string]*atomic.Uint64 // "<op>|ok" / "<op>|error"
}

func (s *cryptoStats) count(op string, failed bool) {
	outcome := "ok"
	if failed {
		outcome = "error"
	}
	key := op + "|" + outcome
	s.mu.Lock()
	if s.m == nil {
		s.m = map[string]*atomic.Uint64{}
	}
	ctr := s.m[key]
	if ctr == nil {
		ctr = &atomic.Uint64{}
		s.m[key] = ctr
	}
	s.mu.Unlock()
	ctr.Add(1)
}

// Stats returns a snapshot of op counts keyed "<op>|<outcome>".
func (c *Crypto) Stats() map[string]uint64 {
	c.stats.mu.Lock()
	defer c.stats.mu.Unlock()
	out := make(map[string]uint64, len(c.stats.m))
	for k, v := range c.stats.m {
		out[k] = v.Load()
	}
	return out
}
