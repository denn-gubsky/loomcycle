package http

// authgate_passkey.go — RFC FA P6: passkeys (WebAuthn), as a second factor at
// a password gate and as a password-less sign-in through a kind: passkey gate.
//
// Every cryptographic step is go-webauthn's: parsing the CBOR attestation and
// authenticator data, the COSE public key, the challenge, the origin and RP ID
// hashes, the user-verification and backup flags, and the signature. This file
// decides what loomcycle does around it:
//
//   - the relying party is LOOMCYCLE_PUBLIC_URL (RP ID = its host, the one
//     accepted origin = its origin; config.AuthPasskeyRP);
//   - a challenge (a ceremony) is single-use (the store deletes it to read
//     it), lives five minutes, and is bound to what began it: a password-less
//     sign-in to the browser that asked (a cookie secret), a second factor to
//     the gate and the account whose password was just verified, a
//     registration to the session that asked;
//   - a password-less sign-in and a registration require user verification;
//   - a signature counter that does not move forward flags the passkey for
//     good (a cloned authenticator), and the store's compare-and-set keeps
//     two replicas from both accepting one counter value;
//   - only the credential id, its public key and its metadata are kept.
//
// Registering and removing a passkey is self-service from the person's own
// Web UI session — a password, identity or passkey sign-in, never a pasted
// token — after a recent sign-in, like linking an identity (RFC FA §8.1 rule
// 8): a passkey made from a narrow token's session would sign in later with
// the user's full reach.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const (
	// amrWebAuthn is the method a passkey adds to a session (RFC FA §3.1).
	amrWebAuthn = "webauthn"
	// credKindPasskey binds a passkey-gate session to the passkey it signed
	// in with: removing or flagging the passkey ends the session.
	credKindPasskey = "passkey"
	// ceremonySecondFactor: a passkey confirming a verified password.
	ceremonySecondFactor = "second_factor"
	// ceremonyLinkFactor: a passkey confirming a reset or attach-invite
	// link, bound to the link's code.
	ceremonyLinkFactor = "link_factor"

	passkeyCeremonyTTL = 5 * time.Minute
	maxPasskeysPerUser = 10
	maxPasskeyName     = 64
	maxWebAuthnBody    = 64 << 10
	// The browser half of a password-less ceremony's binding.
	webauthnCookieSecure = "__Host-lc_webauthn"
	webauthnCookiePlain  = "lc_webauthn"
	// passkeyFailWindow / passkeyFailLimit hold back a client source that
	// keeps presenting assertions that do not verify.
	passkeyFailWindow = 10 * time.Minute
	passkeyFailLimit  = 20
)

var (
	errPasskeysUnavailable = errors.New("passkeys are not available on this server")
	errPasskeyRefused      = errors.New("passkey refused")
)

// passkeyOffer is a begun ceremony as the browser needs it: the id to answer
// with, and the options for navigator.credentials (binary fields base64url).
type passkeyOffer struct {
	Ceremony  string `json:"ceremony"`
	PublicKey any    `json:"publicKey"`
}

// passkeyAnswer is the browser's answer to an offer: the ceremony id and the
// PublicKeyCredential as JSON.
type passkeyAnswer struct {
	Ceremony   string          `json:"ceremony"`
	Credential json.RawMessage `json:"credential"`
}

// relyingParty is the deployment's WebAuthn relying party, or false when
// passkeys are not configured.
func (s *Server) relyingParty() (*webauthn.WebAuthn, bool) {
	rp := s.cfg().ResolvedAuth.Passkeys
	if rp == nil {
		return nil, false
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          rp.ID,
		RPDisplayName: "loomcycle",
		RPOrigins:     []string{rp.Origin},
		// Attestation would identify the authenticator model; loomcycle
		// trusts no attestation roots, so it asks for none.
		AttestationPreference: protocol.PreferNoAttestation,
	})
	if err != nil {
		slog.Error("auth: the WebAuthn relying party is invalid", "rp_id", rp.ID, "err", err)
		return nil, false
	}
	return wa, true
}

// passkeyUser is a loomcycle user as go-webauthn sees it: the opaque handle
// the passkeys were created for and the credentials it may use.
type passkeyUser struct {
	handle      []byte
	name        string
	displayName string
	creds       []webauthn.Credential
}

func (u *passkeyUser) WebAuthnID() []byte                         { return u.handle }
func (u *passkeyUser) WebAuthnName() string                       { return u.name }
func (u *passkeyUser) WebAuthnDisplayName() string                { return u.displayName }
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// toCredential is a stored passkey as go-webauthn's credential record.
func toCredential(r store.UserPasskeyRow) webauthn.Credential {
	tr := make([]protocol.AuthenticatorTransport, 0, len(r.Transports))
	for _, t := range r.Transports {
		tr = append(tr, protocol.AuthenticatorTransport(t))
	}
	return webauthn.Credential{
		ID: r.CredentialID, PublicKey: r.PublicKey, AttestationType: r.AttestationType,
		AttestationFormat: r.AttestationFormat, Transport: tr,
		Flags: webauthn.NewCredentialFlags(protocol.AuthenticatorFlags(r.Flags)),
		Authenticator: webauthn.Authenticator{AAGUID: r.AAGUID, SignCount: r.SignCount,
			Attachment: protocol.AuthenticatorAttachment(r.Attachment)},
	}
}

