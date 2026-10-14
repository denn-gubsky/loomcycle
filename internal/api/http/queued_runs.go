package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EI P3: a run that cannot start at once is accepted as a queued run —
// a row in status "queued", a `queued` event — and starts when a run slot
// frees. Three paths admit a new run (RunOnce, handleRuns, handleMessages);
// they share admitRun for the wait and differ in how they create the row
// and tell their caller.

// queuedRow is the row of a run that had to wait for its slot.
type queuedRow struct {
	sessionID, runID string
	since            time.Time
}

// admitArgs are one path's hooks into admitRun.
type admitArgs struct {
	tenantID, userID string
	// agentID names the run in the queue, so get_run can report its place.
	agentID string
	reject  bool
	// createRow creates the run's row in status queued (and its session).
	// Called at most once, when the run first has to wait.
	createRow func() (sessionID, runID string, err error)
	// announce tells the caller the run exists (its ids) and makes it
	// cancellable. Called once, right after createRow.
	announce func(sessionID, runID string) error
	// forward delivers a queued or admitted event to the caller. The event
	// is already persisted.
	forward func(providers.Event)
}

// admitRun takes a run slot for a new run. A run that has to wait gets its
// row, its announcement and a `queued` event before it blocks, and an
// `admitted` event once it has the slot; q is then non-nil, and the caller
// must start the run with StartQueuedRun (or StartConfiguredRun's queued
// counterpart) instead of creating a row. On an error after the row exists,
// the row is ended here — cancelled when ctx ended, failed otherwise — so the
// caller only has to undo its announcement.
func (s *Server) admitRun(ctx context.Context, a admitArgs) (release func(), q *queuedRow, err error) {
	var mu sync.Mutex // OnQueued runs on the admitting goroutine; mu guards q for the error path
	onQueued := func(info concurrency.QueueInfo) error {
		mu.Lock()
		defer mu.Unlock()
		if q == nil {
			sessionID, runID, err := a.createRow()
			if err != nil {
				return err
			}
			q = &queuedRow{sessionID: sessionID, runID: runID, since: time.Now()}
			if err := a.announce(sessionID, runID); err != nil {
				return err
			}
		}
		s.emitAdmissionEvent(ctx, q.runID, a.forward, providers.Event{
			Type:      providers.EventQueued,
			Admission: &providers.AdmissionInfo{Reason: info.Reason, Position: info.Position},
		})
		return nil
	}
	release, err = s.sem.Admit(ctx, concurrency.AdmitRequest{
		TenantID: a.tenantID,
		UserID:   a.userID,
		Ticket:   a.agentID,
		Reject:   a.reject,
		OnQueued: onQueued,
	})
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		if q != nil {
			s.endQueuedRun(q.runID, err)
		}
		return nil, q, err
	}
	if q != nil {
		s.emitAdmissionEvent(ctx, q.runID, a.forward, providers.Event{
			Type:      providers.EventAdmitted,
			Admission: &providers.AdmissionInfo{WaitMs: time.Since(q.since).Milliseconds()},
		})
	}
	return release, q, nil
}

// endQueuedRun ends a queued run whose wait ended without a slot.
func (s *Server) endQueuedRun(runID string, cause error) {
	if s.store == nil || runID == "" {
		return
	}
	status, msg := store.RunFailed, cause.Error()
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		status, msg = store.RunCancelled, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.FinishRun(ctx, runID, status, "", store.Usage{}, msg); err != nil {
		log.Printf("queue: could not end queued run %s: %v", runID, err)
	}
}

// emitAdmissionEvent persists a queued/admitted event on the run's
// transcript and forwards it. It is sent before the run's recording emit
// exists. It carries no content, so a no-trace run stores it too.
func (s *Server) emitAdmissionEvent(ctx context.Context, runID string, forward func(providers.Event), ev providers.Event) {
	if s.store != nil && runID != "" {
		if payload, err := json.Marshal(ev); err == nil {
			if err := s.store.AppendEvent(context.WithoutCancel(ctx), runID, string(ev.Type), payload); err != nil {
				log.Printf("store: AppendEvent failed (run=%s type=%s): %v", runID, ev.Type, err)
			}
		}
	}
	if forward != nil {
		forward(ev)
	}
}

// admissionError maps a refused or abandoned admission to the runner
// sentinels, as RunOnce has always returned them.
func admissionError(err error) error {
	switch {
	case concurrency.IsPerUserQuotaExhausted(err):
		return fmt.Errorf("%w: %v", runner.ErrPerUserQuotaExhausted, err)
	case concurrency.IsBackpressure(err):
		return fmt.Errorf("%w: %v", runner.ErrBackpressure, err)
	}
	return fmt.Errorf("%w: %v", runner.ErrInternal, err)
}

// errStreamingUnsupported: the ResponseWriter cannot stream, so a queued
// run's SSE stream could not be opened.
var errStreamingUnsupported = errors.New("server does not support streaming on this transport")

// writeRunRowError answers a failure to create a run's row (POST /v1/runs),
// as the row creation after admission always has.
func writeRunRowError(w http.ResponseWriter, err error) {
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if errors.Is(err, runner.ErrRetentionUnsupported) {
		writeNoTraceRefusal(w, err)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// draftStartErr maps a failure to move a configured run on (start, or queue
// for its start) to what RunOnce returns.
func draftStartErr(err error, runID string) error {
	var nf *store.ErrNotFound
	if errors.Is(err, store.ErrRunNotConfigured) || errors.As(err, &nf) {
		return fmt.Errorf("%w: %s", runner.ErrRunNotConfigured, runID)
	}
	if errors.Is(err, store.ErrDraftChanged) {
		return fmt.Errorf("%w: run %s was not started; start it again to run the edited draft", runner.ErrDraftChanged, runID)
	}
	return fmt.Errorf("%w: %v", runner.ErrInternal, err)
}

// queuePositionOf reports where a queued run named agentID stands on this
// replica, for the single-run reads.
func (s *Server) queuePositionOf(agentID string) (concurrency.QueueInfo, bool) {
	if s.sem == nil {
		return concurrency.QueueInfo{}, false
	}
	return s.sem.Position(agentID)
}

// QueuePosition reports where the queued run named agentID waits on this
// replica, for the gRPC single-run reads (grpc.Config.QueuePosition).
func (s *Server) QueuePosition(agentID string) (position int, reason string, ok bool) {
	q, ok := s.queuePositionOf(agentID)
	return q.Position, q.Reason, ok
}

// FailQueuedRunsLostInLastStop fails the runs this replica had queued when it
// last stopped: their waits ended with the process, and nothing will start
// them. Called once at boot, before any run is admitted.
func (s *Server) FailQueuedRunsLostInLastStop(ctx context.Context, bootedAt time.Time) (int, error) {
	if s.store == nil {
		return 0, nil
	}
	return s.store.FailQueuedRuns(ctx, s.replicaID, bootedAt,
		"the run was still queued when loomcycle restarted, so it never started; start it again")
}
