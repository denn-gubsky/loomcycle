package http

// authgate_password.go — RFC FA P2: the password gate (NaiveAuthGate) as a
// gate, and the person-facing endpoints around it: the login form's post, an
// invite's redemption, a reset's completion, and self-service (change the
// password, enrol or remove TOTP) — every one of them a page of loomcycle's own
// Web UI, the only place a password is ever typed (D17).
//
// The unauthenticated endpoints take the invite or reset code in the JSON body,
// never the URL, so a code reaches no access log; the links carry it in the
// fragment for the same reason. All are same-origin only and throttled per
// source, per code and per account, with the counters in the store so every
// replica sees them.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/password"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Authentication methods a password-gate session records (RFC 8176 has pwd
// and otp; a recovery code is its own method).
const (
	amrPassword     = "pwd"
	amrOTP          = "otp"
	amrRecoveryCode = "rcode"
)

// credKindPassword binds a session to the subject's stored password: the
// binding MAC covers the PHC string, so a reset, a password change or a
// removed credential ends every session made from the old one.
const credKindPassword = "password"

var (
	errOTPRequired      = errors.New("authgate: a second factor is required")
	errOTPInvalid       = errors.New("authgate: invalid second factor")
	errMFAEnrolment     = errors.New("authgate: this gate requires a second factor and the account has none")
	errTooManyAttempts  = errors.New("authgate: too many attempts")
	errEnrolmentInvalid = errors.New("authgate: invalid or expired TOTP enrolment")
)

// passwordGate implements authgate.Gate for a kind: password gate.
type passwordGate struct {
	s  *Server
	g  PasswordGateSettings
	pg *PasswordGates
}

// passwordGateNamed returns the configured password gate of that name.
func (s *Server) passwordGateNamed(name string) (*passwordGate, bool) {
	pg := s.pwGates.p.Load()
	if pg == nil {
		return nil, false
	}
	g, ok := pg.byName[name]
	if !ok {
		return nil, false
	}
	return &passwordGate{s: s, g: g, pg: pg}, true
}

func (*passwordGate) Kind() string { return config.AuthGateKindPassword }

// Begin: the form is the Web UI's own login page.
func (*passwordGate) Begin(context.Context, http.ResponseWriter, *http.Request, authgate.LoginState) error {
	return authgate.ErrNotInteractive
}

// Verify: a password is never an API credential (D17, no basic auth).
func (*passwordGate) Verify(context.Context, *http.Request) (authgate.Assertion, error) {
	return authgate.Assertion{}, authgate.ErrNoCredential
}

type passwordLoginRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
	// OTP is a TOTP code or a recovery code.
	OTP string `json:"otp"`
}

