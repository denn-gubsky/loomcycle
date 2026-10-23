//go:build unix

package builtin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// signCtx is cryptoFixture's run, bound to one read-write volume at root.
func signCtx(t *testing.T, root string, scopes ...string) (*Crypto, *CredentialDef, context.Context) {
	t.Helper()
	c, cd, ctx := cryptoFixture(t, scopes...)
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Volumes = tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{{Name: "default", Root: root, Default: true}}}
	})
	return c, cd, ctx
}

// gitVerify runs real git's verifier on rev with an allowed-signers file
// listing the key the tool reported.
func gitVerify(t *testing.T, repo, what, rev, signedBy string) error {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("ssh-keygen is needed: %v", err)
		}
		t.Skipf("SKIPPED, NOT PASSED: ssh-keygen not installed: %v", err)
	}
	allowed := filepath.Join(t.TempDir(), "allowed_signers")
	if err := os.WriteFile(allowed, []byte("signer@test "+signedBy+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-c", "gpg.format=ssh", "-c", "gpg.ssh.allowedSignersFile="+allowed, what, rev)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("git %s %s: %s", what, rev, out)
		return err
	}
	return nil
}

// A commit and an annotated tag signed with a stored ed25519 crypto key are
// verified by real git (gpg.format=ssh) against the public key the tool
// reports; the key itself never appears.
func TestGit_SignedCommitAndTagVerifyWithGit(t *testing.T) {
	root := realDir(t)
	c, cd, ctx := signCtx(t, root, "tenant")
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"commit-key","key_type":"ed25519"}`)
	mustOK(t, "generate", res)
	g := &Git{Crypto: c}

	mustGitOK(t, g, ctx, map[string]any{"op": "init"})
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("signed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitOK(t, g, ctx, map[string]any{"op": "add", "paths": []string{"a.txt"}})
	out := mustGitOK(t, g, ctx, map[string]any{"op": "commit", "message": "signed commit", "sign_key": "commit-key"})
	signedBy, _ := out["signed_by"].(string)
	if !strings.HasPrefix(signedBy, "ssh-ed25519 ") {
		t.Fatalf("signed_by = %q, want an ssh-ed25519 authorized key", signedBy)
	}
	if err := gitVerify(t, root, "verify-commit", "HEAD", signedBy); err != nil {
		t.Fatalf("git verify-commit refused the tool's signature: %v", err)
	}
	tag := mustGitOK(t, g, ctx, map[string]any{"op": "tag_create", "tag": "v1", "message": "release", "sign_key": "commit-key"})
	if tag["signed_by"] != signedBy {
		t.Errorf("tag signed_by %v, want %s", tag["signed_by"], signedBy)
	}
	if err := gitVerify(t, root, "verify-tag", "v1", signedBy); err != nil {
		t.Fatalf("git verify-tag refused the tool's signature: %v", err)
	}
	// A different key's public half does not verify it.
	other := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	if err := gitVerify(t, root, "verify-commit", "HEAD", other); err == nil {
		t.Error("git verified the commit against another key")
	}
}

// Signing uses the run's crypto_scopes: a key in a scope the run may not use
// is refused, and nothing is committed. A key of another type is refused too.
func TestGit_SigningNeedsAReachableEd25519Key(t *testing.T) {
	root := realDir(t)
	c, cd, ctx := signCtx(t, root, "tenant")
	mustOK(t, "generate", func() tools.Result {
		_, r := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"ec","key_type":"ecdsa-p256"}`)
		return r
	}())
	g := &Git{Crypto: c}
	mustGitOK(t, g, ctx, map[string]any{"op": "init"})
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitOK(t, g, ctx, map[string]any{"op": "add", "paths": []string{"a.txt"}})
	wantGitClass(t, g, ctx, map[string]any{"op": "commit", "message": "m", "sign_key": "ec"}, "sign_key_refused")

	// The same run without the tenant scope cannot reach a tenant key.
	noScope := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Crypto = tools.CryptoPolicyValue{} })
	_, res := credExec(t, cd, ctx, `{"op":"generate","scope":"tenant","name":"ed","key_type":"ed25519"}`)
	mustOK(t, "generate", res)
	wantGitClass(t, g, noScope, map[string]any{"op": "commit", "message": "m", "sign_key": "ed"}, "sign_key_refused")
	if h := mustGitOK(t, g, ctx, map[string]any{"op": "status"})["head"].(map[string]any); h["hash"] != "" {
		t.Errorf("a refused signature still committed: head %v", h)
	}
	// A deployment with no Crypto tool cannot sign at all.
	wantGitClass(t, &Git{}, ctx, map[string]any{"op": "commit", "message": "m", "sign_key": "ed"}, "signing_unavailable")
}

