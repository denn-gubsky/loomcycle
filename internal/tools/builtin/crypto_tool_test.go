package builtin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/cryptoops"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// cryptoFixture is a Crypto tool and a CredentialDef tool over one engine, and
// a run context granted the given crypto scopes.
func cryptoFixture(t *testing.T, scopes ...string) (*Crypto, *CredentialDef, context.Context) {
	t.Helper()
	cd, ctx := credFixture(t, "u1", testKEK())
	ctx = tools.WithCryptoPolicy(ctx, tools.CryptoPolicyValue{Scopes: scopes})
	return &Crypto{Engine: cd.Engine}, cd, ctx
}

func cryptoExec(t *testing.T, c *Crypto, ctx context.Context, body string) (map[string]any, tools.Result) {
	t.Helper()
	res, err := c.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute hard error: %v", err)
	}
	var out map[string]any
	if !res.IsError {
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("result is not JSON: %s", res.Text)
		}
	}
	return out, res
}

func mustOK(t *testing.T, what string, res tools.Result) {
	t.Helper()
	if res.IsError {
		t.Fatalf("%s: %s", what, res.Text)
	}
}

func TestCrypto_KeylessOpsNeedNoScope(t *testing.T) {
	c, _, ctx := cryptoFixture(t) // no scopes
	out, res := cryptoExec(t, c, ctx, `{"op":"hash","data":"abc"}`)
	mustOK(t, "hash", res)
	if out["digest"] != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("sha256(abc) = %v", out["digest"])
	}
	out, res = cryptoExec(t, c, ctx, `{"op":"random","kind":"token","length":16}`)
	mustOK(t, "random", res)
	if b, err := base64.RawURLEncoding.DecodeString(out["value"].(string)); err != nil || len(b) != 16 {
		t.Errorf("token %v does not decode to 16 bytes (%v)", out["value"], err)
	}
}

func TestCrypto_AKeyedOpWithoutAScopeIsRefused(t *testing.T) {
	c, cd, ctx := cryptoFixture(t) // no scopes
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"k1","key_type":"aes-256-gcm"}`)
	mustOK(t, "generate", res)
	_, res = cryptoExec(t, c, ctx, `{"op":"encrypt","key":"k1","plaintext":"x"}`)
	if !res.IsError || res.Error == nil || res.Error.Category != tools.CategoryPermission {
		t.Fatalf("encrypt with no crypto scope = %+v, want a permission refusal", res)
	}
}

func TestCrypto_EncryptDecryptRoundTripAndOpaqueFailures(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"fields","key_type":"aes-256-gcm"}`)
	mustOK(t, "generate", res)

	out, res := cryptoExec(t, c, ctx, `{"op":"encrypt","key":"fields","plaintext":"card 4111","context":"customer:42"}`)
	mustOK(t, "encrypt", res)
	ct := out["ciphertext"].(string)
	if !strings.HasPrefix(ct, "lcx1:k:fields:1:") {
		t.Fatalf("ciphertext %q is not a v1 envelope for fields", ct)
	}
	out, res = cryptoExec(t, c, ctx, `{"op":"decrypt","ciphertext":"`+ct+`","context":"customer:42"}`)
	mustOK(t, "decrypt", res)
	if out["plaintext"] != "card 4111" {
		t.Fatalf("decrypt = %v", out["plaintext"])
	}
	for name, body := range map[string]string{
		"wrong context":  `{"op":"decrypt","ciphertext":"` + ct + `","context":"customer:43"}`,
		"no context":     `{"op":"decrypt","ciphertext":"` + ct + `"}`,
		"flipped byte":   `{"op":"decrypt","ciphertext":"` + flipLast(ct) + `","context":"customer:42"}`,
		"other key name": `{"op":"decrypt","ciphertext":"` + strings.Replace(ct, ":fields:", ":other:", 1) + `","context":"customer:42"}`,
	} {
		_, res := cryptoExec(t, c, ctx, body)
		if !res.IsError || !strings.Contains(res.Text, "did not decrypt") {
			t.Errorf("%s: %q, want the opaque did-not-decrypt", name, res.Text)
		}
	}
}

// flipLast changes one character in the middle of an envelope's body. (The
// last character can carry only padding bits, so changing it may make the
// encoding non-canonical: the envelope is then malformed, not undecryptable.)
func flipLast(s string) string {
	b := []byte(s)
	i := strings.LastIndex(s, ":") + 10
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}