// Complete is the login form's post. The identity is authoritative: the login
// maps to exactly one (tenant, subject) through user_identities.
func (pw *passwordGate) Complete(ctx context.Context, r *http.Request, _ authgate.LoginState) (authgate.Assertion, error) {
	var req passwordLoginRequest
	r.Body = http.MaxBytesReader(nil, r.Body, 16<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Login == "" || req.Password == "" {
		return authgate.Assertion{}, authgate.ErrNoCredential
	}
	id, amr, err := pw.s.verifyPassword(ctx, pw, req.Login, req.Password, req.OTP, clientIP(r))
	if err != nil {
		return authgate.Assertion{}, err
	}
	return authgate.Assertion{Subject: id.Subject, TenantHint: id.TenantID, AMR: amr, AuthTime: time.Now()}, nil
}

// sourceKey is a throttle key for a client address, MAC'd so the store holds
// no address.
func (pg *PasswordGates) sourceKey(scope, ip string) string {
	return "src:" + scope + ":" + pg.hasher.MAC("throttle-source", ip)
}

// sourceBlocked reports whether the source is held back now.
func (s *Server) sourceBlocked(ctx context.Context, key string) bool {
	_, until, err := s.store.AuthThrottleGet(ctx, key, time.Now())
	if err != nil {
		// Fail closed: a throttle that cannot be read admits no guess.
		slog.Warn("authgate: throttle read failed (refusing)", "err", err)
		return true
	}
	return until.After(time.Now())
}

func (s *Server) recordSourceFailure(ctx context.Context, key string, block func(int) time.Duration) {
	if _, _, err := s.store.AuthThrottleRecordFailure(ctx, key, time.Now(), password.SourceWindow, block); err != nil {
		slog.Warn("authgate: recording a failure failed", "err", err)
	}
}

// verifyPassword checks a login. An unknown login, a wrong password, a
// locked account and a disabled user all answer ErrInvalidCredential after
// one Argon2id verify's worth of work, so neither the answer nor its time says
// which. Only once the password is right does it say a second factor is
// needed or wrong.
func (s *Server) verifyPassword(ctx context.Context, pw *passwordGate, login, pass, otp, ip string) (store.UserIdentityRow, []string, error) {
	now := time.Now()
	src := pw.pg.sourceKey(pw.g.Name, ip)
	th := pw.g.Throttle
	if s.sourceBlocked(ctx, src) {
		_ = pw.pg.hasher.Dummy(ctx, pass)
		return store.UserIdentityRow{}, nil, errTooManyAttempts
	}
	refuse := func() (store.UserIdentityRow, []string, error) {
		s.recordSourceFailure(ctx, src, th.SourceBlockFor)
		return store.UserIdentityRow{}, nil, authgate.ErrInvalidCredential
	}
	norm, nerr := password.Normalize(pass)
	loginN, lerr := password.NormalizeLogin(login)
	if nerr != nil || lerr != nil {
		_ = pw.pg.hasher.Dummy(ctx, pass)
		return refuse()
	}
	id, err := s.store.UserIdentityGet(ctx, pw.g.Name, "", loginN)
	if err != nil {
		_ = pw.pg.hasher.Dummy(ctx, norm)
		if !isNotFound(err) {
			return store.UserIdentityRow{}, nil, err
		}
		return refuse()
	}
	cred, err := s.store.UserCredentialGet(ctx, id.TenantID, id.Subject, store.CredentialKindPassword)
	if err != nil {
		_ = pw.pg.hasher.Dummy(ctx, norm)
		if !isNotFound(err) {
			return store.UserIdentityRow{}, nil, err
		}
		return refuse()
	}
	if cred.LockedUntil.After(now) {
		_ = pw.pg.hasher.Dummy(ctx, norm)
		return refuse()
	}
	ok, rehash, err := pw.pg.hasher.Verify(ctx, norm, cred.PHC, cred.PepperKID)
	if err != nil {
		return store.UserIdentityRow{}, nil, err
	}
	if !ok {
		s.recordAccountFailure(ctx, pw, id)
		return refuse()
	}
	user, err := s.store.UserGet(ctx, id.TenantID, id.Subject)
	if err != nil || user.Status != "active" {
		if err != nil && !isNotFound(err) {
			return store.UserIdentityRow{}, nil, err
		}
		return refuse()
	}
	amr := []string{amrPassword}
	totp, terr := s.store.UserTOTPGet(ctx, id.TenantID, id.Subject)
	if terr != nil && !isNotFound(terr) {
		return store.UserIdentityRow{}, nil, terr
	}
	if terr == nil && !totp.ConfirmedAt.IsZero() {
		if strings.TrimSpace(otp) == "" {
			return store.UserIdentityRow{}, nil, errOTPRequired
		}
		method, ok, err := s.checkSecondFactor(ctx, pw.pg, id.TenantID, id.Subject, totp, otp)
		if err != nil {
			return store.UserIdentityRow{}, nil, err
		}
		if !ok {
			s.recordAccountFailure(ctx, pw, id)
			s.recordSourceFailure(ctx, src, th.SourceBlockFor)
			return store.UserIdentityRow{}, nil, errOTPInvalid
		}
		amr = append(amr, method)
	} else if pw.g.RequireMFA {
		return store.UserIdentityRow{}, nil, errMFAEnrolment
	}
	if err := s.store.UserCredentialRecordSuccess(ctx, id.TenantID, id.Subject, store.CredentialKindPassword); err != nil {
		slog.Warn("authgate: clearing the failure count failed", "err", err)
	}
	if rehash {
		if phc, kid, err := pw.pg.hasher.Hash(ctx, norm); err == nil {
			if _, err := s.store.UserCredentialRehash(ctx, id.TenantID, id.Subject, store.CredentialKindPassword, cred.PHC, phc, kid, now); err != nil {
				slog.Warn("authgate: re-hashing a password failed", "err", err)
			}
		}
	}
	if err := s.store.UserIdentityTouchLogin(ctx, pw.g.Name, "", loginN, now); err != nil {
		slog.Warn("authgate: recording the login time failed", "err", err)
	}
	return id, amr, nil
}

// recordAccountFailure counts a failure on the account and audits the lockout
// the failure caused.
func (s *Server) recordAccountFailure(ctx context.Context, pw *passwordGate, id store.UserIdentityRow) {
	n, _, err := s.store.UserCredentialRecordFailure(ctx, id.TenantID, id.Subject, store.CredentialKindPassword, time.Now(), pw.g.Throttle.AccountLockFor)
	if err != nil {
		slog.Warn("authgate: recording a failed login failed", "err", err)
		return
	}
	if n == pw.g.Throttle.MaxFailures {
		s.recordAuthAudit(audit.Event{Action: "auth_login_locked", AuthGate: pw.g.Name,
			TargetTenant: id.TenantID, TargetSubject: id.Subject})
	}
}

// checkSecondFactor accepts a TOTP code (once per step) or an unused
// recovery code (once). It returns the method used.
func (s *Server) checkSecondFactor(ctx context.Context, pg *PasswordGates, tenant, subject string, row store.UserTOTPRow, otp string) (string, bool, error) {
	if password.LooksLikeRecoveryCode(otp) {
		norm := password.NormalizeRecoveryCode(otp)
		for _, h := range pg.hasher.MACs(password.RecoveryPurpose, norm) {
			ok, err := s.store.UserRecoveryCodeConsume(ctx, tenant, subject, h, time.Now())
			if err != nil || ok {
				return amrRecoveryCode, ok, err
			}
		}
		return amrRecoveryCode, false, nil
	}
	secret, err := password.OpenTOTP(pg.sealer, tenant, subject, row.Sealed)
	if err != nil {
		return amrOTP, false, err
	}
	step, ok := password.VerifyTOTP(secret, otp, time.Now())
	if !ok {
		return amrOTP, false, nil
	}
	accepted, err := s.store.UserTOTPUseStep(ctx, tenant, subject, step, time.Now())
	return amrOTP, accepted, err
}

// handlePasswordLogin serves POST /v1/_auth/complete/{gate}: the password
// form's post. It runs without the auth middleware; the same-origin check is
// the login-CSRF guard.
func (s *Server) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(uiRequestHeader) != "1" || !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "cross_site_login_refused", "session login must be same-origin")
		return
	}
	name := r.PathValue("gate")
	if !s.authConfigured(r.Context()) {
		writeJSONError(w, http.StatusConflict, "open_mode", "authentication is off on this server; no session is needed")
		return
	}
	if msg := s.sessionRefusal(r); msg != "" {
		writeJSONError(w, http.StatusForbidden, "insecure_transport", msg)
		return
	}
	cfgGate, ok := s.configuredGate(name)
	if !ok || cfgGate.Kind != config.AuthGateKindPassword {
		writeJSONError(w, http.StatusNotFound, "gate_not_found", "no password gate of that name")
		return
	}
	gate, err := s.gateFor(cfgGate)
	if err != nil {
		slog.Error("auth: login gate unavailable", "gate", name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "gate_unavailable", "sign-in is unavailable")
		return
	}
	a, err := gate.Complete(r.Context(), r, authgate.LoginState{})
	switch {
	case err == nil:
	case errors.Is(err, authgate.ErrNoCredential):
		writeJSONError(w, http.StatusBadRequest, "missing_credentials", "login and password are required")
		return
	case errors.Is(err, authgate.ErrInvalidCredential):
		s.recordAuthEvent(audit.Event{Action: "auth_login_failed", AuthGate: name, SourceAddr: clientIP(r)})
		writeJSONError(w, http.StatusUnauthorized, "invalid_credentials", "invalid credentials")
		return
	case errors.Is(err, errTooManyAttempts):
		s.recordAuthEvent(audit.Event{Action: "auth_login_failed", AuthGate: name, AuthReason: "throttled", SourceAddr: clientIP(r)})
		writeJSONError(w, http.StatusTooManyRequests, "too_many_attempts", "too many attempts; wait and try again")
		return
	case errors.Is(err, errOTPRequired):
		writeJSONError(w, http.StatusUnauthorized, "otp_required", "enter the code from your authenticator app or a recovery code")
		return
	case errors.Is(err, errOTPInvalid):
		s.recordAuthEvent(audit.Event{Action: "auth_login_failed", AuthGate: name, AuthReason: "invalid_otp", SourceAddr: clientIP(r)})
		writeJSONError(w, http.StatusUnauthorized, "invalid_otp", "that code was not accepted")
		return
	case errors.Is(err, errMFAEnrolment):
		writeJSONError(w, http.StatusForbidden, "mfa_enrolment_required",
			"this sign-in requires an authenticator app and your account has none; ask an operator for a reset link")
		return
	default:
		slog.Error("auth: password login failed", "gate", name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "session_failed", "sign-in failed")
		return
	}
	if !s.startPasswordSession(w, r, name, a.TenantHint, a.Subject, a.AMR) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// configuredGate is the configured gate of that name.