// livePasskeys is the subject's passkeys that may still be used (not
// flagged).
func (s *Server) livePasskeys(ctx context.Context, tenant, subject string) ([]store.UserPasskeyRow, error) {
	rows, err := s.store.UserPasskeyList(ctx, tenant, subject)
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, r := range rows {
		if r.FlaggedAt.IsZero() {
			out = append(out, r)
		}
	}
	return out, nil
}

func ceremonyHash(id string) string {
	sum := sha256.Sum256([]byte("webauthn-ceremony\x00" + id))
	return hex.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// beginCeremony stores a ceremony's server half and returns the offer.
func (s *Server) beginCeremony(ctx context.Context, kind, gate, tenant, subject, binding string, opts any, sd *webauthn.SessionData) (passkeyOffer, error) {
	id, err := randomToken()
	if err != nil {
		return passkeyOffer{}, err
	}
	data, err := json.Marshal(sd)
	if err != nil {
		return passkeyOffer{}, err
	}
	now := time.Now().UTC()
	if err := s.store.WebAuthnCeremonyCreate(ctx, store.WebAuthnCeremonyRow{CeremonyHash: ceremonyHash(id), Kind: kind,
		Gate: gate, TenantID: tenant, Subject: subject, Binding: binding, SessionData: string(data),
		CreatedAt: now, ExpiresAt: now.Add(passkeyCeremonyTTL)}); err != nil {
		return passkeyOffer{}, err
	}
	return passkeyOffer{Ceremony: id, PublicKey: opts}, nil
}

// takeCeremony consumes the ceremony an answer names and checks it was begun
// for this flow: kind, gate, account and binding. Any mismatch reads as an
// unknown ceremony (errPasskeyRefused); the row is gone either way, so a
// ceremony is answered once.
func (s *Server) takeCeremony(ctx context.Context, a passkeyAnswer, kind, gate, tenant, subject, binding string) (webauthn.SessionData, error) {
	if a.Ceremony == "" || len(a.Credential) == 0 {
		return webauthn.SessionData{}, errPasskeyRefused
	}
	row, err := s.store.WebAuthnCeremonyTake(ctx, ceremonyHash(a.Ceremony), time.Now())
	if err != nil {
		if isNotFound(err) {
			return webauthn.SessionData{}, errPasskeyRefused
		}
		return webauthn.SessionData{}, err
	}
	if row.Kind != kind || row.Gate != gate || row.TenantID != tenant || row.Subject != subject ||
		subtle.ConstantTimeCompare([]byte(row.Binding), []byte(binding)) != 1 {
		return webauthn.SessionData{}, errPasskeyRefused
	}
	var sd webauthn.SessionData
	if err := json.Unmarshal([]byte(row.SessionData), &sd); err != nil {
		return webauthn.SessionData{}, fmt.Errorf("webauthn ceremony: %w", err)
	}
	return sd, nil
}

// acceptAssertion records a verified assertion's counter. A counter that did
// not move forward flags the passkey for good: an authenticator's counter
// only grows, so an older value is a clone or a replayed assertion.
func (s *Server) acceptAssertion(ctx context.Context, row store.UserPasskeyRow, cred *webauthn.Credential) error {
	now := time.Now().UTC()
	if cred.Authenticator.CloneWarning {
		if ok, err := s.store.UserPasskeyFlag(ctx, row.CredentialID, "sign_count_regressed", now); err != nil {
			slog.Warn("auth: flagging a passkey failed", "err", err)
		} else if ok {
			s.recordAuthEvent(audit.Event{Action: "auth_passkey_flagged", TargetTenant: row.TenantID, TargetSubject: row.Subject,
				AuthReason: "sign_count_regressed"})
		}
		return errPasskeyRefused
	}
	ok, err := s.store.UserPasskeyRecordUse(ctx, row.CredentialID, cred.Authenticator.SignCount, cred.Flags.MsgpByte(), now)
	if err != nil {
		return err
	}
	if !ok {
		// Another replica accepted this counter value first, or the
		// passkey was flagged or removed meanwhile.
		return errPasskeyRefused
	}
	return nil
}

// passkeyFailKey is the throttle key of a client source's failed passkey
// assertions.
func (s *Server) passkeyFailKey(r *http.Request) (string, error) {
	h, err := s.hasher()
	if err != nil {
		return "", err
	}
	return "passkeyfail:" + h.Hash("passkey-source\x00"+throttleSourceKey(s.clientSource(r))), nil
}

func (s *Server) passkeySourceBlocked(ctx context.Context, r *http.Request) bool {
	key, err := s.passkeyFailKey(r)
	if err != nil {
		return true
	}
	return s.sourceBlocked(ctx, key)
}

func (s *Server) recordPasskeyFailure(ctx context.Context, r *http.Request) {
	key, err := s.passkeyFailKey(r)
	if err != nil {
		return
	}
	s.recordSourceFailure(ctx, key, func(n int) time.Duration {
		if n >= passkeyFailLimit {
			return passkeyFailWindow
		}
		return 0
	})
}

// passkeyErrDetail is go-webauthn's error with its developer detail, for a
// debug log line (it names the check that failed, never a key or challenge).
func passkeyErrDetail(err error) string {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe.Details + ": " + pe.DevInfo
	}
	return err.Error()
}

