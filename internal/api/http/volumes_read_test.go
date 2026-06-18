package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// reqWithTenant builds a GET request whose context carries an authoritative
// tenant principal — the same shape the auth middleware stamps. The volume
// read handlers derive the tenant from this principal, never from the wire.
func reqWithTenant(path, tenant string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	return r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{
		TenantID: tenant,
		Subject:  "op-" + tenant,
		Scopes:   []string{auth.ScopeTenant},
	}))
}

// reqWithAdmin is reqWithTenant for an operator-equivalent (substrate:admin)
// principal — the set that sees host filesystem paths the tenant operator does
// not (volumesShowPaths).
func reqWithAdmin(path, tenant string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	return r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{
		TenantID: tenant,
		Subject:  "admin-" + tenant,
		Scopes:   []string{auth.ScopeAdmin},
	}))
}

// mkDynamicVolume creates a persistent dynamic VolumeDef for a tenant via the
// real tool, so the test exercises the same write path the runtime uses.
func mkDynamicVolume(t *testing.T, st store.Store, cfg *config.Config, tenant, name, mode string) {
	t.Helper()
	tool := &builtin.VolumeDef{Store: st, Cfg: cfg, MaxNameLen: 64}
	ctx := tools.WithVolumeDefPolicy(
		tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: tenant}),
		tools.VolumeDefPolicyValue{Scopes: []string{"any"}},
	)
	body, _ := json.Marshal(map[string]string{"op": "create", "name": name, "mode": mode})
	if res, _ := tool.Execute(ctx, body); res.IsError {
		t.Fatalf("create dynamic volume %s/%s: %s", tenant, name, res.Text)
	}
}

