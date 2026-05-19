package http

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// v0.8.18 Connector pause/snapshot impl tests. The wire shape is
// covered by pause_test.go + snapshots_test.go against the HTTP
// handlers; these tests assert the Connector method bodies behave
// identically and translate typed errors correctly. Once these pass,
// the MCP server (which dispatches through Connector) gets the same
// behavior for free, and gRPC + Python adapters can be built on top
// without re-implementing business logic.

func newConnectorTestServer(t *testing.T, withPauseMgr bool) (*Server, store.Store, func()) {
	t.Helper()
	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	if withPauseMgr {
		srv.SetPauseManager(pause.NewManager(st, 50*time.Millisecond))
	}
	return srv, st, cleanup
}

// TestConnector_PauseRuntime_NotConfigured pins that the typed error
// is returned when the pause Manager isn't wired. Transports
// translate to 503 / Unavailable / tool error.
func TestConnector_PauseRuntime_NotConfigured(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, false)
	defer cleanup()

	_, err := srv.PauseRuntime(context.Background(), 0)
	if !errors.Is(err, connector.ErrPauseNotConfigured) {
		t.Errorf("err = %v, want connector.ErrPauseNotConfigured", err)
	}
}

func TestConnector_PauseRuntime_HappyPath(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	res, err := srv.PauseRuntime(context.Background(), 0)
	if err != nil {
		t.Fatalf("PauseRuntime: %v", err)
	}
	if res.Status != "paused" {
		t.Errorf("Status = %q, want paused", res.Status)
	}
	if res.FeatureStatus != "" {
		t.Errorf("FeatureStatus = %q, want empty (v0.8.18 drops the preview marker)", res.FeatureStatus)
	}
}

// TestConnector_PauseRuntime_AlreadyPausing pins the typed error
// surfaces correctly when called twice. Transports translate to
// 409 / FailedPrecondition.
func TestConnector_PauseRuntime_AlreadyPausing(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	if _, err := srv.PauseRuntime(context.Background(), 0); err != nil {
		t.Fatalf("first PauseRuntime: %v", err)
	}
	_, err := srv.PauseRuntime(context.Background(), 0)
	if !errors.Is(err, connector.ErrAlreadyPausing) {
		t.Errorf("second PauseRuntime err = %v, want connector.ErrAlreadyPausing", err)
	}
}

func TestConnector_ResumeRuntime_NotPaused(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	_, err := srv.ResumeRuntime(context.Background())
	if !errors.Is(err, connector.ErrNotPaused) {
		t.Errorf("err = %v, want connector.ErrNotPaused", err)
	}
}

func TestConnector_ResumeRuntime_HappyPath(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	_, _ = srv.PauseRuntime(context.Background(), 0)
	res, err := srv.ResumeRuntime(context.Background())
	if err != nil {
		t.Fatalf("ResumeRuntime: %v", err)
	}
	if res.Status != "running" {
		t.Errorf("Status = %q, want running", res.Status)
	}
}

func TestConnector_GetRuntimeState(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	state, err := srv.GetRuntimeState(context.Background())
	if err != nil {
		t.Fatalf("GetRuntimeState: %v", err)
	}
	if state.Status != "running" {
		t.Errorf("Status = %q, want running", state.Status)
	}
	if state.SnapshotsCount != 0 {
		t.Errorf("SnapshotsCount = %d, want 0 on a fresh store", state.SnapshotsCount)
	}
}

