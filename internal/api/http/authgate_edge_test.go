package http

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate/awsiam"
	"github.com/denn-gubsky/loomcycle/internal/authgate/headergate"
	"github.com/denn-gubsky/loomcycle/internal/awsauth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const (
	edgeProxyPeer = "10.0.0.5:41000"
	edgeSecretEnv = "LOOMCYCLE_TEST_EDGE_HMAC_SECRET"
	edgeSecret    = "0123456789abcdef0123456789abcdef-test"
)

// edgeServer is a session server with a forward-auth-hmac header gate "fwd"
// (proxy 10.0.0.0/24, tenant acme, provision auto) and an AWS IAM gate "ci".
func edgeServer(t *testing.T) (*Server, store.Store, *recordingSink, *EdgeGates) {
	t.Helper()
	t.Setenv(edgeSecretEnv, edgeSecret)
	s, st, sink := sessionTestServer(t, "legacy")
	floor := config.AuthNewUser{AccessMode: "isolated", Groups: []string{"everyone", "new-users"}, Status: "active", TokenTTL: 12 * time.Hour}
	fwd := config.AuthGate{Name: "fwd", Kind: config.AuthGateKindHeader, Label: "the corporate proxy",
		Identity: &config.AuthIdentityMap{Tenant: "acme", SubjectClaim: "sub", Provision: config.ProvisionAuto, NewUser: floor},
		Header: &config.AuthHeaderGate{Preset: config.HeaderPresetForwardAuthHMAC, SecretEnv: edgeSecretEnv,
			TrustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}}}
	ci := config.AuthGate{Name: "ci", Kind: config.AuthGateKindAWSIAM, Label: "ci",
		Identity: &config.AuthIdentityMap{Tenant: "acme", SubjectClaim: "arn", Provision: config.ProvisionAuto, NewUser: floor},
		AWSIAM:   &config.AuthAWSIAMGate{ServerID: "lc.acme.test", AllowedARNs: []string{"arn:aws:iam::123456789012:role/ci-*"}}}
	s.cfg().ResolvedAuth.Gates = append(s.cfg().ResolvedAuth.Gates, fwd, ci)
	e, err := EdgeGatesFromConfig(s.cfg(), EdgeGateDeps{Replay: st})
	if err != nil {
		t.Fatal(err)
	}
	s.SetEdgeGates(e)
	return s, st, sink, e
}

func nonce(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// viaProxy makes r arrive from the proxy, carrying its signed identity for sub.
func viaProxy(t *testing.T, sub string) loginOpt {
	return func(r *http.Request) {
		r.RemoteAddr = edgeProxyPeer
		id := fmt.Sprintf(`{"sub":%q,"email":%q}`, sub, sub+"@acme.example")
		r.Header.Set(headergate.IdentityHeader, id)
		r.Header.Set(headergate.IdentitySignatureHeader, headergate.SignForwardAuth([]byte(edgeSecret), id, time.Now(), nonce(t)))
	}
}

func headerLogin(t *testing.T, s *Server, opts ...loginOpt) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/_auth/complete/fwd", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("User-Agent", testUA)
	r.Header.Set(uiRequestHeader, "1")
	for _, o := range opts {
		o(r)
	}
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, r)
	return rec
}

func edgeMe(s *Server, c *http.Cookie, opts ...loginOpt) (int, map[string]any) {
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, sessionReq(http.MethodGet, "/v1/_me", c, opts...))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func headerSession(t *testing.T, s *Server, sub string) *http.Cookie {
	t.Helper()
	rec := headerLogin(t, s, viaProxy(t, sub))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("header login = %d %s", rec.Code, rec.Body.String())
	}
	c := cookieNamed(rec, sessionCookiePlain)
	if c == nil {
		t.Fatalf("no session cookie: %v", rec.Result().Cookies())
	}
	return c
}

