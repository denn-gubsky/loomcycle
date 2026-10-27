package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate/device"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA P5 — DeviceGate (RFC 8628). Each test names the §8 control it pins.

func deviceServer(t *testing.T) (*Server, store.Store, *recordingSink) {
	t.Helper()
	s, st, sink := sessionTestServer(t, "legacy")
	s.cfg().Env.PublicURL = "https://lc.example"
	s.cfg().ResolvedAuth = config.AuthPolicy{Gates: []config.AuthGate{
		{Name: "cli", Kind: config.AuthGateKindDevice, Label: "cli", Device: &config.AuthDeviceGate{TokenTTL: 12 * time.Hour}},
		{Name: "local", Kind: config.AuthGateKindPassword, Label: "Sign in"},
		{Name: "paste", Kind: config.AuthGateKindToken, Label: "Access token"},
	}}
	return s, st, sink
}

type deviceStart struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	Complete        string `json:"verification_uri_complete"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

func formReq(path string, form url.Values, src string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if src != "" {
		r.RemoteAddr = src + ":5000"
	}
	return r
}

func startDevice(t *testing.T, s *Server) deviceStart {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleDeviceAuthorize(rec, formReq("/v1/_auth/device", url.Values{"client_id": {"loomcycle-cli"}}, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("device start = %d %s", rec.Code, rec.Body.String())
	}
	var ds deviceStart
	if err := json.Unmarshal(rec.Body.Bytes(), &ds); err != nil {
		t.Fatal(err)
	}
	return ds
}

type pollAnswer struct {
	Status      int
	Error       string `json:"error"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope"`
}

func pollDevice(t *testing.T, s *Server, code, client, src string) pollAnswer {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleDeviceToken(rec, formReq("/v1/_auth/device/token", url.Values{
		"grant_type": {device.GrantType}, "device_code": {code}, "client_id": {client},
	}, src))
	var a pollAnswer
	_ = json.Unmarshal(rec.Body.Bytes(), &a)
	a.Status = rec.Code
	return a
}

// decide runs the approval page's POST through authMiddleware, as the browser
// would (session cookie, X-Loomcycle-UI, same origin).
func decide(s *Server, c *http.Cookie, path, userCode string, approve *bool, opts ...loginOpt) *httptest.ResponseRecorder {
	body := map[string]any{"user_code": userCode}
	if approve != nil {
		body["approve"] = *approve
	}
	b, _ := json.Marshal(body)
	r := sessionReq(http.MethodPost, path, c, opts...)
	r.Body = io.NopCloser(strings.NewReader(string(b)))
	h := map[string]http.HandlerFunc{"/v1/_auth/device/info": s.handleDeviceInfo, "/v1/_auth/device/approve": s.handleDeviceApprove}[path]
	rec := httptest.NewRecorder()
	s.authMiddleware(h).ServeHTTP(rec, r)
	return rec
}

var (
	yes = func() *bool { b := true; return &b }()
	no  = func() *bool { b := false; return &b }()
)

