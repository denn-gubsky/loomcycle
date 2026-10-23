//go:build unix

package builtin

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// sshVerify runs OpenSSH's own verifier on sig over msg, as git does for
// gpg.format=ssh.
func sshVerify(t *testing.T, pub ssh.PublicKey, msg, sig []byte) error {
	t.Helper()
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("ssh-keygen is needed: %v", err)
		}
		t.Skipf("SKIPPED, NOT PASSED: ssh-keygen not installed: %v", err)
	}
	dir := t.TempDir()
	allowed := filepath.Join(dir, "allowed_signers")
	if err := os.WriteFile(allowed, []byte("signer@test "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sigPath := filepath.Join(dir, "sig")
	if err := os.WriteFile(sigPath, sig, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(keygen, "-Y", "verify", "-f", allowed, "-I", "signer@test", "-n", "git", "-s", sigPath)
	cmd.Stdin = bytes.NewReader(msg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &exec.ExitError{Stderr: out}
	}
	return nil
}

// The signature is what ssh-keygen -Y sign makes, so OpenSSH (and so git
// with gpg.format=ssh) accepts it, and rejects it for any other message.
func TestSSHSignature_VerifiesWithOpenSSHAndOnlyForItsMessage(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor a <a@x> 1 +0000\n\nsigned\n")
	sig, err := sshSignature(priv, msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(sig, []byte("-----BEGIN SSH SIGNATURE-----\n")) {
		t.Fatalf("not an armored SSH signature: %q", sig)
	}
	if err := sshVerify(t, pub, msg, sig); err != nil {
		t.Fatalf("OpenSSH rejected the signature: %s", err.(*exec.ExitError).Stderr)
	}
	if err := sshVerify(t, pub, append(msg, 'x'), sig); err == nil {
		t.Error("OpenSSH accepted the signature for a changed message")
	}
}
