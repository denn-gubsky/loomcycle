package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// minimalServerWithSnapshotStore wires a Server with just enough state
// to exercise the v0.8.17 snapshot handlers: cfg + store. No
// resolver, no providers — handlers don't touch those.
func minimalServerWithSnapshotStore(t *testing.T) (*Server, store.Store, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := &config.Config{
		Channels: map[string]config.Channel{
			"_system/heartbeat": {Scope: "agent", DefaultTTL: 60, Semantic: "heartbeat"},
		},
	}
	hookReg := hooks.NewRegistry()
	srv := &Server{
		cfg:            cfg,
		store:          s,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            concurrency.New(8, 16, 30000),
	}
	return srv, s, func() { _ = s.Close() }
}

// TestCreateSnapshot_HappyPath — POST /v1/_snapshots with a valid
// body returns 201 + a snapshot id + byte_size > 0.
func TestCreateSnapshot_HappyPath(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()

	body := bytes.NewReader([]byte(`{"label": "before-backup"}`))
	rec := httptest.NewRecorder()
	srv.handleCreateSnapshot(rec, httptest.NewRequest("POST", "/v1/_snapshots", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp snapshotCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response unmarshal: %v", err)
	}
	if !strings.HasPrefix(resp.ID, "snap_") {
		t.Errorf("ID = %q, want snap_ prefix", resp.ID)
	}
	if resp.Label != "before-backup" {
		t.Errorf("Label = %q, want before-backup", resp.Label)
	}
	if resp.ByteSize <= 0 {
		t.Errorf("ByteSize = %d, want > 0", resp.ByteSize)
	}
}

// TestCreateSnapshot_EmptyBodyAccepted — empty request body is
// valid (all fields are optional).
func TestCreateSnapshot_EmptyBodyAccepted(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	srv.handleCreateSnapshot(rec, httptest.NewRequest("POST", "/v1/_snapshots", http.NoBody))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestCreateSnapshot_InvalidJSON — malformed body returns 400.
func TestCreateSnapshot_InvalidJSON(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()

	body := bytes.NewReader([]byte(`{"label":}`))
	rec := httptest.NewRecorder()
	srv.handleCreateSnapshot(rec, httptest.NewRequest("POST", "/v1/_snapshots", body))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestCreateSnapshot_MaxBytesEnforced — explicit max_bytes:1 forces
// 413 Payload Too Large.
func TestCreateSnapshot_MaxBytesEnforced(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()

	body := bytes.NewReader([]byte(`{"max_bytes": 1}`))
	rec := httptest.NewRecorder()
	srv.handleCreateSnapshot(rec, httptest.NewRequest("POST", "/v1/_snapshots", body))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "snapshot_too_large") {
		t.Errorf("body missing snapshot_too_large code: %s", rec.Body.String())
	}
}

// TestCreateSnapshot_InvalidSinceTs — invalid RFC3339 → 400.
func TestCreateSnapshot_InvalidSinceTs(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()

	body := bytes.NewReader([]byte(`{"include_history":true,"include_history_since":"not-a-date"}`))
	rec := httptest.NewRecorder()
	srv.handleCreateSnapshot(rec, httptest.NewRequest("POST", "/v1/_snapshots", body))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestListSnapshots_EmptyAndPopulated — list returns the entry just
// created.
func TestListSnapshots_EmptyAndPopulated(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()

	// Empty first.
	rec := httptest.NewRecorder()
	srv.handleListSnapshots(rec, httptest.NewRequest("GET", "/v1/_snapshots", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("empty list status = %d", rec.Code)
	}
	var emptyResp snapshotListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &emptyResp)
	if len(emptyResp.Entries) != 0 {
		t.Errorf("empty list returned %d entries", len(emptyResp.Entries))
	}

	// Create one.
	rec = httptest.NewRecorder()
	srv.handleCreateSnapshot(rec, httptest.NewRequest("POST", "/v1/_snapshots",
		bytes.NewReader([]byte(`{"label":"alpha"}`))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d, %s", rec.Code, rec.Body.String())
	}

	// List again.
	rec = httptest.NewRecorder()
	srv.handleListSnapshots(rec, httptest.NewRequest("GET", "/v1/_snapshots", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list after create: %d", rec.Code)
	}
	var resp snapshotListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Entries) != 1 {
		t.Errorf("entries = %d, want 1", len(resp.Entries))
	}
	if resp.Entries[0].Label != "alpha" {
		t.Errorf("label = %q, want alpha", resp.Entries[0].Label)
	}
}

// TestListSnapshots_LabelFilter — ?label_contains= scopes results.
func TestListSnapshots_LabelFilter(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()

	// Three snapshots with different labels.
	for _, label := range []string{"alpha-pre-backup", "beta-pre-backup", "evening-stop"} {
		rec := httptest.NewRecorder()
		srv.handleCreateSnapshot(rec, httptest.NewRequest("POST", "/v1/_snapshots",
			bytes.NewReader([]byte(`{"label":"`+label+`"}`))))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d", label, rec.Code)
		}
	}

	// Filter on "pre-backup" — expect two matches.
	rec := httptest.NewRecorder()
	srv.handleListSnapshots(rec, httptest.NewRequest("GET", "/v1/_snapshots?label_contains=pre-backup", nil))
	var resp snapshotListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Entries) != 2 {
		t.Errorf("filtered list = %d, want 2", len(resp.Entries))
	}
}

// TestListSnapshots_InvalidLimit — negative limit returns 400.
func TestListSnapshots_InvalidLimit(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	rec := httptest.NewRecorder()
	srv.handleListSnapshots(rec, httptest.NewRequest("GET", "/v1/_snapshots?limit=-1", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestGetSnapshot_FullPayloadReturned — GET /v1/_snapshots/{id}
// returns the full row including the JSON payload.
func TestGetSnapshot_FullPayloadReturned(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()

	// Create one first.
	createRec := httptest.NewRecorder()
	srv.handleCreateSnapshot(createRec, httptest.NewRequest("POST", "/v1/_snapshots",
		bytes.NewReader([]byte(`{}`))))
	var created snapshotCreateResponse
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)

	// Get by id.
	req := httptest.NewRequest("GET", "/v1/_snapshots/"+created.ID, nil)
	req.SetPathValue("id", created.ID)
	rec := httptest.NewRecorder()
	srv.handleGetSnapshot(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp snapshotGetResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.ID != created.ID {
		t.Errorf("ID = %q, want %q", resp.ID, created.ID)
	}
	if len(resp.JSONContent) == 0 {
		t.Error("JSONContent empty; want full envelope payload")
	}
	// Sanity: payload parses as the snapshot envelope.
	var env map[string]any
	if err := json.Unmarshal(resp.JSONContent, &env); err != nil {
		t.Errorf("JSONContent not valid JSON: %v", err)
	}
	if env["schema_version"] == nil {
		t.Error("JSONContent missing schema_version")
	}
}

// TestGetSnapshot_NotFound — missing id returns 404.
func TestGetSnapshot_NotFound(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/v1/_snapshots/snap_does_not_exist", nil)
	req.SetPathValue("id", "snap_does_not_exist")
	rec := httptest.NewRecorder()
	srv.handleGetSnapshot(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestDeleteSnapshot_Idempotent — DELETE returns 204 whether the
// row was present or not.
func TestDeleteSnapshot_Idempotent(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()

	// Create.
	createRec := httptest.NewRecorder()
	srv.handleCreateSnapshot(createRec, httptest.NewRequest("POST", "/v1/_snapshots", http.NoBody))
	var created snapshotCreateResponse
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)

	// Delete.
	req1 := httptest.NewRequest("DELETE", "/v1/_snapshots/"+created.ID, nil)
	req1.SetPathValue("id", created.ID)
	rec1 := httptest.NewRecorder()
	srv.handleDeleteSnapshot(rec1, req1)
	if rec1.Code != http.StatusNoContent {
		t.Errorf("first delete status = %d, want 204", rec1.Code)
	}

	// Delete again (idempotent — still 204).
	req2 := httptest.NewRequest("DELETE", "/v1/_snapshots/"+created.ID, nil)
	req2.SetPathValue("id", created.ID)
	rec2 := httptest.NewRecorder()
	srv.handleDeleteSnapshot(rec2, req2)
	if rec2.Code != http.StatusNoContent {
		t.Errorf("second delete status = %d, want 204 (idempotent)", rec2.Code)
	}

	// Delete with a never-existed id — still 204.
	req3 := httptest.NewRequest("DELETE", "/v1/_snapshots/snap_no_such", nil)
	req3.SetPathValue("id", "snap_no_such")
	rec3 := httptest.NewRecorder()
	srv.handleDeleteSnapshot(rec3, req3)
	if rec3.Code != http.StatusNoContent {
		t.Errorf("delete of non-existent id status = %d, want 204", rec3.Code)
	}
}

// TestCreateSnapshot_ChannelConfigCarriedThrough — operator yaml's
// Channels map flows into the snapshot envelope's channels.config.
func TestCreateSnapshot_ChannelConfigCarriedThrough(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()

	// Create snapshot.
	createRec := httptest.NewRecorder()
	srv.handleCreateSnapshot(createRec, httptest.NewRequest("POST", "/v1/_snapshots", http.NoBody))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: %d", createRec.Code)
	}
	var created snapshotCreateResponse
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)

	// Fetch.
	getReq := httptest.NewRequest("GET", "/v1/_snapshots/"+created.ID, nil)
	getReq.SetPathValue("id", created.ID)
	getRec := httptest.NewRecorder()
	srv.handleGetSnapshot(getRec, getReq)
	var got snapshotGetResponse
	_ = json.Unmarshal(getRec.Body.Bytes(), &got)

	// Drill into the envelope's channels.config.
	var env map[string]any
	_ = json.Unmarshal(got.JSONContent, &env)
	sections, ok := env["sections"].(map[string]any)
	if !ok {
		t.Fatal("envelope missing sections")
	}
	channels, ok := sections["channels"].(map[string]any)
	if !ok {
		t.Fatal("sections missing channels")
	}
	config, ok := channels["config"].([]any)
	if !ok {
		t.Fatal("channels missing config array")
	}
	if len(config) != 1 {
		t.Fatalf("config len = %d, want 1 (operator yaml has one declared channel)", len(config))
	}
	entry := config[0].(map[string]any)
	if entry["name"] != "_system/heartbeat" {
		t.Errorf("config[0].name = %v, want _system/heartbeat", entry["name"])
	}
}