// --- the password-less gate ---

// webauthnCookieName follows the session cookie's transport rule.
func (s *Server) webauthnCookieName(r *http.Request) string {
	if s.requestSecure(r) {
		return webauthnCookieSecure
	}
	return webauthnCookiePlain
}

func loginBinding(secret string) string {
	sum := sha256.Sum256([]byte("webauthn-login\x00" + secret))
	return "login:" + hex.EncodeToString(sum[:])
}

// passkeyGateNamed is the configured passkey gate of that name.
func (s *Server) passkeyGateNamed(name string) (config.AuthGate, bool) {
	g, ok := s.cfg().ResolvedAuth.PasskeyGate()
	if !ok || g.Name != name {
		return config.AuthGate{}, false
	}
	return g, true
}

// handlePasskeyLoginBegin serves POST /v1/_auth/passkey/{gate}/begin: a
// discoverable, user-verifying challenge, bound to this browser by a short
// cookie. Unauthenticated, same-origin, throttled per source like any login
// start (each start writes a row).
func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(uiRequestHeader) != "1" || !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "cross_site_login_refused", "session login must be same-origin")
		return
	}
	g, ok := s.passkeyGateNamed(r.PathValue("gate"))
	if !ok {
		writeJSONError(w, http.StatusNotFound, "gate_not_found", "no passkey gate of that name")
		return
	}
	if !s.authConfigured(r.Context()) {
		writeJSONError(w, http.StatusConflict, "open_mode", "authentication is off on this server; no session is needed")
		return
	}
	if msg := s.sessionRefusal(r); msg != "" {
		writeJSONError(w, http.StatusForbidden, "insecure_transport", msg)
		return
	}
	wa, ok := s.relyingParty()
	if !ok || s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "passkeys_unavailable", "passkeys are not available on this server")
		return
	}
	ctx := r.Context()
	if !s.loginStartAllowed(ctx, r) {
		writeJSONError(w, http.StatusTooManyRequests, "too_many_attempts", "too many sign-in attempts; wait and try again")
		return
	}
	opts, sd, err := wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		slog.Error("auth: beginning a passkey sign-in failed", "gate", g.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "passkey_failed", "sign-in failed")
		return
	}
	secret, err := randomToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "passkey_failed", "sign-in failed")
		return
	}
	offer, err := s.beginCeremony(ctx, store.CeremonyLogin, g.Name, "", "", loginBinding(secret), opts.Response, sd)
	if err != nil {
		slog.Error("auth: storing a passkey ceremony failed", "gate", g.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "passkey_failed", "sign-in failed")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: s.webauthnCookieName(r), Value: secret, Path: "/", HttpOnly: true,
		Secure: s.requestSecure(r), SameSite: http.SameSiteStrictMode, MaxAge: int(passkeyCeremonyTTL / time.Second)})
	writeJSON(w, http.StatusOK, offer)
}

