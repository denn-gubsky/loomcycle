package http

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate/password"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// heldStore holds every request at one store read, after the read and before
// what the handler does with it, until the test lets them all go: the
// requests it holds are in progress together whatever the machine's speed.
// It also counts the failures the handlers record, and makes every hold a
// failure sets an hour long, so that whether a later request finds the hold
// in force does not depend on how fast this machine hashes.
type heldStore struct {
	store.Store
	// holdAt names where to hold: "credential" (the sign-in's read of the
	// account's password row), "identity" (its lookup of the login), or
	// "codeFailure" (a link's wrong second factor, about to be recorded).
	holdAt  string
	held    chan struct{}
	release chan struct{}
	// panicAt names a read that panics, once.
	panicAt  string
	panicked atomic.Bool
	// accountFailures counts the failures recorded on an account: each is a
	// wrong password, or a wrong second factor, that was acted on.
	accountFailures atomic.Int64
	// codeFailures counts the failures recorded on an invite or reset code:
	// each is a wrong second-factor code that was checked.
	codeFailures atomic.Int64
}

func (h *heldStore) hold(at string) {
	if h.panicAt == at && h.panicked.CompareAndSwap(false, true) {
		panic("heldStore: injected store panic at " + at)
	}
	if h.holdAt != at {
		return
	}
	select {
	case <-h.release:
		return
	default:
	}
	h.held <- struct{}{}
	<-h.release
}

func hourLong(blockFor func(int) time.Duration) func(int) time.Duration {
	return func(n int) time.Duration {
		if blockFor(n) > 0 {
			return time.Hour
		}
		return 0
	}
}

func (h *heldStore) UserIdentityGet(ctx context.Context, gate, issuer, externalSubject string) (store.UserIdentityRow, error) {
	row, err := h.Store.UserIdentityGet(ctx, gate, issuer, externalSubject)
	h.hold("identity")
	return row, err
}

func (h *heldStore) UserCredentialGet(ctx context.Context, tenantID, subject, kind string) (store.UserCredentialRow, error) {
	row, err := h.Store.UserCredentialGet(ctx, tenantID, subject, kind)
	h.hold("credential")
	return row, err
}

func (h *heldStore) AuthThrottleGet(ctx context.Context, key string, now time.Time) (int, time.Time, error) {
	h.hold("throttle")
	return h.Store.AuthThrottleGet(ctx, key, now)
}

func (h *heldStore) UserCredentialRecordFailure(ctx context.Context, tenantID, subject, kind string, now time.Time, lockFor func(int) time.Duration) (int, time.Time, error) {
	h.accountFailures.Add(1)
	return h.Store.UserCredentialRecordFailure(ctx, tenantID, subject, kind, now, hourLong(lockFor))
}

func (h *heldStore) AuthThrottleRecordFailure(ctx context.Context, key string, now time.Time, window time.Duration, blockFor func(int) time.Duration) (int, time.Time, error) {
	if strings.HasPrefix(key, "code:") {
		h.hold("codeFailure")
		h.codeFailures.Add(1)
	}
	return h.Store.AuthThrottleRecordFailure(ctx, key, now, window, hourLong(blockFor))
}

// burst is a set of requests sent to a server of its own whose store holds
// them (heldStore) and whose wait for a place is in the test's hands: a
// request that starts to wait says so on parked, and gives up when the test
// closes giveUp, never by the clock.
type burst struct {
	t        *testing.T
	f        *gateFixture
	hs       *heldStore
	ts       *httptest.Server
	parked   chan struct{}
	giveUp   chan time.Time
	answered chan int
	hung     <-chan time.Time
	sent     int
	// settled counts the requests known to be held, waiting or answered.
	settled int
	// atHold and waiting are how many requests are held at the store read
	// and waiting for a place; byStatus is the answers so far.
	atHold, waiting int
	byStatus        map[int]int
	releaseOnce     sync.Once
	giveUpOnce      sync.Once
}

// release lets the held requests go.
func (b *burst) release() { b.releaseOnce.Do(func() { close(b.hs.release) }) }

// endWaits is the wait bound passing for every request that waits.
func (b *burst) endWaits() { b.giveUpOnce.Do(func() { close(b.giveUp) }) }