func (s *Server) configuredGate(name string) (config.AuthGate, bool) {
	for _, g := range s.cfg().ResolvedAuth.Gates {
		if g.Name == name {
			return g, true
		}
	}
	return config.AuthGate{}, false
}

// startPasswordSession establishes the session a password-gate login,
// redemption or reset ends in, writing the refusal when it cannot.
func (s *Server) startPasswordSession(w http.ResponseWriter, r *http.Request, gate, tenant, subject string, amr []string) bool {
	err := s.establishSession(w, r, sessionIdentity{TenantID: tenant, Subject: subject, Gate: gate, AMR: amr})
	if err == nil {
		return true
	}
	if errors.Is(err, errSessionsUnavailable) {
		writeJSONError(w, http.StatusServiceUnavailable, "sessions_unavailable", "this server cannot keep Web UI sessions")
		return false
	}
	slog.Error("auth: creating a Web UI session failed", "gate", gate, "err", err)
	writeJSONError(w, http.StatusInternalServerError, "session_failed", "sign-in failed")
	return false
}

// passwordSessionBinding is a password-gate session's binding: the subject's
// stored password, by its PHC string (so any change of password ends it).
func (s *Server) passwordSessionBinding(ctx context.Context, ident sessionIdentity) (kind, ref, credHash string, err error) {
	if _, ok := s.passwordGateNamed(ident.Gate); !ok {
		return "", "", "", errNoSessionBinding
	}
	cred, err := s.store.UserCredentialGet(ctx, ident.TenantID, ident.Subject, store.CredentialKindPassword)
	if err != nil {
		return "", "", "", err
	}
	return credKindPassword, "", cred.PHC, nil
}

// passwordSessionPrincipal re-resolves a password-gate session: the gate is
// still configured, the password is the one the session was made from, and the
// user is still active. The principal is the user's own — the scopes its
// access_mode grants a delegated token, its ACL set — never more.
func (s *Server) passwordSessionPrincipal(ctx context.Context, row store.UISessionRow, bound func(string) bool) (auth.Principal, bool, bool) {
	if _, ok := s.passwordGateNamed(row.Gate); !ok {
		return auth.Principal{}, false, true
	}
	cred, err := s.store.UserCredentialGet(ctx, row.TenantID, row.Subject, store.CredentialKindPassword)
	if err != nil {
		return auth.Principal{}, false, isNotFound(err)
	}
	if !bound(cred.PHC) {
		return auth.Principal{}, false, true
	}
	u, err := s.store.UserGet(ctx, row.TenantID, row.Subject)
	if err != nil {
		return auth.Principal{}, false, isNotFound(err)
	}
	if u.Status != "active" {
		return auth.Principal{}, false, true
	}
	scopes, err := auth.GrantableUserScopes(u.AccessMode)
	if err != nil {
		return auth.Principal{}, false, true
	}
	return s.enrichPrincipal(ctx, auth.Principal{TenantID: u.TenantID, Subject: u.Subject, Scopes: scopes}), true, true
}

// --- TOTP enrolment tokens ---

// enrolmentTTL bounds how long a page may sit between showing a QR code and
// the person confirming it.
const enrolmentTTL = 15 * time.Minute

type enrolmentBody struct {
	Secret  []byte `json:"s"`
	Expires int64  `json:"e"`
}

// enrolmentIdentity binds an enrolment token to what it was issued for (an
// invite or reset code's hash, or a signed-in subject), so one cannot be
// replayed into another flow.
func enrolmentIdentity(tenant, bindTo string) credential.Identity {
	return credential.Identity{TenantID: tenant, Scope: "authgate/totp-enrol", ScopeID: bindTo, Name: "enrol"}
}

// newEnrolment returns a fresh TOTP secret, the otpauth URI to show and the
// sealed token the page posts back with the first code. The server keeps no
// state: the token carries the secret, encrypted and bound.
func newEnrolment(pg *PasswordGates, gate, tenant, bindTo, account string) (map[string]any, error) {
	secret, err := password.NewTOTPSecret()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(enrolmentBody{Secret: secret, Expires: time.Now().Add(enrolmentTTL).Unix()})
	if err != nil {
		return nil, err
	}
	sealed, err := pg.sealer.Seal(body, enrolmentIdentity(tenant, bindTo))
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(sealed)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"secret":      password.TOTPSecretText(secret),
		"otpauth_uri": password.TOTPURI("loomcycle "+gate, account, secret),
		"enrolment":   base64.RawURLEncoding.EncodeToString(raw),
	}, nil
}