// TestListVolumes_MergesStaticAndTenantDynamic pins GET /v1/_volumes: every
// static volume (the shared bind floor, source=static, read-only) plus ONLY
// the caller's-tenant dynamic VolumeDefs (source=dynamic).
func TestListVolumes_MergesStaticAndTenantDynamic(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Volumes: map[string]config.Volume{
		"pool":      {Path: root, Mode: "rw", DynamicRoot: true},
		"reference": {Path: root, Mode: "ro", Default: true},
	}}
	mkDynamicVolume(t, st, cfg, "acme", "repo-a", "rw")
	// A DIFFERENT tenant's dynamic volume must NOT appear in acme's view.
	mkDynamicVolume(t, st, cfg, "other", "secret", "rw")

	srv := &Server{store: st, cfg: cfg}
	rec := httptest.NewRecorder()
	srv.handleListVolumes(rec, reqWithTenant("/v1/_volumes", "acme"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp persistentVolumesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	byName := map[string]persistentVolumeEntry{}
	for _, e := range resp.Entries {
		byName[e.Name] = e
	}
	// Both statics present.
	if pool := byName["pool"]; pool.Source != "static" || !pool.DynamicRoot {
		t.Errorf("pool entry wrong: %+v", pool)
	}
	if ref := byName["reference"]; ref.Source != "static" || !ref.Default || ref.Mode != "ro" {
		t.Errorf("reference entry wrong: %+v", ref)
	}
	// acme's own dynamic volume present, with source=dynamic + a timestamp.
	if rd := byName["repo-a"]; rd.Source != "dynamic" || rd.Mode != "rw" || rd.CreatedAt == "" {
		t.Errorf("repo-a entry wrong: %+v", rd)
	}
	// The OTHER tenant's dynamic volume must be ABSENT (opaque cross-tenant).
	if _, leaked := byName["secret"]; leaked {
		t.Errorf("cross-tenant dynamic volume leaked into acme's view: %+v", byName["secret"])
	}
	if len(resp.Entries) != 3 {
		t.Errorf("expected 3 entries (2 static + 1 own dynamic), got %d: %+v", len(resp.Entries), resp.Entries)
	}
}

// TestListEphemeralVolumes_TenantScoped pins GET /v1/_volumes/ephemeral: a
// tenant sees only its OWN live ephemeral rows, never another tenant's.
func TestListEphemeralVolumes_TenantScoped(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	mk := func(rootRun, name, tenant, mode string) {
		body, _ := json.Marshal(map[string]string{"path": "/pool/_ephemeral/" + rootRun + "/" + name, "mode": mode})
		if _, err := st.EphemeralVolumeCreate(context.Background(), store.EphemeralVolumeDefRow{
			RootRunID: rootRun, Name: name, TenantID: tenant, Definition: body,
		}); err != nil {
			t.Fatalf("create ephemeral %s/%s: %v", tenant, name, err)
		}
	}
	mk("run-acme-1", "work", "acme", "rw")
	mk("run-acme-2", "scratch", "acme", "ro")
	mk("run-other-1", "work", "other", "rw")

	srv := &Server{store: st, cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	srv.handleListEphemeralVolumes(rec, reqWithTenant("/v1/_volumes/ephemeral", "acme"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp ephemeralVolumesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("acme should see 2 ephemeral rows, got %d: %+v", len(resp.Entries), resp.Entries)
	}
	for _, e := range resp.Entries {
		if e.RootRunID == "run-other-1" {
			t.Errorf("cross-tenant ephemeral row leaked: %+v", e)
		}
		// A tenant-operator principal gets the host path REDACTED (covered by
		// TestListVolumes_RedactsHostPathsForTenantOperator); created_at stays.
		if e.CreatedAt == "" {
			t.Errorf("entry missing created_at: %+v", e)
		}
		if e.Path != "" {
			t.Errorf("tenant operator must not see ephemeral host path: %+v", e)
		}
	}
}

// TestRequiredScopeFor_VolumeReadsAreTenantConfined is the fail-before guard
// for the gate: both volume read endpoints must require ScopeTenant (not the
// /v1/_* ScopeAdmin catch-all), so a tenant-operator bearer can drive the
// Web UI's Volumes tab. Dropping the requiredScopeFor case → ScopeAdmin → a
// tenant token gets 403 → this fails.
func TestRequiredScopeFor_VolumeReadsAreTenantConfined(t *testing.T) {
	for _, path := range []string{"/v1/_volumes", "/v1/_volumes/ephemeral"} {
		if got := requiredScopeFor(http.MethodGet, path); got != auth.ScopeTenant {
			t.Errorf("requiredScopeFor(GET %s) = %q, want %q", path, got, auth.ScopeTenant)
		}
	}
	// The def-authoring route stays tenant-confined too (unchanged).
	if got := requiredScopeFor(http.MethodPost, "/v1/_volumedef"); got != auth.ScopeTenant {
		t.Errorf("requiredScopeFor(POST /v1/_volumedef) = %q, want %q", got, auth.ScopeTenant)
	}
}

// TestListVolumes_RedactsHostPathsForTenantOperator is the fail-before guard
// for the path-disclosure fix: a ScopeTenant caller (the tenant operator who
// drives the Volumes tab) sees the volume UNIVERSE (name / mode / source /
// default / dynamic-root / created-at) but NOT host filesystem paths — those
// are operator infrastructure. Only an operator-equivalent principal
// (substrate:admin / legacy / open dev mode) sees them. Dropping redactedPath
// → a tenant token sees the operator's host layout → both halves below fail.
func TestListVolumes_RedactsHostPathsForTenantOperator(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Volumes: map[string]config.Volume{
		"pool": {Path: root, Mode: "rw", DynamicRoot: true},
	}}
	mkDynamicVolume(t, st, cfg, "acme", "repo-a", "rw")
	body, _ := json.Marshal(map[string]string{"path": "/pool/_ephemeral/run-1/work", "mode": "rw"})
	if _, err := st.EphemeralVolumeCreate(context.Background(), store.EphemeralVolumeDefRow{
		RootRunID: "run-1", Name: "work", TenantID: "acme", Definition: body,
	}); err != nil {
		t.Fatalf("create ephemeral: %v", err)
	}
	srv := &Server{store: st, cfg: cfg}

	// Operator-equivalent (substrate:admin) sees real host paths everywhere.
	recA := httptest.NewRecorder()
	srv.handleListVolumes(recA, reqWithAdmin("/v1/_volumes", "acme"))
	var adminResp persistentVolumesResponse
	if err := json.Unmarshal(recA.Body.Bytes(), &adminResp); err != nil {
		t.Fatalf("decode admin: %v", err)
	}
	for _, e := range adminResp.Entries {
		if e.Path == "" {
			t.Errorf("admin must see host path for %q (source=%s)", e.Name, e.Source)
		}
	}
	recAE := httptest.NewRecorder()
	srv.handleListEphemeralVolumes(recAE, reqWithAdmin("/v1/_volumes/ephemeral", "acme"))
	var adminEph ephemeralVolumesResponse
	if err := json.Unmarshal(recAE.Body.Bytes(), &adminEph); err != nil {
		t.Fatalf("decode admin ephemeral: %v", err)
	}
	if len(adminEph.Entries) != 1 || adminEph.Entries[0].Path == "" {
		t.Errorf("admin must see ephemeral host path, got %+v", adminEph.Entries)
	}

	// Tenant operator (ScopeTenant) gets every host path redacted — but the
	// rest of the universe survives.
	recT := httptest.NewRecorder()
	srv.handleListVolumes(recT, reqWithTenant("/v1/_volumes", "acme"))
	var tenantResp persistentVolumesResponse
	if err := json.Unmarshal(recT.Body.Bytes(), &tenantResp); err != nil {
		t.Fatalf("decode tenant: %v", err)
	}
	if len(tenantResp.Entries) != 2 {
		t.Fatalf("tenant should see 2 persistent entries (1 static + 1 own dynamic), got %d: %+v", len(tenantResp.Entries), tenantResp.Entries)
	}
	for _, e := range tenantResp.Entries {
		if e.Path != "" {
			t.Errorf("tenant operator must NOT see host path for %q: %q", e.Name, e.Path)
		}
		if e.Name == "" || e.Mode == "" || e.Source == "" {
			t.Errorf("redaction nuked non-path fields: %+v", e)
		}
	}
	recTE := httptest.NewRecorder()
	srv.handleListEphemeralVolumes(recTE, reqWithTenant("/v1/_volumes/ephemeral", "acme"))
	var tenantEph ephemeralVolumesResponse
	if err := json.Unmarshal(recTE.Body.Bytes(), &tenantEph); err != nil {
		t.Fatalf("decode tenant ephemeral: %v", err)
	}
	if len(tenantEph.Entries) != 1 {
		t.Fatalf("tenant should see its 1 ephemeral row, got %d", len(tenantEph.Entries))
	}
	if tenantEph.Entries[0].Path != "" {
		t.Errorf("tenant operator must NOT see ephemeral host path: %q", tenantEph.Entries[0].Path)
	}
}