func TestHeaderGate_ProxyAssertionSignsTheBrowserInAndProvisions(t *testing.T) {
	s, st, sink, _ := edgeServer(t)
	c := headerSession(t, s, "alice")
	code, me := edgeMe(s, c, viaProxy(t, "alice"))
	if code != http.StatusOK || me["subject"] != "fwd:alice" || me["tenant_id"] != "acme" {
		t.Fatalf("me = %d %v", code, me)
	}
	if u, err := st.UserGet(context.Background(), "acme", "fwd:alice"); err != nil || u.AccessMode != "isolated" {
		t.Fatalf("provisioned user %+v %v", u, err)
	}
	if !hasAction(sink, "auth_login") {
		t.Fatalf("no auth_login audit: %v", actions(sink))
	}
}

func TestHeaderGate_SessionEndsWhenTheProxyStopsVouching(t *testing.T) {
	s, _, _, _ := edgeServer(t)
	c := headerSession(t, s, "alice")
	// The same cookie without the proxy's assertion: the person signed out at
	// the edge, or the cookie left the browser.
	if code, _ := edgeMe(s, c); code != http.StatusUnauthorized {
		t.Fatalf("session without the assertion = %d, want 401", code)
	}
	// Ended, not just refused once: the assertion coming back does not revive it.
	if code, _ := edgeMe(s, c, viaProxy(t, "alice")); code != http.StatusUnauthorized {
		t.Fatalf("ended session with the assertion back = %d, want 401", code)
	}
}

func TestHeaderGate_SessionEndsWhenTheProxyVouchesForSomeoneElse(t *testing.T) {
	s, _, _, _ := edgeServer(t)
	c := headerSession(t, s, "alice")
	if code, _ := edgeMe(s, c, viaProxy(t, "mallory")); code != http.StatusUnauthorized {
		t.Fatalf("alice's cookie under mallory's assertion = %d, want 401", code)
	}
	if code, _ := edgeMe(s, c, viaProxy(t, "alice")); code != http.StatusUnauthorized {
		t.Fatal("the session survived a mismatch")
	}
}

func TestHeaderGate_ValidAssertionFromOutsideTheTrustedCIDRsIsRefused(t *testing.T) {
	s, _, sink, _ := edgeServer(t)
	rec := headerLogin(t, s, viaProxy(t, "alice"), func(r *http.Request) { r.RemoteAddr = "203.0.113.9:5000" })
	if rec.Code != http.StatusUnauthorized || cookieNamed(rec, sessionCookiePlain) != nil {
		t.Fatalf("login from outside the CIDRs = %d %s", rec.Code, rec.Body.String())
	}
	if !hasAction(sink, "auth_login_failed") {
		t.Fatalf("refusal not audited: %v", actions(sink))
	}
}

func TestHeaderGate_AliasHeadersAreIgnoredAndStrippedBeforeHandlers(t *testing.T) {
	s, _, _, _ := edgeServer(t)
	smuggle := func(r *http.Request) {
		id := `{"sub":"admin"}`
		r.Header["X_Loomcycle_Identity"] = []string{id}
		r.Header["X.Loomcycle.Identity.Signature"] = []string{headergate.SignForwardAuth([]byte(edgeSecret), id, time.Now(), nonce(t))}
	}
	rec := headerLogin(t, s, func(r *http.Request) { r.RemoteAddr = edgeProxyPeer }, smuggle)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "no_assertion") {
		t.Fatalf("alias-only login = %d %s, want 401 no_assertion", rec.Code, rec.Body.String())
	}
	// No handler sees an alias, whatever route it is.
	var seen http.Header
	h := s.edgeHeaderFilter(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.Header.Clone() }))
	r := httptest.NewRequest(http.MethodGet, "/v1/agents/x", nil)
	r.RemoteAddr = "203.0.113.9:1"
	smuggle(r)
	r.Header.Set(headergate.IdentityHeader, `{"sub":"x"}`)
	h.ServeHTTP(httptest.NewRecorder(), r)
	for k := range seen {
		if strings.Contains(strings.ToLower(k), "identity") {
			t.Fatalf("handler saw %q", k)
		}
	}
}

