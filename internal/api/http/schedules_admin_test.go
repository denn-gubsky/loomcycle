package http

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// schedulesTenantFixture builds a Server with one operator-global static yaml
// schedule + two substrate schedules owned by different tenants (acme, globex),
// each with a seeded run-state row. Handlers are called directly with an
// injected principal so each case exercises a specific tenant scope (RFC AS).
func schedulesTenantFixture(t *testing.T) (srv *Server, acmeDef, globexDef string) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{
		ScheduledRuns: map[string]config.ScheduledRun{
			"yaml-sched": {Agent: "researcher", Schedule: "0 6 * * *", Enabled: true},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100},
	}
	cfg.Env.AuthToken = "test-token"

	ctx := t.Context()
	defJSON, _ := json.Marshal(map[string]any{"agent": "researcher", "schedule": "0 9 * * 1", "enabled": true})
	mk := func(defID, name, tenant string) {
		if _, err := st.ScheduleDefCreate(ctx, store.ScheduleDefRow{
			DefID: defID, Name: name, TenantID: tenant, Definition: defJSON,
		}); err != nil {
			t.Fatalf("ScheduleDefCreate(%s): %v", name, err)
		}
		if err := st.ScheduleDefSetActive(ctx, tenant, name, defID, "test"); err != nil {
			t.Fatalf("ScheduleDefSetActive(%s): %v", name, err)
		}
		if err := st.ScheduleRunStateSeed(ctx, defID, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("ScheduleRunStateSeed(%s): %v", name, err)
		}
	}
	acmeDef, globexDef = "sd_acme", "sd_globex"
	mk(acmeDef, "acme-sched", "acme")
	mk(globexDef, "globex-sched", "globex")

	srv = New(cfg, &stubResolver{}, []tools.Tool{}, concurrency.New(1, 1, time.Second), st)
	return srv, acmeDef, globexDef
}

// TestSchedulesList_TenantScoped (RFC AS) — a substrate:tenant principal sees
// only the schedules IT authored (its own tenant's substrate rows), not another
// tenant's and not the operator-global static yaml cron; admin sees all. Fails
// on the pre-RFC-AS handler (which listed every tenant's defs + the static).
func TestSchedulesList_TenantScoped(t *testing.T) {
	srv, _, _ := schedulesTenantFixture(t)
	call := func(p auth.Principal) []string {
		req := httptest.NewRequest("GET", "/v1/_schedules/list-all", nil)
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		rec := httptest.NewRecorder()
		srv.handleListSchedules(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
		}
		var env schedulesListResponse
		if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		names := make([]string, len(env.Entries))
		for i, e := range env.Entries {
			names[i] = e.Name
		}
		return names
	}
	if got := call(auth.Principal{TenantID: "acme", Subject: "acme-op", Scopes: []string{auth.ScopeTenant}}); len(got) != 1 || got[0] != "acme-sched" {
		t.Fatalf("acme tenant sees %v, want [acme-sched] only (no globex, no static)", got)
	}
	if got := call(auth.Principal{TenantID: "default", Subject: "ops", Scopes: []string{auth.ScopeAdmin}}); len(got) != 3 {
		t.Fatalf("admin sees %v, want all 3 (acme-sched, globex-sched, yaml-sched)", got)
	}
}