// A remote can only be recorded as an https URL; its host is checked when
// it is used, not when it is added.
func TestGit_RemoteAddRecordsOnlyHTTPS(t *testing.T) {
	root := realDir(t)
	g := &Git{}
	ctx := gitCtx(root, false, tools.GitRemotePolicyValue{})
	mustGitOK(t, g, ctx, map[string]any{"op": "init"})
	for _, u := range []string{"file:///etc", "ssh://github.com/acme/app.git", "http://github.com/acme/app.git", "https://x:y@github.com/acme/app.git"} {
		wantGitClass(t, g, ctx, map[string]any{"op": "remote_add", "remote": "up", "url": u}, "invalid_argument")
	}
	mustGitOK(t, g, ctx, map[string]any{"op": "remote_add", "remote": "up", "url": "https://github.com/acme/app.git"})
	rl := mustGitOK(t, g, ctx, map[string]any{"op": "remote_list"})
	if !strings.Contains(rl["remotes"].([]any)[0].(map[string]any)["url"].(string), "github.com/acme/app.git") {
		t.Errorf("remote_list: %v", rl)
	}
}

// A submodule's URL is repository content: .gitmodules can name any host.
// submodule_update checks it like a remote, before any request.
func TestGit_SubmoduleURLIsCheckedAtUse(t *testing.T) {
	f := newGitServer(t, false)
	root := realDir(t)
	sup := filepath.Join(root, "sup")
	if err := os.MkdirAll(sup, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, sup, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(sup, ".gitmodules"), []byte("[submodule \"lib\"]\n\tpath = lib\n\turl = https://evil.example/acme/lib.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, sup, "add", ".gitmodules")
	gitRun(t, sup, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("a", 40)+",lib")
	gitRun(t, sup, "commit", "-q", "-m", "with a submodule")

	g := &Git{Source: f.source(nil)}
	ctx := gitCtx(root, false, tools.GitRemotePolicyValue{Fetch: true})
	subs := mustGitOK(t, g, ctx, map[string]any{"op": "submodule_list", "repo": "sup"})
	if n := len(subs["submodules"].([]any)); n != 1 {
		t.Fatalf("submodule_list: %v", subs)
	}
	before := f.requests()
	wantGitClass(t, g, ctx, map[string]any{"op": "submodule_update", "repo": "sup", "submodule": "lib"}, "host_not_allowed")
	wantGitClass(t, g, ctx, map[string]any{"op": "submodule_update", "repo": "sup", "submodule": "nope"}, "invalid_argument")
	if f.requests() != before {
		t.Error("a refused submodule update reached a server")
	}
}

// A submodule's credential is resolved for the submodule's own URL: one
// restricted to the superproject's repository is refused for a submodule on
// the same host, before any request.
func TestGit_SubmoduleCredentialIsCheckedAgainstTheSubmoduleURL(t *testing.T) {
	f := newGitServer(t, false)
	root := realDir(t)
	sup := filepath.Join(root, "sup")
	if err := os.MkdirAll(sup, 0o755); err != nil {
		t.Fatal(err)
	}
	subURL := f.base + "/acme/other.git"
	gitRun(t, sup, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(sup, ".gitmodules"), []byte("[submodule \"lib\"]\n\tpath = lib\n\turl = "+subURL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, sup, "add", ".gitmodules")
	gitRun(t, sup, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("a", 40)+",lib")
	gitRun(t, sup, "commit", "-q", "-m", "with a submodule")

	var askedFor []string
	// A credential restricted to the superproject's repository only.
	resolve := func(_ context.Context, ref, target string) (string, string, error) {
		askedFor = append(askedFor, target)
		if target != f.repoURL {
			return "", "", ErrGitCredentialURLNotAllowed
		}
		return "x-access-token", gitToolTestToken, nil
	}
	g := &Git{Source: f.source(resolve)}
	ctx := gitCtx(root, false, tools.GitRemotePolicyValue{Fetch: true})
	before := f.requests()
	wantGitClass(t, g, ctx, map[string]any{"op": "submodule_update", "repo": "sup", "submodule": "lib", "credential": "$cred:forge"}, "credential_url_not_allowed")
	if len(askedFor) != 1 || askedFor[0] != subURL {
		t.Errorf("the credential was resolved for %v, want the submodule's URL %s", askedFor, subURL)
	}
	if f.requests() != before {
		t.Error("a refused submodule update reached the server")
	}
}
