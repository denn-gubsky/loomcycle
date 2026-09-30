package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// These drive the receiver against a real sqlite store (helpers in
// dedup_scope_test.go) so the recent-deliveries ring is keyed by the def the
// real lookup resolves, exactly like the rate-limit bucket.

// getRecent reads the recent-deliveries list as principal p (nil = open mode,
// no principal in ctx). The admin wrapper is a pass-through: the route's
// substrate:admin gate is enforced by the HTTP server, not here.
func getRecent(t *testing.T, rec *Receiver, path string, p *auth.Principal) (int, []deliveryRecord) {
	t.Helper()
	mux := http.NewServeMux()
	rec.MountAdmin(mux, func(h http.Handler) http.Handler { return h })
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), *p))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		return w.Code, nil
	}
	var resp struct {
		Deliveries []deliveryRecord `json:"deliveries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("GET %s: undecodable body %q: %v", path, w.Body.String(), err)
	}
	return w.Code, resp.Deliveries
}

func runIDs(recs []deliveryRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.RunID)
	}
	return out
}

// assertRunIDs requires the list to be exactly want, newest-first.
func assertRunIDs(t *testing.T, label string, code int, recs []deliveryRecord, want ...string) {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200", label, code)
	}
	got := runIDs(recs)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: run ids = %v, want %v (deliveries of another tenant's webhook mixed in?)", label, got, want)
	}
}

// Two tenants' same-named webhooks are different webhooks, so each has its
// own recent-deliveries list, read through ?tenant=.
func TestReceiver_RecentDeliveries_SameNamedWebhookInTwoTenants_ListedSeparately(t *testing.T) {
	st := openScopeStore(t)
	putWebhookDef(t, st, "acme", "gh", signedSpawnDef("acme"))
	putWebhookDef(t, st, "globex", "gh", signedSpawnDef("globex"))
	rec := newScopeReceiver(st, &storeRunner{st: st}, nil)

	code, got := postSigned(t, rec, "/v1/_webhooks/acme/gh", []byte(`{"goal":"a1"}`), "")
	a1 := assertFreshRun(t, "acme first", code, got)
	code, got = postSigned(t, rec, "/v1/_webhooks/globex/gh", []byte(`{"goal":"g1"}`), "")
	g1 := assertFreshRun(t, "globex first", code, got)
	code, got = postSigned(t, rec, "/v1/_webhooks/acme/gh", []byte(`{"goal":"a2"}`), "")
	a2 := assertFreshRun(t, "acme second", code, got)

	code, recs := getRecent(t, rec, "/v1/_webhooks/gh/recent-deliveries?tenant=acme", nil)
	assertRunIDs(t, "acme", code, recs, a2, a1)
	code, recs = getRecent(t, rec, "/v1/_webhooks/gh/recent-deliveries?tenant=globex", nil)
	assertRunIDs(t, "globex", code, recs, g1)
}

// The endpoint shows the right tenant's list for each kind of caller. A
// tenant-confined principal can read only its own tenant's list: the route is
// substrate:admin-gated today, and this keeps it leak-free if that gate is
// ever widened to tenant operators.
func TestReceiver_RecentDeliveries_ShowsTheCallersTenantAndConfinesNonAdmins(t *testing.T) {
	st := openScopeStore(t)
	putWebhookDef(t, st, "acme", "gh", signedSpawnDef("acme"))
	putWebhookDef(t, st, "globex", "gh", signedSpawnDef("globex"))
	rec := newScopeReceiver(st, &storeRunner{st: st}, nil)

	code, got := postSigned(t, rec, "/v1/_webhooks/acme/gh", []byte(`{"goal":"a1"}`), "")
	a1 := assertFreshRun(t, "acme", code, got)
	code, got = postSigned(t, rec, "/v1/_webhooks/globex/gh", []byte(`{"goal":"g1"}`), "")
	g1 := assertFreshRun(t, "globex", code, got)

	admin := &auth.Principal{TenantID: "ops", Subject: "root", Scopes: []string{auth.ScopeAdmin}}
	acmeOp := &auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}}

	code, recs := getRecent(t, rec, "/v1/_webhooks/gh/recent-deliveries?tenant=globex", admin)
	assertRunIDs(t, "admin focusing globex", code, recs, g1)
	code, recs = getRecent(t, rec, "/v1/_webhooks/gh/recent-deliveries", acmeOp)
	assertRunIDs(t, "acme operator, no ?tenant", code, recs, a1)
	code, recs = getRecent(t, rec, "/v1/_webhooks/gh/recent-deliveries?tenant=acme", acmeOp)
	assertRunIDs(t, "acme operator, own ?tenant", code, recs, a1)
	if code, recs = getRecent(t, rec, "/v1/_webhooks/gh/recent-deliveries?tenant=globex", acmeOp); code != http.StatusNotFound {
		t.Errorf("acme operator reading globex: status = %d, run ids %v; want 404", code, runIDs(recs))
	}
}

// A webhook reached through several URL tenant prefixes is still one def (the
// shared one, when the prefix's tenant has none of its own), so it keeps one
// list, readable with or without ?tenant=. The static-def single-tenant case
// is TestReceiver_RecentDeliveries_ReturnsVerdictsNewestFirstCapped.
func TestReceiver_RecentDeliveries_SharedDefReachedThroughTenantPrefix_OneList(t *testing.T) {
	st := openScopeStore(t)
	putWebhookDef(t, st, "", "gh", signedSpawnDef(""))
	rec := newScopeReceiver(st, &storeRunner{st: st}, nil)

	code, got := postSigned(t, rec, "/v1/_webhooks/gh", []byte(`{"goal":"r1"}`), "")
	r1 := assertFreshRun(t, "bare route", code, got)
	code, got = postSigned(t, rec, "/v1/_webhooks/acme/gh", []byte(`{"goal":"r2"}`), "")
	r2 := assertFreshRun(t, "acme prefix", code, got)

	code, recs := getRecent(t, rec, "/v1/_webhooks/gh/recent-deliveries", nil)
	assertRunIDs(t, "no ?tenant", code, recs, r2, r1)
	legacy := &auth.Principal{TenantID: "default", Subject: "legacy", Legacy: true}
	code, recs = getRecent(t, rec, "/v1/_webhooks/gh/recent-deliveries", legacy)
	assertRunIDs(t, "legacy bearer", code, recs, r2, r1)
	code, recs = getRecent(t, rec, "/v1/_webhooks/gh/recent-deliveries?tenant=acme", nil)
	assertRunIDs(t, "?tenant=acme", code, recs, r2, r1)
}

// The receiver POST is unauthenticated. A delivery to a name that resolves to
// no webhook must not leave a ring behind, or anyone could grow the map
// without bound by posting to made-up names.
func TestReceiver_DeliveriesToUnknownNames_LeaveNoRecentRing(t *testing.T) {
	st := openScopeStore(t)
	rec := newScopeReceiver(st, &storeRunner{st: st}, nil)
	for i := 0; i < 20; i++ {
		code, _ := postSigned(t, rec, fmt.Sprintf("/v1/_webhooks/nope-%d", i), []byte(`{}`), "")
		if code != http.StatusNotFound {
			t.Fatalf("unknown name %d: status = %d, want 404", i, code)
		}
		code, _ = postSigned(t, rec, fmt.Sprintf("/v1/_webhooks/t-%d/nope", i), []byte(`{}`), "")
		if code != http.StatusNotFound {
			t.Fatalf("unknown tenant-prefixed name %d: status = %d, want 404", i, code)
		}
	}
	rec.recentMu.Lock()
	n := len(rec.recent)
	rec.recentMu.Unlock()
	if n != 0 {
		t.Errorf("recent rings after 40 deliveries to unknown webhooks = %d, want 0", n)
	}
	if code, _ := getRecent(t, rec, "/v1/_webhooks/nope-0/recent-deliveries", nil); code != http.StatusNotFound {
		t.Errorf("recent-deliveries for an unknown name: status = %d, want 404", code)
	}
}