// openEnrolment checks the first code against an enrolment token and returns
// the secret and the step the code was for.
func openEnrolment(pg *PasswordGates, tenant, bindTo, token, code string) ([]byte, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) > 4096 {
		return nil, 0, errEnrolmentInvalid
	}
	var sealed credential.Sealed
	if json.Unmarshal(raw, &sealed) != nil {
		return nil, 0, errEnrolmentInvalid
	}
	plain, err := pg.sealer.Open(sealed, enrolmentIdentity(tenant, bindTo))
	if err != nil {
		return nil, 0, errEnrolmentInvalid
	}
	var body enrolmentBody
	if json.Unmarshal(plain, &body) != nil || time.Now().Unix() > body.Expires {
		return nil, 0, errEnrolmentInvalid
	}
	step, ok := password.VerifyTOTP(body.Secret, code, time.Now())
	if !ok {
		return nil, 0, errOTPInvalid
	}
	return body.Secret, step, nil
}

// secondFactorRows seals a confirmed TOTP secret for the subject and mints
// ten recovery codes, returning the store rows and the codes to show once.
func secondFactorRows(pg *PasswordGates, tenant, subject string, secret []byte, step int64, now time.Time) (*store.AuthSecondFactor, []string, error) {
	sealed, err := password.SealTOTP(pg.sealer, tenant, subject, secret)
	if err != nil {
		return nil, nil, err
	}
	codes, err := password.NewRecoveryCodes(password.RecoveryCodeCount)
	if err != nil {
		return nil, nil, err
	}
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = pg.hasher.MAC(password.RecoveryPurpose, password.NormalizeRecoveryCode(c))
	}
	return &store.AuthSecondFactor{
		TOTP:           store.UserTOTPRow{TenantID: tenant, Subject: subject, Sealed: sealed, CreatedAt: now, ConfirmedAt: now, LastStep: step},
		RecoveryHashes: hashes,
	}, codes, nil
}

// policyError answers a password the policy refuses.
func writePolicyError(w http.ResponseWriter, err error) {
	var pe *password.PolicyError
	code := "password_refused"
	switch {
	case errors.Is(err, password.ErrTooShort):
		code = "password_too_short"
	case errors.Is(err, password.ErrTooLong):
		code = "password_too_long"
	case errors.Is(err, password.ErrBreached):
		code = "password_breached"
	case errors.Is(err, password.ErrContextual):
		code = "password_is_login"
	}
	body := map[string]any{"code": code, "error": err.Error()}
	if errors.As(err, &pe) && pe.MinLength > 0 {
		body["min_length"] = pe.MinLength
	}
	writeJSONErrorBody(w, http.StatusBadRequest, body)
}

// unauthenticatedPost is the common front of the invite and reset endpoints:
// same-origin, a store, a transport that may carry a session, and the source
// not held back. It decodes the body into req.
func (s *Server) unauthenticatedPost(w http.ResponseWriter, r *http.Request, pg *PasswordGates, scope string, req any) (string, bool) {
	if r.Header.Get(uiRequestHeader) != "1" || !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "cross_site_refused", "this request must come from this server's own pages")
		return "", false
	}
	if pg == nil || !s.requireStore(w) {
		if pg == nil {
			writeJSONError(w, http.StatusNotFound, "link_invalid", "this link is not valid")
		}
		return "", false
	}
	src := pg.sourceKey(scope, clientIP(r))
	if s.sourceBlocked(r.Context(), src) {
		writeJSONError(w, http.StatusTooManyRequests, "too_many_attempts", "too many attempts; wait and try again")
		return "", false
	}
	if !decodeStrict(w, r, req) {
		return "", false
	}
	return src, true
}

// linkInvalid answers a code that is unknown, used, revoked or expired — one
// answer for all, and the source is charged a failure.
func (s *Server) linkInvalid(w http.ResponseWriter, ctx context.Context, pg *PasswordGates, src string) {
	// The code names no gate yet, so the default lockout paces the source.
	th := password.Throttle{MaxFailures: password.DefaultMaxFailures, Lockout: password.DefaultLockout}
	s.recordSourceFailure(ctx, src, th.SourceBlockFor)
	writeJSONError(w, http.StatusNotFound, "link_invalid", "this link is not valid; it may have been used or have expired")
}

// codeBlocked reports whether a code has been hammered with bad submissions.
func (s *Server) codeBlocked(w http.ResponseWriter, ctx context.Context, codeHash string) bool {
	if s.sourceBlocked(ctx, "code:"+codeHash) {
		writeJSONError(w, http.StatusTooManyRequests, "too_many_attempts", "too many attempts on this link; wait and try again")
		return true
	}
	return false
}

func (s *Server) codeFailure(ctx context.Context, g PasswordGateSettings, codeHash string) {
	s.recordSourceFailure(ctx, "code:"+codeHash, g.Throttle.CodeBlockFor)
}

// --- invites ---

type inviteCodeRequest struct {
	Code string `json:"code"`
}

type redeemInviteRequest struct {
	Code          string `json:"code"`
	Login         string `json:"login"`
	Password      string `json:"password"`
	TOTPEnrolment string `json:"totp_enrolment"`
	TOTPCode      string `json:"totp_code"`
}

// liveInvite resolves an invite code to a live invite on a configured gate.
func (s *Server) liveInvite(ctx context.Context, pg *PasswordGates, code string) (store.AuthInviteRow, PasswordGateSettings, bool) {
	if !password.WellFormedCode(password.InvitePrefix, code) {
		return store.AuthInviteRow{}, PasswordGateSettings{}, false
	}
	inv, err := s.store.AuthInviteGetByCode(ctx, password.HashCode(code))
	if err != nil || !inv.Live(time.Now()) {
		return store.AuthInviteRow{}, PasswordGateSettings{}, false
	}
	g, ok := pg.byName[inv.Gate]
	return inv, g, ok
}