func TestHeaderGate_CrossSiteLoginIsRefused(t *testing.T) {
	s, _, _, _ := edgeServer(t)
	rec := headerLogin(t, s, viaProxy(t, "alice"), func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") })
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site header login = %d, want 403", rec.Code)
	}
}

func TestHeaderGate_ReplayedProxyHeaderIsRefused(t *testing.T) {
	s, _, _, _ := edgeServer(t)
	var captured http.Header
	capture := func(r *http.Request) {
		viaProxy(t, "alice")(r)
		captured = r.Header.Clone()
	}
	if rec := headerLogin(t, s, capture); rec.Code != http.StatusNoContent {
		t.Fatalf("first login = %d", rec.Code)
	}
	replay := func(r *http.Request) {
		r.RemoteAddr = edgeProxyPeer
		for _, k := range []string{headergate.IdentityHeader, headergate.IdentitySignatureHeader} {
			r.Header.Set(k, captured.Get(k))
		}
	}
	if rec := headerLogin(t, s, replay); rec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed header = %d, want 401", rec.Code)
	}
}

func TestHeaderGate_LoginConfigListsHeaderGatesButNotMachineGates(t *testing.T) {
	s, _, _, _ := edgeServer(t)
	lc := s.loginConfig(httptest.NewRequest(http.MethodGet, "/ui/login-config.json", nil))
	var kinds []string
	for _, g := range lc.Gates {
		kinds = append(kinds, g.Kind+":"+g.Label)
		if g.Kind == config.AuthGateKindAWSIAM {
			t.Fatalf("the login page lists the machine gate %q", g.Name)
		}
	}
	if !strings.Contains(strings.Join(kinds, ","), "header:the corporate proxy") {
		t.Fatalf("login gates %v", kinds)
	}
}

// ---- AWS IAM ----

// stsAt answers GetCallerIdentity for every request it receives with arn,
// counting calls.
type stsAt struct {
	srv   *httptest.Server
	calls int
}

func newSTS(t *testing.T, arn string) *stsAt {
	t.Helper()
	f := &stsAt{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") || r.Host != awsiam.GlobalHost {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		acct := strings.Split(arn, ":")[4]
		fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Arn>%s</Arn><UserId>AROA:x</UserId><Account>%s</Account>`+
			`</GetCallerIdentityResult></GetCallerIdentityResponse>`, arn, acct)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// redirectTo sends every request to target, keeping the Host it was for.
type redirectTo struct{ target *url.URL }

func (rt redirectTo) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(r2)
}

func pointGateAtSTS(e *EdgeGates, f *stsAt) {
	u, _ := url.Parse(f.srv.URL)
	e.awsiam["ci"].HTTP = &http.Client{Transport: redirectTo{u}}
}

func stsLoginBody(t *testing.T, gate string, at time.Time, serverID string) []byte {
	t.Helper()
	const body = "Action=GetCallerIdentity&Version=2011-06-15"
	req, _ := http.NewRequest(http.MethodPost, "https://sts.amazonaws.com/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set(awsiam.ServerIDHeader, serverID)
	awsauth.SignRequest(req, []byte(body), awsauth.Credentials{AccessKeyID: "AKIA", SecretAccessKey: "s-" + nonce(t)}, "us-east-1", "sts", at)
	hb, _ := json.Marshal(req.Header)
	enc := base64.StdEncoding.EncodeToString
	out, _ := json.Marshal(map[string]string{
		"gate": gate, "iam_http_request_method": "POST", "iam_request_url": enc([]byte("https://sts.amazonaws.com/")),
		"iam_request_headers": enc(hb), "iam_request_body": enc([]byte(body)),
	})
	return out
}

func assertReq(s *Server, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/_auth/assert", bytes.NewReader(body))
	r.RemoteAddr = "198.51.100.7:1234"
	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, r)
	return rec
}

func TestAWSIAMAssert_IssuesAShortLivedTokenForTheRoleARN(t *testing.T) {
	s, st, _, e := edgeServer(t)
	f := newSTS(t, "arn:aws:sts::123456789012:assumed-role/ci-deploy/i-0abc")
	pointGateAtSTS(e, f)
	rec := assertReq(s, stsLoginBody(t, "ci", time.Now(), "lc.acme.test"))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("assert = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Token, Subject string
		Tenant         string    `json:"tenant"`
		ExpiresAt      time.Time `json:"expires_at"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Subject != "ci:arn:aws:iam::123456789012:role/ci-deploy" || out.Tenant != "acme" || !strings.HasPrefix(out.Token, "lct_") {
		t.Fatalf("issued %+v", out)
	}
	if left := time.Until(out.ExpiresAt); left > awsiamTokenTTL || left < awsiamTokenTTL-time.Minute {
		t.Fatalf("token lives %s, want about %s", left, awsiamTokenTTL)
	}
	// The token works as a bearer for exactly that user.
	r := httptest.NewRequest(http.MethodGet, "/v1/_me", nil)
	r.Header.Set("Authorization", "Bearer "+out.Token)
	me := httptest.NewRecorder()
	s.Mux().ServeHTTP(me, r)
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), "ci:arn:aws:iam::123456789012:role/ci-deploy") {
		t.Fatalf("me with the issued token = %d %s", me.Code, me.Body.String())
	}
	rows, _ := st.OperatorTokenDefListBySubject(context.Background(), "acme", out.Subject)
	tagged := false
	for _, row := range rows {
		tagged = tagged || row.IssuedByGate == "ci"
	}
	if !tagged {
		t.Fatal("the issued token is not tagged with its gate")
	}
}

