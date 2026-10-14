package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Backend identifiers. inline stores the secret sealed in the DB; the others
// store only a POINTER (addr/path) and fetch at bind time from an external
// secret manager. v1 implements inline; the externals are locked-interface
// stubs (RFC AR Phase 4).
const (
	BackendInline            = "inline"
	BackendVault             = "vault"
	BackendAWSSecretsManager = "aws_sm"
	BackendGCPSecretManager  = "gcp_sm"
	BackendOnePassword       = "onepassword"
)

// ErrUnsupportedBackend is returned when a credential names an external backend
// this build doesn't implement yet.
var ErrUnsupportedBackend = errors.New("credential: external backend not supported in this build (use backend=inline)")

// KnownBackend reports whether name is a recognised backend id.
func KnownBackend(name string) bool {
	switch name {
	case BackendInline, BackendVault, BackendAWSSecretsManager, BackendGCPSecretManager, BackendOnePassword:
		return true
	}
	return false
}

// Backend turns a stored credential definition into its plaintext secret value
// at bind time. The interface is locked in v1; only inline is implemented.
type Backend interface {
	Name() string
	// Resolve returns the plaintext secret for a stored definition. For inline,
	// id binds the ciphertext to its row (GCM AAD); external backends ignore it.
	Resolve(ctx context.Context, def json.RawMessage, id Identity) (string, error)
}

// inlineDefinition is the stored `definition` shape for backend=inline: the
// sealed envelope, no plaintext.
type inlineDefinition struct {
	Value Sealed `json:"value"`
	// AllowedURLs restricts where the secret may be sent (see urls.go). nil =
	// no restriction; present and empty = nowhere. It is stored in the clear,
	// so a listing can show it without opening the secret, and it is part of
	// what the value is sealed with (Identity.Bound): a row whose list was
	// edited, added or removed no longer decrypts.
	AllowedURLs *[]string `json:"allowed_urls,omitempty"`
	// Username is the user name sent with the value where a destination
	// takes one (HTTP basic auth for a git source). Optional. Stored in the
	// clear and NOT part of what the value is sealed with: it is not secret,
	// and changing it cannot send the value anywhere new.
	Username string `json:"username,omitempty"`
}

// usernameOf reads the user name out of a stored definition ("" for none).
func usernameOf(def json.RawMessage) string {
	var d inlineDefinition
	if err := json.Unmarshal(def, &d); err != nil {
		return ""
	}
	return d.Username
}

// restrictionOf reads the URL restriction out of a stored definition without
// opening the secret. What it returns is NOT yet authenticated; only a
// successful Resolve proves the list is the one the secret was stored with.
func restrictionOf(def json.RawMessage) *[]string {
	var d inlineDefinition
	if err := json.Unmarshal(def, &d); err != nil {
		return nil
	}
	return d.AllowedURLs
}

// inlineBackend decrypts sealed values with the deployment Sealer.
type inlineBackend struct{ sealer *Sealer }

func (b inlineBackend) Name() string { return BackendInline }

func (b inlineBackend) Resolve(_ context.Context, def json.RawMessage, id Identity) (string, error) {
	var d inlineDefinition
	if err := json.Unmarshal(def, &d); err != nil {
		return "", fmt.Errorf("credential: malformed inline definition: %w", err)
	}
	// The restriction stored beside the value is part of what it was sealed
	// with, so this opens only when that list is untouched.
	id.Bound = boundURLs(d.AllowedURLs)
	pt, err := b.sealer.Open(d.Value, id)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// sealInline builds the stored definition for a plaintext secret, its URL
// restriction (nil for none) and its user name ("" for none).
func (b inlineBackend) sealInline(plaintext string, id Identity, allowedURLs *[]string, username string) (json.RawMessage, error) {
	// A pointer to a nil slice would be written as JSON null and read back
	// as "no restriction", while the value was sealed as "sent nowhere": a
	// credential that can never be opened. Store an empty list as one.
	if allowedURLs != nil && *allowedURLs == nil {
		allowedURLs = &[]string{}
	}
	id.Bound = boundURLs(allowedURLs)
	sealed, err := b.sealer.Seal([]byte(plaintext), id)
	if err != nil {
		return nil, err
	}
	return json.Marshal(inlineDefinition{Value: sealed, AllowedURLs: allowedURLs, Username: username})
}

// externalStub is a placeholder for the Phase-4 external backends: the pointer
// definition can be stored, but resolution isn't wired yet.
type externalStub struct{ name string }

func (b externalStub) Name() string { return b.name }

func (b externalStub) Resolve(context.Context, json.RawMessage, Identity) (string, error) {
	return "", fmt.Errorf("%w: %q", ErrUnsupportedBackend, b.name)
}
