package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestPause_ReturnsServiceUnavailableWhenNoManager — handler returns
// 503 when SetPauseManager hasn't been called. This is the boot-time
// guarantee: routes always registered, behaviour gated on the manager
// being wired. Mirrors the metrics endpoint pattern.
func TestPause_ReturnsServiceUnavailableWhenNoManager(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	for _, path := range []string{"/v1/_pause", "/v1/_resume"} {
		rec := httptest.NewRecorder()
		srv.handlePauseRuntime(rec, httptest.NewRequest("POST", path, nil))
		if path == "/v1/_resume" {
			rec = httptest.NewRecorder()
			srv.handleResumeRuntime(rec, httptest.NewRequest("POST", path, nil))
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	srv.handleRuntimeState(rec, httptest.NewRequest("GET", "/v1/_state", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /v1/_state: status = %d, want 503", rec.Code)
	}
}

// TestPause_HappyPath_TransitionsRunningToPaused — fresh manager,
// state is running, POST /v1/_pause returns 200 with state=paused.
func TestPause_HappyPath_TransitionsRunningToPaused(t *testing.T) {
	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	srv.SetPauseManager(pause.NewManager(st, 50*time.Millisecond))

	rec := httptest.NewRecorder()
	srv.handlePauseRuntime(rec, httptest.NewRequest("POST", "/v1/_pause", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out pause.PauseResult
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.State != "paused" {
		t.Errorf("State = %q, want paused", out.State)
	}
	if out.DurationMs < 0 {
		t.Errorf("DurationMs = %d, want >= 0", out.DurationMs)
	}
}

// TestPause_BodyTimeoutOverride — the body's timeout_ms is honoured
// over the manager's default. Use a generous timeout so the test
// doesn't race against tool drain.
func TestPause_BodyTimeoutOverride(t *testing.T) {
	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	srv.SetPauseManager(pause.NewManager(st, 5*time.Second))

	body := bytes.NewReader([]byte(`{"timeout_ms": 100}`))
	rec := httptest.NewRecorder()
	srv.handlePauseRuntime(rec, httptest.NewRequest("POST", "/v1/_pause", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestPause_InvalidJSONBody — malformed body returns 400; empty body
// is tolerated (all fields optional).
func TestPause_InvalidJSONBody(t *testing.T) {
	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	srv.SetPauseManager(pause.NewManager(st, 50*time.Millisecond))

	rec := httptest.NewRecorder()
	srv.handlePauseRuntime(rec, httptest.NewRequest("POST", "/v1/_pause", bytes.NewReader([]byte(`{not json`))))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestPause_AlreadyPausingReturns409 — second POST while paused
// returns 409 with already_pausing code.
func TestPause_AlreadyPausingReturns409(t *testing.T) {
	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	srv.SetPauseManager(pause.NewManager(st, 50*time.Millisecond))

	rec := httptest.NewRecorder()
	srv.handlePauseRuntime(rec, httptest.NewRequest("POST", "/v1/_pause", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("first pause status = %d", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	srv.handlePauseRuntime(rec2, httptest.NewRequest("POST", "/v1/_pause", http.NoBody))
	if rec2.Code != http.StatusConflict {
		t.Errorf("second pause status = %d, want 409", rec2.Code)
	}
}

// TestResume_RequiresPaused — Resume from StateRunning returns 409.
func TestResume_RequiresPaused(t *testing.T) {
	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	srv.SetPauseManager(pause.NewManager(st, 50*time.Millisecond))

	rec := httptest.NewRecorder()
	srv.handleResumeRuntime(rec, httptest.NewRequest("POST", "/v1/_resume", http.NoBody))
	if rec.Code != http.StatusConflict {
		t.Errorf("resume from running: status = %d, want 409", rec.Code)
	}
}

// TestResume_AfterPauseSucceeds — walk the full Pause → Resume cycle
// via the HTTP surface.
func TestResume_AfterPauseSucceeds(t *testing.T) {
	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	srv.SetPauseManager(pause.NewManager(st, 50*time.Millisecond))

	rec := httptest.NewRecorder()
	srv.handlePauseRuntime(rec, httptest.NewRequest("POST", "/v1/_pause", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("pause status = %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	srv.handleResumeRuntime(rec2, httptest.NewRequest("POST", "/v1/_resume", http.NoBody))
	if rec2.Code != http.StatusOK {
		t.Fatalf("resume status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	var out pause.ResumeResult
	_ = json.Unmarshal(rec2.Body.Bytes(), &out)
	if out.State != "running" {
		t.Errorf("resume State = %q, want running", out.State)
	}
}

// TestState_ReturnsCurrentStateSnapshot — GET /v1/_state returns the
// current state + paused_runs_count. After a Pause cycle the count
// reflects whatever runs have pause_state='paused' in the store.
func TestState_ReturnsCurrentStateSnapshot(t *testing.T) {
	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	srv.SetPauseManager(pause.NewManager(st, 50*time.Millisecond))

	// Pre-state: running, no paused rows.
	rec := httptest.NewRecorder()
	srv.handleRuntimeState(rec, httptest.NewRequest("GET", "/v1/_state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("state status = %d", rec.Code)
	}
	var snap pause.StateSnapshot
	_ = json.Unmarshal(rec.Body.Bytes(), &snap)
	if snap.State != "running" {
		t.Errorf("State = %q, want running", snap.State)
	}
	if snap.PausedRunsCount != 0 {
		t.Errorf("PausedRunsCount = %d, want 0", snap.PausedRunsCount)
	}

	// Seed a paused run, observe count goes up.
	ctx := httptest.NewRequest("GET", "/", nil).Context()
	sess, _ := st.CreateSession(ctx, "t", "a", "u")
	run, _ := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "agent-1"})
	_ = st.SetRunPauseState(ctx, run.ID, store.PauseStatePaused)

	rec2 := httptest.NewRecorder()
	srv.handleRuntimeState(rec2, httptest.NewRequest("GET", "/v1/_state", nil))
	var snap2 pause.StateSnapshot
	_ = json.Unmarshal(rec2.Body.Bytes(), &snap2)
	if snap2.PausedRunsCount != 1 {
		t.Errorf("after paused row: PausedRunsCount = %d, want 1", snap2.PausedRunsCount)
	}
}
