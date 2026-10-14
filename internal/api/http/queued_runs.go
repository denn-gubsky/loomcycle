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
	"github.com/denn-gubsky/loomcycle/internal/credential"
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

// --- restart (RFC EI P3) ---
//
// A queued run's waiter is a goroutine, gone with the process. So that a
// restart can start the run again, the request it was queued with is stored
// on its row (runs.queued_input) when it queues: the RunInput without its
// secrets, and the secrets — user_bearer, user_credentials — sealed per tenant
// and bound to the run. Every queued run is restarted, a caller-bound one too:
// its caller lost the connection with the restart and re-attaches by run id.
//
// Stored only for a run that can be started that way. A no-trace run stores
// its prompt nowhere, and a run with secrets on a deployment with no
// LOOMCYCLE_SECRET_KEY never stores them in plaintext: both record why they
// cannot be restarted instead, and the restart fails them with that reason.

// queuedRecord is what runs.queued_input holds.
type queuedRecord struct {
	// Input is the run's request, secrets removed. Nil when Lost is set.
	Input *runner.RunInput `json:"input,omitempty"`
	// Secrets are the run's per-run secrets (queuedSecrets), sealed.
	Secrets *credential.Sealed `json:"secrets,omitempty"`
	// Lost says why the run cannot be started after a restart.
	Lost string `json:"lost,omitempty"`
}

type queuedSecrets struct {
	UserBearer      string            `json:"user_bearer,omitempty"`
	UserCredentials map[string]string `json:"user_credentials,omitempty"`
}

// queuedSecretsIdentity binds sealed secrets to one run of one tenant: a
// ciphertext copied onto another row does not open.
func queuedSecretsIdentity(tenantID, runID string) credential.Identity {
	return credential.Identity{TenantID: tenantID, Scope: "queued_run", ScopeID: runID, Name: "run_secrets"}
}

const (
	lostNoTrace = "a no-trace run stores its prompt nowhere, so a restart cannot start it again; start it again"
	lostNoKey   = "the run carried per-run secrets and LOOMCYCLE_SECRET_KEY is not set, so they were not stored and a restart cannot start it again; start it again"
)

// SetQueueSealer gives the server the sealer for a queued run's secrets. A
// disabled sealer (no LOOMCYCLE_SECRET_KEY) or none stores no secrets.
func (s *Server) SetQueueSealer(sealer *credential.Sealer) { s.queueSealer = sealer }

// recordQueuedInput stores what a restart needs to start the queued run runID
// again. Best effort: a run whose request could not be stored is failed at
// the next restart, never lost silently.
func (s *Server) recordQueuedInput(ctx context.Context, runID, tenantID, retention string, in runner.RunInput) {
	if s.store == nil || runID == "" {
		return
	}
	var rec queuedRecord
	switch {
	case retention == store.RetentionNone:
		rec.Lost = lostNoTrace
	case in.UserBearer != "" || len(in.UserCredentials) > 0:
		if s.queueSealer == nil || !s.queueSealer.Enabled() {
			rec.Lost = lostNoKey
			break
		}
		pt, err := json.Marshal(queuedSecrets{UserBearer: in.UserBearer, UserCredentials: in.UserCredentials})
		if err != nil {
			log.Printf("queue: run %s: could not encode its secrets: %v", runID, err)
			return
		}
		sealed, err := s.queueSealer.Seal(pt, queuedSecretsIdentity(tenantID, runID))
		if err != nil {
			log.Printf("queue: run %s: could not seal its secrets: %v", runID, err)
			return
		}
		rec.Secrets = &sealed
	}
	if rec.Lost == "" {
		stored := in
		stored.UserBearer, stored.UserCredentials = "", nil
		rec.Input = &stored
	}
	b, err := json.Marshal(rec)
	if err != nil {
		log.Printf("queue: run %s: could not encode its request: %v", runID, err)
		return
	}
	if err := s.store.SetQueuedInput(context.WithoutCancel(ctx), runID, b); err != nil && !errors.Is(err, store.ErrRunNotQueued) {
		log.Printf("queue: run %s: could not store its request: %v", runID, err)
	}
}

// requestRunInput is the RunInput a POST /v1/runs request starts as after a
// restart: the conversion a configured run's start uses, so a request field
// that conversion carries is carried here too (TestRunDraft_CarriesEveryRequestField).
func requestRunInput(req runRequest) (runner.RunInput, error) {
	d, err := requestToDraft(req)
	if err != nil {
		return runner.RunInput{}, err
	}
	in := spawnRequestToRunInput(d.SpawnRunRequest)
	in.RunTimeoutSeconds = d.RunTimeoutSeconds
	in.Interactive = req.Interactive
	return in, nil
}

