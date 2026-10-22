package builtin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cryptoops"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"golang.org/x/crypto/ssh"
)

// A commit or tag the Git tool makes can be signed with a key the runtime
// holds (RFC FB D7): an Ed25519 crypto key, used through the Crypto tool's
// own key resolution, op budget and audit, so an agent signs only with keys
// its crypto_scopes reach and never sees the key. The signature is git's SSH
// format (gpg.format=ssh): `git verify-commit` checks it against an
// allowed-signers file listing the key's public half, which the Git tool's
// result returns as an authorized_keys line for the operator to list.

// sshSigNamespace is git's namespace for commit and tag signatures.
const sshSigNamespace = "git"

// sshSignature returns the armored SSH signature ("-----BEGIN SSH
// SIGNATURE-----") of message under priv, as ssh-keygen -Y sign makes it
// with namespace git and SHA-512 (PROTOCOL.sshsig).
func sshSignature(priv ed25519.PrivateKey, message []byte) ([]byte, error) {
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		return nil, err
	}
	h := sha512.Sum512(message)
	signed := ssh.Marshal(struct {
		Magic     [6]byte
		Namespace string
		Reserved  string
		HashAlg   string
		Hash      string
	}{sshSigMagic(), sshSigNamespace, "", "sha512", string(h[:])})
	// The magic preamble is raw bytes, not an SSH string: ssh.Marshal of a
	// fixed array writes it as is.
	sig := ssh.Marshal(ssh.Signature{Format: ssh.KeyAlgoED25519, Blob: ed25519.Sign(priv, signed)})
	blob := ssh.Marshal(struct {
		Magic     [6]byte
		Version   uint32
		PublicKey string
		Namespace string
		Reserved  string
		HashAlg   string
		Signature string
	}{sshSigMagic(), 1, string(pub.Marshal()), sshSigNamespace, "", "sha512", string(sig)})
	var b bytes.Buffer
	b.WriteString("-----BEGIN SSH SIGNATURE-----\n")
	enc := base64.StdEncoding.EncodeToString(blob)
	for len(enc) > 70 {
		b.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	b.WriteString(enc + "\n-----END SSH SIGNATURE-----\n")
	return b.Bytes(), nil
}

func sshSigMagic() [6]byte { return [6]byte{'S', 'S', 'H', 'S', 'I', 'G'} }

// errGitSignKey is a sign request the Crypto tool refused; its text is the
// Crypto tool's own message, safe to show.
var errGitSignKey = errors.New("sign_key_refused")

// signForGit signs message with the run's crypto key name (scope optional,
// as key_scope pins it), as one Crypto op: it spends the run's Crypto budget
// and is audited as crypto_op "git_sign". It returns the armored signature
// and the key's public half as an authorized_keys line.
func (c *Crypto) signForGit(ctx context.Context, name, scope string, message []byte) (armored []byte, authorized string, err error) {
	if res, ok := c.spend(ctx); !ok {
		return nil, "", fmt.Errorf("%w: %s", errGitSignKey, res.Text)
	}
	ident := tools.RunIdentity(ctx)
	ev := c.gitSignEvent(ctx, ident)
	failed := true
	defer func() {
		c.stats.count("sign", failed)
		c.record(ctx, ev, failed)
	}()
	ref, res, ok := parseKeyRef(keyRef{Name: name, Scope: scope}, nil)
	if !ok {
		return nil, "", fmt.Errorf("%w: %s", errGitSignKey, res.Text)
	}
	ev.TargetName = refName(ref)
	key, res, ok := c.resolveKey(ctx, ref, nil)
	if !ok {
		return nil, "", fmt.Errorf("%w: %s", errGitSignKey, res.Text)
	}
	ev.CryptoSource, ev.CryptoScope = sourceWord(key.source), key.scope
	ev.CryptoKeyType, ev.CryptoVersion = string(key.keyType), key.current
	if key.keyType != cryptoops.Ed25519 || key.ext != nil {
		return nil, "", fmt.Errorf("%w: git signing needs an ed25519 key the runtime holds (this one is %s)", errGitSignKey, key.keyType)
	}
	m, ok := key.material(key.current)
	if !ok {
		return nil, "", fmt.Errorf("%w: the key has no usable version", errGitSignKey)
	}
	priv := ed25519.NewKeyFromSeed(m)
	armored, err = sshSignature(priv, message)
	if err != nil {
		return nil, "", fmt.Errorf("%w: sign failed", errGitSignKey)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		return nil, "", fmt.Errorf("%w: sign failed", errGitSignKey)
	}
	failed = false
	return armored, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), nil
}

// gitSignEvent is the audit record of one signature, as execKeyed starts
// one for a Crypto call.
func (c *Crypto) gitSignEvent(ctx context.Context, ident tools.RunIdentityValue) *audit.Event {
	ev := &audit.Event{
		Action:       "crypto_op",
		ActorTenant:  ident.TenantID,
		ActorSubject: ident.UserID,
		TargetTenant: ident.TenantID,
		CryptoOp:     "git_sign",
		CryptoRunID:  tools.RunID(ctx),
		CryptoAgent:  tools.AgentName(ctx),
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		ev.ActorTenant, ev.ActorSubject, ev.ActorTokenSuffix = p.TenantID, p.Subject, p.TokenSuffix
	}
	return ev
}

// gitSigner is go-git's Signer over signForGit, for one commit or tag. The
// authorized key of the last signature is kept for the tool's result.
type gitSigner struct {
	ctx         context.Context
	crypto      *Crypto
	name, scope string
	authorized  string
}

func (s *gitSigner) Sign(message io.Reader) ([]byte, error) {
	msg, err := io.ReadAll(io.LimitReader(message, 64<<20))
	if err != nil {
		return nil, err
	}
	sig, pub, err := s.crypto.signForGit(s.ctx, s.name, s.scope, msg)
	if err != nil {
		return nil, err
	}
	s.authorized = pub
	return sig, nil
}
