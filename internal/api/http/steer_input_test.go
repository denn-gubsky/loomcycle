package http

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/steer"
)

// POST /v1/runs/{run_id}/input delivers an operator steering message to the
// live run's queue (PR 2). Registers an entry with no SessionID so the
// tenant-ownership branch is skipped — that gate reuses sessionOwnershipOK,
// covered by its own tests.
func TestHandleRunInput_DeliversToInFlightRun(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	srv.SetSteerRegistry(steer.NewRegistry(2))
	q, dereg := srv.steerReg.Register(steer.Entry{RunID: "run-1"})
	defer dereg()

	rec := doJSON(t, srv, "POST", "/v1/runs/run-1/input", `{"text":"focus on auth"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	select {
	case m := <-q:
		if m.Text != "focus on auth" {
			t.Errorf("delivered text = %q, want focus on auth", m.Text)
		}
	default:
		t.Fatal("no message delivered to the run's steering queue")
	}
}

func TestHandleRunInput_404WhenNoLiveRun(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	srv.SetSteerRegistry(steer.NewRegistry(2))

	rec := doJSON(t, srv, "POST", "/v1/runs/ghost/input", `{"text":"hi"}`)
	if rec.Code != 404 {
		t.Errorf("status = %d, want 404 (no in-flight run); body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleRunInput_422WhenEmptyText(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	srv.SetSteerRegistry(steer.NewRegistry(2))
	_, dereg := srv.steerReg.Register(steer.Entry{RunID: "run-1"})
	defer dereg()

	rec := doJSON(t, srv, "POST", "/v1/runs/run-1/input", `{"text":"   "}`)
	if rec.Code != 422 {
		t.Errorf("status = %d, want 422 (empty text)", rec.Code)
	}
}

func TestHandleRunInput_429WhenQueueFull(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	srv.SetSteerRegistry(steer.NewRegistry(1)) // buffer depth 1
	_, dereg := srv.steerReg.Register(steer.Entry{RunID: "run-1"})
	defer dereg()

	// Fill the single-slot buffer, then the next push overflows → 429.
	if rec := doJSON(t, srv, "POST", "/v1/runs/run-1/input", `{"text":"1"}`); rec.Code != 200 {
		t.Fatalf("first push status = %d, want 200", rec.Code)
	}
	rec := doJSON(t, srv, "POST", "/v1/runs/run-1/input", `{"text":"2"}`)
	if rec.Code != 429 {
		t.Errorf("status = %d, want 429 (queue full)", rec.Code)
	}
}
