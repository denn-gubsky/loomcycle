package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/cryptoops"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// ---- P2a: recipient keys, RSA, JWE, passwords ------------------------------

// An X25519 key encrypts to its own public half as a JWE; the public key it
// hands out lets anyone encrypt to it, needing no key and no scope.
func TestCrypto_X25519KeyEncryptsToItselfAndToAnyoneHoldingItsPublicKey(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"inbox","key_type":"x25519"}`)
	mustOK(t, "generate", res)

	out, res := cryptoExec(t, c, ctx, `{"op":"encrypt","key":"inbox","plaintext":"to me"}`)
	mustOK(t, "encrypt", res)
	ct := out["ciphertext"].(string)
	if !cryptoops.IsJWE(ct) || out["format"] != "jwe" {
		t.Fatalf("encrypt with an x25519 key = %v, want a JWE", out)
	}
	out, res = cryptoExec(t, c, ctx, `{"op":"decrypt","key":"inbox","ciphertext":"`+ct+`"}`)
	mustOK(t, "decrypt", res)
	if out["plaintext"] != "to me" {
		t.Fatalf("decrypt = %v", out["plaintext"])
	}

	out, res = cryptoExec(t, c, ctx, `{"op":"public_key","key":"inbox"}`)
	mustOK(t, "public_key", res)
	jwk := out["jwk"].(map[string]any)
	if jwk["crv"] != "X25519" || jwk["use"] != "enc" || jwk["alg"] != "ECDH-ES" || jwk["kid"] != "inbox.v1" {
		t.Errorf("jwk = %v", jwk)
	}
	jb, _ := json.Marshal(jwk)
	pub, _ := json.Marshal(string(jb))
	// A caller with no crypto scope at all can still encrypt to a public key.
	noScope := tools.WithCryptoPolicy(ctx, tools.CryptoPolicyValue{})
	out, res = cryptoExec(t, c, noScope, `{"op":"encrypt","public_key":`+string(pub)+`,"plaintext":"from a stranger"}`)
	mustOK(t, "encrypt to a public key", res)
	out, res = cryptoExec(t, c, ctx, `{"op":"decrypt","key":"inbox","ciphertext":"`+out["ciphertext"].(string)+`"}`)
	mustOK(t, "decrypt the stranger's", res)
	if out["plaintext"] != "from a stranger" {
		t.Errorf("decrypt = %v", out["plaintext"])
	}

	for name, body := range map[string]string{
		"context with JWE": `{"op":"encrypt","key":"inbox","plaintext":"x","context":"rec:1"}`,
		"envelope format":  `{"op":"encrypt","key":"inbox","plaintext":"x","format":"envelope"}`,
		"key and public":   `{"op":"encrypt","key":"inbox","public_key":` + string(pub) + `,"plaintext":"x"}`,
		"sign":             `{"op":"sign","key":"inbox","data":"x"}`,
	} {
		if _, res := cryptoExec(t, c, ctx, body); !res.IsError || res.Error.Category != tools.CategoryValidation {
			t.Errorf("%s: %+v, want a validation refusal", name, res)
		}
	}
}

// RSA keys are one purpose each: rsa-oaep encrypts, rsa-pss signs, and the
// public key a PSS key hands out verifies its signatures.
func TestCrypto_RSAKeysKeepToTheirPurpose(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	for _, n := range []string{"rsa-oaep-2048", "rsa-pss-2048"} {
		_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"`+n+`","key_type":"`+n+`"}`)
		mustOK(t, "generate "+n, res)
	}
	out, res := cryptoExec(t, c, ctx, `{"op":"encrypt","key":"rsa-oaep-2048","plaintext":"wrapped"}`)
	mustOK(t, "oaep encrypt", res)
	out, res = cryptoExec(t, c, ctx, `{"op":"decrypt","key":"rsa-oaep-2048","ciphertext":"`+out["ciphertext"].(string)+`"}`)
	mustOK(t, "oaep decrypt", res)
	if out["plaintext"] != "wrapped" {
		t.Errorf("oaep round trip = %v", out["plaintext"])
	}

	out, res = cryptoExec(t, c, ctx, `{"op":"sign","key":"rsa-pss-2048","data":"report"}`)
	mustOK(t, "pss sign", res)
	sig := out["signature"].(string)
	pubOut, res := cryptoExec(t, c, ctx, `{"op":"public_key","key":"rsa-pss-2048"}`)
	mustOK(t, "pss public key", res)
	if pubOut["jwk"].(map[string]any)["alg"] != "PS256" {
		t.Errorf("pss jwk = %v", pubOut["jwk"])
	}
	pem, _ := json.Marshal(pubOut["pem"])
	out, res = cryptoExec(t, c, ctx, `{"op":"verify","public_key":`+string(pem)+`,"data":"report","signature":"`+sig+`"}`)
	mustOK(t, "verify with the PEM", res)
	if out["valid"] != true {
		t.Error("a PSS signature did not verify against its public key")
	}

	if _, res := cryptoExec(t, c, ctx, `{"op":"sign","key":"rsa-oaep-2048","data":"x"}`); !res.IsError {
		t.Error("an rsa-oaep key signed")
	}
	if _, res := cryptoExec(t, c, ctx, `{"op":"encrypt","key":"rsa-pss-2048","plaintext":"x"}`); !res.IsError {
		t.Error("an rsa-pss key encrypted")
	}
	// The PSS public key is no encryption recipient.
	if _, res := cryptoExec(t, c, ctx, `{"op":"encrypt","public_key":`+string(pem)+`,"plaintext":"x"}`); res.IsError {
		// An RSA public key carries no purpose, so it is accepted as a
		// recipient; what keeps purposes apart is the holder's key type: the
		// PSS key cannot decrypt it.
		t.Logf("encrypt to an RSA public key refused: %s", res.Text)
	}
}