// handleInviteInfo serves POST /v1/_auth/invite/info: what the invite page
// needs to render — the gate, a fixed login if any, whether a second factor
// is required (with a fresh enrolment), the minimum password length. Nothing
// about the tenant.
func (s *Server) handleInviteInfo(w http.ResponseWriter, r *http.Request) {
	pg := s.pwGates.p.Load()
	var req inviteCodeRequest
	src, ok := s.unauthenticatedPost(w, r, pg, "invite", &req)
	if !ok {
		return
	}
	inv, g, ok := s.liveInvite(r.Context(), pg, req.Code)
	if !ok {
		s.linkInvalid(w, r.Context(), pg, src)
		return
	}
	out := map[string]any{"gate": g.Name, "login": inv.Login, "email": inv.Email, "require_mfa": g.RequireMFA,
		"min_length": password.MinLengthSoleFactor, "min_length_with_mfa": password.MinLengthWithMFA, "expires_at": inv.ExpiresAt}
	account := inv.Login
	if account == "" {
		account = inv.Email
	}
	enrol, err := newEnrolment(pg, g.Name, inv.TenantID, "invite:"+inv.CodeHash, account)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not prepare the authenticator enrolment")
		return
	}
	out["totp"] = enrol
	writeJSON(w, http.StatusOK, out)
}

// handleRedeemInvite serves POST /v1/_auth/invite: the invitee takes a
// login, sets a password (and a second factor when the gate requires one),
// and is signed in. One transaction writes the user, the login, the password
// and the factor and consumes the invite; nothing is written if any of it
// fails, and the invite stays usable.
func (s *Server) handleRedeemInvite(w http.ResponseWriter, r *http.Request) {
	pg := s.pwGates.p.Load()
	var req redeemInviteRequest
	src, ok := s.unauthenticatedPost(w, r, pg, "invite", &req)
	if !ok {
		return
	}
	ctx := r.Context()
	if msg := s.sessionRefusal(r); msg != "" {
		writeJSONError(w, http.StatusForbidden, "insecure_transport", msg)
		return
	}
	inv, g, ok := s.liveInvite(ctx, pg, req.Code)
	if !ok {
		s.linkInvalid(w, ctx, pg, src)
		return
	}
	if s.codeBlocked(w, ctx, inv.CodeHash) {
		return
	}
	login := inv.Login
	if login == "" {
		raw := req.Login
		if strings.TrimSpace(raw) == "" {
			raw = inv.Email
		}
		var err error
		if login, err = password.NormalizeLogin(raw); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_login", err.Error())
			return
		}
	} else if strings.TrimSpace(req.Login) != "" {
		if n, err := password.NormalizeLogin(req.Login); err != nil || n != login {
			writeJSONError(w, http.StatusBadRequest, "login_fixed", "this invitation is for the login "+login)
			return
		}
	}
	withMFA := g.RequireMFA || req.TOTPEnrolment != ""
	pass, err := password.Normalize(req.Password)
	if err == nil {
		err = pg.policy.Check(pass, withMFA, login, inv.Email)
	}
	if err != nil {
		writePolicyError(w, err)
		return
	}
	now := time.Now().UTC()
	subject := inv.Subject
	if subject == "" {
		if subject, err = password.NewSubject(g.Name); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal", "could not create the account")
			return
		}
	}
	var factor *store.AuthSecondFactor
	var recovery []string
	amr := []string{amrPassword}
	if req.TOTPEnrolment != "" {
		secret, step, err := openEnrolment(pg, inv.TenantID, "invite:"+inv.CodeHash, req.TOTPEnrolment, req.TOTPCode)
		if err != nil {
			s.codeFailure(ctx, g, inv.CodeHash)
			writeJSONError(w, http.StatusBadRequest, "invalid_totp", "the authenticator code was not accepted; scan the code again")
			return
		}
		if factor, recovery, err = secondFactorRows(pg, inv.TenantID, subject, secret, step, now); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal", "could not store the authenticator")
			return
		}
		amr = append(amr, amrOTP)
	} else if g.RequireMFA {
		writeJSONError(w, http.StatusBadRequest, "totp_required", "this sign-in requires an authenticator app")
		return
	}
	// A website's link to an existing user is held to the floor again now:
	// the user may have been promoted since the link was issued.
	if inv.IssuedByManager && inv.Subject != "" && !s.managerLinkStillAllowed(ctx, &g, inv.TenantID, inv.Subject, "redeem_invite") {
		if _, err := s.store.AuthInviteRevoke(ctx, inv.InviteID, now); err != nil {
			slog.Warn("authgate: revoking a refused invite failed", "err", err)
		}
		s.linkInvalid(w, ctx, pg, src)
		return
	}
	phc, kid, err := pg.hasher.Hash(ctx, pass)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not store the password")
		return
	}
	red := store.AuthInviteRedemption{
		CodeHash: inv.CodeHash, Now: now, SecondFactor: factor,
		Identity:   store.UserIdentityRow{TenantID: inv.TenantID, Subject: subject, Gate: g.Name, ExternalSubject: login, Email: inv.Email, CreatedAt: now},
		Credential: store.UserCredentialRow{TenantID: inv.TenantID, Subject: subject, Kind: store.CredentialKindPassword, PHC: phc, PepperKID: kid, UpdatedAt: now},
	}
	if inv.Subject == "" {
		red.User = &store.UserRow{TenantID: inv.TenantID, Subject: subject, AccessMode: inv.AccessMode, Status: "active",
			CreatedAt: now, CreatedBy: "invite:" + inv.InviteID}
	}
	if err := s.store.AuthInviteRedeem(ctx, red); err != nil {
		var conflict *store.ErrConflict
		switch {
		case errors.As(err, &conflict) && conflict.Kind == "user_identity":
			writeJSONError(w, http.StatusConflict, "login_taken", "that login is taken; choose another")
		case errors.As(err, &conflict) && conflict.Kind == "tenant_not_empty":
			writeJSONError(w, http.StatusConflict, "link_invalid", "this invitation can no longer be used")
		case isNotFound(err):
			s.linkInvalid(w, ctx, pg, src)
		default:
			slog.Error("authgate: redeeming an invite failed", "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal", "the invitation could not be redeemed")
		}
		return
	}
	if red.User != nil {
		s.joinInviteGroups(ctx, g, inv, subject)
	}
	s.recordGateAudit(ctx, "auth_invite_redeemed", g.Name, inv.InviteID, inv.TenantID, subject, floorTarget{})
	s.recordGateAudit(ctx, "auth_password_set", g.Name, inv.InviteID, inv.TenantID, subject, floorTarget{})
	if !s.startPasswordSession(w, r, g.Name, inv.TenantID, subject, amr) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"redirect": redirectAfter(g, inv.ReturnTo), "login": login, "recovery_codes": recovery})
}