// TestSchedulePause_TenantConfined (RFC AS) — a substrate:tenant principal may
// pause only its OWN tenant's schedule def; another tenant's def (and a static /
// unknown def) opaque-404s. Admin acts on any. Fails on the pre-RFC-AS handler
// (which paused any def_id for any authenticated caller).
func TestSchedulePause_TenantConfined(t *testing.T) {
	srv, acmeDef, globexDef := schedulesTenantFixture(t)
	pause := func(p auth.Principal, defID string) int {
		req := httptest.NewRequest("POST", "/v1/_schedules/"+defID+"/pause", nil)
		req.SetPathValue("def_id", defID)
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		rec := httptest.NewRecorder()
		srv.handleSchedulePause(rec, req)
		return rec.Code
	}
	acme := auth.Principal{TenantID: "acme", Subject: "acme-op", Scopes: []string{auth.ScopeTenant}}
	admin := auth.Principal{TenantID: "default", Subject: "ops", Scopes: []string{auth.ScopeAdmin}}

	if c := pause(acme, acmeDef); c != http.StatusOK {
		t.Errorf("acme pausing its own schedule = %d, want 200", c)
	}
	if c := pause(acme, globexDef); c != http.StatusNotFound {
		t.Errorf("acme pausing globex's schedule = %d, want 404 (cross-tenant opaque)", c)
	}
	if c := pause(acme, "sd_nope"); c != http.StatusNotFound {
		t.Errorf("acme pausing unknown/static def = %d, want 404", c)
	}
	if c := pause(admin, globexDef); c != http.StatusOK {
		t.Errorf("admin pausing any schedule = %d, want 200", c)
	}
}

// schedulesAdminFixture spins up an HTTP server with one yaml-defined
// schedule + one substrate-defined schedule, ready for the list +
// state + run-now/pause/resume tests.
func schedulesAdminFixture(t *testing.T) (*httptest.Server, store.Store, string) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{
		ScheduledRuns: map[string]config.ScheduledRun{
			"yaml-sched": {
				Agent:    "researcher",
				Schedule: "0 6 * * *",
				Enabled:  true,
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100},
	}
	cfg.Env.AuthToken = "test-token"

	// Seed one substrate schedule.
	ctx := t.Context()
	defID := "sd_test_1"
	defJSON, _ := json.Marshal(map[string]any{
		"agent": "researcher", "schedule": "0 9 * * 1", "enabled": true, "user_id": "alice",
	})
	_, _ = st.ScheduleDefCreate(ctx, store.ScheduleDefRow{
		DefID:      defID,
		Name:       "substrate-sched",
		Definition: defJSON,
	})
	_ = st.ScheduleDefSetActive(ctx, "", "substrate-sched", defID, "test")
	_ = st.ScheduleRunStateSeed(ctx, defID, time.Now().Add(1*time.Hour))

	srv := New(cfg, &stubResolver{}, []tools.Tool{}, concurrency.New(1, 1, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts, st, defID
}

func authGET(t *testing.T, ts *httptest.Server, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func authPOST(t *testing.T, ts *httptest.Server, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSchedulesList_MergesStaticAndSubstrate(t *testing.T) {
	ts, _, _ := schedulesAdminFixture(t)
	resp := authGET(t, ts, "/v1/_schedules/list-all")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; body=%s", resp.StatusCode, raw)
	}
	var env schedulesListResponse
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Entries) != 2 {
		t.Fatalf("got %d entries, want 2 (yaml + substrate)", len(env.Entries))
	}
	bySrc := map[string]ScheduleListEntry{}
	for _, e := range env.Entries {
		bySrc[e.Name] = e
	}
	yamlEntry, ok := bySrc["yaml-sched"]
	if !ok {
		t.Fatalf("yaml-sched not in entries")
	}
	if yamlEntry.Source != "static-only" {
		t.Errorf("yaml-sched source = %q, want static-only", yamlEntry.Source)
	}
	if len(yamlEntry.StaticDefinition) == 0 {
		t.Errorf("yaml-sched should inline static_definition")
	}
	subEntry, ok := bySrc["substrate-sched"]
	if !ok {
		t.Fatalf("substrate-sched not in entries")
	}
	if subEntry.Source != "dynamic-only" {
		t.Errorf("substrate-sched source = %q, want dynamic-only", subEntry.Source)
	}
	if subEntry.ActiveDefID == "" {
		t.Errorf("substrate-sched active_def_id should be set")
	}
}

func TestScheduleState_ReturnsRow(t *testing.T) {
	ts, _, defID := schedulesAdminFixture(t)
	resp := authGET(t, ts, "/v1/_schedules/"+defID+"/state")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; body=%s", resp.StatusCode, raw)
	}
	var view ScheduleStateView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.DefID != defID {
		t.Errorf("def_id = %q, want %q", view.DefID, defID)
	}
	if view.NextRunAt.IsZero() {
		t.Errorf("next_run_at should be set")
	}
}

func TestScheduleState_NotFound(t *testing.T) {
	ts, _, _ := schedulesAdminFixture(t)
	resp := authGET(t, ts, "/v1/_schedules/unknown_def/state")
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestScheduleRunNow_MovesNextRunAtToPast(t *testing.T) {
	ts, st, defID := schedulesAdminFixture(t)
	// Initially next_run_at is 1h in the future (per fixture seed).
	resp := authPOST(t, ts, "/v1/_schedules/"+defID+"/run-now")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; body=%s", resp.StatusCode, raw)
	}
	got, err := st.ScheduleRunStateGet(t.Context(), defID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if !got.NextRunAt.Before(time.Now()) {
		t.Errorf("next_run_at = %v, expected past — sweeper would not pick this up on next tick", got.NextRunAt)
	}
}

func TestSchedulePauseResume_RoundTrip(t *testing.T) {
	ts, st, defID := schedulesAdminFixture(t)

	// Pause.
	resp := authPOST(t, ts, "/v1/_schedules/"+defID+"/pause")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("pause status = %d", resp.StatusCode)
	}
	got, _ := st.ScheduleRunStateGet(t.Context(), defID)
	if got.PausedUntil.IsZero() {
		t.Errorf("paused_until should be set after pause")
	}
	// Should be ~100 years in the future (the handler's "indefinite
	// pause" sentinel). Cap is just a sanity check that it's not the
	// near future.
	if !got.PausedUntil.After(time.Now().Add(50 * 365 * 24 * time.Hour)) {
		t.Errorf("paused_until = %v, expected far-future (>= 50y)", got.PausedUntil)
	}

	// Resume.
	resp = authPOST(t, ts, "/v1/_schedules/"+defID+"/resume")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("resume status = %d", resp.StatusCode)
	}
	got2, _ := st.ScheduleRunStateGet(t.Context(), defID)
	if !got2.PausedUntil.IsZero() {
		t.Errorf("paused_until should be cleared after resume; got %v", got2.PausedUntil)
	}
}

