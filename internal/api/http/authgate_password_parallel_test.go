package http

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate/password"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// heldLoginStore holds every sign-in at the point where it has read the
// account's credential row and has not yet verified the password, until the
// test lets them all go: the attempts it holds are in progress together
// whatever the machine's speed. It also counts the wrong passwords that were
// verified against an account (each one records an account failure).
type heldLoginStore struct {
	store.Store
	held     chan struct{}
	release  chan struct{}
	verified atomic.Int64
}

func (h *heldLoginStore) UserCredentialGet(ctx context.Context, tenantID, subject, kind string) (store.UserCredentialRow, error) {
	row, err := h.Store.UserCredentialGet(ctx, tenantID, subject, kind)
	h.held <- struct{}{}
	<-h.release
	return row, err
}

func (h *heldLoginStore) UserCredentialRecordFailure(ctx context.Context, tenantID, subject, kind string, now time.Time, lockFor func(int) time.Duration) (int, time.Time, error) {
	h.verified.Add(1)
	return h.Store.UserCredentialRecordFailure(ctx, tenantID, subject, kind, now, lockFor)
}

// signIn is one sign-in the burst sends: who it says it is, from which client
// address (through the trusted proxy), with which password.
type signIn struct{ client, login, pass string }

// parallelSignIns sends every sign-in at once to the local gate and lets none
// verify a password until each has either been answered or is held at the
// credential read. It returns how many wrong passwords were verified against
// an account, and the answers by status.
func (f *gateFixture) parallelSignIns(t *testing.T, attempts []signIn) (verified int, byStatus map[int]int) {
	t.Helper()
	f.srv.cfg().ResolvedAuth.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	f.srv.cfg().ResolvedAuth.ClientIPHeader = "X-Client-Ip"
	hs := &heldLoginStore{Store: f.st, held: make(chan struct{}, len(attempts)), release: make(chan struct{})}
	f.srv.store = hs
	// A server of its own, started after the store was wrapped.
	ts := httptest.NewServer(f.srv.Mux())
	t.Cleanup(ts.Close)
	answered := make(chan int, len(attempts))
	for _, a := range attempts {
		go func() {
			req, err := http.NewRequest("POST", ts.URL+"/v1/_auth/complete/local",
				strings.NewReader(jsonBody(t, map[string]string{"login": a.login, "password": a.pass})))
			if err != nil {
				answered <- 0
				return
			}
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			req.Header.Set(uiRequestHeader, "1")
			req.Header.Set("User-Agent", testUA)
			req.Header.Set("X-Client-Ip", a.client)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				answered <- 0
				return
			}
			resp.Body.Close()
			answered <- resp.StatusCode
		}()
	}
	// A safety net for a hung test, not a measurement: nothing below depends
	// on how long anything takes.
	hung := time.After(2 * time.Minute)
	byStatus = map[int]int{}
	settled := 0
	for settled < len(attempts) {
		select {
		case <-hs.held:
			settled++
		case code := <-answered:
			byStatus[code]++
			settled++
		case <-hung:
			t.Fatalf("only %d of %d sign-ins were answered or held", settled, len(attempts))
		}
	}
	close(hs.release)
	for n := 0; n < len(attempts); {
		n = 0
		for _, c := range byStatus {
			n += c
		}
		if n == len(attempts) {
			break
		}
		select {
		case code := <-answered:
			byStatus[code]++
		case <-hung:
			t.Fatalf("only %d of %d sign-ins were answered", n, len(attempts))
		}
	}
	return int(hs.verified.Load()), byStatus
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

// Sign-ins sent together are paced like sign-ins sent one after another. One
// client guessing one login gets, in a row, the three free failures and the
// one that sets the first hold; sent at once, the same four are verified and
// the rest are told to wait.
func TestPasswordGate_ParallelGuessesAreBoundedLikeSequentialOnes(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "olga@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	const sent = 12
	verified, by := f.parallelSignIns(t, sameGuess(sent, "198.51.100.7", "olga@example.com"))
	if verified != 4 || by[http.StatusUnauthorized] != 4 || by[http.StatusTooManyRequests] != sent-4 {
		t.Fatalf("%d guesses sent at once: %d verified, answers %v; want 4 verified (401) and %d told to wait (429)", sent, verified, by, sent-4)
	}
}