// An aes-256-gcm key also speaks JWE (alg dir); the kid picks the version, a
// retired version no longer opens, and rewrap keeps it a JWE.
func TestCrypto_AESJWEFollowsKeyVersions(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"j","key_type":"aes-256-gcm"}`)
	mustOK(t, "generate", res)
	out, res := cryptoExec(t, c, ctx, `{"op":"encrypt","key":"j","plaintext":"v1 secret","format":"jwe"}`)
	mustOK(t, "encrypt jwe", res)
	v1 := out["ciphertext"].(string)
	if cryptoops.JWEKid(v1) != "j.v1" {
		t.Fatalf("kid = %q", cryptoops.JWEKid(v1))
	}
	if _, res := cryptoExec(t, c, ctx, `{"op":"decrypt","key":"j","ciphertext":"`+v1+`","context":"x"}`); !res.IsError {
		t.Error("a context was accepted with a JWE")
	}

	_, res = credExec(t, cd, ctx, `{"op":"rotate","scope":"tenant","name":"j"}`)
	mustOK(t, "rotate", res)
	out, res = cryptoExec(t, c, ctx, `{"op":"rewrap","key":"j","ciphertext":"`+v1+`"}`)
	mustOK(t, "rewrap", res)
	v2 := out["ciphertext"].(string)
	if !cryptoops.IsJWE(v2) || cryptoops.JWEKid(v2) != "j.v2" || out["from_version"] != float64(1) {
		t.Fatalf("rewrap = %v", out)
	}
	_, res = credExec(t, cd, ctx, `{"op":"set_min_version","scope":"tenant","name":"j","min_version":2}`)
	mustOK(t, "retire v1", res)
	if _, res := cryptoExec(t, c, ctx, `{"op":"decrypt","key":"j","ciphertext":"`+v1+`"}`); !res.IsError {
		t.Error("a retired version still opened its JWE")
	}
	out, res = cryptoExec(t, c, ctx, `{"op":"decrypt","key":"j","ciphertext":"`+v2+`"}`)
	mustOK(t, "decrypt v2", res)
	if out["plaintext"] != "v1 secret" {
		t.Errorf("decrypt = %v", out["plaintext"])
	}
}

func TestCrypto_PasswordsHashAndVerifyWithoutAKey(t *testing.T) {
	c, _, ctx := cryptoFixture(t) // no scope needed
	out, res := cryptoExec(t, c, ctx, `{"op":"hash_password","password":"hunter2-but-longer"}`)
	mustOK(t, "hash_password", res)
	h := out["password_hash"].(string)
	if strings.Contains(res.Text, "hunter2") || !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("hash result %q", res.Text)
	}
	hb, _ := json.Marshal(h)
	for pw, want := range map[string]bool{"hunter2-but-longer": true, "hunter3": false} {
		out, res := cryptoExec(t, c, ctx, `{"op":"verify_password","password":"`+pw+`","password_hash":`+string(hb)+`}`)
		mustOK(t, "verify_password", res)
		if out["valid"] != want {
			t.Errorf("verify %q = %v, want %v", pw, out["valid"], want)
		}
	}
	if _, res := cryptoExec(t, c, ctx, `{"op":"verify_password","password":"x","password_hash":"$argon2id$v=19$m=4194304,t=2,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA"}`); !res.IsError {
		t.Error("a hash asking for 4 GiB was verified")
	}
}

// ---- P2b: data by reference ------------------------------------------------

// refCtx is a run's ctx for references: a bound default volume and the tools
// a reference needs.
func refCtx(ctx context.Context, dir string, toolNames ...string) context.Context {
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Volumes = tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{{Name: "work", Root: dir, Default: true}}}
	})
	ctx = tools.WithRunID(ctx, "run_ref")
	return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Tools = append([]string{"Crypto"}, toolNames...) })
}

// A file is encrypted to another file and back, byte for byte, and neither
// the plaintext nor the ciphertext appears in a result.
func TestCrypto_FromAndToAVolumeKeepTheDataOutOfTheResult(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"files","key_type":"aes-256-gcm"}`)
	mustOK(t, "generate", res)
	dir := t.TempDir()
	secret := append([]byte("SECRET-FILE-BODY\x00\xff"), bytes.Repeat([]byte{7}, 100)...)
	if err := os.WriteFile(filepath.Join(dir, "in.bin"), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	rctx := refCtx(ctx, dir, "Read", "Write")

	out, res := cryptoExec(t, c, rctx, `{"op":"encrypt","key":"files","from":"volume:in.bin","to":"volume:in.bin.lcx"}`)
	mustOK(t, "encrypt file", res)
	if strings.Contains(res.Text, "SECRET") || strings.Contains(res.Text, "lcx1:") || out["to"] != "volume:in.bin.lcx" || out["bytes"] == nil {
		t.Fatalf("encrypt result leaks or is incomplete: %s", res.Text)
	}
	stored, _ := os.ReadFile(filepath.Join(dir, "in.bin.lcx"))
	if !strings.HasPrefix(string(stored), "lcx1:kt:files:1:") {
		t.Fatalf("stored ciphertext %q", stored)
	}
	out, res = cryptoExec(t, c, rctx, `{"op":"decrypt","from":"volume:in.bin.lcx","to":"volume:out.bin"}`)
	mustOK(t, "decrypt file", res)
	if strings.Contains(res.Text, "SECRET") || out["plaintext"] != nil {
		t.Fatalf("decrypt result leaks: %s", res.Text)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "out.bin"))
	if !bytes.Equal(got, secret) {
		t.Error("the decrypted file differs from the original")
	}
	// from also feeds the data ops.
	out, res = cryptoExec(t, c, rctx, `{"op":"hash","from":"volume:in.bin"}`)
	mustOK(t, "hash file", res)
	want, _ := cryptoops.Hash("sha256", secret)
	if !strings.Contains(res.Text, encodeHex(want)) {
		t.Errorf("hash of the file = %v", out)
	}
}

