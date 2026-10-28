package store

import (
	"context"
	"time"
)

// RFC FA P6 — passkeys (WebAuthn credentials) and the one-time ceremonies
// that register and use them.
//
// A passkey row holds only what a relying party is meant to keep: the
// credential id, its PUBLIC key, the authenticator model (AAGUID), the
// signature counter, the transports and flags. The private key never leaves
// the authenticator, so a full read of this table authenticates nobody.

// UserPasskeyRow is one WebAuthn credential of a user. Passkeys belong to the
// subject, like its password (user_credentials has no gate column either): a
// passkey a person enrolled as a second factor on a password gate is the same
// credential a passkey gate would accept, when it is discoverable.
type UserPasskeyRow struct {
	// CredentialID is the authenticator's credential id, globally unique.
	CredentialID []byte
	TenantID     string
	Subject      string
	// UserHandle is the opaque WebAuthn user.id the credential was created
	// for: random bytes, one value per subject, never a name or an e-mail.
	// A discoverable login returns it, and it must match.
	UserHandle []byte
	// PublicKey is the COSE-encoded public key.
	PublicKey         []byte
	AttestationType   string
	AttestationFormat string
	Transports        []string
	// Flags is the credential record's flags byte (UP, UV, BE, BS).
	Flags uint8
	// AAGUID names the authenticator model (16 bytes, or empty).
	AAGUID    []byte
	SignCount uint32
	// Attachment is "platform" or "cross-platform" when the browser said.
	Attachment string
	// Discoverable is true for a resident credential (registered with
	// residentKey required): only those can sign in without a login name.
	Discoverable bool
	// Name is the label the person gave it ("YubiKey", "laptop").
	Name       string
	CreatedAt  time.Time
	LastUsedAt time.Time
	// FlaggedAt is set when the credential was refused for a signature
	// counter that went backwards (a cloned authenticator) or another
	// integrity failure; FlagReason says which. A flagged credential is
	// never accepted again; an operator removes it.
	FlaggedAt  time.Time
	FlagReason string
}

// WebAuthn ceremony kinds.
const (
	CeremonyRegister = "register"
	CeremonyLogin    = "login"
)

// WebAuthnCeremonyRow is one begun ceremony: the server's half of a WebAuthn
// challenge, kept until the browser answers and then consumed exactly once.
// CeremonyHash is the hex SHA-256 of the random id the browser holds, so a
// read of the table yields no id that could answer a ceremony.
type WebAuthnCeremonyRow struct {
	CeremonyHash string
	Kind         string
	Gate         string
	TenantID     string
	Subject      string
	// Binding names what the ceremony was begun for (a sign-in, an invite's
	// code hash, a signed-in session). The consumer compares it, so a
	// ceremony begun for one flow cannot finish another.
	Binding string
	// SessionData is the library's session state as JSON: the challenge, the
	// expected user, the allowed credentials, the user-verification rule.
	SessionData string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// PasskeyStore persists passkeys and WebAuthn ceremonies (RFC FA P6).
type PasskeyStore interface {
	// UserPasskeyCreate inserts a passkey. *ErrConflict{Kind:"user_passkey"}
	// when the credential id is already registered (to anyone).
	UserPasskeyCreate(ctx context.Context, row UserPasskeyRow) error
	// UserPasskeyGet returns a passkey by credential id, flagged or not.
	// *ErrNotFound{Kind:"user_passkey"}.
	UserPasskeyGet(ctx context.Context, credentialID []byte) (UserPasskeyRow, error)
	// UserPasskeyList returns the subject's passkeys, oldest first. Empty
	// slice when there are none.
	UserPasskeyList(ctx context.Context, tenantID, subject string) ([]UserPasskeyRow, error)
	// UserPasskeyRecordUse accepts one assertion's signature counter: it sets
	// sign_count, flags and last_used_at only when the passkey is not flagged
	// and the counter moved forward (or both the stored and the new counter
	// are zero, which authenticators without a counter report). False means
	// nothing changed: the counter did not advance (a clone, or a replay of
	// an older assertion), the passkey is flagged, or it is gone.
	UserPasskeyRecordUse(ctx context.Context, credentialID []byte, signCount uint32, flags uint8, at time.Time) (bool, error)
	// UserPasskeyFlag marks a passkey refused for good (see FlaggedAt) and
	// reports whether it did; a passkey already flagged keeps its first
	// reason.
	UserPasskeyFlag(ctx context.Context, credentialID []byte, reason string, at time.Time) (bool, error)
	// UserPasskeyRename sets the label of one of the subject's passkeys and
	// reports whether it did. Another subject's credential id changes nothing.
	UserPasskeyRename(ctx context.Context, tenantID, subject string, credentialID []byte, name string) (bool, error)
	// UserPasskeyDelete removes one of the subject's passkeys and reports
	// whether one went. Another subject's credential id removes nothing.
	UserPasskeyDelete(ctx context.Context, tenantID, subject string, credentialID []byte) (bool, error)
	// UserPasskeyDeleteKeeping removes one of the subject's passkeys unless
	// it is the subject's last usable (unflagged) one, in one transaction, so
	// two concurrent removals can never take the last usable passkey between
	// them. deleted is false when the subject has no such passkey; refused is
	// true when it was the last usable one and stays.
	UserPasskeyDeleteKeeping(ctx context.Context, tenantID, subject string, credentialID []byte) (deleted, refused bool, err error)
	// UserPasskeyDeleteSubject removes every passkey of the subject and
	// returns how many went.
	UserPasskeyDeleteSubject(ctx context.Context, tenantID, subject string) (int, error)

	// WebAuthnCeremonyCreate stores a begun ceremony.
	WebAuthnCeremonyCreate(ctx context.Context, row WebAuthnCeremonyRow) error
	// WebAuthnCeremonyTake consumes a ceremony: it deletes the row and
	// returns it when it is live at now. *ErrNotFound{Kind:"webauthn_ceremony"}
	// for an unknown, already consumed or expired one — so a challenge is
	// answered at most once, however many replicas race for it.
	WebAuthnCeremonyTake(ctx context.Context, ceremonyHash string, now time.Time) (WebAuthnCeremonyRow, error)
	// WebAuthnCeremonyDeleteExpired removes ceremonies that expired before
	// `before`.
	WebAuthnCeremonyDeleteExpired(ctx context.Context, before time.Time) (int, error)
}