// userSession signs a tenant user in through the paste gate and returns the
// session cookie.
func userSession(t *testing.T, s *Server, st store.Store, tenant, subject, mode, body string) (*http.Cookie, string) {
	t.Helper()
	ops := auth.Principal{TenantID: tenant, Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	seedUser(t, st, tenant, subject, mode, "active")
	tok := mintFor(t, s, ops, subject, body).Token
	return mustSession(t, s, tok), tok
}

func TestDeviceLogin_IsOffUnlessADeviceGateIsConfigured(t *testing.T) {
	s, _, _ := sessionTestServer(t, "legacy")
	rec := httptest.NewRecorder()
	s.handleDeviceAuthorize(rec, formReq("/v1/_auth/device", url.Values{"client_id": {"x"}}, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no device gate: %d", rec.Code)
	}
}

// The happy path, and "a code approved by user A cannot be redeemed for B":
// only A's decision lands, and the token is A's, at A's member reach.
func TestDeviceLogin_TokenIsTheApproversAndNoOneElseCanTakeTheCode(t *testing.T) {
	s, st, sink := deviceServer(t)
	ds := startDevice(t, s)
	if ds.VerificationURI != "https://lc.example/ui/device" || ds.Complete != ds.VerificationURI+"#"+ds.UserCode ||
		ds.Interval != 5 || ds.ExpiresIn != 600 || !device.WellFormedDeviceCode(ds.DeviceCode) {
		t.Fatalf("start = %+v", ds)
	}
	alice, _ := userSession(t, s, st, "acme", "alice", "isolated", "")
	bob, _ := userSession(t, s, st, "acme", "bob", "tenant", "")

	if a := pollDevice(t, s, ds.DeviceCode, "loomcycle-cli", ""); a.Error != device.ErrAuthorizationPending {
		t.Fatalf("before approval: %+v", a)
	}
	info := decide(s, alice, "/v1/_auth/device/info", strings.ToLower(ds.UserCode), nil)
	if info.Code != http.StatusOK || !strings.Contains(info.Body.String(), `"client_id":"loomcycle-cli"`) ||
		!strings.Contains(info.Body.String(), `"scopes":["substrate:user"]`) {
		t.Fatalf("info = %d %s", info.Code, info.Body.String())
	}
	if rec := decide(s, alice, "/v1/_auth/device/approve", ds.UserCode, yes); rec.Code != http.StatusOK {
		t.Fatalf("alice approves: %d %s", rec.Code, rec.Body.String())
	}
	// Bob can neither take it over nor deny it.
	for _, d := range []*bool{yes, no} {
		if rec := decide(s, bob, "/v1/_auth/device/approve", ds.UserCode, d); rec.Code != http.StatusNotFound {
			t.Fatalf("bob decides an approved code: %d %s", rec.Code, rec.Body.String())
		}
	}
	time.Sleep(5 * time.Millisecond)
	st2 := s.store
	// Wait out the interval the first poll started.
	if err := forcePollable(st2, ds.DeviceCode); err != nil {
		t.Fatal(err)
	}
	a := pollDevice(t, s, ds.DeviceCode, "loomcycle-cli", "")
	if a.Status != http.StatusOK || a.AccessToken == "" || a.Scope != "substrate:user" || a.ExpiresIn <= 0 || a.ExpiresIn > 12*3600 {
		t.Fatalf("redeem = %+v", a)
	}
	p, ok := s.resolvePrincipal(context.Background(), a.AccessToken)
	if !ok || p.TenantID != "acme" || p.Subject != "alice" || auth.HasScope(p.Scopes, auth.ScopeTenant) {
		t.Fatalf("the token is %+v %v, want alice's member token", p, ok)
	}
	row, err := st.OperatorTokenDefGet(context.Background(), p.TokenDefID)
	if err != nil || row.IssuedByGate != "paste" || row.ExpiresAt.IsZero() {
		t.Fatalf("token row = %+v %v (want tagged with the approving gate, expiring)", row, err)
	}
	if ev := lastEvent(t, sink, "auth_device_approved"); ev.ActorSubject != "alice" {
		t.Fatalf("approval audit = %+v", ev)
	}
	// Single use.
	if err := forcePollable(st2, ds.DeviceCode); err != nil {
		t.Fatal(err)
	}
	if again := pollDevice(t, s, ds.DeviceCode, "loomcycle-cli", ""); again.Error != device.ErrInvalidGrant || again.AccessToken != "" {
		t.Fatalf("second redemption = %+v", again)
	}
}

// forcePollable moves the row's last poll back past its interval, so the next
// poll is on time (the store's clock is the request's).
func forcePollable(st store.Store, code string) error {
	_, _, err := st.DeviceAuthPoll(context.Background(), device.HashDeviceCode(code), time.Now().Add(-2*device.MaxInterval), 0, device.MaxInterval)
	return err
}

func TestDeviceLogin_PollingFasterThanTheIntervalGetsSlowDown(t *testing.T) {
	s, _, _ := deviceServer(t)
	ds := startDevice(t, s)
	if a := pollDevice(t, s, ds.DeviceCode, "loomcycle-cli", ""); a.Error != device.ErrAuthorizationPending {
		t.Fatalf("first poll: %+v", a)
	}
	if a := pollDevice(t, s, ds.DeviceCode, "loomcycle-cli", ""); a.Error != device.ErrSlowDown || a.Status != http.StatusBadRequest {
		t.Fatalf("immediate second poll: %+v", a)
	}
	row, _, err := s.store.DeviceAuthPoll(context.Background(), device.HashDeviceCode(ds.DeviceCode), time.Now().Add(time.Hour), 0, device.MaxInterval)
	if err != nil || row.Interval != device.DefaultInterval+device.SlowDownStep {
		t.Fatalf("interval after one early poll = %s, %v", row.Interval, err)
	}
}

func TestDeviceLogin_ExpiredDeniedAndForeignClientCodesAreRefused(t *testing.T) {
	s, st, _ := deviceServer(t)
	ctx := context.Background()
	h, _ := s.hasher()
	code, hash, _ := device.NewDeviceCode()
	if err := st.DeviceAuthCreate(ctx, store.DeviceAuthRow{DeviceCodeHash: hash, UserCodeHash: userCodeHash(h, "BCDFGHJK"),
		ClientID: "loomcycle-cli", CreatedAt: time.Now().Add(-20 * time.Minute), ExpiresAt: time.Now().Add(-10 * time.Minute),
		Interval: device.DefaultInterval}); err != nil {
		t.Fatal(err)
	}
	if a := pollDevice(t, s, code, "loomcycle-cli", ""); a.Error != device.ErrExpiredToken {
		t.Fatalf("expired: %+v", a)
	}
	// An expired code cannot be approved either.
	alice, _ := userSession(t, s, st, "acme", "alice", "isolated", "")
	if rec := decide(s, alice, "/v1/_auth/device/approve", "BCDF-GHJK", yes); rec.Code != http.StatusNotFound {
		t.Fatalf("approving an expired code: %d", rec.Code)
	}

	ds := startDevice(t, s)
	if a := pollDevice(t, s, ds.DeviceCode, "someone-else", ""); a.Error != device.ErrInvalidClient {
		t.Fatalf("foreign client_id: %+v", a)
	}
	if rec := decide(s, alice, "/v1/_auth/device/approve", ds.UserCode, no); rec.Code != http.StatusOK {
		t.Fatalf("deny: %d", rec.Code)
	}
	if err := forcePollable(st, ds.DeviceCode); err != nil {
		t.Fatal(err)
	}
	if a := pollDevice(t, s, ds.DeviceCode, "loomcycle-cli", ""); a.Error != device.ErrAccessDenied {
		t.Fatalf("denied: %+v", a)
	}
	rec := httptest.NewRecorder()
	s.handleDeviceAuthorize(rec, formReq("/v1/_auth/device", url.Values{"client_id": {"loomcycle-cli"}, "scope": {"substrate:admin"}}, ""))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), device.ErrInvalidScope) {
		t.Fatalf("asking for a scope: %d %s", rec.Code, rec.Body.String())
	}
}