func TestCrypto_SensitiveDecryptIsNotRetained(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"f","key_type":"xchacha20-poly1305"}`)
	mustOK(t, "generate", res)
	out, _ := cryptoExec(t, c, ctx, `{"op":"encrypt","key":"f","plaintext":"s3cret"}`)
	ct := out["ciphertext"].(string)

	callCtx, mark := tools.WithContentMark(ctx)
	_, res = cryptoExec(t, c, callCtx, `{"op":"decrypt","ciphertext":"`+ct+`"}`)
	mustOK(t, "decrypt", res)
	if mark.NotRetained() {
		t.Error("a decrypt without sensitive was marked not retained")
	}
	callCtx, mark = tools.WithContentMark(ctx)
	_, res = cryptoExec(t, c, callCtx, `{"op":"decrypt","ciphertext":"`+ct+`","sensitive":true}`)
	mustOK(t, "sensitive decrypt", res)
	if !mark.NotRetained() {
		t.Error("a sensitive decrypt was not marked not retained")
	}
}

func TestCrypto_RotationRewrapAndRetirement(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"r","key_type":"aes-256-gcm"}`)
	mustOK(t, "generate", res)
	out, _ := cryptoExec(t, c, ctx, `{"op":"encrypt","key":"r","plaintext":"old"}`)
	v1 := out["ciphertext"].(string)

	_, res = credExec(t, cd, ctx, `{"op":"rotate","scope":"tenant","name":"r"}`)
	mustOK(t, "rotate", res)
	out, res = cryptoExec(t, c, ctx, `{"op":"decrypt","ciphertext":"`+v1+`"}`)
	mustOK(t, "decrypt v1 after rotate", res)
	if out["key_version"] != float64(1) {
		t.Errorf("decrypted with version %v, want 1", out["key_version"])
	}
	out, res = cryptoExec(t, c, ctx, `{"op":"rewrap","ciphertext":"`+v1+`"}`)
	mustOK(t, "rewrap", res)
	v2 := out["ciphertext"].(string)
	if !strings.HasPrefix(v2, "lcx1:k:r:2:") {
		t.Fatalf("rewrap = %q, want a v2 envelope", v2)
	}

	// The key still protects version-1 data: it cannot be deleted.
	_, res = credExec(t, cd, ctx, `{"op":"delete","scope":"tenant","name":"r"}`)
	if !res.IsError {
		t.Fatal("deleting a key whose versions are live succeeded")
	}
	_, res = credExec(t, cd, ctx, `{"op":"delete_version","scope":"tenant","name":"r","version":1}`)
	if !res.IsError {
		t.Fatal("deleting an unretired version succeeded")
	}
	_, res = credExec(t, cd, ctx, `{"op":"set_min_version","scope":"tenant","name":"r","min_version":2}`)
	mustOK(t, "set_min_version", res)
	if _, res := cryptoExec(t, c, ctx, `{"op":"decrypt","ciphertext":"`+v1+`"}`); !res.IsError {
		t.Error("a retired version still decrypts")
	}
	if out, res := cryptoExec(t, c, ctx, `{"op":"decrypt","ciphertext":"`+v2+`"}`); res.IsError || out["plaintext"] != "old" {
		t.Errorf("the rewrapped ciphertext: %v %s", out, res.Text)
	}
	_, res = credExec(t, cd, ctx, `{"op":"delete_version","scope":"tenant","name":"r","version":1}`)
	mustOK(t, "delete retired version", res)
}

func TestCrypto_OrdinaryCredentialAsKeyMaterial(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	_, res := credExec(t, cd, ctx, `{"op":"create","scope":"tenant","name":"partner","value":"0123456789abcdef0123456789abcdef-shared"}`)
	mustOK(t, "create", res)
	out, res := cryptoExec(t, c, ctx, `{"op":"mac","cred":"partner","key_type":"hmac-sha256","key_encoding":"raw","data":"body"}`)
	mustOK(t, "mac", res)
	h := hmac.New(sha256.New, []byte("0123456789abcdef0123456789abcdef-shared"))
	h.Write([]byte("body"))
	if out["mac"] != hex.EncodeToString(h.Sum(nil)) {
		t.Errorf("mac = %v, want the HMAC of the stored secret", out["mac"])
	}

	// A credential restricted to destinations is not key material.
	_, res = credExec(t, cd, ctx, `{"op":"create","scope":"tenant","name":"pinned","value":"0123456789abcdef0123456789abcdef-pinned","allowed_urls":["https://api.example.com"]}`)
	mustOK(t, "create restricted", res)
	if _, res := cryptoExec(t, c, ctx, `{"op":"mac","cred":"pinned","key_type":"hmac-sha256","key_encoding":"raw","data":"x"}`); !res.IsError {
		t.Error("a restricted credential was used as key material")
	}
}

