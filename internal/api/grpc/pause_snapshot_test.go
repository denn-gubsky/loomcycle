package grpc

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// v0.8.18 gRPC pause/snapshot tests. The wire shape is covered by the
// HTTP handler tests + the Connector impl tests in
// internal/api/http/connector_impl_pause_snapshot_test.go. These tests
// assert (a) the gRPC handlers dispatch through s.connector, (b) the
// proto wire shape round-trips field values correctly, (c) typed
// errors translate to the expected gRPC status codes.

// pauseSnapshotMock is a programmable Connector stub purpose-built
// for these tests. Each method returns whatever the test stages.
// Unrelated Connector methods are inherited from the existing
// mockConnector via embedding.
type pauseSnapshotMock struct {
	mockConnector

	pauseResult         connector.PauseResult
	pauseErr            error
	resumeResult        connector.ResumeResult
	resumeErr           error
	stateResult         connector.RuntimeState
	stateErr            error
	createSnapshotResp  connector.SnapshotDescriptor
	createSnapshotErr   error
	listSnapshotsResp   []connector.SnapshotDescriptor
	getSnapshotResp     connector.SnapshotEnvelope
	getSnapshotErr      error
	exportSnapshotResp  connector.ExportSnapshotResult
	exportSnapshotErr   error
	restoreSnapshotResp connector.RestoreSnapshotResult
	restoreSnapshotErr  error
	deleteErr           error

	lastCreateReq  connector.CreateSnapshotRequest
	lastRestoreReq connector.RestoreSnapshotRequest
	lastDeleteID   string
	lastGetID      string
	lastExportID   string
	lastPauseMS    int
}

func (m *pauseSnapshotMock) PauseRuntime(_ context.Context, timeoutMS int) (connector.PauseResult, error) {
	m.lastPauseMS = timeoutMS
	return m.pauseResult, m.pauseErr
}
func (m *pauseSnapshotMock) ResumeRuntime(context.Context) (connector.ResumeResult, error) {
	return m.resumeResult, m.resumeErr
}
func (m *pauseSnapshotMock) GetRuntimeState(context.Context) (connector.RuntimeState, error) {
	return m.stateResult, m.stateErr
}
func (m *pauseSnapshotMock) CreateSnapshot(_ context.Context, req connector.CreateSnapshotRequest) (connector.SnapshotDescriptor, error) {
	m.lastCreateReq = req
	return m.createSnapshotResp, m.createSnapshotErr
}
func (m *pauseSnapshotMock) ListSnapshots(context.Context) ([]connector.SnapshotDescriptor, error) {
	return m.listSnapshotsResp, nil
}
func (m *pauseSnapshotMock) GetSnapshot(_ context.Context, id string) (connector.SnapshotEnvelope, error) {
	m.lastGetID = id
	return m.getSnapshotResp, m.getSnapshotErr
}
func (m *pauseSnapshotMock) ExportSnapshot(_ context.Context, id string) (connector.ExportSnapshotResult, error) {
	m.lastExportID = id
	return m.exportSnapshotResp, m.exportSnapshotErr
}
func (m *pauseSnapshotMock) RestoreSnapshot(_ context.Context, req connector.RestoreSnapshotRequest) (connector.RestoreSnapshotResult, error) {
	m.lastRestoreReq = req
	return m.restoreSnapshotResp, m.restoreSnapshotErr
}
func (m *pauseSnapshotMock) DeleteSnapshot(_ context.Context, id string) error {
	m.lastDeleteID = id
	return m.deleteErr
}

// startTestServerWithConnector mirrors startTestServer but wires the
// supplied Connector. Returns a connected client + the cleanup.
func startTestServerWithConnector(t *testing.T, mc connector.Connector) (loomcyclepb.LoomcycleClient, func()) {
	t.Helper()
	adapter := New(Config{
		Store:     newTestStore(t),
		CancelReg: cancel.NewRegistry(),
		Connector: mc,
	})
	grpcSrv := googlegrpc.NewServer()
	loomcyclepb.RegisterLoomcycleServer(grpcSrv, adapter)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = grpcSrv.Serve(lis) }()
	conn, err := googlegrpc.NewClient(lis.Addr().String(),
		googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client := loomcyclepb.NewLoomcycleClient(conn)
	return client, func() {
		_ = conn.Close()
		grpcSrv.GracefulStop()
	}
}

// --- PauseRuntime ---