func TestAWSIAMAssert_ReplayedRequestIsRefused(t *testing.T) {
	s, _, _, e := edgeServer(t)
	f := newSTS(t, "arn:aws:sts::123456789012:assumed-role/ci-deploy/i-0abc")
	pointGateAtSTS(e, f)
	body := stsLoginBody(t, "ci", time.Now(), "lc.acme.test")
	if rec := assertReq(s, body); rec.Code != http.StatusOK {
		t.Fatalf("first = %d %s", rec.Code, rec.Body.String())
	}
	if rec := assertReq(s, body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("replay = %d, want 401", rec.Code)
	}
}

func TestAWSIAMAssert_ARNOutsideAllowedARNsIsRefusedAndNothingProvisioned(t *testing.T) {
	s, st, _, e := edgeServer(t)
	f := newSTS(t, "arn:aws:iam::123456789012:user/alice")
	pointGateAtSTS(e, f)
	if rec := assertReq(s, stsLoginBody(t, "ci", time.Now(), "lc.acme.test")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("assert = %d, want 401", rec.Code)
	}
	if _, err := st.UserGet(context.Background(), "acme", "ci:arn:aws:iam::123456789012:user/alice"); err == nil {
		t.Fatal("a refused ARN was provisioned")
	}
}

func TestAWSIAMAssert_RepeatedFailuresFromOneSourceAreThrottled(t *testing.T) {
	s, _, _, e := edgeServer(t)
	f := newSTS(t, "arn:aws:iam::123456789012:user/alice") // never allowed
	pointGateAtSTS(e, f)
	var last int
	for i := 0; i < assertSourceFree+3; i++ {
		last = assertReq(s, stsLoginBody(t, "ci", time.Now(), "lc.acme.test")).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after %d failures the source gets %d, want 429", assertSourceFree+3, last)
	}
	calls := f.calls
	assertReq(s, stsLoginBody(t, "ci", time.Now(), "lc.acme.test"))
	if f.calls != calls {
		t.Fatal("a throttled request still reached STS")
	}
}

// The assert route is shared with the external gates (RFC FA P4): a name
// that is no awsiam gate goes to their path, which refuses an unknown name, a
// header gate and a password gate with one answer, so the route does not
// say which gates exist.
func TestAWSIAMAssert_UnknownGateOrHeaderGateGetsOneOpaqueRefusal(t *testing.T) {
	s, _, _, _ := edgeServer(t)
	var first string
	for _, gate := range []string{"nope", "fwd", "legacy"} {
		rec := assertReq(s, []byte(`{"gate":"`+gate+`"}`))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("gate %q: %d, want 401", gate, rec.Code)
		}
		if first == "" {
			first = rec.Body.String()
		} else if rec.Body.String() != first {
			t.Fatalf("gate %q answers %s, unlike the unknown gate's %s", gate, rec.Body.String(), first)
		}
	}
}

