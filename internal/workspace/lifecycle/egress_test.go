package lifecycle

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// egressToken is the credential every credentialed test sends; not a real
// one.
const egressToken = "tok-lifecycle-egress-never-leaves-the-core"

// countingUpstream is a local TLS server that counts what reached it.
type countingUpstream struct {
	*httptest.Server
	hits atomic.Int32
	mu   sync.Mutex
	auth []string
}

func newCountingUpstream(t *testing.T) *countingUpstream {
	t.Helper()
	u := &countingUpstream{}
	u.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		u.mu.Lock()
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.mu.Unlock()
		_, _ = io.WriteString(w, "upstream:"+r.URL.Path)
	}))
	t.Cleanup(u.Close)
	return u
}

// egressCreds resolves egressToken for one name and records the agent name
// it was asked for.
type egressCreds struct {
	name    string
	allowed *[]string
	mu      sync.Mutex
	agents  []string
}

func (c *egressCreds) ResolveFor(_ context.Context, _, agentName, _, name, target string) (credential.Resolved, bool, error) {
	c.mu.Lock()
	c.agents = append(c.agents, agentName)
	c.mu.Unlock()
	if name != c.name {
		return credential.Resolved{}, false, nil
	}
	if !credential.URLAllowed(c.allowed, target) {
		return credential.Resolved{}, false, credential.ErrURLNotAllowed
	}
	return credential.Resolved{Value: egressToken, AllowedURLs: c.allowed}, true, nil
}