// device_code brute force is bounded: 256-bit codes, and a source that polls
// unknown codes is blocked after deviceBadPollLimit tries.
func TestDeviceLogin_GuessingDeviceCodesIsBlockedPerSource(t *testing.T) {
	s, _, _ := deviceServer(t)
	ds := startDevice(t, s)
	for i := 0; i < deviceBadPollLimit; i++ {
		guess, _, _ := device.NewDeviceCode()
		if a := pollDevice(t, s, guess, "loomcycle-cli", "198.51.100.9"); a.Error != device.ErrInvalidGrant {
			t.Fatalf("guess %d: %+v", i, a)
		}
	}
	// Blocked now, even with the real code; another source is not.
	if a := pollDevice(t, s, ds.DeviceCode, "loomcycle-cli", "198.51.100.9"); a.Status != http.StatusTooManyRequests {
		t.Fatalf("after %d bad polls: %+v", deviceBadPollLimit, a)
	}
	if a := pollDevice(t, s, ds.DeviceCode, "loomcycle-cli", "203.0.113.5"); a.Error != device.ErrAuthorizationPending {
		t.Fatalf("another source: %+v", a)
	}
}

// user_code guessing on the approval page is bounded per person: after
// deviceGuessLimit wrong codes even the right code is refused for a while.
func TestDeviceLogin_GuessingUserCodesIsBlockedPerPerson(t *testing.T) {
	s, st, _ := deviceServer(t)
	ds := startDevice(t, s)
	mallory, _ := userSession(t, s, st, "acme", "mallory", "isolated", "")
	for i := 0; i < deviceGuessLimit; i++ {
		guess := fmt.Sprintf("BBBB-BB%c%c", device.UserCodeAlphabet[i%19], device.UserCodeAlphabet[(i/19)%19])
		if rec := decide(s, mallory, "/v1/_auth/device/info", guess, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("guess %d: %d", i, rec.Code)
		}
	}
	if rec := decide(s, mallory, "/v1/_auth/device/approve", ds.UserCode, yes); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d wrong codes: %d %s", deviceGuessLimit, rec.Code, rec.Body.String())
	}
}

