package http

import (
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate/password"
)

// A reset link for an account with an authenticator app needs the app's
// code, and a link is blocked after CodeMaxFailures wrong ones. Wrong codes
// posted together are checked no faster than posted in turn: as many as the
// link's failures leave room for, and the rest find the link blocked.
func TestPasswordGate_ParallelWrongCodesOnAResetLinkAreBoundedLikeSequentialOnes(t *testing.T) {
	f := newGateFixture(t)
	inv := f.inviteCode(t, bAcmeOp, "mfa", `{}`)
	br := f.browser(t)
	_, info := br.do("POST", "/v1/_auth/invite/info", jsonBody(t, map[string]string{"code": inv}))
	totp := info["totp"].(map[string]any)
	secret, err := totpB32Decode(totp["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if code, body := br.do("POST", "/v1/_auth/invite", jsonBody(t, map[string]string{"code": inv, "login": "finn@example.com",
		"password": "eight+ok", "totp_enrolment": totp["enrolment"].(string), "totp_code": password.TOTPCode(secret, password.TOTPStep(time.Now()))})); code != http.StatusCreated {
		t.Fatalf("redeem with TOTP: %d %v", code, body)
	}
	_, subject := br.me()
	rst := f.resetCode(t, bAcmeOp, "mfa", jsonBody(t, map[string]string{"subject": subject}))
	wrong := wrongTOTP(secret)
	const sent = password.CodeMaxFailures + 10
	b := f.newBurst(t, "codeFailure", "")
	for range sent {
		b.post(nil, "198.51.100.80", "/v1/_auth/reset", map[string]string{"code": rst, "password": "another long passphrase entirely", "otp": wrong})
	}
	b.settle()
	if b.atHold != password.CodeMaxFailures {
		t.Fatalf("%d wrong codes posted at once to a reset link: %d were checked, want the %d the link allows", sent, b.atHold, password.CodeMaxFailures)
	}
	_, by := b.finish()
	if n := int(b.hs.codeFailures.Load()); n != password.CodeMaxFailures || by[http.StatusUnauthorized] != password.CodeMaxFailures || by[http.StatusTooManyRequests] != sent-password.CodeMaxFailures {
		t.Fatalf("%d wrong codes posted at once to a reset link: %d checked, answers %v; want %d checked (401) and %d told to wait (429)",
			sent, n, by, password.CodeMaxFailures, sent-password.CodeMaxFailures)
	}
}

// The same for an invite that enrols an authenticator app: wrong
// confirmation codes posted together are bounded by the invite's allowance.
func TestPasswordGate_ParallelWrongEnrolmentCodesOnAnInviteAreBoundedLikeSequentialOnes(t *testing.T) {
	f := newGateFixture(t)
	inv := f.inviteCode(t, bAcmeOp, "mfa", `{}`)
	br := f.browser(t)
	_, info := br.do("POST", "/v1/_auth/invite/info", jsonBody(t, map[string]string{"code": inv}))
	totp := info["totp"].(map[string]any)
	secret, err := totpB32Decode(totp["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	wrong := wrongTOTP(secret)
	const sent = password.CodeMaxFailures + 10
	b := f.newBurst(t, "codeFailure", "")
	for range sent {
		b.post(nil, "198.51.100.81", "/v1/_auth/invite", map[string]string{"code": inv, "login": "gwen@example.com",
			"password": "eight+ok", "totp_enrolment": totp["enrolment"].(string), "totp_code": wrong})
	}
	b.settle()
	if b.atHold != password.CodeMaxFailures {
		t.Fatalf("%d wrong enrolment codes posted at once to an invite: %d were checked, want the %d the invite allows", sent, b.atHold, password.CodeMaxFailures)
	}
	_, by := b.finish()
	if n := int(b.hs.codeFailures.Load()); n != password.CodeMaxFailures || by[http.StatusBadRequest] != password.CodeMaxFailures || by[http.StatusTooManyRequests] != sent-password.CodeMaxFailures {
		t.Fatalf("%d wrong enrolment codes posted at once to an invite: %d checked, answers %v; want %d checked (400) and %d told to wait (429)",
			sent, n, by, password.CodeMaxFailures, sent-password.CodeMaxFailures)
	}
}
