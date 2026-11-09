package http

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A credential change ends the person's other ways in: app sign-ins
// (refresh-token families), delegated and device-login tokens, and Web UI
// sessions. A reset ends every one of them; a self-service change keeps only
// the browser that made it.

// seedAppSignIn writes a live app sign-in (a refresh-token family) for
// (acme, subject), as an OAuth code exchange would.
func seedAppSignIn(t *testing.T, st store.Store, familyID, subject string) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.OAuthFamilyCreate(context.Background(), store.OAuthFamilyRow{FamilyID: familyID, ClientID: "board",
		TenantID: "acme", Subject: subject, Gate: "local", CreatedAt: now, LastUsedAt: now,
		IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

// seedForeignSession writes a live Web UI session of (acme, subject) that no
// password backs (a passkey or identity-gate sign-in): the password's own
// binding cannot end it, only an explicit revoke can.
func seedForeignSession(t *testing.T, st store.Store, hash, subject string) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.UISessionCreate(context.Background(), store.UISessionRow{SessionHash: hash, TenantID: "acme", Subject: subject,
		Gate: "sso", CredKind: "token", CredRef: "other", CredMAC: "x", CreatedAt: now, LastSeenAt: now,
		IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func assertFamilyRevoked(t *testing.T, st store.Store, familyID, after string) {
	t.Helper()
	fam, err := st.OAuthFamilyGet(context.Background(), familyID)
	if err != nil || fam.RevokedAt.IsZero() {
		t.Fatalf("an app sign-in survived %s: revoked_at=%v err=%v", after, fam.RevokedAt, err)
	}
}

func assertSessionRevoked(t *testing.T, st store.Store, hash, after string) {
	t.Helper()
	row, err := st.UISessionGet(context.Background(), hash)
	if err != nil || row.RevokedAt.IsZero() {
		t.Fatalf("a Web UI session survived %s: revoked_at=%v err=%v", after, row.RevokedAt, err)
	}
}

func assertNoActiveDelegatedToken(t *testing.T, st store.Store, subject, after string) {
	t.Helper()
	rows, err := st.OperatorTokenDefListBySubject(context.Background(), "acme", subject)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if tokenActive(r) {
			t.Fatalf("a delegated token survived %s: %s", after, r.DefID)
		}
	}
}

func TestPasswordReset_EndsAppSignInsAndEverySession(t *testing.T) {
	f := newGateFixture(t)
	old := f.redeemed(t, "local", "rita@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	_, subject := old.me()
	seedAppSignIn(t, f.st, "fam-reset", subject)
	seedForeignSession(t, f.st, "sess-reset-sso", subject)
	if code, b := f.call(t, bAcmeOp, "POST", "/v1/_users/"+subject+"/tokens", `{}`); code != http.StatusCreated {
		t.Fatalf("mint a user token: %d %v", code, b)
	}

	rst := f.resetCode(t, bAcmeOp, "local", jsonBody(t, map[string]string{"subject": subject}))
	fresh := f.browser(t)
	if code, body := fresh.do("POST", "/v1/_auth/reset", jsonBody(t, map[string]string{"code": rst, "password": "a reset passphrase for rita"})); code != http.StatusOK {
		t.Fatalf("complete the reset: %d %v", code, body)
	}

	assertFamilyRevoked(t, f.st, "fam-reset", "a password reset")
	assertSessionRevoked(t, f.st, "sess-reset-sso", "a password reset")
	assertNoActiveDelegatedToken(t, f.st, subject, "a password reset")
	if code, sub := fresh.me(); code != http.StatusOK || sub != subject {
		t.Fatalf("the browser that reset is not signed in: %d %q", code, sub)
	}
}

func TestPasswordChange_EndsAppSignInsAndTokensButKeepsThisBrowser(t *testing.T) {
	f := newGateFixture(t)
	a := f.redeemed(t, "local", "sam@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	_, subject := a.me()
	seedAppSignIn(t, f.st, "fam-change", subject)
	seedForeignSession(t, f.st, "sess-change-sso", subject)
	// A delegated token stands in for a device-login token: the device gate
	// issues exactly this kind of row (issueGateToken).
	if code, b := f.call(t, bAcmeOp, "POST", "/v1/_users/"+subject+"/tokens", `{}`); code != http.StatusCreated {
		t.Fatalf("mint a user token: %d %v", code, b)
	}

	if code, body := a.do("POST", "/v1/_auth/password", jsonBody(t, map[string]string{"current_password": goodPassword, "new_password": "the sam account passphrase two"})); code != http.StatusNoContent {
		t.Fatalf("change: %d %v", code, body)
	}

	assertFamilyRevoked(t, f.st, "fam-change", "a password change")
	assertSessionRevoked(t, f.st, "sess-change-sso", "a password change")
	assertNoActiveDelegatedToken(t, f.st, subject, "a password change")
	if code, _ := a.me(); code != http.StatusOK {
		t.Fatalf("the browser that changed the password lost its session: %d", code)
	}
}

// signInPrimitives are the calls that end one kind of sign-in, and the only
// functions allowed to make each: everything else asks endSubjectCredentials.
var signInPrimitives = map[string]string{
	"endSubjectAppSignIns":         "endSubjectCredentials",
	"retireDelegatedUserTokens":    "endSubjectCredentials",
	"UISessionRevokeSubject":       "endSubjectCredentials",
	"OAuthFamilyRevokeSubject":     "endSubjectAppSignIns",
	"OAuthTokenRetireEndedSignIns": "endSubjectAppSignIns",
}

// credentialChanges are the store writes that change or end how a person
// signs in. A function making one must end the person's other sign-ins
// through endSubjectCredentials (directly or through a function that does).
var credentialChanges = map[string]bool{
	"UserCredentialSet":    true,
	"UserCredentialDelete": true,
	"AuthResetComplete":    true,
	"AuthRemovalStart":     true,
	"UserDelete":           true,
}

// TestSignInTeardown_EveryCallerGoesThroughTheHelper is derived from this
// package's source: no function but endSubjectCredentials ends one kind of
// sign-in by itself, and every function that changes a credential (reset,
// change, removal, delete, disable) reaches endSubjectCredentials. A new
// credential path, or a new kind of sign-in ended by hand, fails here.
func TestSignInTeardown_EveryCallerGoesThroughTheHelper(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]map[string]bool{} // function -> names it calls
	var problems []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			owner := fn.Name.Name
			if calls[owner] == nil {
				calls[owner] = map[string]bool{}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					callee = fun.Name
				case *ast.SelectorExpr:
					callee = fun.Sel.Name
				default:
					return true
				}
				calls[owner][callee] = true
				if allowed, ok := signInPrimitives[callee]; ok && allowed != owner {
					problems = append(problems, fmt.Sprintf("%s: %s calls %s; end sign-ins through endSubjectCredentials",
						fset.Position(call.Pos()), owner, callee))
				}
				return true
			})
		}
	}
	// reaches: the functions that call endSubjectCredentials, directly or
	// through another function of this package that does.
	reaches := map[string]bool{"endSubjectCredentials": true}
	for changed := true; changed; {
		changed = false
		for fn, callees := range calls {
			if reaches[fn] {
				continue
			}
			for c := range callees {
				if reaches[c] {
					reaches[fn], changed = true, true
					break
				}
			}
		}
	}
	var changers []string
	for fn, callees := range calls {
		for c := range callees {
			if credentialChanges[c] {
				changers = append(changers, fn)
				if !reaches[fn] {
					problems = append(problems, fmt.Sprintf("%s changes a credential (%s) but never reaches endSubjectCredentials", fn, c))
				}
			}
		}
	}
	// The census must find the paths it exists for, or it asserts nothing.
	for _, want := range []string{"handleChangePassword", "handleCompleteReset", "startRemovalWith", "handleDeleteUser"} {
		if !slices.Contains(changers, want) {
			problems = append(problems, "census did not find "+want+" among the credential changers")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// totpDeleteFails is a store whose UserTOTPDelete faults: the step after
// the new password is stored.
type totpDeleteFails struct{ store.Store }

func (totpDeleteFails) UserTOTPDelete(context.Context, string, string) (bool, error) {
	return false, errors.New("injected: totp delete failed")
}

// A new password set while removing the authenticator ends the other
// sign-ins as soon as it is stored: a later step that fails must not leave
// them live under a password that already changed.
func TestTOTPRemove_ANewPasswordEndsSignInsEvenWhenALaterStepFails(t *testing.T) {
	f := newGateFixture(t)
	a := f.redeemed(t, "local", "tess@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	_, subject := a.me()
	seedAppSignIn(t, f.st, "fam-totp", subject)
	seedForeignSession(t, f.st, "sess-totp-sso", subject)
	f.srv.store = totpDeleteFails{f.st}
	code, body := a.do("DELETE", "/v1/_auth/totp", jsonBody(t, map[string]string{
		"current_password": goodPassword, "new_password": "a long enough sole-factor passphrase"}))
	if code != http.StatusInternalServerError {
		t.Fatalf("precondition: the injected fault did not fail the request: %d %v", code, body)
	}
	assertFamilyRevoked(t, f.st, "fam-totp", "a new password whose TOTP removal then failed")
	assertSessionRevoked(t, f.st, "sess-totp-sso", "a new password whose TOTP removal then failed")
}

// An operator's credential removal that faults after the password went
// still ends the person's sign-ins.
func TestCredentialRemoval_EndsSignInsEvenWhenALaterStepFails(t *testing.T) {
	f := newGateFixture(t)
	a := f.redeemed(t, "local", "uma@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	_, subject := a.me()
	seedAppSignIn(t, f.st, "fam-removal", subject)
	f.srv.store = totpDeleteFails{f.st}
	if code, body := f.call(t, bAcmeOp, "DELETE", "/v1/_authgate/local/credentials/"+subject, ""); code != http.StatusInternalServerError {
		t.Fatalf("precondition: the injected fault did not fail the request: %d %v", code, body)
	}
	assertFamilyRevoked(t, f.st, "fam-removal", "a credential removal that then failed")
}