// Once a client has been held back on a login, the end of the hold admits one
// guess, whose failure sets the next, longer hold. Guesses sent together at
// that moment must not all be that one guess.
func TestPasswordGate_ParallelGuessesAfterAHoldEndsAreOneGuess(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "pavel@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	pg := f.srv.pwGates.p.Load()
	th := pg.byName["local"].Throttle
	f.pastFailures(t, pg.sourceLoginKey("local", "198.51.100.8", "pavel@example.com"), 6, th.SourceLoginBlockFor)
	const sent = 12
	verified, by := f.parallelSignIns(t, sameGuess(sent, "198.51.100.8", "pavel@example.com"))
	if verified != 1 || by[http.StatusUnauthorized] != 1 || by[http.StatusTooManyRequests] != sent-1 {
		t.Fatalf("%d guesses sent at once after a hold ended: %d verified, answers %v; want 1 verified and %d told to wait", sent, verified, by, sent-1)
	}
}

// The account's own lock bounds guesses from any number of addresses: an
// account past max_failures admits one guess when its lock ends. Guesses sent
// together from different addresses are still one guess, and the others are
// answered exactly as a wrong password is (a locked account never says so).
func TestPasswordGate_ParallelGuessesFromManySourcesCannotOutrunTheAccountLock(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "rita@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	ctx := context.Background()
	id, err := f.st.UserIdentityGet(ctx, "local", "", "rita@example.com")
	if err != nil {
		t.Fatal(err)
	}
	th := f.srv.pwGates.p.Load().byName["local"].Throttle
	for range th.MaxFailures {
		if _, _, err := f.st.UserCredentialRecordFailure(ctx, id.TenantID, id.Subject, store.CredentialKindPassword, time.Now().Add(-time.Hour), th.AccountLockFor); err != nil {
			t.Fatal(err)
		}
	}
	const sent = 12
	attempts := make([]signIn, sent)
	for i := range attempts {
		attempts[i] = signIn{client: fmt.Sprintf("198.51.100.%d", 20+i), login: "rita@example.com", pass: wrongPassword}
	}
	verified, by := f.parallelSignIns(t, attempts)
	if verified != 1 || by[http.StatusUnauthorized] != sent {
		t.Fatalf("%d guesses sent at once from %d addresses at a locked-out account: %d verified, answers %v; want 1 verified and every answer 401", sent, sent, verified, by)
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
	verified, by := f.parallelSignIns(t, attempts)
	if verified != 1 || by[http.StatusUnauthorized] != 1 || by[http.StatusTooManyRequests] != sent-1 {
		t.Fatalf("%d logins guessed at once from an address past its free failures: %d verified, answers %v; want 1 verified and %d told to wait", sent, verified, by, sent-1)
	}
}

// People signing in at the same moment are not held back: right passwords
// sent together, from one address and from several, all sign in.
func TestPasswordGate_ParallelRightPasswordsAllSignIn(t *testing.T) {
	f := newGateFixture(t)
	f.redeemed(t, "local", "sam@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	f.redeemed(t, "local", "tess@example.com", f.inviteCode(t, bAcmeOp, "local", `{}`))
	attempts := []signIn{
		{client: "198.51.100.40", login: "sam@example.com", pass: goodPassword},
		{client: "198.51.100.40", login: "sam@example.com", pass: goodPassword},
		{client: "198.51.100.40", login: "tess@example.com", pass: goodPassword},
		{client: "198.51.100.41", login: "tess@example.com", pass: goodPassword},
	}
	verified, by := f.parallelSignIns(t, attempts)
	if verified != 0 || by[http.StatusNoContent] != len(attempts) {
		t.Fatalf("right passwords sent at once: %d failures recorded, answers %v; want all %d signed in", verified, by, len(attempts))
	}
}