// The approval page refuses a cross-site POST, a POST without the UI header,
// and a caller with no session (a bearer cannot approve device codes).
func TestDeviceLogin_ApprovalNeedsASameOriginSession(t *testing.T) {
	s, st, _ := deviceServer(t)
	ds := startDevice(t, s)
	alice, tok := userSession(t, s, st, "acme", "alice", "isolated", "")
	cross := func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }
	noHeader := func(r *http.Request) { r.Header.Del(uiRequestHeader) }
	for name, opt := range map[string]loginOpt{"cross-site": cross, "no UI header": noHeader} {
		if rec := decide(s, alice, "/v1/_auth/device/approve", ds.UserCode, yes, opt); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// The bearer behind the session, without the cookie.
	b, _ := json.Marshal(map[string]any{"user_code": ds.UserCode, "approve": true})
	r := httptest.NewRequest(http.MethodPost, "/v1/_auth/device/approve", strings.NewReader(string(b)))
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	s.authMiddleware(http.HandlerFunc(s.handleDeviceApprove)).ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "session_required") {
		t.Fatalf("bearer approval: %d %s", rec.Code, rec.Body.String())
	}
	got, _ := st.DeviceAuthGetByUserCode(context.Background(), userCodeHash(mustHasher(t, s), strings.ReplaceAll(ds.UserCode, "-", "")))
	if got.Status != store.DeviceAuthPending {
		t.Fatalf("a refused approval changed the row: %+v", got)
	}
}

// The issued token never exceeds the approving session: a session made from
// a restricted token cannot trade itself for the user's full member token.
func TestDeviceLogin_ARestrictedSessionCannotApprove(t *testing.T) {
	s, st, _ := deviceServer(t)
	ds := startDevice(t, s)
	alice, _ := userSession(t, s, st, "acme", "alice", "tenant", `{"rate":{"per_minute":5}}`)
	rec := decide(s, alice, "/v1/_auth/device/approve", ds.UserCode, yes)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "device_not_allowed") {
		t.Fatalf("restricted session approves: %d %s", rec.Code, rec.Body.String())
	}
}

// The tenant's gate policy holds at approval and again at redemption.
func TestDeviceLogin_TenantPolicyRefusesAnUnlistedApprovingGate(t *testing.T) {
	s, st, _ := deviceServer(t)
	ds := startDevice(t, s)
	alice, _ := userSession(t, s, st, "acme", "alice", "isolated", "")
	if rec := decide(s, alice, "/v1/_auth/device/approve", ds.UserCode, yes); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d", rec.Code)
	}
	// The policy changes between approval and the poll.
	if err := st.TenantAuthPolicyPut(context.Background(), store.TenantAuthPolicyRow{TenantID: "acme", Gates: []string{"local"}}); err != nil {
		t.Fatal(err)
	}
	// The API flushes every replica's policy memo; a direct store write must too.
	s.tenantPolicyCache.flush()
	if a := pollDevice(t, s, ds.DeviceCode, "loomcycle-cli", ""); a.Error != device.ErrAccessDenied || a.AccessToken != "" {
		t.Fatalf("redeem after the policy dropped the approving gate: %+v", a)
	}
	// The approving session itself came through the now-unlisted gate: it no
	// longer authenticates at all, let alone approves.
	ds2 := startDevice(t, s)
	if rec := decide(s, alice, "/v1/_auth/device/approve", ds2.UserCode, yes); rec.Code != http.StatusUnauthorized {
		t.Fatalf("approve through an unlisted gate: %d", rec.Code)
	}
}

func mustHasher(t *testing.T, s *Server) auth.TokenHasher {
	t.Helper()
	h, err := s.hasher()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// GET /v1/_authgate tells an admin which gates a browser can sign in through:
// the device gate is not one (its logins are approved through another gate).
func TestAuthgateList_ADeviceGateIsNotABrowserSignIn(t *testing.T) {
	s, _, _ := deviceServer(t)
	rec := httptest.NewRecorder()
	s.handleAuthgateList(rec, httptest.NewRequest(http.MethodGet, "/v1/_authgate", nil))
	var got struct {
		Gates []struct {
			Name        string `json:"name"`
			Interactive bool   `json:"interactive"`
		} `json:"gates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, g := range got.Gates {
		seen[g.Name] = g.Interactive
	}
	if want := map[string]bool{"cli": false, "local": true, "paste": true}; fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("interactive by gate = %v, want %v", seen, want)
	}
}
