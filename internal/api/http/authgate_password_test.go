package http

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/authgate/password"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

const goodPassword = "a long passphrase nobody breached"

// browser is a Web UI client: a cookie jar, the same-origin headers the SPA's
// requests carry, and a fixed User-Agent (sessions are bound to it).
type browser struct {
	t  *testing.T
	f  *gateFixture
	hc *http.Client
}

func (f *gateFixture) browser(t *testing.T) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &browser{t: t, f: f, hc: &http.Client{Jar: jar}}
}

func (b *browser) do(method, path, body string) (int, map[string]any) {
	b.t.Helper()
	req, err := http.NewRequest(method, b.f.ts.URL+path, strings.NewReader(body))
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set(uiRequestHeader, "1")
	req.Header.Set("User-Agent", testUA)
	resp, err := b.hc.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (b *browser) me() (int, string) {
	b.t.Helper()
	code, body := b.do("GET", "/v1/_me", "")
	sub, _ := body["subject"].(string)
	return code, sub
}

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// inviteCode creates an invite as the bearer and returns its code.
func (f *gateFixture) inviteCode(t *testing.T, bearer, gate, body string) string {
	t.Helper()
	code, b := f.call(t, bearer, "POST", "/v1/_authgate/"+gate+"/invites", body)
	if code != http.StatusCreated {
		t.Fatalf("create invite on %s: %d %v", gate, code, b)
	}
	return codeOf(t, b["url"].(string), password.InvitePrefix)
}

func (f *gateFixture) resetCode(t *testing.T, bearer, gate, body string) string {
	t.Helper()
	code, b := f.call(t, bearer, "POST", "/v1/_authgate/"+gate+"/resets", body)
	if code != http.StatusCreated {
		t.Fatalf("create reset on %s: %d %v", gate, code, b)
	}
	return codeOf(t, b["url"].(string), password.ResetPrefix)
}

// redeemed signs a new browser up through an invite on gate.
func (f *gateFixture) redeemed(t *testing.T, gate, login, invite string) *browser {
	t.Helper()
	b := f.browser(t)
	code, body := b.do("POST", "/v1/_auth/invite", jsonBody(t, map[string]string{"code": invite, "login": login, "password": goodPassword}))
	if code != http.StatusCreated {
		t.Fatalf("redeem on %s: %d %v", gate, code, body)
	}
	return b
}

func (b *browser) login(gate, login, pass, otp string) (int, map[string]any) {
	b.t.Helper()
	return b.do("POST", "/v1/_auth/complete/"+gate, jsonBody(b.t, map[string]string{"login": login, "password": pass, "otp": otp}))
}

// The whole path: an operator's invite is redeemed on loomcycle's own page,
// which creates a <gate>:<random> user, its login and its password and signs
// the browser in; the login form then needs no tenant; and the code works
// once (RFC FA security row "codes single-use").
func TestPasswordGate_InviteRedeemsOnceAndTheLoginNeedsNoTenant(t *testing.T) {
	f := newGateFixture(t)
	inv := f.inviteCode(t, bAcmeOp, "local", `{"email":"alice@example.com"}`)
	b := f.browser(t)
	if code, info := b.do("POST", "/v1/_auth/invite/info", jsonBody(t, map[string]string{"code": inv})); code != http.StatusOK || info["gate"] != "local" || info["email"] != "alice@example.com" {
		t.Fatalf("info: %d %v", code, info)
	}
	code, body := b.do("POST", "/v1/_auth/invite", jsonBody(t, map[string]string{"code": inv, "login": "Alice@Example.com", "password": goodPassword}))
	if code != http.StatusCreated || body["login"] != "alice@example.com" || body["redirect"] != "/ui/" {
		t.Fatalf("redeem: %d %v", code, body)
	}
	code, subject := b.me()
	if code != http.StatusOK || !strings.HasPrefix(subject, "local:") {
		t.Fatalf("/v1/_me after redeem: %d %q", code, subject)
	}
	u, err := f.st.UserGet(context.Background(), "acme", subject)
	if err != nil || u.AccessMode != "isolated" {
		t.Fatalf("user row = %+v, %v", u, err)
	}
	if code, body := f.browser(t).do("POST", "/v1/_auth/invite", jsonBody(t, map[string]string{"code": inv, "login": "bob@example.com", "password": goodPassword})); code != http.StatusNotFound || body["code"] != "link_invalid" {
		t.Fatalf("second redemption: %d %v", code, body)
	}
	b2 := f.browser(t)
	if code, body := b2.login("local", "ALICE@example.com", goodPassword, ""); code != http.StatusNoContent {
		t.Fatalf("login: %d %v", code, body)
	}
	if code, sub := b2.me(); code != http.StatusOK || sub != subject {
		t.Fatalf("/v1/_me after login: %d %q", code, sub)
	}
	if !slices.Contains(f.audit.actions(), "auth_invite_redeemed") || !slices.Contains(f.audit.actions(), "auth_password_set") {
		t.Fatalf("audit = %v", f.audit.actions())
	}
}

func TestPasswordGate_RedemptionRefusesAWeakOrBreachedPassword(t *testing.T) {
	f := newGateFixture(t)
	inv := f.inviteCode(t, bAcmeOp, "local", `{}`)
	b := f.browser(t)
	// "films+pic+galeries" is in the built-in top-10k (its one entry of 15+
	// characters, the sole-factor minimum).
	for _, c := range []struct{ pw, want string }{{"short-one", "password_too_short"}, {"Films+Pic+Galeries", "password_breached"}, {"Carol@Example.com", "password_is_login"}} {
		code, body := b.do("POST", "/v1/_auth/invite", jsonBody(t, map[string]string{"code": inv, "login": "carol@example.com", "password": c.pw}))
		if code != http.StatusBadRequest || body["code"] != c.want {
			t.Errorf("%q: %d %v, want %s", c.pw, code, body, c.want)
		}
	}
	// The refusals did not consume the invite.
	f.redeemed(t, "local", "carol@example.com", inv)
}

// An unknown login and a wrong password get the same answer, and both spend
// an Argon2id verify (RFC FA security row "no user-existence oracle; timing
// equal for unknown users").
func TestPasswordGate_UnknownLoginAndWrongPasswordAreIndistinguishable(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "dana@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	median := func(login string) (time.Duration, int, map[string]any) {
		var ds []time.Duration
		var code int
		var body map[string]any
		// Three tries each: the per-(source, login) backoff starts after the
		// third, and a throttled answer would be a different one.
		for range 3 {
			start := time.Now()
			code, body = f.browser(t).login("local", login, "a wrong passphrase of some length", "")
			ds = append(ds, time.Since(start))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[1], code, body
	}
	known, kc, kb := median("dana@example.com")
	unknown, uc, ub := median("nobody@example.com")
	if kc != http.StatusUnauthorized || uc != kc || kb["code"] != ub["code"] || kb["error"] != ub["error"] {
		t.Fatalf("answers differ: known %d %v, unknown %d %v", kc, kb, uc, ub)
	}
	if unknown < known*2/5 {
		t.Fatalf("an unknown login answered in %s against %s for a wrong password: it skipped the hash", unknown, known)
	}
}

// Lockout (RFC FA security row "N failures lock"): once the account reaches
// max_failures, even the right password is refused — with the same answer as
// a wrong one — and the lockout is audited.
func TestPasswordGate_TheAccountLocksAtMaxFailures(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "strict3", "erin@example.com", f.inviteCode(t, bAcmeOp, "strict3", `{}`))
	b := f.browser(t)
	for i := range 3 {
		if code, _ := b.login("strict3", "erin@example.com", "the wrong passphrase again", ""); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i+1, code)
		}
	}
	if code, body := b.login("strict3", "erin@example.com", goodPassword, ""); code != http.StatusUnauthorized || body["code"] != "invalid_credentials" {
		t.Fatalf("right password while locked: %d %v", code, body)
	}
	if !slices.Contains(f.audit.actions(), "auth_login_locked") {
		t.Fatalf("lockout not audited: %v", f.audit.actions())
	}
}