// joinInviteGroups adds a new user to the gate's floor groups that exist in
// its tenant (RFC FA §3.7a) and to the invite's groups. A failure leaves the
// user with less than the invite named, never more.
func (s *Server) joinInviteGroups(ctx context.Context, g PasswordGateSettings, inv store.AuthInviteRow, subject string) {
	names := append(append([]string(nil), floorOf(&g).groups...), inv.Groups...)
	added := false
	for _, n := range names {
		err := s.store.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: inv.TenantID, GroupName: n,
			MemberKind: store.GroupMemberUser, MemberID: subject, AddedBy: "invite:" + inv.InviteID})
		var conflict *store.ErrConflict
		switch {
		case err == nil:
			added = true
		case isNotFound(err), errors.As(err, &conflict):
			// A floor group the tenant does not have, or already a member.
		default:
			slog.Warn("authgate: adding an invited user to a group failed", "group", n, "err", err)
		}
	}
	if added {
		s.aclChanged(ctx, inv.TenantID, "group_member_added", "invite:"+inv.InviteID)
	}
}

// redirectAfter is where the browser goes after a redemption: the invite's
// return_to when it is still allowed, else the Web UI.
func redirectAfter(g PasswordGateSettings, returnTo string) string {
	if returnTo != "" && validReturnTo(g, returnTo) {
		return returnTo
	}
	return "/ui/"
}

// managerLinkStillAllowed re-applies the floor to an auth:manage link's
// target at redemption (closing the window between issue and use), auditing
// a refusal or a recovery.
func (s *Server) managerLinkStillAllowed(ctx context.Context, g *PasswordGateSettings, tenant, subject, op string) bool {
	above, u, err := s.aboveFloor(ctx, tenant, subject, g)
	if isNotFound(err) || !above {
		return err == nil || isNotFound(err)
	}
	if err == nil && g.ManagerMayRecoverOwners {
		s.recordAuthAudit(audit.Event{Action: "auth_manage_recovery", AuthGate: g.Name, AuthReason: op, AuthRecovery: true,
			AuthTargetAccessMode: u.AccessMode, TargetTenant: tenant, TargetSubject: subject})
		return true
	}
	s.recordAuthAudit(audit.Event{Action: "auth_manage_refused", AuthGate: g.Name, AuthReason: op,
		AuthTargetAccessMode: u.AccessMode, TargetTenant: tenant, TargetSubject: subject})
	return false
}

// --- resets ---

type completeResetRequest struct {
	Code          string `json:"code"`
	Password      string `json:"password"`
	OTP           string `json:"otp"`
	TOTPEnrolment string `json:"totp_enrolment"`
	TOTPCode      string `json:"totp_code"`
}

func (s *Server) liveReset(ctx context.Context, pg *PasswordGates, code string) (store.AuthResetRow, PasswordGateSettings, bool) {
	if !password.WellFormedCode(password.ResetPrefix, code) {
		return store.AuthResetRow{}, PasswordGateSettings{}, false
	}
	rs, err := s.store.AuthResetGetByCode(ctx, password.HashCode(code))
	if err != nil || !rs.Live(time.Now()) {
		return store.AuthResetRow{}, PasswordGateSettings{}, false
	}
	g, ok := pg.byName[rs.Gate]
	return rs, g, ok
}

// confirmedTOTP returns the subject's confirmed TOTP row, if any.
func (s *Server) confirmedTOTP(ctx context.Context, tenant, subject string) (store.UserTOTPRow, bool, error) {
	row, err := s.store.UserTOTPGet(ctx, tenant, subject)
	if isNotFound(err) {
		return store.UserTOTPRow{}, false, nil
	}
	if err != nil {
		return store.UserTOTPRow{}, false, err
	}
	return row, !row.ConfirmedAt.IsZero(), nil
}

