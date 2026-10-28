package http

import (
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate/password"
)

// A password-policy refusal on a reset link leaves the account's factor
// unspent: the same authenticator code works on the retry.
func TestPasswordGate_APolicyRefusalLeavesTheResetFactorUnspent(t *testing.T) {
	f := newGateFixture(t)
	inv := f.inviteCode(t, bAcmeOp, "mfa", `{}`)
	b := f.browser(t)
	_, info := b.do("POST", "/v1/_auth/invite/info", jsonBody(t, map[string]string{"code": inv}))
	totp := info["totp"].(map[string]any)
	secret, err := totpB32Decode(totp["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	step := password.TOTPStep(time.Now())
	if code, body := b.do("POST", "/v1/_auth/invite", jsonBody(t, map[string]string{"code": inv, "login": "hank@example.com",
		"password": "eight+ok", "totp_enrolment": totp["enrolment"].(string), "totp_code": password.TOTPCode(secret, step)})); code != http.StatusCreated {
		t.Fatalf("redeem: %d %v", code, body)
	}
	_, sub := b.me()
	reset := f.resetCode(t, bAcmeOp, "mfa", jsonBody(t, map[string]string{"subject": sub}))
	otp := password.TOTPCode(secret, step+1)
	holder := f.browser(t)
	if code, out := holder.do("POST", "/v1/_auth/reset", jsonBody(t, map[string]string{"code": reset, "password": "short", "otp": otp})); code != http.StatusBadRequest {
		t.Fatalf("a reset to a too-short password = %d %v, want 400", code, out)
	}
	if code, out := holder.do("POST", "/v1/_auth/reset", jsonBody(t, map[string]string{"code": reset, "password": "nine+okay", "otp": otp})); code != http.StatusOK {
		t.Fatalf("the retry with the same code = %d %v, want 200 (the refusal must not spend it)", code, out)
	}
}