// handlePasskeyLogin finishes a password-less sign-in (POST
// /v1/_auth/complete/{gate} for a passkey gate). Every refusal is the same
// 401 invalid_credentials: which passkey, whose, and why are in the audit
// record and the log, never in the answer.
func (s *Server) handlePasskeyLogin(w http.ResponseWriter, r *http.Request, g config.AuthGate) {
	if r.Header.Get(uiRequestHeader) != "1" || !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "cross_site_login_refused", "session login must be same-origin")
		return
	}
	if !s.authConfigured(r.Context()) {
		writeJSONError(w, http.StatusConflict, "open_mode", "authentication is off on this server; no session is needed")
		return
	}
	if msg := s.sessionRefusal(r); msg != "" {
		writeJSONError(w, http.StatusForbidden, "insecure_transport", msg)
		return
	}
	wa, ok := s.relyingParty()
	if !ok || s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "passkeys_unavailable", "passkeys are not available on this server")
		return
	}
	ctx := r.Context()
	if s.passkeySourceBlocked(ctx, r) {
		writeJSONError(w, http.StatusTooManyRequests, "too_many_attempts", "too many attempts; wait and try again")
		return
	}
	// The ceremony's browser binding is single-use too.
	cookie, cerr := r.Cookie(s.webauthnCookieName(r))
	http.SetCookie(w, expiredCookie(s.webauthnCookieName(r), s.requestSecure(r)))
	var a passkeyAnswer
	r.Body = http.MaxBytesReader(w, r.Body, maxWebAuthnBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		writeJSONError(w, http.StatusBadRequest, "missing_credentials", "a passkey answer is required")
		return
	}
	refuse := func(reason string) {
		s.recordPasskeyFailure(ctx, r)
		s.recordAuthEvent(audit.Event{Action: "auth_login_failed", AuthGate: g.Name, AuthReason: reason, SourceAddr: s.clientSource(r)})
		writeJSONError(w, http.StatusUnauthorized, "invalid_credentials", "invalid credentials")
	}
	if cerr != nil || cookie.Value == "" {
		refuse("no ceremony cookie")
		return
	}
	sd, err := s.takeCeremony(ctx, a, store.CeremonyLogin, g.Name, "", "", loginBinding(cookie.Value))
	if err != nil {
		if errors.Is(err, errPasskeyRefused) {
			refuse("unknown ceremony")
			return
		}
		slog.Error("auth: reading a passkey ceremony failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "session_failed", "sign-in failed")
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(a.Credential)
	if err != nil {
		refuse("unparsable assertion")
		return
	}
	var row store.UserPasskeyRow
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		pk, err := s.store.UserPasskeyGet(ctx, rawID)
		if err != nil {
			return nil, err
		}
		if !pk.FlaggedAt.IsZero() {
			return nil, errPasskeyRefused
		}
		if subtle.ConstantTimeCompare(userHandle, pk.UserHandle) != 1 {
			return nil, errPasskeyRefused
		}
		row = pk
		return &passkeyUser{handle: pk.UserHandle, name: pk.Subject, displayName: pk.Subject,
			creds: []webauthn.Credential{toCredential(pk)}}, nil
	}
	_, cred, err := wa.ValidatePasskeyLogin(handler, sd, parsed)
	if err != nil {
		slog.Debug("auth: a passkey assertion did not verify", "gate", g.Name, "err", passkeyErrDetail(err))
		refuse("assertion did not verify")
		return
	}
	if err := s.acceptAssertion(ctx, row, cred); err != nil {
		if errors.Is(err, errPasskeyRefused) {
			refuse("sign count did not advance")
			return
		}
		slog.Error("auth: recording a passkey use failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "session_failed", "sign-in failed")
		return
	}
	u, err := s.store.UserGet(ctx, row.TenantID, row.Subject)
	if err != nil || u.Status != "active" {
		if err != nil && !isNotFound(err) {
			slog.Error("auth: reading a passkey's user failed", "err", err)
			writeJSONError(w, http.StatusInternalServerError, "session_failed", "sign-in failed")
			return
		}
		refuse("user not active")
		return
	}
	err = s.establishSession(w, r, sessionIdentity{TenantID: row.TenantID, Subject: row.Subject, Gate: g.Name,
		AMR: []string{amrWebAuthn}, Passkey: row.CredentialID})
	if err != nil {
		if writeTenantPolicyRefusal(w, err) {
			return
		}
		slog.Error("auth: creating a Web UI session failed", "gate", g.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "session_failed", "sign-in failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// passkeySessionBinding binds a passkey-gate session to the passkey's public
// key: removing the passkey (or a credential id re-registered with another
// key) ends it.
func (s *Server) passkeySessionBinding(ctx context.Context, ident sessionIdentity) (kind, ref, credHash string, err error) {
	if len(ident.Passkey) == 0 {
		return "", "", "", errNoSessionBinding
	}
	pk, err := s.store.UserPasskeyGet(ctx, ident.Passkey)
	if err != nil {
		return "", "", "", err
	}
	if pk.TenantID != ident.TenantID || pk.Subject != ident.Subject {
		return "", "", "", errors.New("web ui session: the passkey is not the identity's")
	}
	return credKindPasskey, base64.RawURLEncoding.EncodeToString(pk.CredentialID), passkeyKeyHash(pk.PublicKey), nil
}

func passkeyKeyHash(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// passkeySessionPrincipal re-resolves a passkey-gate session: the gate is
// still configured, the passkey still exists unflagged with the key the
// session was made from, and the user is active.
func (s *Server) passkeySessionPrincipal(ctx context.Context, row store.UISessionRow, bound func(string) bool) (auth.Principal, bool, bool) {
	if _, ok := s.passkeyGateNamed(row.Gate); !ok {
		return auth.Principal{}, false, true
	}
	id, err := base64.RawURLEncoding.DecodeString(row.CredRef)
	if err != nil {
		return auth.Principal{}, false, true
	}
	pk, err := s.store.UserPasskeyGet(ctx, id)
	if err != nil {
		return auth.Principal{}, false, isNotFound(err)
	}
	if !pk.FlaggedAt.IsZero() || pk.TenantID != row.TenantID || pk.Subject != row.Subject || !bound(passkeyKeyHash(pk.PublicKey)) {
		return auth.Principal{}, false, true
	}
	return s.activeUserSessionPrincipal(ctx, row.TenantID, row.Subject)
}

// --- the second factor at a password gate ---

// secondFactorChallenge is verifyPassword's answer when the password was
// right and the account has a second factor that was not given: the passkey
// offer (when it has passkeys) and whether a TOTP code would do too.
type secondFactorChallenge struct {
	offer *passkeyOffer
	otp   bool
}

func (*secondFactorChallenge) Error() string { return "authgate: a second factor is required" }

// writeSecondFactorRequired is the 401 that asks for one.
func writeSecondFactorRequired(w http.ResponseWriter, ch *secondFactorChallenge) {
	if ch.offer == nil {
		writeJSONError(w, http.StatusUnauthorized, "otp_required", "enter the code from your authenticator app or a recovery code")
		return
	}
	writeJSONErrorBody(w, http.StatusUnauthorized, map[string]any{"code": "second_factor_required",
		"error": "confirm with your passkey", "webauthn": ch.offer, "otp": ch.otp})
}

func secondFactorBinding(gate string) string { return "second_factor:" + gate }

// passkeySecondFactor begins (answer nil) or checks a passkey second factor
// for a verified password. It returns the offer to send, or whether the
// answer verified. User verification is preferred, not required: the
// password is the first factor, the passkey proves possession.
func (s *Server) passkeySecondFactor(ctx context.Context, gate, tenant, subject string, keys []store.UserPasskeyRow, answer *passkeyAnswer) (*passkeyOffer, bool, error) {
	return s.passkeyAssertion(ctx, ceremonySecondFactor, gate, tenant, subject, secondFactorBinding(gate),
		protocol.VerificationPreferred, keys, answer)
}

// passkeyAssertion begins (answer nil) or checks an assertion of one of the
// subject's passkeys, in a ceremony of kind bound to binding.
func (s *Server) passkeyAssertion(ctx context.Context, kind, gate, tenant, subject, binding string,
	uv protocol.UserVerificationRequirement, keys []store.UserPasskeyRow, answer *passkeyAnswer) (*passkeyOffer, bool, error) {
	wa, ok := s.relyingParty()
	if !ok || len(keys) == 0 {
		return nil, false, errPasskeysUnavailable
	}
	user := &passkeyUser{handle: keys[0].UserHandle, name: subject, displayName: subject}
	for _, k := range keys {
		if bytes.Equal(k.UserHandle, user.handle) {
			user.creds = append(user.creds, toCredential(k))
		}
	}
	if answer == nil {
		opts, sd, err := wa.BeginLogin(user, webauthn.WithUserVerification(uv))
		if err != nil {
			return nil, false, err
		}
		offer, err := s.beginCeremony(ctx, kind, gate, tenant, subject, binding, opts.Response, sd)
		if err != nil {
			return nil, false, err
		}
		return &offer, false, nil
	}
	sd, err := s.takeCeremony(ctx, *answer, kind, gate, tenant, subject, binding)
	if err != nil {
		if errors.Is(err, errPasskeyRefused) {
			return nil, false, nil
		}
		return nil, false, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(answer.Credential)
	if err != nil {
		return nil, false, nil
	}
	cred, err := wa.ValidateLogin(user, sd, parsed)
	if err != nil {
		return nil, false, nil
	}
	for _, k := range keys {
		if bytes.Equal(k.CredentialID, cred.ID) {
			if err := s.acceptAssertion(ctx, k, cred); err != nil {
				if errors.Is(err, errPasskeyRefused) {
					return nil, false, nil
				}
				return nil, false, err
			}
			return nil, true, nil
		}
	}
	return nil, false, nil
}

// --- self-service: register, list, rename, remove ---

// passkeySelf is the caller's own Web UI session, when it may manage the
// person's passkeys: a session whose credential IS the user (password,
// identity or passkey sign-in), not awaiting approval, with the user's own
// reach. Writes the refusal.
func (s *Server) passkeySelf(w http.ResponseWriter, r *http.Request) (*store.UISessionRow, store.UserRow, bool) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "passkeys need a persistent store")
		return nil, store.UserRow{}, false
	}
	if s.cfg().ResolvedAuth.Passkeys == nil {
		writeJSONError(w, http.StatusNotFound, "passkeys_unavailable", "passkeys are not available on this server")
		return nil, store.UserRow{}, false
	}
	ctx := r.Context()
	ua, ok := uiAuthFrom(ctx)
	p, _ := auth.PrincipalFromContext(ctx)
	if !ok || ua.session == nil || ua.pending || p.TenantID != ua.session.TenantID || p.Subject != ua.session.Subject {
		writeJSONError(w, http.StatusForbidden, "session_required", "manage passkeys from your own signed-in Web UI session")
		return nil, store.UserRow{}, false
	}
	if !userCredentialSession(ua.session) {
		writeJSONError(w, http.StatusForbidden, "passkey_needs_user_session",
			"manage passkeys from a session you signed in to with your password, a passkey or an identity provider, not with a pasted token")
		return nil, store.UserRow{}, false
	}
	u, err := s.store.UserGet(ctx, ua.session.TenantID, ua.session.Subject)
	if err != nil || u.Status != "active" || !principalIsTheUser(p, u) {
		writeJSONError(w, http.StatusForbidden, "passkey_needs_user_session", "this session cannot manage passkeys")
		return nil, store.UserRow{}, false
	}
	return ua.session, u, true
}

// recentSignIn reports whether the session authenticated within the
// re-authentication window (RFC FA §5).
func (s *Server) recentSignIn(row *store.UISessionRow) bool {
	window := s.cfg().ResolvedAuth.ReauthWindow
	if window <= 0 {
		window = config.DefaultSessionReauthWindow
	}
	return !row.AuthTime.IsZero() && time.Since(row.AuthTime) <= window
}

func writeReauthRequired(w http.ResponseWriter) {
	writeJSONError(w, http.StatusForbidden, "reauth_required", "sign in again first: this needs a recent sign-in")
}

type wirePasskey struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	Discoverable bool       `json:"discoverable"`
	Attachment   string     `json:"attachment,omitempty"`
	Flagged      bool       `json:"flagged"`
	FlagReason   string     `json:"flag_reason,omitempty"`
}

// handleListPasskeys serves GET /v1/_auth/passkeys: the caller's own passkeys
// (never a key), and whether passkeys are available at all.
func (s *Server) handleListPasskeys(w http.ResponseWriter, r *http.Request) {
	if s.cfg().ResolvedAuth.Passkeys == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "passkeys": []wirePasskey{}})
		return
	}
	sess, _, ok := s.passkeySelf(w, r)
	if !ok {
		return
	}
	rows, err := s.store.UserPasskeyList(r.Context(), sess.TenantID, sess.Subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not list passkeys")
		return
	}
	out := make([]wirePasskey, 0, len(rows))
	for _, k := range rows {
		wp := wirePasskey{ID: base64.RawURLEncoding.EncodeToString(k.CredentialID), Name: k.Name, CreatedAt: k.CreatedAt.UTC(),
			Discoverable: k.Discoverable, Attachment: k.Attachment, Flagged: !k.FlaggedAt.IsZero(), FlagReason: k.FlagReason}
		if !k.LastUsedAt.IsZero() {
			t := k.LastUsedAt.UTC()
			wp.LastUsedAt = &t
		}
		out = append(out, wp)
	}
	_, gate := s.cfg().ResolvedAuth.PasskeyGate()
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "passkeys": out, "passwordless": gate})
}