// handleResetInfo serves POST /v1/_auth/reset/info.
func (s *Server) handleResetInfo(w http.ResponseWriter, r *http.Request) {
	pg := s.pwGates.p.Load()
	var req inviteCodeRequest
	src, ok := s.unauthenticatedPost(w, r, pg, "reset", &req)
	if !ok {
		return
	}
	ctx := r.Context()
	rs, g, ok := s.liveReset(ctx, pg, req.Code)
	if !ok {
		s.linkInvalid(w, ctx, pg, src)
		return
	}
	login := ""
	if ids, err := s.store.UserIdentityListSubject(ctx, rs.TenantID, rs.Subject); err == nil {
		for _, id := range ids {
			if id.Gate == g.Name {
				login = id.ExternalSubject
			}
		}
	}
	_, hasTOTP, err := s.confirmedTOTP(ctx, rs.TenantID, rs.Subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := map[string]any{"gate": g.Name, "login": login, "has_totp": hasTOTP, "require_mfa": g.RequireMFA,
		"min_length": password.MinLengthSoleFactor, "min_length_with_mfa": password.MinLengthWithMFA, "expires_at": rs.ExpiresAt}
	if !hasTOTP {
		enrol, err := newEnrolment(pg, g.Name, rs.TenantID, "reset:"+rs.CodeHash, login)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal", "could not prepare the authenticator enrolment")
			return
		}
		out["totp"] = enrol
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCompleteReset serves POST /v1/_auth/reset: a new password for the
// link's user. A user with an authenticator must also give a code from it (or
// a recovery code): the link alone does not bypass the second factor. It ends
// every session made from the old password (the binding is the PHC string)
// and retires the user's delegated API tokens, since a reset is how a
// compromised account is recovered.
func (s *Server) handleCompleteReset(w http.ResponseWriter, r *http.Request) {
	pg := s.pwGates.p.Load()
	var req completeResetRequest
	src, ok := s.unauthenticatedPost(w, r, pg, "reset", &req)
	if !ok {
		return
	}
	ctx := r.Context()
	if msg := s.sessionRefusal(r); msg != "" {
		writeJSONError(w, http.StatusForbidden, "insecure_transport", msg)
		return
	}
	rs, g, ok := s.liveReset(ctx, pg, req.Code)
	if !ok {
		s.linkInvalid(w, ctx, pg, src)
		return
	}
	if s.codeBlocked(w, ctx, rs.CodeHash) {
		return
	}
	totp, hasTOTP, err := s.confirmedTOTP(ctx, rs.TenantID, rs.Subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	now := time.Now().UTC()
	amr := []string{amrPassword}
	var factor *store.AuthSecondFactor
	var recovery []string
	switch {
	case hasTOTP:
		if strings.TrimSpace(req.OTP) == "" {
			writeJSONError(w, http.StatusUnauthorized, "otp_required", "enter the code from your authenticator app or a recovery code")
			return
		}
		method, ok, err := s.checkSecondFactor(ctx, pg, rs.TenantID, rs.Subject, totp, req.OTP)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if !ok {
			s.codeFailure(ctx, g, rs.CodeHash)
			writeJSONError(w, http.StatusUnauthorized, "invalid_otp", "that code was not accepted")
			return
		}
		amr = append(amr, method)
	case req.TOTPEnrolment != "":
		secret, step, err := openEnrolment(pg, rs.TenantID, "reset:"+rs.CodeHash, req.TOTPEnrolment, req.TOTPCode)
		if err != nil {
			s.codeFailure(ctx, g, rs.CodeHash)
			writeJSONError(w, http.StatusBadRequest, "invalid_totp", "the authenticator code was not accepted; scan the code again")
			return
		}
		if factor, recovery, err = secondFactorRows(pg, rs.TenantID, rs.Subject, secret, step, now); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal", "could not store the authenticator")
			return
		}
		amr = append(amr, amrOTP)
	case g.RequireMFA:
		writeJSONError(w, http.StatusBadRequest, "totp_required", "this sign-in requires an authenticator app")
		return
	}
	login := ""
	if ids, err := s.store.UserIdentityListSubject(ctx, rs.TenantID, rs.Subject); err == nil {
		for _, id := range ids {
			if id.Gate == g.Name {
				login = id.ExternalSubject
			}
		}
	}
	pass, err := password.Normalize(req.Password)
	if err == nil {
		err = pg.policy.Check(pass, len(amr) > 1, login)
	}
	if err != nil {
		writePolicyError(w, err)
		return
	}
	if rs.IssuedByManager && !s.managerLinkStillAllowed(ctx, &g, rs.TenantID, rs.Subject, "complete_reset") {
		if _, err := s.store.AuthRevokeManagerIssued(ctx, rs.TenantID, rs.Subject, now); err != nil {
			slog.Warn("authgate: revoking a refused reset failed", "err", err)
		}
		s.linkInvalid(w, ctx, pg, src)
		return
	}
	phc, kid, err := pg.hasher.Hash(ctx, pass)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not store the password")
		return
	}
	c := store.AuthResetCompletion{CodeHash: rs.CodeHash, Now: now, SecondFactor: factor,
		Credential: store.UserCredentialRow{TenantID: rs.TenantID, Subject: rs.Subject, Kind: store.CredentialKindPassword, PHC: phc, PepperKID: kid, UpdatedAt: now}}
	if err := s.store.AuthResetComplete(ctx, c); err != nil {
		if isNotFound(err) {
			s.linkInvalid(w, ctx, pg, src)
			return
		}
		slog.Error("authgate: completing a reset failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal", "the reset could not be completed")
		return
	}
	if _, err := s.retireDelegatedUserTokens(ctx, rs.TenantID, rs.Subject); err != nil {
		slog.Error("authgate: retiring tokens after a reset failed", "err", err)
	}
	s.recordGateAudit(ctx, "auth_password_set", g.Name, rs.ResetID, rs.TenantID, rs.Subject, floorTarget{})
	if !s.startPasswordSession(w, r, g.Name, rs.TenantID, rs.Subject, amr) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"redirect": "/ui/", "login": login, "recovery_codes": recovery})
}

// --- self-service (a signed-in password-gate session) ---

// selfService resolves the caller's password gate and identity from its
// session: only a session of a password gate may use these routes.
func (s *Server) selfService(w http.ResponseWriter, r *http.Request) (*passwordGate, store.UISessionRow, string, bool) {
	a, ok := uiAuthFrom(r.Context())
	if !ok || a.session == nil {
		writeJSONError(w, http.StatusForbidden, "session_required", "sign in through a password gate in the Web UI")
		return nil, store.UISessionRow{}, "", false
	}
	pw, ok := s.passwordGateNamed(a.session.Gate)
	if !ok {
		writeJSONError(w, http.StatusForbidden, "session_required", "sign in through a password gate in the Web UI")
		return nil, store.UISessionRow{}, "", false
	}
	ids, err := s.store.UserIdentityListSubject(r.Context(), a.session.TenantID, a.session.Subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return nil, store.UISessionRow{}, "", false
	}
	for _, id := range ids {
		if id.Gate == pw.g.Name {
			return pw, *a.session, id.ExternalSubject, true
		}
	}
	writeJSONError(w, http.StatusForbidden, "session_required", "this account has no login at its gate")
	return nil, store.UISessionRow{}, "", false
}