// testProxy is a proxy that trusts httptest's certificate and is vouched
// for 127.0.0.1, auditing through the manager.
func testProxy(t *testing.T, m *Manager, creds egress.Credentials, limits egress.Limits) *egress.Proxy {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	roots := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	srv.Close()
	p, err := egress.New(egress.Options{
		Credentials:          creds,
		PrivateHostAllowlist: []string{"127.0.0.1/32"},
		TLSConfig:            &tls.Config{RootCAs: roots.Clone(), MinVersion: tls.VersionTLS12},
		Audit:                m.RecordEgress,
		Limits:               limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func send(t *testing.T, m *Manager, row store.WorkspaceRow, route string) (int, string, error) {
	t.Helper()
	resp, err := m.Egress(context.Background(), row, egress.Request{Route: route, Method: http.MethodGet, Path: "x"})
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.Status, string(b), err
}

func refusalStatus(err error) int {
	var r *egress.Refusal
	if errors.As(err, &r) {
		return r.Status
	}
	return 0
}

// The routes given at open are recorded on the row, and a repeated open
// returns the workspace with the routes it was opened with.
func TestOpen_RecordsTheEgressOnTheRow(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	o := f.run(t, "alice")
	r, err := f.m.Open(context.Background(), o, OpenRequest{Name: "work", Toolchains: []string{"python"}, Egress: []string{"pypi", "pypi-files"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Egress, []string{"pypi", "pypi-files"}) {
		t.Fatalf("row egress = %v", r.Egress)
	}
	got, err := f.m.Get(context.Background(), o, "work")
	if err != nil || !reflect.DeepEqual(got.Egress, []string{"pypi", "pypi-files"}) {
		t.Fatalf("read back = %v, %v", got.Egress, err)
	}
	plain := f.open(t, o, "plain")
	if plain.Egress == nil || len(plain.Egress) != 0 {
		t.Fatalf("a workspace opened with no routes has %#v, want none", plain.Egress)
	}
}

// A route the operator configured but the workspace's row does not name is
// refused before any network call; one the row names goes out. A name on
// the row that is no operator route (a tenant's, not resolved yet) is
// refused the same way.
func TestEgress_ARouteNotOnTheRowIsRefusedBeforeAnyNetworkCall(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	up := newCountingUpstream(t)
	f.m.SetEgress(testProxy(t, f.m, nil, egress.Limits{}), OperatorRoutes(map[string]config.EgressRoute{
		"pypi":   {URL: up.URL + "/pypi/"},
		"secret": {URL: up.URL + "/secret/"},
	}))
	o := f.run(t, "alice")
	row, err := f.m.Open(context.Background(), o, OpenRequest{Name: "work", Toolchains: []string{"python"}, Egress: []string{"pypi", "crm"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"secret", "crm", "nope"} {
		if _, _, err := send(t, f.m, row, route); refusalStatus(err) != http.StatusForbidden {
			t.Errorf("route %s: %v, want a 403 refusal", route, err)
		}
	}
	if n := up.hits.Load(); n != 0 {
		t.Fatalf("the upstream was reached %d times for refused routes", n)
	}
	status, body, err := send(t, f.m, row, "pypi")
	if err != nil || status != http.StatusOK || body != "upstream:/pypi/x" {
		t.Fatalf("the row's own route = %d %q, %v", status, body, err)
	}
}

// A credentialed route's credential is resolved for the workspace's owner
// run (its tenant, user and agent name), sent upstream, and the audit line
// written for it names the route and carries neither the token nor the
// query.
func TestEgress_ACredentialedRouteResolvesForTheOwnerRunAndIsAudited(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	log := &auditLog{}
	f.m.SetAudit(log)
	up := newCountingUpstream(t)
	allowed := []string{up.URL + "/api"} // canonical, as credential stores it
	creds := &egressCreds{name: "crm_token", allowed: &allowed}
	f.m.SetEgress(testProxy(t, f.m, creds, egress.Limits{}), OperatorRoutes(map[string]config.EgressRoute{
		"crm": {URL: up.URL + "/api/", Credential: "$cred:crm_token"},
	}))
	o := f.run(t, "alice")
	run, err := f.st.GetRun(context.Background(), o.RunID)
	if err != nil {
		t.Fatal(err)
	}
	row, err := f.m.Open(context.Background(), o, OpenRequest{Name: "work", Toolchains: []string{"python"}, Egress: []string{"crm"}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.m.Egress(context.Background(), row, egress.Request{Route: "crm", Method: http.MethodGet, Path: "deals?since=2026"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.Status != http.StatusOK || up.auth[0] != "Bearer "+egressToken {
		t.Fatalf("status %d, upstream saw Authorization %q", resp.Status, up.auth)
	}
	if len(creds.agents) != 1 || creds.agents[0] != run.Agent {
		t.Errorf("the credential was resolved for agents %q, want the owner run's %q", creds.agents, run.Agent)
	}
	if len(log.evs) != 1 {
		t.Fatalf("%d audit events, want 1", len(log.evs))
	}
	ev := log.evs[0]
	if ev.Action != "workspace_egress" || ev.EgressRoute != "crm" || ev.EgressPath != "deals" || ev.EgressStatus != http.StatusOK ||
		ev.WorkspaceID != row.ID || ev.WorkspaceOwnerRunID != o.RunID || ev.TargetTenant != "acme" {
		t.Errorf("audit event = %+v", ev)
	}
	b, _ := json.Marshal(ev)
	if strings.Contains(string(b), egressToken) || strings.Contains(string(b), "since=") {
		t.Errorf("the audit event carries the token or the query: %s", b)
	}
}

// A credentialed route whose owner run cannot be read is refused rather
// than resolved without the agent's name.
func TestEgress_ACredentialedRouteWithoutItsRunIsRefused(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	up := newCountingUpstream(t)
	allowed := []string{up.URL + "/api"} // canonical, as credential stores it
	creds := &egressCreds{name: "crm_token", allowed: &allowed}
	f.m.SetEgress(testProxy(t, f.m, creds, egress.Limits{}), OperatorRoutes(map[string]config.EgressRoute{
		"crm": {URL: up.URL + "/api/", Credential: "$cred:crm_token"},
	}))
	row := store.WorkspaceRow{ID: "ws_x", TenantID: "acme", UserID: "alice", OwnerRunID: "run-gone", State: store.WorkspaceReady, Egress: []string{"crm"}}
	if _, _, err := send(t, f.m, row, "crm"); refusalStatus(err) != http.StatusForbidden {
		t.Fatalf("a credentialed route with no run: %v, want a 403 refusal", err)
	}
	if up.hits.Load() != 0 || len(creds.agents) != 0 {
		t.Fatalf("upstream hits %d, credential lookups %d; want none", up.hits.Load(), len(creds.agents))
	}
}

// Releasing a workspace drops the proxy's counts for it.
func TestRelease_ForgetsTheWorkspacesEgressCounts(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	up := newCountingUpstream(t)
	f.m.SetEgress(testProxy(t, f.m, nil, egress.Limits{RatePerMinute: 1}), OperatorRoutes(map[string]config.EgressRoute{
		"pypi": {URL: up.URL + "/pypi/"},
	}))
	o := f.run(t, "alice")
	row, err := f.m.Open(context.Background(), o, OpenRequest{Name: "work", Toolchains: []string{"python"}, Egress: []string{"pypi"}})
	if err != nil {
		t.Fatal(err)
	}
	if status, _, err := send(t, f.m, row, "pypi"); err != nil || status != http.StatusOK {
		t.Fatalf("first request = %d, %v", status, err)
	}
	if _, _, err := send(t, f.m, row, "pypi"); refusalStatus(err) != http.StatusTooManyRequests {
		t.Fatalf("second request in the minute: %v, want 429", err)
	}
	if err := f.m.Release(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if status, _, err := send(t, f.m, row, "pypi"); err != nil || status != http.StatusOK {
		t.Fatalf("after the release the workspace's count is still held: %d, %v", status, err)
	}
}

// With no proxy set every request is refused.
func TestEgress_WithoutAProxyEveryRequestIsRefused(t *testing.T) {
	m := &Manager{}
	if _, err := m.Egress(context.Background(), store.WorkspaceRow{Egress: []string{"pypi"}}, egress.Request{Route: "pypi"}); refusalStatus(err) != http.StatusForbidden {
		t.Fatalf("%v, want a 403 refusal", err)
	}
}

func TestOperatorRoutes_ResolvesDefaultsAndTheCredentialName(t *testing.T) {
	got := OperatorRoutes(map[string]config.EgressRoute{
		"pypi": {URL: "https://pypi.org/"},
		"crm":  {URL: "https://api.crm.example/v2/", Methods: []string{"GET", "POST"}, Credential: "$cred:crm_token", Auth: "header", Header: "X-Api-Key"},
	})
	want := []egress.Route{
		{Name: "crm", URL: "https://api.crm.example/v2/", Methods: []string{"GET", "POST"}, Credential: "crm_token", Auth: "header", Header: "X-Api-Key"},
		{Name: "pypi", URL: "https://pypi.org/", Methods: []string{"GET", "HEAD"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %+v\nwant %+v", got, want)
	}
}

// A fork (RFC GW P6) may send requests out through exactly the routes its
// source may: the same owner opened the source within its grant, and the
// fork is that owner's too.
func TestFork_CarriesTheSourcesEgressRoutes(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	o := f.run(t, "alice")
	src, err := f.m.Open(context.Background(), o, OpenRequest{Name: "work", Toolchains: []string{"python"}, Egress: []string{"pypi", "pypi-files"}})
	if err != nil {
		t.Fatal(err)
	}
	fork, _, err := f.m.Fork(context.Background(), o, src, "work-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fork.Egress, []string{"pypi", "pypi-files"}) {
		t.Fatalf("the fork's egress = %v, want the source's", fork.Egress)
	}
}