// One way only: a crypto key is never an ordinary $cred: secret, and an
// ordinary credential is never a crypto key by name.
func TestCrypto_KeysAndSecretsDoNotCross(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"hk","key_type":"hmac-sha256"}`)
	mustOK(t, "generate", res)
	if _, res := cryptoExec(t, c, ctx, `{"op":"mac","cred":"hk","key_type":"hmac-sha256","data":"x"}`); !res.IsError {
		t.Error("a crypto key was read as cred key material")
	}
	if _, _, err := cd.Engine.Resolve(ctx, "tnt", "agent-1", "u1", "hk"); err == nil {
		t.Error("a crypto key resolved as a $cred: secret")
	}
	_, res = credExec(t, cd, ctx, `{"op":"create","scope":"tenant","name":"plain","value":"just-a-token-value-0123456789"}`)
	mustOK(t, "create", res)
	if _, res := cryptoExec(t, c, ctx, `{"op":"mac","key":"plain","data":"x"}`); !res.IsError {
		t.Error("an ordinary credential was used as a crypto key by name")
	}
}

func TestCrypto_RunCredentialsNeedTheRunScope(t *testing.T) {
	_, _, base := cryptoFixture(t, "tenant")
	ident := tools.RunIdentity(base)
	ident.UserCredentials = map[string]string{"sess": base64.StdEncoding.EncodeToString(make([]byte, 32))}
	ctx := tools.WithRunIdentity(base, ident)
	c := &Crypto{}
	if _, res := cryptoExec(t, c, ctx, `{"op":"encrypt","run":"sess","key_type":"aes-256-gcm","plaintext":"x"}`); !res.IsError {
		t.Fatal("a run credential was used without the run scope")
	}
	ctx = tools.WithCryptoPolicy(ctx, tools.CryptoPolicyValue{Scopes: []string{"run"}})
	out, res := cryptoExec(t, c, ctx, `{"op":"encrypt","run":"sess","key_type":"aes-256-gcm","plaintext":"hand-off"}`)
	mustOK(t, "encrypt", res)
	ct := out["ciphertext"].(string)
	if _, res := cryptoExec(t, c, ctx, `{"op":"decrypt","ciphertext":"`+ct+`"}`); !res.IsError {
		t.Error("a run-key envelope decrypted without naming the run key")
	}
	out, res = cryptoExec(t, c, ctx, `{"op":"decrypt","ciphertext":"`+ct+`","run":"sess","key_type":"aes-256-gcm"}`)
	mustOK(t, "decrypt", res)
	if out["plaintext"] != "hand-off" {
		t.Errorf("decrypt = %v", out["plaintext"])
	}
}

func TestCrypto_SignVerifyAndPublicKey(t *testing.T) {
	for _, kt := range []string{"ed25519", "ecdsa-p256"} {
		t.Run(kt, func(t *testing.T) {
			c, cd, ctx := cryptoFixture(t, "tenant")
			_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"sig","key_type":"`+kt+`"}`)
			mustOK(t, "generate", res)
			out, res := cryptoExec(t, c, ctx, `{"op":"sign","key":"sig","data":"report"}`)
			mustOK(t, "sign", res)
			sig := out["signature"].(string)
			out, res = cryptoExec(t, c, ctx, `{"op":"public_key","key":"sig"}`)
			mustOK(t, "public_key", res)
			pem, _ := json.Marshal(out["pem"])
			out, res = cryptoExec(t, c, ctx, `{"op":"verify","public_key":`+string(pem)+`,"data":"report","signature":"`+sig+`"}`)
			mustOK(t, "verify", res)
			if out["valid"] != true {
				t.Error("a signature did not verify with the key's own public key")
			}
			out, _ = cryptoExec(t, c, ctx, `{"op":"verify","key":"sig","data":"tampered","signature":"`+sig+`"}`)
			if out["valid"] != false {
				t.Error("a signature over other data verified")
			}
			out, res = cryptoExec(t, c, ctx, `{"op":"sign","key":"sig","data":"{\"sub\":\"a\"}","format":"jws"}`)
			mustOK(t, "sign jws", res)
			out, res = cryptoExec(t, c, ctx, `{"op":"verify","key":"sig","signature":"`+out["signature"].(string)+`"}`)
			mustOK(t, "verify jws", res)
			if out["valid"] != true || out["payload"] != `{"sub":"a"}` {
				t.Errorf("jws verify = %v", out)
			}
		})
	}
}