func TestAWSIAMAssert_TenantClaimOutsideAllowedTenantsIsRefused(t *testing.T) {
	s, _, _, e := edgeServer(t)
	// A header gate whose tenant comes from the proxy's claim: a tenant the
	// allowlist does not name is refused, never mapped to the shared tenant.
	for i, g := range s.cfg().ResolvedAuth.Gates {
		if g.Name == "fwd" {
			m := *g.Identity
			m.Tenant, m.TenantClaim, m.AllowedTenants = "", "tenant", map[string]string{"acme": "acme"}
			s.cfg().ResolvedAuth.Gates[i].Identity = &m
		}
	}
	_ = e
	withTenant := func(tenant string) loginOpt {
		return func(r *http.Request) {
			r.RemoteAddr = edgeProxyPeer
			id := fmt.Sprintf(`{"sub":"bob","tenant":%q}`, tenant)
			r.Header.Set(headergate.IdentityHeader, id)
			r.Header.Set(headergate.IdentitySignatureHeader, headergate.SignForwardAuth([]byte(edgeSecret), id, time.Now(), nonce(t)))
		}
	}
	if rec := headerLogin(t, s, withTenant("evilcorp")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tenant outside allowed_tenants = %d, want 401", rec.Code)
	}
	if rec := headerLogin(t, s, withTenant("acme")); rec.Code != http.StatusNoContent {
		t.Fatalf("allowed tenant = %d %s", rec.Code, rec.Body.String())
	}
}

func hasAction(sink *recordingSink, action string) bool {
	for _, a := range actions(sink) {
		if a == action {
			return true
		}
	}
	return false
}

// failingReplay is a shared replay record whose store is down.
type failingReplay struct{ store.Store }

func (failingReplay) AuthReplayAdmit(context.Context, string, string, time.Time, time.Time) (bool, error) {
	return false, fmt.Errorf("store down")
}

func TestHeaderGate_ProxyHeaderReplayedToAnotherReplicaIsRefused(t *testing.T) {
	s, st, _, _ := edgeServer(t)
	var captured http.Header
	capture := func(r *http.Request) {
		viaProxy(t, "alice")(r)
		captured = r.Header.Clone()
	}
	if rec := headerLogin(t, s, capture); rec.Code != http.StatusNoContent {
		t.Fatalf("login on the first replica = %d %s", rec.Code, rec.Body.String())
	}
	// A second replica over the same store: its own in-process caches, the
	// shared replay record.
	other, err := EdgeGatesFromConfig(s.cfg(), EdgeGateDeps{Replay: st})
	if err != nil {
		t.Fatal(err)
	}
	s.SetEdgeGates(other)
	replay := func(r *http.Request) {
		r.RemoteAddr = edgeProxyPeer
		for _, k := range []string{headergate.IdentityHeader, headergate.IdentitySignatureHeader} {
			r.Header.Set(k, captured.Get(k))
		}
	}
	if rec := headerLogin(t, s, replay); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the same proxy header on another replica = %d, want 401", rec.Code)
	}
}

func TestHeaderGate_UnavailableSharedReplayRecordRefusesTheHeader(t *testing.T) {
	s, st, _, _ := edgeServer(t)
	down, err := EdgeGatesFromConfig(s.cfg(), EdgeGateDeps{Replay: failingReplay{st}})
	if err != nil {
		t.Fatal(err)
	}
	s.SetEdgeGates(down)
	if rec := headerLogin(t, s, viaProxy(t, "alice")); rec.Code == http.StatusNoContent {
		t.Fatal("signed in although the nonce could not be recorded for the other replicas")
	}
}