func registerBinding(sessionHash string) string { return "register:" + sessionHash }

// ceremonyStepUp: a passkey re-confirming its person before a passkey change.
const ceremonyStepUp = "step_up"

func stepUpBinding(sessionHash string) string { return "step_up:" + sessionHash }

// passkeyStepUp is the re-authentication a passkey change carries in its own
// request, as the password and TOTP changes do.
type passkeyStepUp struct {
	CurrentPassword string         `json:"current_password,omitempty"`
	OTP             string         `json:"otp,omitempty"`
	WebAuthn        *passkeyAnswer `json:"webauthn,omitempty"`
}

// passwordLoginOf is the subject's login at a configured password gate where
// it has a password, if any.
func (s *Server) passwordLoginOf(ctx context.Context, tenant, subject string) (*passwordGate, string, bool, error) {
	if _, err := s.store.UserCredentialGet(ctx, tenant, subject, store.CredentialKindPassword); err != nil {
		if isNotFound(err) {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	ids, err := s.store.UserIdentityListSubject(ctx, tenant, subject)
	if err != nil {
		return nil, "", false, err
	}
	for _, id := range ids {
		if pw, ok := s.passwordGateNamed(id.Gate); ok {
			return pw, id.ExternalSubject, true, nil
		}
	}
	return nil, "", false, nil
}

// passkeyStepUpOK re-authenticates the person within this request before a
// passkey is added or removed, so a borrowed session alone cannot change how
// the account signs in: an account with a password gives it and its second
// factor (the TOTP or passkey a login would ask for); an account without one
// confirms with one of its passkeys (user-verifying); an account with
// neither — an identity provider's — must have signed in at the provider
// within the re-authentication window. Writes the refusal (a 401
// second_factor_required carries the passkey offer to answer).
func (s *Server) passkeyStepUpOK(w http.ResponseWriter, r *http.Request, sess *store.UISessionRow, req passkeyStepUp) bool {
	ctx := r.Context()
	pw, login, hasPassword, err := s.passwordLoginOf(ctx, sess.TenantID, sess.Subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not read the account")
		return false
	}
	if hasPassword {
		if req.CurrentPassword == "" {
			writeJSONError(w, http.StatusUnauthorized, "password_required", "confirm with your current password")
			return false
		}
		return s.reauthenticate(w, r, pw, *sess, login, req.CurrentPassword, secondFactor{otp: req.OTP, webauthn: req.WebAuthn})
	}
	keys, err := s.livePasskeys(ctx, sess.TenantID, sess.Subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not read passkeys")
		return false
	}
	if len(keys) > 0 {
		offer, ok, err := s.passkeyAssertion(ctx, ceremonyStepUp, "", sess.TenantID, sess.Subject, stepUpBinding(sess.SessionHash),
			protocol.VerificationRequired, keys, req.WebAuthn)
		switch {
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "internal", "could not confirm the passkey")
			return false
		case offer != nil:
			writeSecondFactorRequired(w, &secondFactorChallenge{offer: offer})
			return false
		case !ok:
			writeJSONError(w, http.StatusUnauthorized, "reauth_failed", "the passkey was not accepted")
			return false
		}
		return true
	}
	if !s.recentSignIn(sess) {
		writeReauthRequired(w)
		return false
	}
	return true
}

// decodeStepUp reads a passkey change's optional step-up body (an empty body
// is no step-up).
func decodeStepUp(w http.ResponseWriter, r *http.Request) (passkeyStepUp, bool) {
	var req passkeyStepUp
	r.Body = http.MaxBytesReader(w, r.Body, maxWebAuthnBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
		return passkeyStepUp{}, false
	}
	return req, true
}

// cleanPasskeyName bounds a label: printable, trimmed, at most 64 bytes.
func cleanPasskeyName(v string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(v) {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			continue
		}
		if b.Len()+len(string(r)) > maxPasskeyName {
			break
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "Passkey"
	}
	return b.String()
}

// handlePasskeyRegisterBegin serves POST /v1/_auth/passkeys/register/begin:
// a creation challenge for the signed-in person, after a step-up in this
// request (passkeyStepUpOK), bound to this session. The passkey must be discoverable and verify its
// user, so it can sign in without a password.
func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	sess, u, ok := s.passkeySelf(w, r)
	if !ok {
		return
	}
	req, ok := decodeStepUp(w, r)
	if !ok || !s.passkeyStepUpOK(w, r, sess, req) {
		return
	}
	wa, ok := s.relyingParty()
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "passkeys_unavailable", "passkeys are not available on this server")
		return
	}
	ctx := r.Context()
	keys, err := s.store.UserPasskeyList(ctx, sess.TenantID, sess.Subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not read passkeys")
		return
	}
	if len(keys) >= maxPasskeysPerUser {
		writeJSONError(w, http.StatusConflict, "too_many_passkeys", fmt.Sprintf("an account keeps at most %d passkeys; remove one first", maxPasskeysPerUser))
		return
	}
	user := &passkeyUser{name: u.Subject, displayName: u.DisplayName}
	if user.displayName == "" {
		user.displayName = u.Subject
	}
	// One opaque handle per subject: a discoverable sign-in returns it, and
	// every passkey of the account must answer to the same one.
	if len(keys) > 0 {
		user.handle = keys[0].UserHandle
	} else {
		user.handle = make([]byte, 32)
		if _, err := rand.Read(user.handle); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal", "could not begin the registration")
			return
		}
	}
	exclude := make([]protocol.CredentialDescriptor, 0, len(keys))
	for _, k := range keys {
		c := toCredential(k)
		exclude = append(exclude, c.Descriptor())
	}
	opts, sd, err := wa.BeginRegistration(user,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey: protocol.ResidentKeyRequirementRequired, RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification: protocol.VerificationRequired}),
		webauthn.WithExclusions(exclude),
		webauthn.WithExtensions(webauthn.WithExtensionCredProps()))
	if err != nil {
		slog.Error("auth: beginning a passkey registration failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not begin the registration")
		return
	}
	offer, err := s.beginCeremony(ctx, store.CeremonyRegister, "", sess.TenantID, sess.Subject, registerBinding(sess.SessionHash), opts.Response, sd)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not begin the registration")
		return
	}
	writeJSON(w, http.StatusOK, offer)
}