func TestCrypto_TOTPFromAStoredSeed(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	seed := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // base32 of "12345678901234567890"
	_, res := credExec(t, cd, ctx, `{"op":"create","scope":"tenant","name":"portal-2fa","value":"`+seed+`"}`)
	mustOK(t, "create", res)
	out, res := cryptoExec(t, c, ctx, `{"op":"totp","cred":"portal-2fa","key_type":"totp"}`)
	mustOK(t, "totp", res)
	want, _, _ := cryptoops.TOTPCode([]byte("12345678901234567890"), time.Now(), 6, 30)
	if out["code"] != want {
		t.Errorf("totp = %v, want %s", out["code"], want)
	}
}

// Inside a run, crypto keys are made and changed only in agent scope; moving
// a key between deployments is an operator's, outside any run.
func TestCredentialDefKeys_InsideARunOnlyAgentScope(t *testing.T) {
	_, cd, ctx := cryptoFixture(t, "agent", "tenant")
	runCtx := tools.WithRunID(ctx, "r_1")
	for _, op := range []string{
		`{"op":"generate","scope":"tenant","name":"k","key_type":"aes-256-gcm"}`,
		`{"op":"create","usage":"crypto","scope":"tenant","name":"k","key_type":"aes-256-gcm","value":"` + base64.StdEncoding.EncodeToString(make([]byte, 32)) + `"}`,
	} {
		if _, res := credExec(t, cd, runCtx, op); !res.IsError {
			t.Errorf("inside a run, %s succeeded", op)
		}
	}
	_, res := credExec(t, cd, runCtx, `{"op":"generate","scope":"agent","name":"k","key_type":"aes-256-gcm"}`)
	mustOK(t, "agent-scope generate in a run", res)
	if _, res := credExec(t, cd, runCtx, `{"op":"export_key","scope":"agent","name":"k","passphrase":"a long enough passphrase"}`); !res.IsError {
		t.Error("export_key succeeded inside a run")
	}
}

func TestCredentialDefKeys_ExportImportMovesAKeyBetweenDeployments(t *testing.T) {
	src, srcCD, srcCtx := cryptoFixture(t, "tenant")
	_, res := credExec(t, srcCD, srcCtx, `{"op":"generate","scope":"tenant","name":"move","key_type":"aes-256-gcm"}`)
	mustOK(t, "generate", res)
	out, _ := cryptoExec(t, src, srcCtx, `{"op":"encrypt","key":"move","plaintext":"travels"}`)
	ct := out["ciphertext"].(string)
	out, res = credExec(t, srcCD, srcCtx, `{"op":"export_key","scope":"tenant","name":"move","passphrase":"correct horse battery staple"}`)
	mustOK(t, "export_key", res)
	blob := out["blob"].(string)

	dst, dstCD, dstCtx := cryptoFixture(t, "tenant")
	if _, res := credExec(t, dstCD, dstCtx, `{"op":"import_key","scope":"tenant","name":"move","blob":"`+blob+`","passphrase":"wrong horse battery staple"}`); !res.IsError {
		t.Fatal("import_key with the wrong passphrase succeeded")
	}
	_, res = credExec(t, dstCD, dstCtx, `{"op":"import_key","scope":"tenant","name":"move","blob":"`+blob+`","passphrase":"correct horse battery staple"}`)
	mustOK(t, "import_key", res)
	out, res = cryptoExec(t, dst, dstCtx, `{"op":"decrypt","ciphertext":"`+ct+`"}`)
	mustOK(t, "decrypt on the target", res)
	if out["plaintext"] != "travels" {
		t.Errorf("decrypt on the target = %v", out["plaintext"])
	}
}

func TestCrypto_TheRunBudgetIsEnforced(t *testing.T) {
	c := &Crypto{MaxOpsPerRun: 3}
	ctx := tools.WithRunID(context.Background(), "r_budget")
	for i := 0; i < 3; i++ {
		if _, res := cryptoExec(t, c, ctx, `{"op":"random"}`); res.IsError {
			t.Fatalf("op %d refused: %s", i+1, res.Text)
		}
	}
	if _, res := cryptoExec(t, c, ctx, `{"op":"random"}`); !res.IsError {
		t.Fatal("the op past the budget succeeded")
	}
	other := tools.WithRunID(context.Background(), "r_other")
	if _, res := cryptoExec(t, c, other, `{"op":"random"}`); res.IsError {
		t.Error("another run's budget was spent")
	}
}

// An op name is the model's input: an unknown one is counted under one label,
// so the metric's series stay bounded.
func TestCrypto_AnUnknownOpIsCountedUnderOneLabel(t *testing.T) {
	c := &Crypto{}
	for _, op := range []string{"nope-1", "nope-2", "nope-3"} {
		cryptoExec(t, c, context.Background(), `{"op":"`+op+`"}`)
	}
	st := c.Stats()
	if len(st) != 1 || st["unknown|error"] != 3 {
		t.Errorf("stats = %v, want only unknown|error = 3", st)
	}
}

