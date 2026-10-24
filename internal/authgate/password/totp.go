package password

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
)

// TOTP per RFC 6238 with the parameters every authenticator app defaults to:
// HMAC-SHA1, 30-second steps, 6 digits. A code is accepted for the current
// step and one step either side (clock drift); the store refuses a step at or
// before the last one accepted, so a code is single-use within its window.
const (
	TOTPPeriod    = 30 * time.Second
	TOTPDigits    = 6
	totpSkew      = 1
	totpSecretLen = 20
)

var totpB32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh 160-bit secret.
func NewTOTPSecret() ([]byte, error) {
	b := make([]byte, totpSecretLen)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// TOTPSecretText is the secret as an authenticator app's "enter key" field
// wants it (base32, no padding).
func TOTPSecretText(secret []byte) string { return totpB32.EncodeToString(secret) }

// TOTPStep is the step number of t.
func TOTPStep(t time.Time) int64 { return t.Unix() / int64(TOTPPeriod/time.Second) }

// TOTPCode is the code for step (RFC 4226 dynamic truncation).
func TOTPCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000)
}

// VerifyTOTP returns the step code is valid for at now (within ±1 step) and
// whether it is. Every candidate step is compared in constant time and all are
// tried, so the answer's timing does not say which step matched. The caller
// must then record the step with the store to refuse a replay.
func VerifyTOTP(secret []byte, code string, now time.Time) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != TOTPDigits {
		return 0, false
	}
	cur := TOTPStep(now)
	var matched int64
	found := 0
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		if subtle.ConstantTimeCompare([]byte(TOTPCode(secret, cur+d)), []byte(code)) == 1 {
			matched, found = cur+d, 1
		}
	}
	return matched, found == 1
}

// TOTPURI is the otpauth:// URI an authenticator app scans as a QR code.
func TOTPURI(issuer, account string, secret []byte) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", TOTPSecretText(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(TOTPDigits))
	q.Set("period", fmt.Sprint(int(TOTPPeriod/time.Second)))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// totpScope is the credential.Identity scope a TOTP secret is sealed under. No
// CredentialDef uses it, so a sealed value can never be copied between the
// credential store and user_totp and still open.
const totpScope = "authgate/totp"

func totpIdentity(tenantID, subject string) credential.Identity {
	return credential.Identity{TenantID: tenantID, Scope: totpScope, ScopeID: subject, Name: "totp"}
}

// SealTOTP encrypts a TOTP secret for the subject's user_totp row with the
// deployment's credential key (RFC AR envelope encryption). The ciphertext is
// bound to (tenant, subject): copied to another user's row it does not open.
func SealTOTP(s *credential.Sealer, tenantID, subject string, secret []byte) (string, error) {
	sealed, err := s.Seal(secret, totpIdentity(tenantID, subject))
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(sealed)
	return string(b), err
}

// OpenTOTP decrypts a user_totp row's secret.
func OpenTOTP(s *credential.Sealer, tenantID, subject, sealedJSON string) ([]byte, error) {
	var sealed credential.Sealed
	if err := json.Unmarshal([]byte(sealedJSON), &sealed); err != nil {
		return nil, credential.ErrDecrypt
	}
	return s.Open(sealed, totpIdentity(tenantID, subject))
}

// Recovery codes: ten single-use codes of 50 random bits each, shown once at
// enrolment and stored as peppered MACs (Hasher.MAC with RecoveryPurpose).
const (
	RecoveryCodeCount = 10
	RecoveryPurpose   = "recovery-code"
	recoveryCodeLen   = 10
)

// NewRecoveryCodes returns n codes formatted "xxxxx-xxxxx".
func NewRecoveryCodes(n int) ([]string, error) {
	out := make([]string, n)
	for i := range out {
		var b [7]byte // 56 bits, 50 used
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		c := lowerBase32.EncodeToString(b[:])[:recoveryCodeLen]
		out[i] = c[:5] + "-" + c[5:]
	}
	return out, nil
}

// NormalizeRecoveryCode is the form a typed recovery code is MAC'd in:
// lowercase, without the dash or spaces a person may add or drop.
func NormalizeRecoveryCode(code string) string {
	code = strings.ToLower(code)
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, code)
}

// LooksLikeRecoveryCode distinguishes a recovery code from a TOTP code in a
// single "second factor" field.
func LooksLikeRecoveryCode(code string) bool {
	return len(NormalizeRecoveryCode(code)) == recoveryCodeLen
}