// TestScheduleRunNow_UnknownDefIDReturns404 regresses the v1.x review
// finding: previously run-now hit the FK constraint on the upsert and
// returned 500. Now the pre-flight ScheduleRunStateGet check produces
// a typed 404 matching pause/resume's shape.
func TestScheduleRunNow_UnknownDefIDReturns404(t *testing.T) {
	ts, _, _ := schedulesAdminFixture(t)
	resp := authPOST(t, ts, "/v1/_schedules/unknown_def_xyz/run-now")
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 404; body=%s", resp.StatusCode, raw)
	}
}

// TestScheduleAdmin_AllEndpointsRequireBearer covers ALL five new
// endpoints, not just list-all. The auth middleware is shared, but
// explicit coverage ensures a future route added without
// authMiddleware doesn't silently ship.
func TestScheduleAdmin_AllEndpointsRequireBearer(t *testing.T) {
	ts, _, defID := schedulesAdminFixture(t)
	cases := []struct {
		method string
		path   string
	}{
		{"GET", "/v1/_schedules/list-all"},
		{"GET", "/v1/_schedules/" + defID + "/state"},
		{"POST", "/v1/_schedules/" + defID + "/run-now"},
		{"POST", "/v1/_schedules/" + defID + "/pause"},
		{"POST", "/v1/_schedules/" + defID + "/resume"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			req, _ := http.NewRequest(c.method, ts.URL+c.path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Errorf("unauthenticated %s %s should 401; got %d", c.method, c.path, resp.StatusCode)
			}
		})
	}
}