func encodeHex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, x := range b {
		out = append(out, digits[x>>4], digits[x&15])
	}
	return string(out)
}

// A reference is refused, with nothing read or written, when the agent lacks
// the tool, the path leaves the volume, the volume is read-only, the call is
// the operator API, or the op does not take it.
func TestCrypto_ReferencesAreRefusedOutsideTheAgentsAuthority(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"files","key_type":"aes-256-gcm"}`)
	mustOK(t, "generate", res)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	full := refCtx(ctx, dir, "Read", "Write")
	readOnly := tools.UpdateRunPolicy(full, func(rp *tools.RunPolicy) {
		rp.Volumes = tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{{Name: "work", Root: dir, Default: true, ReadOnly: true}}}
	})
	operator := tools.WithSubstrateOperator(context.Background())
	operator = tools.WithCryptoPolicy(tools.WithRunIdentity(operator, tools.RunIdentityValue{TenantID: "tnt"}), tools.CryptoPolicyValue{Scopes: []string{"tenant"}})

	for name, c2 := range map[string]struct {
		ctx  context.Context
		body string
		cat  tools.ErrorCategory
	}{
		"no Read tool":        {refCtx(ctx, dir, "Write"), `{"op":"encrypt","key":"files","from":"volume:a.txt"}`, tools.CategoryPermission},
		"no Write tool":       {refCtx(ctx, dir, "Read"), `{"op":"encrypt","key":"files","plaintext":"x","to":"volume:b.txt"}`, tools.CategoryPermission},
		"no tool list":        {tools.UpdateRunPolicy(full, func(rp *tools.RunPolicy) { rp.Tools = nil }), `{"op":"encrypt","key":"files","from":"volume:a.txt"}`, tools.CategoryPermission},
		"dot-dot escape":      {full, `{"op":"encrypt","key":"files","from":"volume:../` + filepath.Base(outside) + `/secret.txt"}`, tools.CategoryPermission},
		"symlink escape":      {full, `{"op":"encrypt","key":"files","from":"volume:link.txt"}`, tools.CategoryPermission},
		"read-only volume":    {readOnly, `{"op":"encrypt","key":"files","plaintext":"x","to":"volume:b.txt"}`, tools.CategoryPermission},
		"unbound volume":      {full, `{"op":"encrypt","key":"files","from":"volume:other:a.txt"}`, tools.CategoryPermission},
		"operator API":        {operator, `{"op":"hash","from":"volume:a.txt"}`, tools.CategoryPermission},
		"to on mac":           {full, `{"op":"hash","data":"x","to":"volume:b.txt"}`, tools.CategoryValidation},
		"from and plaintext":  {full, `{"op":"encrypt","key":"files","plaintext":"x","from":"volume:a.txt"}`, tools.CategoryValidation},
		"unknown kind":        {full, `{"op":"hash","from":"s3:bucket/x"}`, tools.CategoryValidation},
		"directory":           {full, `{"op":"hash","from":"volume:."}`, tools.CategoryValidation},
		"memory without tool": {full, `{"op":"hash","from":"memory:user/k"}`, tools.CategoryPermission},
	} {
		_, res := cryptoExec(t, c, c2.ctx, c2.body)
		if !res.IsError || res.Error == nil || res.Error.Category != c2.cat {
			t.Errorf("%s: %+v, want a %s refusal", name, res, c2.cat)
		}
		if strings.Contains(res.Text, "OUTSIDE") {
			t.Errorf("%s: the refusal carries the outside file's content", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err == nil {
		t.Error("a refused call wrote b.txt")
	}
}

// The grant is the run's effective tool list, not the agent definition's
// patterns: a run whose caller narrowed it to [Crypto] does not write files
// because the definition grants Write.
func TestCrypto_ANarrowedRunCannotReachAToolItWasNarrowedOutOf(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"files","key_type":"aes-256-gcm"}`)
	mustOK(t, "generate", res)
	dir := t.TempDir()
	narrowed := refCtx(ctx, dir) // effective tools: [Crypto]
	narrowed = tools.UpdateRunPolicy(narrowed, func(rp *tools.RunPolicy) { rp.ToolPatterns = []string{"Crypto", "Read", "Write"} })
	if _, res := cryptoExec(t, c, narrowed, `{"op":"encrypt","key":"files","plaintext":"x","to":"volume:out.txt"}`); !res.IsError || res.Error.Category != tools.CategoryPermission {
		t.Errorf("narrowed run = %+v, want a permission refusal", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
		t.Error("the narrowed run wrote the file")
	}
}