type registerFinishRequest struct {
	Ceremony   string          `json:"ceremony"`
	Credential json.RawMessage `json:"credential"`
	Name       string          `json:"name"`
}

// handlePasskeyRegisterFinish serves POST /v1/_auth/passkeys/register/finish:
// the authenticator's attestation, verified against the ceremony this
// session began. Only the credential id, the public key and its metadata are
// stored.
func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	sess, u, ok := s.passkeySelf(w, r)
	if !ok {
		return
	}
	wa, ok := s.relyingParty()
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "passkeys_unavailable", "passkeys are not available on this server")
		return
	}
	var req registerFinishRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxWebAuthnBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
		return
	}
	ctx := r.Context()
	refuse := func() {
		writeJSONError(w, http.StatusBadRequest, "passkey_not_accepted", "the passkey was not accepted; try again")
	}
	sd, err := s.takeCeremony(ctx, passkeyAnswer{Ceremony: req.Ceremony, Credential: req.Credential}, store.CeremonyRegister, "",
		sess.TenantID, sess.Subject, registerBinding(sess.SessionHash))
	if err != nil {
		if errors.Is(err, errPasskeyRefused) {
			refuse()
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not read the registration")
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(req.Credential)
	if err != nil {
		refuse()
		return
	}
	user := &passkeyUser{handle: sd.UserID, name: u.Subject, displayName: u.Subject}
	cred, err := wa.CreateCredential(user, sd, parsed)
	if err != nil {
		slog.Info("auth: a passkey registration did not verify", "tenant", sess.TenantID, "err", err)
		refuse()
		return
	}
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	discoverable := true // residentKey was required
	if cred.Extensions.RK != nil {
		discoverable = *cred.Extensions.RK
	}
	row := store.UserPasskeyRow{CredentialID: cred.ID, TenantID: sess.TenantID, Subject: sess.Subject, UserHandle: sd.UserID,
		PublicKey: cred.PublicKey, AttestationType: cred.AttestationType, AttestationFormat: cred.AttestationFormat,
		Transports: transports, Flags: cred.Flags.MsgpByte(), AAGUID: cred.Authenticator.AAGUID,
		SignCount: cred.Authenticator.SignCount, Attachment: string(cred.Authenticator.Attachment), Discoverable: discoverable,
		Name: cleanPasskeyName(req.Name), CreatedAt: time.Now().UTC()}
	if err := s.store.UserPasskeyCreate(ctx, row); err != nil {
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) {
			writeJSONError(w, http.StatusConflict, "passkey_exists", "this passkey is registered already")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not store the passkey")
		return
	}
	s.recordGateAudit(ctx, "auth_passkey_registered", sess.Gate, "", sess.TenantID, sess.Subject, floorTarget{})
	writeJSON(w, http.StatusCreated, map[string]any{"id": base64.RawURLEncoding.EncodeToString(row.CredentialID), "name": row.Name})
}

type renamePasskeyRequest struct {
	Name string `json:"name"`
}

// handleRenamePasskey serves PATCH /v1/_auth/passkeys/{id}.
func (s *Server) handleRenamePasskey(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.passkeySelf(w, r)
	if !ok {
		return
	}
	id, err := base64.RawURLEncoding.DecodeString(r.PathValue("id"))
	if err != nil || len(id) == 0 {
		writeJSONError(w, http.StatusNotFound, "passkey_not_found", "no such passkey")
		return
	}
	var req renamePasskeyRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	ok, err = s.store.UserPasskeyRename(r.Context(), sess.TenantID, sess.Subject, id, cleanPasskeyName(req.Name))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not rename the passkey")
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "passkey_not_found", "no such passkey")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRemovePasskey serves DELETE /v1/_auth/passkeys/{id}, after a step-up
// in this request (passkeyStepUpOK). It refuses to remove the account's last way in, and the last
// second factor of a password a gate requires one for. Sessions made with
// the passkey end on their next request (their binding is its key).
func (s *Server) handleRemovePasskey(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.passkeySelf(w, r)
	if !ok {
		return
	}
	id, err := base64.RawURLEncoding.DecodeString(r.PathValue("id"))
	if err != nil || len(id) == 0 {
		writeJSONError(w, http.StatusNotFound, "passkey_not_found", "no such passkey")
		return
	}
	req, ok := decodeStepUp(w, r)
	if !ok || !s.passkeyStepUpOK(w, r, sess, req) {
		return
	}
	ctx := r.Context()
	keys, err := s.livePasskeys(ctx, sess.TenantID, sess.Subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not read passkeys")
		return
	}
	last := len(keys) == 1 && bytes.Equal(keys[0].CredentialID, id)
	if last {
		if code, msg, err := s.lastPasskeyRefusal(ctx, sess.TenantID, sess.Subject); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal", "could not read the account")
			return
		} else if code != "" {
			writeJSONError(w, http.StatusConflict, code, msg)
			return
		}
	}
	ok, err = s.store.UserPasskeyDelete(ctx, sess.TenantID, sess.Subject, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not remove the passkey")
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "passkey_not_found", "no such passkey")
		return
	}
	s.recordGateAudit(ctx, "auth_passkey_removed", sess.Gate, "", sess.TenantID, sess.Subject, floorTarget{})
	w.WriteHeader(http.StatusNoContent)
}