// A reset ends every session made from the old password and every delegated
// token of the user (RFC FA security row "a reset ends existing sessions").
func TestPasswordGate_ResetEndsTheUsersSessionsAndTokens(t *testing.T) {
	f := newGateFixture(t)
	old := f.redeemed(t, "local", "frank@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	_, subject := old.me()
	if code, b := f.call(t, bAcmeOp, "POST", "/v1/_users/"+subject+"/tokens", `{}`); code != http.StatusCreated {
		t.Fatalf("mint a user token: %d %v", code, b)
	}
	rst := f.resetCode(t, bAcmeOp, "local", jsonBody(t, map[string]string{"subject": subject}))
	fresh := f.browser(t)
	if code, info := fresh.do("POST", "/v1/_auth/reset/info", jsonBody(t, map[string]string{"code": rst})); code != http.StatusOK || info["login"] != "frank@example.com" {
		t.Fatalf("reset info: %d %v", code, info)
	}
	const newPass = "an entirely different passphrase"
	if code, body := fresh.do("POST", "/v1/_auth/reset", jsonBody(t, map[string]string{"code": rst, "password": newPass})); code != http.StatusOK {
		t.Fatalf("complete: %d %v", code, body)
	}
	if code, _ := old.me(); code != http.StatusUnauthorized {
		t.Fatalf("the pre-reset session still authenticates: %d", code)
	}
	if code, sub := fresh.me(); code != http.StatusOK || sub != subject {
		t.Fatalf("the reset browser is not signed in: %d %q", code, sub)
	}
	rows, _ := f.st.OperatorTokenDefListBySubject(context.Background(), "acme", subject)
	for _, r := range rows {
		if tokenActive(r) {
			t.Fatalf("a delegated token survived the reset: %s", r.DefID)
		}
	}
	if code, _ := f.browser(t).login("local", "frank@example.com", goodPassword, ""); code != http.StatusUnauthorized {
		t.Fatalf("the old password still logs in: %d", code)
	}
	if code, _ := fresh.do("POST", "/v1/_auth/reset", jsonBody(t, map[string]string{"code": rst, "password": newPass + "!"})); code != http.StatusNotFound {
		t.Fatalf("a used reset link worked again: %d", code)
	}
}

// Redemption is throttled per code and per source (RFC FA security row).
func TestPasswordGate_RedemptionIsThrottledPerCodeAndPerSource(t *testing.T) {
	f := newGateFixture(t)
	inv := f.inviteCode(t, bAcmeOp, "mfa", `{}`)
	b := f.browser(t)
	_, info := b.do("POST", "/v1/_auth/invite/info", jsonBody(t, map[string]string{"code": inv}))
	enrol := info["totp"].(map[string]any)["enrolment"].(string)
	for i := range password.CodeMaxFailures {
		code, _ := b.do("POST", "/v1/_auth/invite", jsonBody(t, map[string]string{"code": inv, "login": "gina@example.com",
			"password": goodPassword, "totp_enrolment": enrol, "totp_code": "000000"}))
		if code != http.StatusBadRequest {
			t.Fatalf("bad code %d: %d", i+1, code)
		}
	}
	if code, body := b.do("POST", "/v1/_auth/invite", jsonBody(t, map[string]string{"code": inv})); code != http.StatusTooManyRequests {
		t.Fatalf("after %d bad codes on one invite: %d %v", password.CodeMaxFailures, code, body)
	}

	other := f.browser(t)
	for range password.SourceFree + 1 {
		fake, _, _ := password.NewCode(password.InvitePrefix)
		other.do("POST", "/v1/_auth/invite/info", jsonBody(t, map[string]string{"code": fake}))
	}
	good := f.inviteCode(t, bAcmeOp, "local", `{}`)
	if code, _ := other.do("POST", "/v1/_auth/invite/info", jsonBody(t, map[string]string{"code": good})); code != http.StatusTooManyRequests {
		t.Fatalf("a source that guessed codes is not held back: %d", code)
	}
}

// A gate that requires MFA enrols TOTP at redemption; a login then needs a
// code, a code is accepted once per step (replay refused), and a recovery code
// works once.
func TestPasswordGate_RequiredTOTPIsEnrolledAndEnforcedWithoutReplay(t *testing.T) {
	f := newGateFixture(t)
	inv := f.inviteCode(t, bAcmeOp, "mfa", `{}`)
	b := f.browser(t)
	if code, body := b.do("POST", "/v1/_auth/invite", jsonBody(t, map[string]string{"code": inv, "login": "hank@example.com", "password": "eight+ok"})); code != http.StatusBadRequest || body["code"] != "totp_required" {
		t.Fatalf("no enrolment: %d %v", code, body)
	}
	_, info := b.do("POST", "/v1/_auth/invite/info", jsonBody(t, map[string]string{"code": inv}))
	totp := info["totp"].(map[string]any)
	secret, err := totpB32Decode(totp["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	now := password.TOTPStep(time.Now())
	code, body := b.do("POST", "/v1/_auth/invite", jsonBody(t, map[string]string{"code": inv, "login": "hank@example.com",
		"password": "eight+ok", "totp_enrolment": totp["enrolment"].(string), "totp_code": password.TOTPCode(secret, now)}))
	if code != http.StatusCreated {
		t.Fatalf("redeem with TOTP (8-char password allowed with MFA): %d %v", code, body)
	}
	recovery := body["recovery_codes"].([]any)
	if len(recovery) != password.RecoveryCodeCount {
		t.Fatalf("recovery codes = %v", recovery)
	}
	c := f.browser(t)
	if code, body := c.login("mfa", "hank@example.com", "eight+ok", ""); code != http.StatusUnauthorized || body["code"] != "otp_required" {
		t.Fatalf("no otp: %d %v", code, body)
	}
	next := password.TOTPCode(secret, now+1)
	if code, body := c.login("mfa", "hank@example.com", "eight+ok", next); code != http.StatusNoContent {
		t.Fatalf("otp: %d %v", code, body)
	}
	if code, body := f.browser(t).login("mfa", "hank@example.com", "eight+ok", next); code != http.StatusUnauthorized || body["code"] != "invalid_otp" {
		t.Fatalf("replayed otp: %d %v", code, body)
	}
	rc := recovery[0].(string)
	if code, _ := f.browser(t).login("mfa", "hank@example.com", "eight+ok", rc); code != http.StatusNoContent {
		t.Fatalf("recovery code: %d", code)
	}
	if code, _ := f.browser(t).login("mfa", "hank@example.com", "eight+ok", rc); code != http.StatusUnauthorized {
		t.Fatalf("a recovery code worked twice: %d", code)
	}
}

func totpB32Decode(s string) ([]byte, error) {
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
}

// The floor is re-checked when a website's link is used (TOCTOU, the
// coordinator's finding 1a): a reset issued for a floor user who is promoted
// before it is completed is refused and burned — unless the gate allows
// owner recovery, where it goes through, audited as a recovery. The
// promotion here is a config-declared group, which no store write sees, so
// only the redemption-time check can catch it.
func TestPasswordGate_AManagerLinkIsHeldToTheFloorAgainWhenUsed(t *testing.T) {
	for _, tc := range []struct {
		gate string
		ok   bool
	}{{"local", false}, {"cloud", true}} {
		t.Run(tc.gate, func(t *testing.T) {
			f := newGateFixture(t)
			b := f.redeemed(t, tc.gate, "ivy@example.com", f.inviteCode(t, bAcmeOp, tc.gate, `{}`))
			_, subject := b.me()
			rst := f.resetCode(t, bSite, tc.gate, jsonBody(t, map[string]string{"tenant": "acme", "subject": subject}))
			res := aclres.New(f.st, authz.ModeCompat, 0)
			f.srv.SetACLResolver(res)
			static := &authz.Static{Groups: []authz.StaticGroup{{Tenant: "acme", Name: "cfg-admins", Members: []string{subject}}}}
			res.SetStatic(func() *authz.Static { return static })
			code, body := f.browser(t).do("POST", "/v1/_auth/reset", jsonBody(t, map[string]string{"code": rst, "password": "a promoted users new passphrase"}))
			if tc.ok {
				if code != http.StatusOK {
					t.Fatalf("recovery gate: %d %v", code, body)
				}
				ev := lastCapture(t, f.audit, "auth_manage_recovery")
				if !ev.AuthRecovery {
					t.Fatalf("recovery not marked: %+v", ev)
				}
				return
			}
			if code != http.StatusNotFound {
				t.Fatalf("promoted target: %d %v, want the link refused", code, body)
			}
			r, _ := f.st.AuthResetGetByCode(context.Background(), password.HashCode(rst))
			if r.Live(time.Now()) {
				t.Fatal("the refused reset link is still usable")
			}
		})
	}
}

// Finding 1b: raising a user's authority through the store revokes the
// website's outstanding links to it at once.
func TestPasswordGate_PromotingAUserRevokesAManagersOutstandingLinks(t *testing.T) {
	f := newGateFixture(t)
	b := f.redeemed(t, "local", "jack@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	_, subject := b.me()
	rst := f.resetCode(t, bSite, "local", jsonBody(t, map[string]string{"tenant": "acme", "subject": subject}))
	if code, _ := f.call(t, bAcmeOp, "PATCH", "/v1/_users/"+subject, `{"access_mode":"tenant"}`); code != http.StatusOK {
		t.Fatalf("promote: %d", code)
	}
	r, _ := f.st.AuthResetGetByCode(context.Background(), password.HashCode(rst))
	if r.Live(time.Now()) {
		t.Fatal("promotion left the website's reset link live")
	}
}

func lastCapture(t *testing.T, c *captureSink, action string) (ev audit.Event) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.evs) - 1; i >= 0; i-- {
		if c.evs[i].Action == action {
			return c.evs[i]
		}
	}
	t.Fatalf("no %s event in %v", action, c.evs)
	return
}

// A password change re-authenticates, ends every other session (their
// binding is the old password) and keeps this browser signed in.
func TestPasswordGate_ChangingThePasswordNeedsTheOldOneAndEndsOtherSessions(t *testing.T) {
	f := newGateFixture(t)
	a := f.redeemed(t, "local", "kim@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	other := f.browser(t)
	if code, _ := other.login("local", "kim@example.com", goodPassword, ""); code != http.StatusNoContent {
		t.Fatal("second login")
	}
	const newPass = "the kim account passphrase two"
	if code, body := a.do("POST", "/v1/_auth/password", jsonBody(t, map[string]string{"current_password": "not it at all, no", "new_password": newPass})); code != http.StatusUnauthorized || body["code"] != "reauth_failed" {
		t.Fatalf("wrong current: %d %v", code, body)
	}
	if code, body := a.do("POST", "/v1/_auth/password", jsonBody(t, map[string]string{"current_password": goodPassword, "new_password": newPass})); code != http.StatusNoContent {
		t.Fatalf("change: %d %v", code, body)
	}
	if code, _ := a.me(); code != http.StatusOK {
		t.Fatalf("the changing browser lost its session: %d", code)
	}
	if code, _ := other.me(); code != http.StatusUnauthorized {
		t.Fatalf("another session survived the change: %d", code)
	}
}

// The account routes serve only a password-gate session: a bearer, even the
// user's own token, is refused, and no API route accepts a password as basic
// auth.
func TestPasswordGate_AccountRoutesNeedAPasswordSessionAndBasicAuthIsRefused(t *testing.T) {
	f := newGateFixture(t)
	if code, b := f.call(t, bAcmeOp, "POST", "/v1/_auth/password", `{"current_password":"x","new_password":"y"}`); code != http.StatusForbidden || b["code"] != "session_required" {
		t.Fatalf("bearer on the account route: %d %v", code, b)
	}
	f.redeemed(t, "local", "lee@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	req, _ := http.NewRequest("GET", f.ts.URL+"/v1/_me", nil)
	req.SetBasicAuth("lee@example.com", goodPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("basic auth: %d", resp.StatusCode)
	}
}

// Behind a trusted proxy each client is throttled by its own address: one
// client's failures hold back that client, not every client the proxy
// carries (the coordinator's finding: keying on the peer made one attacker
// block every sign-in through a Cloudflare tunnel).
func TestPasswordGate_ThrottleSeesEachClientBehindATrustedProxy(t *testing.T) {
	f := newGateFixture(t)
	f.srv.cfg().ResolvedAuth.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	f.srv.cfg().ResolvedAuth.ClientIPHeader = "X-Client-Ip"
	f.redeemed(t, "local", "nora@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	attempt := func(client, login, pass string) int {
		req, _ := http.NewRequest("POST", f.ts.URL+"/v1/_auth/complete/local", strings.NewReader(jsonBody(t, map[string]string{"login": login, "password": pass})))
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set(uiRequestHeader, "1")
		req.Header.Set("User-Agent", testUA)
		req.Header.Set("X-Client-Ip", client)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := range password.SourceFree + 2 {
		attempt("198.51.100.1", fmt.Sprintf("guess-%d@example.com", i), "a guessed passphrase here")
	}
	if code := attempt("198.51.100.1", "nora@example.com", goodPassword); code != http.StatusTooManyRequests {
		t.Fatalf("the guessing client is not held back: %d", code)
	}
	if code := attempt("198.51.100.2", "nora@example.com", goodPassword); code != http.StatusNoContent {
		t.Fatalf("another client through the same proxy is held back: %d", code)
	}
}

// Removing the authenticator leaves the password as the only factor, so a
// password that was long enough only beside it (8 characters) must be
// replaced in the same request (the coordinator's finding 2).
func TestPasswordGate_RemovingTOTPNeedsAPasswordThatStandsAlone(t *testing.T) {
	f := newGateFixture(t)
	inv := f.inviteCode(t, bAcmeOp, "local", `{}`)
	b := f.browser(t)
	_, info := b.do("POST", "/v1/_auth/invite/info", jsonBody(t, map[string]string{"code": inv}))
	totp := info["totp"].(map[string]any)
	secret, _ := totpB32Decode(totp["secret"].(string))
	step := password.TOTPStep(time.Now())
	code, redeemed := b.do("POST", "/v1/_auth/invite", jsonBody(t, map[string]string{"code": inv, "login": "olga@example.com",
		"password": "eight+ok", "totp_enrolment": totp["enrolment"].(string), "totp_code": password.TOTPCode(secret, step)}))
	if code != http.StatusCreated {
		t.Fatalf("redeem with an optional authenticator: %d %v", code, redeemed)
	}
	otp := password.TOTPCode(secret, step+1)
	if code, body := b.do("DELETE", "/v1/_auth/totp", jsonBody(t, map[string]string{"current_password": "eight+ok", "otp": otp})); code != http.StatusBadRequest || body["code"] != "password_too_weak_alone" {
		t.Fatalf("removal with an 8-character password: %d %v", code, body)
	}
	_, subject := b.me()
	if _, err := f.st.UserTOTPGet(context.Background(), "acme", subject); err != nil {
		t.Fatalf("the refused removal removed the authenticator: %v", err)
	}
	const strong = "a passphrase that stands on its own"
	// The next TOTP step is already spent; a recovery code re-authenticates.
	otp = redeemed["recovery_codes"].([]any)[0].(string)
	if code, body := b.do("DELETE", "/v1/_auth/totp", jsonBody(t, map[string]string{"current_password": "eight+ok", "otp": otp, "new_password": strong})); code != http.StatusNoContent {
		t.Fatalf("removal with a new sole-factor password: %d %v", code, body)
	}
	if code, body := f.browser(t).login("local", "olga@example.com", strong, ""); code != http.StatusNoContent {
		t.Fatalf("sign-in with the new password and no code: %d %v", code, body)
	}
}

// The unauthenticated posts are refused from another site: without the UI
// header, or cross-origin.
func TestPasswordGate_PublicPostsAreSameOriginOnly(t *testing.T) {
	f := newGateFixture(t)
	inv := f.inviteCode(t, bAcmeOp, "local", `{}`)
	for _, path := range []string{"/v1/_auth/invite/info", "/v1/_auth/invite", "/v1/_auth/reset", "/v1/_auth/complete/local"} {
		req, _ := http.NewRequest("POST", f.ts.URL+path, strings.NewReader(jsonBody(t, map[string]string{"code": inv})))
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set(uiRequestHeader, "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s cross-site: %d", path, resp.StatusCode)
		}
	}
	// And the refusals did not touch the invite.
	f.redeemed(t, "local", "mo@example.com", inv)
}