func (f *gateFixture) newBurst(t *testing.T, holdAt, panicAt string) *burst {
	t.Helper()
	b := &burst{t: t, f: f, parked: make(chan struct{}, 256), giveUp: make(chan time.Time), answered: make(chan int, 256),
		// A safety net for a hung test, not a measurement: nothing here
		// depends on how long anything takes.
		hung: time.After(3 * time.Minute), byStatus: map[int]int{}}
	f.srv.cfg().ResolvedAuth.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	f.srv.cfg().ResolvedAuth.ClientIPHeader = "X-Client-Ip"
	b.hs = &heldStore{Store: f.st, holdAt: holdAt, panicAt: panicAt, held: make(chan struct{}, 256), release: make(chan struct{})}
	f.srv.store = b.hs
	f.srv.checks.after = func(time.Duration) <-chan time.Time {
		b.parked <- struct{}{}
		return b.giveUp
	}
	// A server of its own, started after the store was wrapped.
	b.ts = httptest.NewServer(f.srv.Mux())
	t.Cleanup(b.ts.Close)
	// Before the server closes (cleanups run last-first): a test that failed
	// part-way leaves requests held or waiting, and Close waits for them.
	t.Cleanup(func() { b.release(); b.endWaits() })
	return b
}

// post sends one request without waiting for its answer. client is the
// address it comes from through the trusted proxy ("" for the proxy's own);
// hc carries a session's cookies when the request needs one.
func (b *burst) post(hc *http.Client, client, path string, body any) {
	b.t.Helper()
	b.sent++
	raw := jsonBody(b.t, body)
	if hc == nil {
		hc = http.DefaultClient
	}
	go func() {
		req, err := http.NewRequest("POST", b.ts.URL+path, strings.NewReader(raw))
		if err != nil {
			b.answered <- 0
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set(uiRequestHeader, "1")
		req.Header.Set("User-Agent", testUA)
		if client != "" {
			req.Header.Set("X-Client-Ip", client)
		}
		resp, err := hc.Do(req)
		if err != nil {
			b.answered <- 0
			return
		}
		resp.Body.Close()
		b.answered <- resp.StatusCode
	}()
}

// signIn is one sign-in: who it says it is, from which client address, with
// which password (and second-factor code), at which gate ("" is local).
type signIn struct{ client, login, pass, otp, gate string }

func (b *burst) signIn(a signIn) {
	b.t.Helper()
	gate := a.gate
	if gate == "" {
		gate = "local"
	}
	b.post(nil, a.client, "/v1/_auth/complete/"+gate, map[string]string{"login": a.login, "password": a.pass, "otp": a.otp})
}

func (b *burst) signIns(as []signIn) {
	b.t.Helper()
	for _, a := range as {
		b.signIn(a)
	}
}

// settle waits until every request sent so far is held at the store read,
// waiting for a place, or answered.
func (b *burst) settle() {
	b.t.Helper()
	for b.settled < b.sent {
		select {
		case <-b.hs.held:
			b.atHold++
		case <-b.parked:
			b.waiting++
		case code := <-b.answered:
			b.byStatus[code]++
		case <-b.hung:
			b.t.Fatalf("only %d of %d requests were held, waiting or answered", b.settled, b.sent)
		}
		b.settled++
	}
}

func (b *burst) answers() int {
	n := 0
	for _, c := range b.byStatus {
		n += c
	}
	return n
}

// next waits for one more answer and returns its status.
func (b *burst) next() int {
	b.t.Helper()
	select {
	case code := <-b.answered:
		b.byStatus[code]++
		return code
	case <-b.hung:
		b.t.Fatalf("only %d of %d requests were answered", b.answers(), b.sent)
		return 0
	}
}

// finish lets the held requests go, waits for every answer, and checks that
// no place is left taken. It returns how many failures were recorded on an
// account, and the answers by status.
func (b *burst) finish() (accountFailures int, byStatus map[int]int) {
	b.t.Helper()
	b.settle()
	b.release()
	for b.answers() < b.sent {
		b.next()
	}
	// A handler that keeps a place until it returns may answer first; Close
	// waits for the handlers.
	b.ts.Close()
	b.noPlacesLeft()
	return int(b.hs.accountFailures.Load()), b.byStatus
}

// noPlacesLeft: every place and every wait was given back.
func (b *burst) noPlacesLeft() {
	b.t.Helper()
	p := &b.f.srv.checks
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, k := range p.keys {
		b.t.Errorf("a place was not given back: key %q, %d held, %d waiting", key, k.held, k.waiting)
	}
}

const wrongPassword = "a guessed passphrase here"

// sameGuess is n wrong-password sign-ins for one login from one address.
func sameGuess(n int, client, login string) []signIn {
	out := make([]signIn, n)
	for i := range out {
		out[i] = signIn{client: client, login: login, pass: wrongPassword}
	}
	return out
}

// pastFailures records n failures on a throttle key ten minutes ago: counted
// in the current window, and any hold they set has ended.
func (f *gateFixture) pastFailures(t *testing.T, key string, n int, blockFor func(int) time.Duration) {
	t.Helper()
	for range n {
		if _, _, err := f.st.AuthThrottleRecordFailure(context.Background(), key, time.Now().Add(-10*time.Minute), password.SourceWindow, blockFor); err != nil {
			t.Fatal(err)
		}
	}
}

// pastAccountFailures records n failures on a login's account an hour ago:
// counted (the count has no window), and any lock they set has ended.
func (f *gateFixture) pastAccountFailures(t *testing.T, gate, login string, n int) {
	t.Helper()
	ctx := context.Background()
	id, err := f.st.UserIdentityGet(ctx, gate, "", login)
	if err != nil {
		t.Fatal(err)
	}
	th := f.srv.pwGates.p.Load().byName[gate].Throttle
	for range n {
		if _, _, err := f.st.UserCredentialRecordFailure(ctx, id.TenantID, id.Subject, store.CredentialKindPassword, time.Now().Add(-time.Hour), th.AccountLockFor); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *gateFixture) throttleFailures(t *testing.T, key string) int {
	t.Helper()
	n, _, err := f.st.AuthThrottleGet(context.Background(), key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Sign-ins sent together are paced like sign-ins sent one after another. One
// client guessing one login gets, in a row, the three free failures and the
// one that sets the first hold; sent at once, the same four are verified, one
// at a time, and the rest, which waited for a place, find the hold and are
// told to wait.
func TestPasswordGate_ParallelGuessesAreBoundedLikeSequentialOnes(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "olga@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	const sent = 12
	b := f.newBurst(t, "credential", "")
	b.signIns(sameGuess(sent, "198.51.100.7", "olga@example.com"))
	b.settle()
	if b.atHold != 1 || b.waiting != sent-1 {
		t.Fatalf("%d guesses sent at once: %d being checked, %d waiting; want 1 and %d", sent, b.atHold, b.waiting, sent-1)
	}
	verified, by := b.finish()
	if verified != 4 || by[http.StatusUnauthorized] != 4 || by[http.StatusTooManyRequests] != sent-4 {
		t.Fatalf("%d guesses sent at once: %d verified, answers %v; want 4 verified (401) and %d told to wait (429)", sent, verified, by, sent-4)
	}
}

// Once a client has been held back on a login, the end of the hold admits one
// guess, whose failure sets the next, longer hold. Guesses sent together at
// that moment must not all be that one guess; and only so many wait for the
// place, the rest are refused at once.
func TestPasswordGate_ParallelGuessesAfterAHoldEndsAreOneGuess(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "pavel@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	pg := f.srv.pwGates.p.Load()
	th := pg.byName["local"].Throttle
	f.pastFailures(t, pg.sourceLoginKey("local", "198.51.100.8", "pavel@example.com"), 6, th.SourceLoginBlockFor)
	const sent = 12
	b := f.newBurst(t, "credential", "")
	b.signIns(sameGuess(sent, "198.51.100.8", "pavel@example.com"))
	b.settle()
	overCap := sent - 1 - maxCheckWaiters
	if b.atHold != 1 || b.waiting != maxCheckWaiters || b.byStatus[http.StatusTooManyRequests] != overCap {
		t.Fatalf("%d guesses sent at once after a hold ended: %d in a place, %d waiting, answers %v; want 1 in a place, %d waiting, %d refused at once",
			sent, b.atHold, b.waiting, b.byStatus, maxCheckWaiters, overCap)
	}
	verified, by := b.finish()
	if verified != 1 || by[http.StatusUnauthorized] != 1 || by[http.StatusTooManyRequests] != sent-1 {
		t.Fatalf("%d guesses sent at once after a hold ended: %d verified, answers %v; want 1 verified and %d told to wait", sent, verified, by, sent-1)
	}
}

// The account's own lock bounds guesses from any number of addresses: an
// account past max_failures admits one guess when its lock ends. Guesses sent
// together from different addresses are still one guess: the others wait
// their turn, find the account locked, and are answered exactly as a wrong
// password is (a locked account never says so).
func TestPasswordGate_ParallelGuessesFromManySourcesCannotOutrunTheAccountLock(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "rita@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	f.pastAccountFailures(t, "local", "rita@example.com", f.srv.pwGates.p.Load().byName["local"].Throttle.MaxFailures)
	// One being checked and as many waiting as a login lets wait.
	const sent = 1 + maxCheckWaiters
	b := f.newBurst(t, "credential", "")
	for i := range sent {
		b.signIn(signIn{client: fmt.Sprintf("198.51.100.%d", 20+i), login: "rita@example.com", pass: wrongPassword})
	}
	verified, by := b.finish()
	if verified != 1 || by[http.StatusUnauthorized] != sent {
		t.Fatalf("%d guesses sent at once from %d addresses at a locked-out account: %d verified, answers %v; want 1 verified and every answer 401", sent, sent, verified, by)
	}
}

// An account with a login at two gates has two turns, one per login. Its lock
// still admits one guess when it ends, whichever logins the guesses come
// through.
func TestPasswordGate_ParallelGuessesThroughTwoLoginsOfOneAccountAreOneGuess(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "ines@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	id, err := f.st.UserIdentityGet(context.Background(), "local", "", "ines@example.com")
	if err != nil {
		t.Fatal(err)
	}
	f.addLogin(t, id.TenantID, id.Subject, "cloud", "ines.cloud@example.com")
	f.pastAccountFailures(t, "local", "ines@example.com", f.srv.pwGates.p.Load().byName["local"].Throttle.MaxFailures)
	b := f.newBurst(t, "credential", "")
	b.signIn(signIn{client: "198.51.100.30", login: "ines@example.com", pass: wrongPassword})
	b.signIn(signIn{client: "198.51.100.31", login: "ines.cloud@example.com", pass: wrongPassword, gate: "cloud"})
	b.settle()
	if b.atHold != 2 {
		t.Fatalf("%d at the account's row, want both (each login has its own turn)", b.atHold)
	}
	verified, by := b.finish()
	if verified != 1 || by[http.StatusUnauthorized] != 2 {
		t.Fatalf("two guesses at once through two logins of a locked-out account: %d verified, answers %v; want 1 verified and both answered 401", verified, by)
	}
}

// One address guessing many logins is paced by the address's own count: past
// its free failures, one guess per hold, however many logins it tries at once.
func TestPasswordGate_ParallelGuessesAcrossLoginsAreBoundedPerSource(t *testing.T) {
	f := newGateFixture(t)
	const sent = 6
	attempts := make([]signIn, sent)
	for i := range attempts {
		login := fmt.Sprintf("spray-%d@example.com", i)
		f.redeemed(t, "local", login, f.inviteCode(t, bAcmeOp, "local", `{}`))
		attempts[i] = signIn{client: "198.51.100.9", login: login, pass: wrongPassword}
	}
	pg := f.srv.pwGates.p.Load()
	f.pastFailures(t, pg.sourceKey("local", "198.51.100.9"), password.SourceFree, pg.byName["local"].Throttle.SourceBlockFor)
	b := f.newBurst(t, "credential", "")
	b.signIns(attempts)
	verified, by := b.finish()
	if verified != 1 || by[http.StatusUnauthorized] != 1 || by[http.StatusTooManyRequests] != sent-1 {
		t.Fatalf("%d logins guessed at once from an address past its free failures: %d verified, answers %v; want 1 verified and %d told to wait", sent, verified, by, sent-1)
	}
}

// The step-up a password change takes is the same check: wrong current
// passwords sent together from one session are paced like a sign-in's.
func TestPasswordGate_ParallelWrongStepUpsAreBoundedLikeSignIns(t *testing.T) {
	f := newGateFixture(t)
	br := f.redeemed(t, "local", "uma@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	const sent = 12
	// Held at the login lookup: the session check of each request reads the
	// password row too, before the step-up does.
	b := f.newBurst(t, "identity", "")
	for range sent {
		b.post(br.hc, "", "/v1/_auth/password", map[string]string{"current_password": wrongPassword, "new_password": "another long passphrase entirely"})
	}
	verified, by := b.finish()
	if verified != 4 || by[http.StatusUnauthorized] != 4 || by[http.StatusTooManyRequests] != sent-4 {
		t.Fatalf("%d wrong step-ups sent at once: %d verified, answers %v; want 4 verified (401) and %d told to wait (429)", sent, verified, by, sent-4)
	}
}

// A second-factor code is a guess too: with the right password, wrong codes
// sent together are acted on no faster than wrong codes sent in turn.
func TestPasswordGate_ParallelWrongSecondFactorsAreBoundedLikeSequentialOnes(t *testing.T) {
	f := newGateFixture(t)
	inv := f.inviteCode(t, bAcmeOp, "mfa", `{}`)
	br := f.browser(t)
	_, info := br.do("POST", "/v1/_auth/invite/info", jsonBody(t, map[string]string{"code": inv}))
	totp := info["totp"].(map[string]any)
	secret, err := totpB32Decode(totp["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	step := password.TOTPStep(time.Now())
	if code, body := br.do("POST", "/v1/_auth/invite", jsonBody(t, map[string]string{"code": inv, "login": "vera@example.com",
		"password": "eight+ok", "totp_enrolment": totp["enrolment"].(string), "totp_code": password.TOTPCode(secret, step)})); code != http.StatusCreated {
		t.Fatalf("redeem with TOTP: %d %v", code, body)
	}
	wrong := wrongTOTP(secret)
	const sent = 12
	b := f.newBurst(t, "credential", "")
	for range sent {
		b.signIn(signIn{client: "198.51.100.10", login: "vera@example.com", pass: "eight+ok", otp: wrong, gate: "mfa"})
	}
	acted, by := b.finish()
	if acted != 4 || by[http.StatusUnauthorized] != 4 || by[http.StatusTooManyRequests] != sent-4 {
		t.Fatalf("%d wrong codes sent at once with the right password: %d acted on, answers %v; want 4 (401) and %d told to wait (429)", sent, acted, by, sent-4)
	}
}

// wrongTOTP is a six-digit code that is not the secret's in any step near
// now.
func wrongTOTP(secret []byte) string {
	step := password.TOTPStep(time.Now())
	var near []string
	for s := step - 3; s <= step+3; s++ {
		near = append(near, password.TOTPCode(secret, s))
	}
	wrong := "000000"
	for i := 1; slices.Contains(near, wrong); i++ {
		wrong = fmt.Sprintf("%06d", i)
	}
	return wrong
}

// People signing in at the same moment are not held back: right passwords
// sent together, from one address and from several, all sign in.
func TestPasswordGate_ParallelRightPasswordsAllSignIn(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "sam@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	f.redeemed(t, "local", "tess@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	b := f.newBurst(t, "credential", "")
	b.signIns([]signIn{
		{client: "198.51.100.40", login: "sam@example.com", pass: goodPassword},
		{client: "198.51.100.40", login: "sam@example.com", pass: goodPassword},
		{client: "198.51.100.40", login: "tess@example.com", pass: goodPassword},
		{client: "198.51.100.41", login: "tess@example.com", pass: goodPassword},
	})
	failures, by := b.finish()
	if failures != 0 || by[http.StatusNoContent] != b.sent {
		t.Fatalf("right passwords sent at once: %d failures recorded, answers %v; want all %d signed in", failures, by, b.sent)
	}
}

// An address many people share that has used its free failures this hour
// admits one check at a time. Two people signing in at the same moment both
// get in: the second waits for the first, and neither is charged a failure.
func TestPasswordGate_OverlappingRightPasswordsFromABusyAddressBothSignIn(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "wade@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	f.redeemed(t, "local", "xena@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	pg := f.srv.pwGates.p.Load()
	office := pg.sourceKey("local", "198.51.100.50")
	f.pastFailures(t, office, password.SourceFree, pg.byName["local"].Throttle.SourceBlockFor)
	b := f.newBurst(t, "credential", "")
	b.signIns([]signIn{
		{client: "198.51.100.50", login: "wade@example.com", pass: goodPassword},
		{client: "198.51.100.50", login: "xena@example.com", pass: goodPassword},
	})
	b.settle()
	if b.atHold != 1 || b.waiting != 1 {
		t.Fatalf("two sign-ins from an address past its free failures: %d in a place, %d waiting; want one of each", b.atHold, b.waiting)
	}
	failures, by := b.finish()
	if failures != 0 || by[http.StatusNoContent] != 2 {
		t.Fatalf("two right passwords at once from a busy address: %d failures recorded, answers %v; want both signed in", failures, by)
	}
	if n := f.throttleFailures(t, office); n != password.SourceFree {
		t.Fatalf("the address was charged for a right password: %d failures, want the %d it had", n, password.SourceFree)
	}
}

// Someone who mistyped three times a few minutes ago and now submits the
// right password from two tabs at once is signed in on both.
func TestPasswordGate_TheRightPasswordFromTwoTabsAfterTyposSignsInOnBoth(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "yuri@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	pg := f.srv.pwGates.p.Load()
	tabs := pg.sourceLoginKey("local", "198.51.100.51", "yuri@example.com")
	f.pastFailures(t, tabs, 3, pg.byName["local"].Throttle.SourceLoginBlockFor)
	f.pastAccountFailures(t, "local", "yuri@example.com", 3)
	b := f.newBurst(t, "credential", "")
	b.signIns([]signIn{
		{client: "198.51.100.51", login: "yuri@example.com", pass: goodPassword},
		{client: "198.51.100.51", login: "yuri@example.com", pass: goodPassword},
	})
	failures, by := b.finish()
	if failures != 0 || by[http.StatusNoContent] != 2 {
		t.Fatalf("the right password from two tabs after three typos: %d failures recorded, answers %v; want both signed in", failures, by)
	}
	if n := f.throttleFailures(t, tabs); n != 3 {
		t.Fatalf("a right password was charged a failure: %d on the login's counter, want the 3 it had", n)
	}
}

// Right passwords for one account at once from several addresses, more than
// a fresh account's failures leave room for (four): they are checked in turn
// and all sign in, with no address charged a failure.
func TestPasswordGate_RightPasswordsForOneAccountFromManyAddressesAllSignIn(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "zoe@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	const sent = 6
	b := f.newBurst(t, "credential", "")
	for i := range sent {
		b.signIn(signIn{client: fmt.Sprintf("198.51.100.%d", 60+i), login: "zoe@example.com", pass: goodPassword})
	}
	failures, by := b.finish()
	if failures != 0 || by[http.StatusNoContent] != sent {
		t.Fatalf("%d right passwords at once for a fresh account from %d addresses: %d failures recorded, answers %v; want all signed in", sent, sent, failures, by)
	}
	pg := f.srv.pwGates.p.Load()
	for i := range sent {
		if n := f.throttleFailures(t, pg.sourceKey("local", fmt.Sprintf("198.51.100.%d", 60+i))); n != 0 {
			t.Fatalf("address %d was charged %d failures for a right password", i, n)
		}
	}
}

// A request that waited is judged on what the throttle says once it has its
// place. The check it waited for failed and set a hold, so the waiter is told
// to wait, though its own password is right: it was read again, not verified
// on what it read before.
func TestPasswordGate_AWaiterIsRefusedOnceTheCheckItWaitedForSetsAHold(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "abel@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	pg := f.srv.pwGates.p.Load()
	f.pastFailures(t, pg.sourceLoginKey("local", "198.51.100.52", "abel@example.com"), 6, pg.byName["local"].Throttle.SourceLoginBlockFor)
	b := f.newBurst(t, "credential", "")
	b.signIn(signIn{client: "198.51.100.52", login: "abel@example.com", pass: wrongPassword})
	b.settle()
	b.signIn(signIn{client: "198.51.100.52", login: "abel@example.com", pass: goodPassword})
	b.settle()
	if b.atHold != 1 || b.waiting != 1 {
		t.Fatalf("a wrong password, then a right one: %d in a place, %d waiting; want one of each", b.atHold, b.waiting)
	}
	verified, by := b.finish()
	if verified != 1 || by[http.StatusUnauthorized] != 1 || by[http.StatusTooManyRequests] != 1 {
		t.Fatalf("a right password that waited behind a failing guess: %d verified, answers %v; want the guess 401 and the waiter 429", verified, by)
	}
}

// The same on the account, from another address: the right password waits
// its turn behind a guess, and is not acted on once that guess has locked
// the account.
func TestPasswordGate_AWaiterOnTheAccountIsRefusedOnceItIsLocked(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "enid@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	f.pastAccountFailures(t, "local", "enid@example.com", 3)
	b := f.newBurst(t, "credential", "")
	// The guess is sent first and is at the account's row before the right
	// password is sent.
	b.signIn(signIn{client: "198.51.100.70", login: "enid@example.com", pass: wrongPassword})
	b.settle()
	b.signIn(signIn{client: "198.51.100.71", login: "enid@example.com", pass: goodPassword})
	b.settle()
	if b.atHold != 1 || b.waiting != 1 {
		t.Fatalf("a guess, then a right password from another address: %d being checked, %d waiting; want one of each", b.atHold, b.waiting)
	}
	verified, by := b.finish()
	if verified != 1 || by[http.StatusUnauthorized] != 2 {
		t.Fatalf("a right password that waited behind a guess that locked the account: %d failures, answers %v; want one failure and both answered 401", verified, by)
	}
}

// The wait for a place is bounded: when it runs out, the waiter is told to
// wait and try again, while the check it waited for is still in progress.
func TestPasswordGate_AWaitForAPlaceEndsAtItsBound(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "bea@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	f.redeemed(t, "local", "carl@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	pg := f.srv.pwGates.p.Load()
	f.pastFailures(t, pg.sourceKey("local", "198.51.100.53"), password.SourceFree, pg.byName["local"].Throttle.SourceBlockFor)
	b := f.newBurst(t, "credential", "")
	b.signIn(signIn{client: "198.51.100.53", login: "bea@example.com", pass: goodPassword})
	b.settle()
	b.signIn(signIn{client: "198.51.100.53", login: "carl@example.com", pass: goodPassword})
	b.settle()
	if b.atHold != 1 || b.waiting != 1 {
		t.Fatalf("%d in a place, %d waiting; want one of each", b.atHold, b.waiting)
	}
	// The bound passes; the first sign-in is still held.
	b.endWaits()
	if code := b.next(); code != http.StatusTooManyRequests {
		t.Fatalf("the waiter at its bound: %d, want 429", code)
	}
	if _, by := b.finish(); by[http.StatusNoContent] != 1 {
		t.Fatalf("the sign-in that held the place: answers %v, want it signed in", by)
	}
}

// A login's turn is waited for within the same bound, and a sign-in that
// cannot have its turn in time is told to wait — for a login nobody has as
// for one somebody has.
func TestPasswordGate_AWaitForALoginsTurnEndsAtItsBound(t *testing.T) {
	for _, tc := range []struct{ name, login string }{{"an account", "hugo@example.com"}, {"an unknown login", "nobody@example.com"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateFixture(t)
			f.redeemed(t, "local", "hugo@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
			b := f.newBurst(t, "identity", "")
			b.signIn(signIn{client: "198.51.100.92", login: tc.login, pass: wrongPassword})
			b.settle()
			b.signIn(signIn{client: "198.51.100.93", login: tc.login, pass: wrongPassword})
			b.settle()
			if b.atHold != 1 || b.waiting != 1 {
				t.Fatalf("%d being checked, %d waiting; want one of each", b.atHold, b.waiting)
			}
			b.endWaits()
			if code := b.next(); code != http.StatusTooManyRequests {
				t.Fatalf("the sign-in whose wait for its turn ran out: %d, want 429", code)
			}
			if n := f.throttleFailures(t, f.srv.pwGates.p.Load().sourceKey("local", "198.51.100.93")); n != 0 {
				t.Fatalf("it was charged %d failures for a password nobody checked", n)
			}
			if _, by := b.finish(); by[http.StatusUnauthorized] != 1 {
				t.Fatalf("the sign-in that had the turn: answers %v, want 401", by)
			}
		})
	}
}

// A store fault that panics inside a check gives the check's places back
// (the server recovers the panic, so nothing else would): the next sign-in for
// the same keys is not refused for a place nobody holds.
func TestPasswordGate_PlacesAreGivenBackWhenTheStorePanics(t *testing.T) {
	for _, at := range []string{"throttle", "identity", "credential"} {
		t.Run(at, func(t *testing.T) {
			f := newGateFixture(t)
			f.redeemed(t, "local", "dora@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
			pg := f.srv.pwGates.p.Load()
			// One check at a time on every key, so a place left behind
			// would refuse the next sign-in.
			f.pastFailures(t, pg.sourceKey("local", "198.51.100.54"), password.SourceFree, pg.byName["local"].Throttle.SourceBlockFor)
			f.pastFailures(t, pg.sourceLoginKey("local", "198.51.100.54", "dora@example.com"), 3, pg.byName["local"].Throttle.SourceLoginBlockFor)
			f.pastAccountFailures(t, "local", "dora@example.com", 3)
			b := f.newBurst(t, "", at)
			b.signIn(signIn{client: "198.51.100.54", login: "dora@example.com", pass: goodPassword})
			if code := b.next(); code != http.StatusInternalServerError {
				t.Fatalf("the sign-in whose store read panicked: %d, want the recovered panic's 500", code)
			}
			// verifyPassword gives its places back before the answer is
			// written, so this holds as soon as the answer is in.
			b.noPlacesLeft()
			b.signIn(signIn{client: "198.51.100.54", login: "dora@example.com", pass: goodPassword})
			if code := b.next(); code != http.StatusNoContent {
				t.Fatalf("the sign-in after the panic: %d, want 204", code)
			}
		})
	}
}

// Whether a login exists shows in nothing about how overlapping sign-ins for
// it are handled: two wrong passwords at once from two addresses get the same
// answers, the same one of them waits for the other, and each address is
// charged the same, for an unknown login and for an account in every state a
// stranger can put one into: untouched, with some of its free failures used,
// past them, at max_failures, and locked.
func TestPasswordGate_OverlappingSignInsDoNotShowWhetherTheLoginExists(t *testing.T) {
	type seen struct {
		atHold, waited   int
		unauth, tooMany  int
		charged1, charge int
	}
	run := func(t *testing.T, login string, prepare func(f *gateFixture)) seen {
		f := newGateFixture(t)
		if prepare != nil {
			f.redeemed(t, "local", login, f.inviteCode(t, bAcmeOp, "local", `{}`))
			prepare(f)
		}
		// Held at the login lookup, which an unknown login reaches too.
		b := f.newBurst(t, "identity", "")
		b.signIn(signIn{client: "198.51.100.90", login: login, pass: wrongPassword})
		b.settle()
		b.signIn(signIn{client: "198.51.100.91", login: login, pass: wrongPassword})
		b.settle()
		out := seen{atHold: b.atHold}
		_, by := b.finish()
		// Every request that waited said so once, during the burst or
		// after the held one was let go.
		out.waited = b.waiting + len(b.parked)
		out.unauth, out.tooMany = by[http.StatusUnauthorized], by[http.StatusTooManyRequests]
		pg := f.srv.pwGates.p.Load()
		out.charged1 = f.throttleFailures(t, pg.sourceKey("local", "198.51.100.90"))
		out.charge = f.throttleFailures(t, pg.sourceKey("local", "198.51.100.91"))
		return out
	}
	unknown := run(t, "nobody@example.com", nil)
	for name, prepare := range map[string]func(f *gateFixture){
		"untouched":                func(*gateFixture) {},
		"with one failure":         func(f *gateFixture) { f.pastAccountFailures(t, "local", "known@example.com", 1) },
		"at its last free failure": func(f *gateFixture) { f.pastAccountFailures(t, "local", "known@example.com", 3) },
		"past its free failures, its lock ended": func(f *gateFixture) {
			f.pastAccountFailures(t, "local", "known@example.com", 6)
		},
		"at max_failures, its lock ended": func(f *gateFixture) {
			f.pastAccountFailures(t, "local", "known@example.com", f.srv.pwGates.p.Load().byName["local"].Throttle.MaxFailures)
		},
		"locked": func(f *gateFixture) {
			ctx := context.Background()
			id, err := f.st.UserIdentityGet(ctx, "local", "", "known@example.com")
			if err != nil {
				t.Fatal(err)
			}
			th := f.srv.pwGates.p.Load().byName["local"].Throttle
			for range th.MaxFailures {
				if _, _, err := f.st.UserCredentialRecordFailure(ctx, id.TenantID, id.Subject, store.CredentialKindPassword, time.Now(), th.AccountLockFor); err != nil {
					t.Fatal(err)
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := run(t, "known@example.com", prepare); got != unknown {
				t.Fatalf("an account %s: %+v; an unknown login: %+v", name, got, unknown)
			}
		})
	}
	// And what they all do: one check at a time for a login, the other
	// waiting its turn, each answered and charged as a wrong password.
	if want := (seen{atHold: 1, waited: 1, unauth: 2, charged1: 1, charge: 1}); unknown != want {
		t.Fatalf("two wrong passwords at once for an unknown login: %+v, want %+v", unknown, want)
	}
}

// The turns are counted per login as written, so a burst naming many logins
// nobody has must leave nothing behind: every key is dropped when its last
// request leaves (finish checks that no place is left).
func TestPasswordGate_ABurstOfManyLoginsLeavesNoPlaceBehind(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "jack@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	b := f.newBurst(t, "identity", "")
	for i := range 24 {
		// Three addresses, eight logins nobody has from each.
		b.signIn(signIn{client: fmt.Sprintf("198.51.100.%d", 100+i%3), login: fmt.Sprintf("nobody-%d@example.com", i), pass: wrongPassword})
	}
	for i := range 4 {
		b.signIn(signIn{client: fmt.Sprintf("198.51.100.%d", 110+i), login: "jack@example.com", pass: wrongPassword})
	}
	b.signIn(signIn{client: "198.51.100.120", login: "jack@example.com", pass: goodPassword})
	b.signIn(signIn{client: "198.51.100.121", login: "not a login", pass: wrongPassword})
	_, by := b.finish()
	if n := by[http.StatusUnauthorized] + by[http.StatusNoContent] + by[http.StatusTooManyRequests]; n != b.sent {
		t.Fatalf("%d sign-ins, answers %v", b.sent, by)
	}
}