// restartInput rebuilds the RunInput that starts a queued run again, or says
// why it cannot be.
func (s *Server) restartInput(q store.QueuedRun) (runner.RunInput, string) {
	const unrecorded = "the run was still queued when loomcycle restarted and its request was not stored, so it never started; start it again"
	if len(q.Input) == 0 {
		return runner.RunInput{}, unrecorded
	}
	var rec queuedRecord
	if err := json.Unmarshal(q.Input, &rec); err != nil {
		return runner.RunInput{}, "the run was still queued when loomcycle restarted and its stored request is unreadable; start it again"
	}
	if rec.Lost != "" {
		return runner.RunInput{}, rec.Lost
	}
	if rec.Input == nil {
		return runner.RunInput{}, unrecorded
	}
	in := *rec.Input
	r := q.Run
	if rec.Secrets != nil {
		if s.queueSealer == nil || !s.queueSealer.Enabled() {
			return runner.RunInput{}, "the run's sealed secrets cannot be opened: LOOMCYCLE_SECRET_KEY is not set; start it again"
		}
		pt, err := s.queueSealer.Open(*rec.Secrets, queuedSecretsIdentity(r.TenantID, r.ID))
		if err != nil {
			return runner.RunInput{}, "the run's sealed secrets cannot be opened with this LOOMCYCLE_SECRET_KEY; start it again"
		}
		var sec queuedSecrets
		if err := json.Unmarshal(pt, &sec); err != nil {
			return runner.RunInput{}, "the run's sealed secrets are unreadable; start it again"
		}
		in.UserBearer, in.UserCredentials = sec.UserBearer, sec.UserCredentials
	}
	// Identity and confinement are the row's, fixed when the run was accepted.
	in.QueuedRunID = r.ID
	in.AgentID, in.TenantID, in.UserID, in.UserTier = r.AgentID, r.TenantID, r.UserID, r.UserTier
	in.ParentContext = r.ParentContext
	in.Interactive = r.Interactive
	in.OperatorKeyRestricted, in.Isolated = r.OperatorKeyRestricted, r.Isolated
	// The keys the row already holds; nothing is claimed a second time.
	in.IdempotencyKey, in.DeliveryAltKey, in.ClientIdempotencyKey = r.IdempotencyKey, r.DeliveryAltKey, ""
	// A started draft's row is queued now; it starts as a queued run.
	in.ConfiguredRunID, in.ConfiguredDraft = "", nil
	// Nobody waits on it: it runs to its end, and its caller re-attaches.
	in.Detached = true
	in.Admission = runner.AdmissionWait
	return in, ""
}

// RestartQueuedRuns starts again the runs this replica had queued when it
// last stopped — those created before bootedAt; a run queued since belongs to
// this process — in the order they queued. A run that cannot be started again
// is failed with the reason. Called once at boot.
func (s *Server) RestartQueuedRuns(ctx context.Context, bootedAt time.Time) (restarted, failed int, err error) {
	if s.store == nil {
		return 0, 0, nil
	}
	rows, err := s.store.ListQueuedRuns(ctx, s.replicaID, bootedAt)
	if err != nil {
		return 0, 0, err
	}
	for _, q := range rows {
		in, why := s.restartInput(q)
		if why != "" {
			if ferr := s.store.FailQueuedRun(ctx, q.Run.ID, why); ferr != nil && !errors.Is(ferr, store.ErrRunNotQueued) {
				log.Printf("queue: restart: could not fail run %s: %v", q.Run.ID, ferr)
			}
			failed++
			continue
		}
		s.startQueuedAgain(in)
		restarted++
	}
	return restarted, failed, nil
}

// startQueuedAgain runs a restarted queued run in its own goroutine, and
// returns once it has its place in the queue (or has started, or failed), so
// the runs re-queue in their original order. A run RunOnce refuses before it
// reaches the queue is failed here: its row would otherwise stay queued with
// nothing to start it.
func (s *Server) startQueuedAgain(in runner.RunInput) {
	placed := make(chan struct{})
	var once sync.Once
	mark := func() { once.Do(func() { close(placed) }) }
	go func() {
		defer mark()
		err := s.RunOnce(context.Background(), in, runner.RunCallbacks{
			OnQueued:     mark,
			OnRegistered: func(string, string, string, string) { mark() },
		})
		if err == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ferr := s.store.FailQueuedRun(ctx, in.QueuedRunID, "loomcycle restarted and the run could not be queued again: "+err.Error()); ferr != nil && !errors.Is(ferr, store.ErrRunNotQueued) {
			log.Printf("queue: restart: could not fail run %s: %v", in.QueuedRunID, ferr)
		}
	}()
	<-placed
}