// Every string argument has a cap, the small ones included.
func TestCrypto_EveryArgumentIsCapped(t *testing.T) {
	c := &Crypto{MaxBytes: 64}
	for name, body := range map[string]string{
		"context":    `{"op":"hash","data":"x","context":"` + strings.Repeat("a", cryptoMaxContextBytes+1) + `"}`,
		"mac":        `{"op":"hash","data":"x","mac":"` + strings.Repeat("a", cryptoMaxMACBytes+1) + `"}`,
		"public_key": `{"op":"hash","data":"x","public_key":"` + strings.Repeat("a", cryptoMaxPublicKeyBytes+1) + `"}`,
		"data":       `{"op":"hash","data":"` + strings.Repeat("a", 64/3*4+1025) + `"}`,
	} {
		if _, res := cryptoExec(t, c, context.Background(), body); !res.IsError || !strings.Contains(res.Text, name) {
			t.Errorf("%s past its cap: %q, want a refusal naming it", name, res.Text)
		}
	}
}

// A key an agent stores under the tenant key's name never stands in for it:
// the tool refuses the name, and CredentialDef refuses to create it.
func TestCrypto_ANarrowerKeyNeverStandsInForTheTenantKey(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "agent", "tenant")
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"signing","key_type":"ed25519"}`)
	mustOK(t, "generate tenant", res)
	if _, res := credExec(t, cd, ctx, `{"op":"generate","scope":"agent","name":"signing","key_type":"ed25519"}`); !res.IsError {
		t.Fatal("an agent-scope key under the tenant key's name was created")
	}
	// Planted past the tool, as a key made before the check would be.
	if _, err := cd.Engine.PutCryptoKey(ctx, credential.Identity{TenantID: "tnt", Scope: "agent", ScopeID: "agent-1", Name: "signing"},
		"ed25519", make([]byte, 32), nil); err != nil {
		t.Fatal(err)
	}
	if _, res := cryptoExec(t, c, ctx, `{"op":"sign","key":"signing","data":"x"}`); !res.IsError || !strings.Contains(res.Text, "more than one scope") {
		t.Errorf("sign with a name held twice = %q, want the ambiguity refusal", res.Text)
	}
}

func TestCrypto_ARunCallWithoutARunIDStillSpendsABudget(t *testing.T) {
	c := &Crypto{MaxOpsPerRun: 2}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_1"})
	for i := 0; i < 2; i++ {
		if _, res := cryptoExec(t, c, ctx, `{"op":"random"}`); res.IsError {
			t.Fatalf("op %d: %s", i+1, res.Text)
		}
	}
	if _, res := cryptoExec(t, c, ctx, `{"op":"random"}`); !res.IsError {
		t.Error("a run's call without a run id skipped the budget")
	}
}

// No result of any op carries the key material, in any encoding.
func TestCrypto_KeyMaterialNeverAppearsInAResult(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	for _, kt := range []string{"aes-256-gcm", "hmac-sha256", "ed25519", "totp"} {
		name := "leak-" + strings.ReplaceAll(kt, "-", "")
		_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"`+name+`","key_type":"`+kt+`"}`)
		mustOK(t, "generate", res)
		k, found, err := cd.Engine.ResolveCryptoKey(ctx, "tnt", "agent-1", "u1", name, credential.Scopes{Tenant: true})
		if err != nil || !found {
			t.Fatalf("resolve %s: %v %v", name, found, err)
		}
		m := k.Versions[k.Current]
		forms := []string{hex.EncodeToString(m), base64.StdEncoding.EncodeToString(m), base64.RawURLEncoding.EncodeToString(m)}
		var results []string
		for _, body := range []string{
			`{"op":"encrypt","key":"` + name + `","plaintext":"x"}`,
			`{"op":"mac","key":"` + name + `","data":"x"}`,
			`{"op":"sign","key":"` + name + `","data":"x"}`,
			`{"op":"public_key","key":"` + name + `"}`,
			`{"op":"totp","key":"` + name + `"}`,
		} {
			_, res := cryptoExec(t, c, ctx, body)
			results = append(results, res.Text)
		}
		_, res = credExec(t, cd, ctx, `{"op":"get","scope":"tenant","name":"`+name+`"}`)
		results = append(results, res.Text)
		for _, r := range results {
			for _, f := range forms {
				if strings.Contains(r, f) {
					t.Errorf("%s: a result carries the key material: %s", kt, r)
				}
			}
		}
	}
}