func TestGrpcPauseRuntime_HappyPath(t *testing.T) {
	mc := &pauseSnapshotMock{
		pauseResult: connector.PauseResult{
			Status:              "paused",
			DurationMS:          42,
			ForceCancelledCount: 1,
			PausedRunsCount:     2,
			Warnings:            []string{"flaky-tool"},
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.PauseRuntime(context.Background(), &loomcyclepb.PauseRuntimeRequest{TimeoutMs: 5000})
	if err != nil {
		t.Fatalf("PauseRuntime: %v", err)
	}
	if resp.GetStatus() != "paused" {
		t.Errorf("status = %q, want paused", resp.GetStatus())
	}
	if resp.GetDurationMs() != 42 {
		t.Errorf("duration_ms = %d, want 42", resp.GetDurationMs())
	}
	if resp.GetPausedRunsCount() != 2 {
		t.Errorf("paused_runs_count = %d, want 2", resp.GetPausedRunsCount())
	}
	if got := mc.lastPauseMS; got != 5000 {
		t.Errorf("Connector saw timeout_ms = %d, want 5000", got)
	}
}

func TestGrpcPauseRuntime_AlreadyPausing(t *testing.T) {
	mc := &pauseSnapshotMock{pauseErr: connector.ErrAlreadyPausing}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	_, err := client.PauseRuntime(context.Background(), &loomcyclepb.PauseRuntimeRequest{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestGrpcPauseRuntime_NotConfigured(t *testing.T) {
	mc := &pauseSnapshotMock{pauseErr: connector.ErrPauseNotConfigured}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	_, err := client.PauseRuntime(context.Background(), &loomcyclepb.PauseRuntimeRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", status.Code(err))
	}
}

func TestGrpcResumeRuntime_NotPaused(t *testing.T) {
	mc := &pauseSnapshotMock{resumeErr: connector.ErrNotPaused}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	_, err := client.ResumeRuntime(context.Background(), &loomcyclepb.ResumeRuntimeRequest{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestGrpcResumeRuntime_HappyPath(t *testing.T) {
	mc := &pauseSnapshotMock{
		resumeResult: connector.ResumeResult{
			Status:          "running",
			ResumedRunCount: 5,
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.ResumeRuntime(context.Background(), &loomcyclepb.ResumeRuntimeRequest{})
	if err != nil {
		t.Fatalf("ResumeRuntime: %v", err)
	}
	if resp.GetStatus() != "running" {
		t.Errorf("status = %q, want running", resp.GetStatus())
	}
	if resp.GetResumedRunCount() != 5 {
		t.Errorf("resumed_run_count = %d, want 5", resp.GetResumedRunCount())
	}
}

func TestGrpcGetRuntimeState(t *testing.T) {
	mc := &pauseSnapshotMock{
		stateResult: connector.RuntimeState{
			Status:         "paused",
			PausedRunCount: 3,
			SnapshotsCount: 7,
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.GetRuntimeState(context.Background(), &loomcyclepb.GetRuntimeStateRequest{})
	if err != nil {
		t.Fatalf("GetRuntimeState: %v", err)
	}
	if resp.GetStatus() != "paused" {
		t.Errorf("status = %q, want paused", resp.GetStatus())
	}
	if resp.GetPausedRunCount() != 3 {
		t.Errorf("paused_run_count = %d, want 3", resp.GetPausedRunCount())
	}
	if resp.GetSnapshotsCount() != 7 {
		t.Errorf("snapshots_count = %d, want 7", resp.GetSnapshotsCount())
	}
}

// --- Snapshot lifecycle ---

func TestGrpcCreateSnapshot_HappyPath(t *testing.T) {
	mc := &pauseSnapshotMock{
		createSnapshotResp: connector.SnapshotDescriptor{
			SnapshotID:    "snap_xyz",
			CreatedAt:     time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC),
			SizeBytes:     1024,
			Description:   "test",
			FormatVersion: "1",
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.CreateSnapshot(context.Background(), &loomcyclepb.CreateSnapshotRequest{
		Description: "test",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if resp.GetSnapshotId() != "snap_xyz" {
		t.Errorf("snapshot_id = %q, want snap_xyz", resp.GetSnapshotId())
	}
	if mc.lastCreateReq.Description != "test" {
		t.Errorf("Connector saw Description = %q, want test", mc.lastCreateReq.Description)
	}
}

func TestGrpcCreateSnapshot_TooLarge(t *testing.T) {
	mc := &pauseSnapshotMock{createSnapshotErr: connector.ErrSnapshotTooLarge}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	_, err := client.CreateSnapshot(context.Background(), &loomcyclepb.CreateSnapshotRequest{})
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("code = %v, want ResourceExhausted", status.Code(err))
	}
}

func TestGrpcListSnapshots(t *testing.T) {
	mc := &pauseSnapshotMock{
		listSnapshotsResp: []connector.SnapshotDescriptor{
			{SnapshotID: "snap_a", CreatedAt: time.Now()},
			{SnapshotID: "snap_b", CreatedAt: time.Now()},
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.ListSnapshots(context.Background(), &loomcyclepb.ListSnapshotsRequest{})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(resp.GetSnapshots()) != 2 {
		t.Errorf("len(snapshots) = %d, want 2", len(resp.GetSnapshots()))
	}
}

func TestGrpcGetSnapshot_HappyPath(t *testing.T) {
	envelope := json.RawMessage(`{"schema_version":1,"sections":{}}`)
	mc := &pauseSnapshotMock{
		getSnapshotResp: connector.SnapshotEnvelope{
			SnapshotID:    "snap_xyz",
			CreatedAt:     time.Now(),
			Description:   "test",
			FormatVersion: "1",
			SizeBytes:     int64(len(envelope)),
			JSONContent:   envelope,
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.GetSnapshot(context.Background(), &loomcyclepb.GetSnapshotRequest{SnapshotId: "snap_xyz"})
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if resp.GetSnapshotId() != "snap_xyz" {
		t.Errorf("snapshot_id = %q, want snap_xyz", resp.GetSnapshotId())
	}
	if string(resp.GetJsonContent()) != string(envelope) {
		t.Errorf("json_content round-trip mismatch")
	}
	if mc.lastGetID != "snap_xyz" {
		t.Errorf("Connector saw id = %q, want snap_xyz", mc.lastGetID)
	}
}

func TestGrpcGetSnapshot_NotFound(t *testing.T) {
	mc := &pauseSnapshotMock{getSnapshotErr: connector.ErrSnapshotNotFound}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	_, err := client.GetSnapshot(context.Background(), &loomcyclepb.GetSnapshotRequest{SnapshotId: "x"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %v, want NotFound", status.Code(err))
	}
}

func TestGrpcExportSnapshot_HappyPath(t *testing.T) {
	envelope := []byte(`{"schema_version":1,"sections":{}}`)
	mc := &pauseSnapshotMock{
		exportSnapshotResp: connector.ExportSnapshotResult{
			SnapshotID: "snap_xyz",
			SizeBytes:  int64(len(envelope)),
			RawJSON:    envelope,
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.ExportSnapshot(context.Background(), &loomcyclepb.ExportSnapshotRequest{SnapshotId: "snap_xyz"})
	if err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if string(resp.GetRawJson()) != string(envelope) {
		t.Errorf("raw_json round-trip mismatch")
	}
}

func TestGrpcExportSnapshot_NotFound(t *testing.T) {
	mc := &pauseSnapshotMock{exportSnapshotErr: connector.ErrSnapshotNotFound}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	_, err := client.ExportSnapshot(context.Background(), &loomcyclepb.ExportSnapshotRequest{SnapshotId: "x"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %v, want NotFound", status.Code(err))
	}
}

func TestGrpcRestoreSnapshot_HappyPath(t *testing.T) {
	mc := &pauseSnapshotMock{
		restoreSnapshotResp: connector.RestoreSnapshotResult{
			MemoryRestored:     3,
			PausedRunsRestored: 1,
		},
	}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.RestoreSnapshot(context.Background(), &loomcyclepb.RestoreSnapshotRequest{
		SnapshotId: "snap_xyz",
	})
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if resp.GetMemoryRestored() != 3 {
		t.Errorf("memory_restored = %d, want 3", resp.GetMemoryRestored())
	}
	if resp.GetPausedRunsRestored() != 1 {
		t.Errorf("paused_runs_restored = %d, want 1", resp.GetPausedRunsRestored())
	}
	if mc.lastRestoreReq.SnapshotID != "snap_xyz" {
		t.Errorf("Connector saw SnapshotID = %q, want snap_xyz", mc.lastRestoreReq.SnapshotID)
	}
}

func TestGrpcRestoreSnapshot_VersionTooNew(t *testing.T) {
	mc := &pauseSnapshotMock{restoreSnapshotErr: connector.ErrSnapshotVersionTooNew}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	_, err := client.RestoreSnapshot(context.Background(), &loomcyclepb.RestoreSnapshotRequest{SnapshotId: "x"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestGrpcDeleteSnapshot_Idempotent(t *testing.T) {
	mc := &pauseSnapshotMock{}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.DeleteSnapshot(context.Background(), &loomcyclepb.DeleteSnapshotRequest{SnapshotId: "snap_xyz"})
	if err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if !resp.GetDeleted() {
		t.Error("deleted = false, want true (idempotent contract)")
	}
	if mc.lastDeleteID != "snap_xyz" {
		t.Errorf("Connector saw id = %q, want snap_xyz", mc.lastDeleteID)
	}
}

// --- Connector wiring guard ---

// TestGrpcPauseRuntime_NoConnectorReturnsUnavailable pins that a
// gRPC server constructed without a Connector (legacy test config)
// returns codes.Unavailable rather than panicking — matches the
// HTTP connector layer's "pause manager not configured" posture.
func TestGrpcPauseRuntime_NoConnectorReturnsUnavailable(t *testing.T) {
	client, _, _, cleanup := startTestServer(t, "")
	defer cleanup()

	_, err := client.PauseRuntime(context.Background(), &loomcyclepb.PauseRuntimeRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", status.Code(err))
	}
}