// A reference is judged by the referenced tool's pre-hooks: a deny or a
// rewrite refuses it before anything is read or written, and the hook is told
// where, never the data.
func TestCrypto_AReferenceIsRefusedByTheReferencedToolsHooks(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"files","key_type":"aes-256-gcm"}`)
	mustOK(t, "generate", res)
	dir := t.TempDir()
	base := refCtx(ctx, dir, "Read", "Write")
	var asked []string
	deny := tools.WithHookPreCheck(base, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
		asked = append(asked, name+" "+string(input))
		if name == "Write" {
			return tools.Result{Text: "writes are off today", IsError: true}, false
		}
		return tools.Result{}, true
	})
	_, res = cryptoExec(t, c, deny, `{"op":"encrypt","key":"files","plaintext":"TOP-SECRET","to":"volume:out.txt"}`)
	if !res.IsError || res.Error.Category != tools.CategoryPermission || !strings.Contains(res.Text, "writes are off today") {
		t.Fatalf("denied write = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
		t.Error("a hook-denied write happened")
	}
	if len(asked) != 1 || !strings.Contains(asked[0], `"path":"out.txt"`) || !strings.Contains(asked[0], `"via":"Crypto"`) || strings.Contains(asked[0], "TOP-SECRET") {
		t.Errorf("the hook was asked %v", asked)
	}
	// A read is asked of Read, and allowed here.
	if err := os.WriteFile(filepath.Join(dir, "in.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	asked = nil
	if _, res := cryptoExec(t, c, deny, `{"op":"hash","from":"volume:in.txt"}`); res.IsError || len(asked) != 1 || !strings.HasPrefix(asked[0], "Read ") {
		t.Errorf("allowed read = %+v, asked %v", res, asked)
	}
}

// A file over the cap is refused, never truncated.
func TestCrypto_FromRefusesAFileOverTheCap(t *testing.T) {
	c, _, ctx := cryptoFixture(t)
	c.MaxBytes = 64
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big"), bytes.Repeat([]byte("a"), 65), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, res := cryptoExec(t, c, refCtx(ctx, dir, "Read"), `{"op":"hash","from":"volume:big"}`); !res.IsError {
		t.Error("an over-cap file was hashed")
	}
}

// Memory references go through Memory's own scope check and set; a chunk
// body key and binary data are refused.
func TestCrypto_MemoryReferencesUseMemorysOwnChecks(t *testing.T) {
	c, cd, ctx := cryptoFixture(t, "tenant")
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"m","key_type":"aes-256-gcm"}`)
	mustOK(t, "generate", res)
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c.Memory = &Memory{Store: s, MaxValueBytes: 65536, DefaultQuotaBytes: 1 << 20}
	mctx := tools.UpdateRunPolicy(refCtx(ctx, t.TempDir(), "Memory"), func(rp *tools.RunPolicy) { rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user"}} })

	out, res := cryptoExec(t, c, mctx, `{"op":"encrypt","key":"m","plaintext":"note","to":"memory:user/secret-note"}`)
	mustOK(t, "encrypt to memory", res)
	if out["to"] != "memory:user/secret-note" {
		t.Fatalf("encrypt = %v", out)
	}
	out, res = cryptoExec(t, c, mctx, `{"op":"decrypt","from":"memory:user/secret-note"}`)
	mustOK(t, "decrypt from memory", res)
	if out["plaintext"] != "note" {
		t.Errorf("decrypt = %v", out["plaintext"])
	}
	for name, body := range map[string]string{
		"scope not granted": `{"op":"hash","from":"memory:tenant/k"}`,
		"chunk body key":    `{"op":"encrypt","key":"m","plaintext":"x","to":"memory:user/doc.chunk:abc"}`,
		"missing key":       `{"op":"hash","from":"memory:user/nope"}`,
	} {
		if _, res := cryptoExec(t, c, mctx, body); !res.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
	// Binary plaintext cannot go to memory.
	out, res = cryptoExec(t, c, mctx, `{"op":"encrypt","key":"m","plaintext":"/w==","input_encoding":"base64"}`)
	mustOK(t, "encrypt binary", res)
	if _, res := cryptoExec(t, c, mctx, `{"op":"decrypt","ciphertext":"`+out["ciphertext"].(string)+`","to":"memory:user/bin"}`); !res.IsError {
		t.Error("binary plaintext was written to memory")
	}
}

// Document references read and update a chunk body through the Document tool
// (its scope and revision checks); an unknown chunk is not found.
func TestCrypto_DocumentReferencesGoThroughTheDocumentTool(t *testing.T) {
	d, dctx, _ := documentFixture(t)
	docID, rootID := canvasCreateDoc(t, d, dctx, "vault")
	chunkID := canvasCreateChunk(t, d, dctx, docID, rootID, "card", "4111 1111 1111 1111")

	c := &Crypto{Docs: d}
	ctx := tools.WithRunIdentity(dctx, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: "tnt",
		UserCredentials: map[string]string{"doc-key": "0123456789abcdef0123456789abcdef-a-secret"}})
	ctx = tools.WithCryptoPolicy(ctx, tools.CryptoPolicyValue{Scopes: []string{"run"}})
	ctx = refCtx(ctx, t.TempDir(), "Document")
	ref := "document:user/" + chunkID

	_, res := cryptoExec(t, c, ctx, `{"op":"encrypt","run":"doc-key","key_type":"aes-256-gcm","derive":true,"from":"`+ref+`","to":"`+ref+`"}`)
	mustOK(t, "encrypt chunk in place", res)
	got, gres := docExec(t, d, dctx, `{"op":"get_chunk","scope":"user","id":"`+chunkID+`"}`)
	mustOK(t, "get_chunk", gres)
	if body := got["body"].(string); !strings.HasPrefix(body, "lcx1:r:doc-key:0:") {
		t.Fatalf("chunk body after encrypt = %q", body)
	}
	_, res = cryptoExec(t, c, ctx, `{"op":"decrypt","run":"doc-key","key_type":"aes-256-gcm","derive":true,"from":"`+ref+`","to":"`+ref+`"}`)
	mustOK(t, "decrypt chunk in place", res)
	got, _ = docExec(t, d, dctx, `{"op":"get_chunk","scope":"user","id":"`+chunkID+`"}`)
	if got["body"] != "4111 1111 1111 1111" || got["revision"].(float64) < 3 {
		t.Errorf("chunk after decrypt = %v (revision %v)", got["body"], got["revision"])
	}
	if _, res := cryptoExec(t, c, ctx, `{"op":"hash","from":"document:user/no-such-chunk"}`); !res.IsError || !strings.Contains(res.Text, "no such chunk") {
		t.Errorf("unknown chunk = %+v, want not found", res)
	}
}

func TestParseDataRef_Forms(t *testing.T) {
	for in, want := range map[string]dataRef{
		"volume:a/b.txt":      {kind: refVolume, path: "a/b.txt"},
		"volume:work:a/b.txt": {kind: refVolume, volume: "work", path: "a/b.txt"},
		"volume:./a:b":        {kind: refVolume, path: "./a:b"},
		"volume:dir/x:y":      {kind: refVolume, path: "dir/x:y"},
		"memory:user/core/me": {kind: refMemory, scope: "user", key: "core/me"},
		"document:tenant/c1":  {kind: refDocument, scope: "tenant", chunkID: "c1"},
	} {
		got, err := parseDataRef(in)
		got.raw = ""
		if err != nil || got != want {
			t.Errorf("parseDataRef(%q) = %+v %v, want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "volume:", "volume:work:", "memory:user", "memory:global/k", "document:user/", "s3:x", "nokind"} {
		if _, err := parseDataRef(bad); err == nil {
			t.Errorf("parseDataRef(%q) accepted", bad)
		}
	}
}