// lastPasskeyRefusal says why the account's last usable passkey may not go:
// without it the account has no password and no linked identity (no way in),
// or it has a password at a gate that requires a second factor and no
// authenticator app (its password alone would no longer sign in).
func (s *Server) lastPasskeyRefusal(ctx context.Context, tenant, subject string) (code, msg string, err error) {
	ids, err := s.store.UserIdentityListSubject(ctx, tenant, subject)
	if err != nil {
		return "", "", err
	}
	_, perr := s.store.UserCredentialGet(ctx, tenant, subject, store.CredentialKindPassword)
	if perr != nil && !isNotFound(perr) {
		return "", "", perr
	}
	hasPassword := perr == nil
	if !hasPassword && len(ids) == 0 {
		return "last_sign_in_method", "this passkey is the only way to sign in to this account; add another first", nil
	}
	if !hasPassword {
		return "", "", nil
	}
	_, hasTOTP, err := s.confirmedTOTP(ctx, tenant, subject)
	if err != nil || hasTOTP {
		return "", "", err
	}
	for _, id := range ids {
		if pw, ok := s.passwordGateNamed(id.Gate); ok && pw.g.RequireMFA {
			return "last_second_factor", "this sign-in requires a second factor; add an authenticator app or another passkey first", nil
		}
	}
	return "", "", nil
}