func TestConnector_CreateSnapshot_HappyPath(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	desc, err := srv.CreateSnapshot(context.Background(), connector.CreateSnapshotRequest{
		Description: "test-snap",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if desc.SnapshotID == "" {
		t.Error("SnapshotID empty — real impl mints snap_<ms>_<hex>")
	}
	if desc.Description != "test-snap" {
		t.Errorf("Description = %q, want test-snap", desc.Description)
	}
	if desc.FormatVersion == "" {
		t.Error("FormatVersion empty — should reflect schema_version")
	}
}

// TestConnector_GetSnapshot_NotFound pins ErrSnapshotNotFound
// propagation. Transports map to 404 / NotFound.
func TestConnector_GetSnapshot_NotFound(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	_, err := srv.GetSnapshot(context.Background(), "snap_does_not_exist")
	if !errors.Is(err, connector.ErrSnapshotNotFound) {
		t.Errorf("err = %v, want connector.ErrSnapshotNotFound", err)
	}
}

func TestConnector_GetSnapshot_HappyPath(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	desc, _ := srv.CreateSnapshot(context.Background(), connector.CreateSnapshotRequest{})
	env, err := srv.GetSnapshot(context.Background(), desc.SnapshotID)
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if env.SnapshotID != desc.SnapshotID {
		t.Errorf("SnapshotID round-trip mismatch: %q vs %q", env.SnapshotID, desc.SnapshotID)
	}
	if len(env.JSONContent) == 0 {
		t.Error("JSONContent empty — should hold the full envelope bytes")
	}
	// Confirm it's valid JSON.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(env.JSONContent, &probe); err != nil {
		t.Errorf("JSONContent not valid JSON: %v", err)
	}
	if _, ok := probe["sections"]; !ok {
		t.Error("envelope missing top-level 'sections' key")
	}
}

func TestConnector_ListSnapshots(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	_, _ = srv.CreateSnapshot(context.Background(), connector.CreateSnapshotRequest{Description: "a"})
	_, _ = srv.CreateSnapshot(context.Background(), connector.CreateSnapshotRequest{Description: "b"})

	list, err := srv.ListSnapshots(context.Background())
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("len(list) = %d, want 2", len(list))
	}
}

func TestConnector_ExportSnapshot_NotFound(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	_, err := srv.ExportSnapshot(context.Background(), "snap_nope")
	if !errors.Is(err, connector.ErrSnapshotNotFound) {
		t.Errorf("err = %v, want connector.ErrSnapshotNotFound", err)
	}
}

func TestConnector_ExportSnapshot_RawJSONPopulated(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	desc, _ := srv.CreateSnapshot(context.Background(), connector.CreateSnapshotRequest{})
	out, err := srv.ExportSnapshot(context.Background(), desc.SnapshotID)
	if err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if len(out.RawJSON) == 0 {
		t.Error("RawJSON empty — v0.8.18 path puts envelope bytes here")
	}
	if out.SizeBytes != desc.SizeBytes {
		t.Errorf("SizeBytes = %d, want %d (matches descriptor)", out.SizeBytes, desc.SizeBytes)
	}
}

func TestConnector_RestoreSnapshot_RawJSONInline(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	desc, _ := srv.CreateSnapshot(context.Background(), connector.CreateSnapshotRequest{})
	env, _ := srv.GetSnapshot(context.Background(), desc.SnapshotID)

	res, err := srv.RestoreSnapshot(context.Background(), connector.RestoreSnapshotRequest{
		RawJSON: env.JSONContent,
	})
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	// Restore against the same store is idempotent — ON CONFLICT DO NOTHING.
	// Counters all 0 because every row already exists. Don't assert on
	// values; just confirm the structure round-trips.
	if res.Restored == nil {
		t.Error("Restored map nil — should always have entries even when zero")
	}
}

func TestConnector_RestoreSnapshot_SnapshotIDLookup(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	desc, _ := srv.CreateSnapshot(context.Background(), connector.CreateSnapshotRequest{})
	_, err := srv.RestoreSnapshot(context.Background(), connector.RestoreSnapshotRequest{
		SnapshotID: desc.SnapshotID,
	})
	if err != nil {
		t.Fatalf("RestoreSnapshot by id: %v", err)
	}
}

func TestConnector_RestoreSnapshot_SnapshotIDNotFound(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	_, err := srv.RestoreSnapshot(context.Background(), connector.RestoreSnapshotRequest{
		SnapshotID: "snap_nope",
	})
	if !errors.Is(err, connector.ErrSnapshotNotFound) {
		t.Errorf("err = %v, want connector.ErrSnapshotNotFound", err)
	}
}

func TestConnector_DeleteSnapshot_Idempotent(t *testing.T) {
	srv, _, cleanup := newConnectorTestServer(t, true)
	defer cleanup()

	desc, _ := srv.CreateSnapshot(context.Background(), connector.CreateSnapshotRequest{})
	if err := srv.DeleteSnapshot(context.Background(), desc.SnapshotID); err != nil {
		t.Fatalf("DeleteSnapshot first: %v", err)
	}
	// Second delete: idempotent — returns nil despite no row present.
	if err := srv.DeleteSnapshot(context.Background(), desc.SnapshotID); err != nil {
		t.Errorf("DeleteSnapshot second (idempotent path): %v", err)
	}
}