// reauthenticate is the step-up a sensitive self-service action takes: the
// current password (and the second factor, when the account has one), through
// the same throttled check as a login.
func (s *Server) reauthenticate(w http.ResponseWriter, r *http.Request, pw *passwordGate, sess store.UISessionRow, login, current, otp string) bool {
	id, _, err := s.verifyPassword(r.Context(), pw, login, current, otp, clientIP(r))
	switch {
	case err == nil && id.TenantID == sess.TenantID && id.Subject == sess.Subject:
		return true
	case errors.Is(err, errOTPRequired):
		writeJSONError(w, http.StatusUnauthorized, "otp_required", "enter the code from your authenticator app or a recovery code")
	case errors.Is(err, errTooManyAttempts):
		writeJSONError(w, http.StatusTooManyRequests, "too_many_attempts", "too many attempts; wait and try again")
	case err == nil, errors.Is(err, authgate.ErrInvalidCredential), errors.Is(err, errOTPInvalid), errors.Is(err, errMFAEnrolment):
		writeJSONError(w, http.StatusUnauthorized, "reauth_failed", "the current password or code was not accepted")
	default:
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
	}
	return false
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	OTP             string `json:"otp"`
}

// handleChangePassword serves POST /v1/_auth/password. The new password ends
// every other session (their binding is the old PHC string); this browser gets
// a new session.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	pw, sess, login, ok := s.selfService(w, r)
	if !ok {
		return
	}
	var req changePasswordRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !s.reauthenticate(w, r, pw, sess, login, req.CurrentPassword, req.OTP) {
		return
	}
	ctx := r.Context()
	_, hasTOTP, err := s.confirmedTOTP(ctx, sess.TenantID, sess.Subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	pass, err := password.Normalize(req.NewPassword)
	if err == nil {
		err = pw.pg.policy.Check(pass, hasTOTP, login)
	}
	if err != nil {
		writePolicyError(w, err)
		return
	}
	phc, kid, err := pw.pg.hasher.Hash(ctx, pass)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not store the password")
		return
	}
	if err := s.store.UserCredentialSet(ctx, store.UserCredentialRow{TenantID: sess.TenantID, Subject: sess.Subject,
		Kind: store.CredentialKindPassword, PHC: phc, PepperKID: kid, UpdatedAt: time.Now().UTC()}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.recordGateAudit(ctx, "auth_password_set", pw.g.Name, "", sess.TenantID, sess.Subject, floorTarget{})
	if !s.startPasswordSession(w, r, pw.g.Name, sess.TenantID, sess.Subject, sess.AMR) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTOTPBegin serves POST /v1/_auth/totp/begin: a fresh secret to scan.
func (s *Server) handleTOTPBegin(w http.ResponseWriter, r *http.Request) {
	pw, sess, login, ok := s.selfService(w, r)
	if !ok {
		return
	}
	enrol, err := newEnrolment(pw.pg, pw.g.Name, sess.TenantID, "self:"+sess.Subject, login)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not prepare the authenticator enrolment")
		return
	}
	writeJSON(w, http.StatusOK, enrol)
}

type confirmTOTPRequest struct {
	CurrentPassword string `json:"current_password"`
	OTP             string `json:"otp"`
	Enrolment       string `json:"enrolment"`
	Code            string `json:"code"`
}

// handleTOTPConfirm serves POST /v1/_auth/totp/confirm: after re-entering the
// password (and the current factor, if one is being replaced) and a first code
// from the new app, the factor is stored and ten new recovery codes shown
// once.
func (s *Server) handleTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	pw, sess, login, ok := s.selfService(w, r)
	if !ok {
		return
	}
	var req confirmTOTPRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !s.reauthenticate(w, r, pw, sess, login, req.CurrentPassword, req.OTP) {
		return
	}
	ctx := r.Context()
	secret, step, err := openEnrolment(pw.pg, sess.TenantID, "self:"+sess.Subject, req.Enrolment, req.Code)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_totp", "the authenticator code was not accepted; scan the code again")
		return
	}
	now := time.Now().UTC()
	factor, codes, err := secondFactorRows(pw.pg, sess.TenantID, sess.Subject, secret, step, now)
	if err == nil {
		err = s.store.UserTOTPPut(ctx, factor.TOTP)
	}
	if err == nil {
		err = s.store.UserRecoveryCodesReplace(ctx, sess.TenantID, sess.Subject, factor.RecoveryHashes, now)
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "could not store the authenticator")
		return
	}
	s.recordGateAudit(ctx, "auth_totp_enrolled", pw.g.Name, "", sess.TenantID, sess.Subject, floorTarget{})
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

type removeTOTPRequest struct {
	CurrentPassword string `json:"current_password"`
	OTP             string `json:"otp"`
}

// handleTOTPRemove serves DELETE /v1/_auth/totp. A gate that requires a
// second factor refuses it.
func (s *Server) handleTOTPRemove(w http.ResponseWriter, r *http.Request) {
	pw, sess, login, ok := s.selfService(w, r)
	if !ok {
		return
	}
	if pw.g.RequireMFA {
		writeJSONError(w, http.StatusForbidden, "mfa_required", "this sign-in requires an authenticator app")
		return
	}
	var req removeTOTPRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !s.reauthenticate(w, r, pw, sess, login, req.CurrentPassword, req.OTP) {
		return
	}
	if _, err := s.store.UserTOTPDelete(r.Context(), sess.TenantID, sess.Subject); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.recordGateAudit(r.Context(), "auth_totp_removed", pw.g.Name, "", sess.TenantID, sess.Subject, floorTarget{})
	w.WriteHeader(http.StatusNoContent)
}

// handleAccountInfo serves GET /v1/_auth/account: the signed-in password
// user's login, whether it has an authenticator and how many recovery codes
// are left, and whether the gate requires one.
func (s *Server) handleAccountInfo(w http.ResponseWriter, r *http.Request) {
	pw, sess, login, ok := s.selfService(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	_, hasTOTP, err := s.confirmedTOTP(ctx, sess.TenantID, sess.Subject)
	var left int
	if err == nil {
		left, err = s.store.UserRecoveryCodesRemaining(ctx, sess.TenantID, sess.Subject)
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gate": pw.g.Name, "login": login, "has_totp": hasTOTP,
		"recovery_codes_left": left, "require_mfa": pw.g.RequireMFA})
}
