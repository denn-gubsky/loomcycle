package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/retentionmode"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EI P3: a run that cannot start at once is accepted as a queued run —
// a row in status "queued", a `queued` event — and starts when a run slot
// frees. Every new run is admitted by RunOnce, through admitRun; its caller
// learns of the wait through RunCallbacks.

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
	// waitOutPause makes a run that did not queue wait out a pause too: a run
	// a restart is queuing again is not a new run, so the pause does not
	// refuse it. Such a run is announced before it waits, as a queued one is.
	waitOutPause bool
	// createRow creates the run's row in status queued (and its session).
	// Called at most once, when the run first has to wait.
	createRow func() (sessionID, runID string, err error)
	// announce tells the caller the run exists (its ids) and makes it
	// cancellable. Called once, right after createRow.
	announce func(sessionID, runID string) error
	// forward delivers a queued or admitted event to the caller. The event
	// is already persisted.
	forward func(providers.Event)
	// slot is bound to the admitted run so its turn calls can lend its
	// global slot while they wait for a provider lane. nil lends nothing.
	slot *concurrency.RunSlot
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
		Slot:     a.slot,
	})
	// unqueuedRunID is the row of a run announced without having queued.
	var unqueuedRunID string
	if err == nil && q == nil && a.waitOutPause {
		// A run a restart queues again that found its slot free may still have
		// to wait out a pause below. It holds its place already, so it is
		// announced first: until then it is in no cancel registry, and its
		// restarter — which waits for each run to be placed, to keep the queue
		// order — would be held until the resume, and with it the boot or the
		// snapshot restore that restarted it.
		sessionID, runID, cerr := a.createRow()
		if cerr == nil {
			unqueuedRunID = runID
			cerr = a.announce(sessionID, runID)
		}
		if cerr != nil {
			release()
			release, err = nil, cerr
		}
	}
	if err == nil && (q != nil || a.waitOutPause) {
		// A paused runtime starts nothing: a run that got its slot during a
		// pause keeps the slot, stays queued, and starts on the resume.
		if werr := s.waitOutPause(ctx); werr != nil {
			release()
			release, err = nil, werr
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		switch {
		case q != nil:
			s.endQueuedRun(q.runID, err)
		case unqueuedRunID != "":
			s.endQueuedRun(unqueuedRunID, err)
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

// waitOutPause returns once the runtime is not paused (at once when it is
// not), or ctx's error if ctx ends first.
func (s *Server) waitOutPause(ctx context.Context) error {
	for {
		paused, changed := s.pauseMgr.PauseWatch()
		if !paused {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// endQueuedRun ends a queued run whose wait ended without a slot, or that
// was refused at its start. It never started, so the work was never done:
// its idempotency keys are released, as for a run a restart cannot start, so
// a caller's retry runs the work rather than being answered with this run.
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
	if err := s.store.EndQueuedRun(ctx, runID, status, "", msg); err != nil {
		// Not queued any more: whoever ended or started it reports that.
		if !errors.Is(err, store.ErrRunNotQueued) {
			slog.Warn("queue: could not end queued run", "run_id", runID, "err", err)
		}
		return
	}
	s.publishQueuedRunEnd(ctx, runID, status, "", msg)
}

// failQueuedRun fails a queued run a restart cannot start again, with the
// reason, and reports that end. ErrRunNotQueued when it is no longer queued.
func (s *Server) failQueuedRun(ctx context.Context, runID, reason string) error {
	if err := s.store.FailQueuedRun(ctx, runID, reason); err != nil {
		return err
	}
	s.publishQueuedRunEnd(ctx, runID, store.RunFailed, "restarted", reason)
	return nil
}

// publishQueuedRunEnd reports on the run-state stream the end of a queued run
// that never started. No loop will report it, and a consumer holding the
// run's id from its queued answer waits for a terminal frame. The run's
// identity is read back from its row: the paths that end a queued run do not
// all have it.
func (s *Server) publishQueuedRunEnd(ctx context.Context, runID string, status store.RunStatus, stopReason, errMsg string) {
	if s.runStateBus == nil {
		return
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		slog.Warn("queue: could not report the end of queued run", "run_id", runID, "err", err)
		return
	}
	s.publishRunState(runStateMeta{
		RunID: run.ID, AgentID: run.AgentID, Agent: run.Agent, UserID: run.UserID, TenantID: run.TenantID,
		ParentAgentID: run.ParentAgentID, ParentRunID: run.ParentRunID, ParentContext: run.ParentContext,
	}, string(status), stopReason, errMsg)
}

// emitAdmissionEvent persists a queued/admitted event on the run's
// transcript and forwards it. It is sent before the run's recording emit
// exists. It carries no content, so a no-trace run stores it too.
func (s *Server) emitAdmissionEvent(ctx context.Context, runID string, forward func(providers.Event), ev providers.Event) {
	if s.store != nil && runID != "" {
		if payload, err := json.Marshal(ev); err == nil {
			if err := s.store.AppendEvent(context.WithoutCancel(ctx), runID, string(ev.Type), payload); err != nil {
				slog.Error("store: AppendEvent failed", "run_id", runID, "type", ev.Type, "err", err)
			}
		}
	}
	if forward != nil {
		forward(ev)
	}
}

// admissionError maps a refused or abandoned admission to the runner
// sentinels, as RunOnce has always returned them. The concurrency error stays
// wrapped too (same text), so an HTTP caller answers with its details — the
// user and the cap of a per-user refusal.
func admissionError(err error) error {
	switch {
	case concurrency.IsPerUserQuotaExhausted(err):
		return fmt.Errorf("%w: %w", runner.ErrPerUserQuotaExhausted, err)
	case concurrency.IsBackpressure(err):
		return fmt.Errorf("%w: %w", runner.ErrBackpressure, err)
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
	// ACLOrigin names whose access-control rules the run was accepted under
	// (RFC EY), so a restart starts it under that caller's CURRENT set — and
	// asks `run agent:` for that caller again — rather than under none. Absent
	// when no set applied, which starts it as before.
	ACLOrigin *aclOriginRecord `json:"acl_origin,omitempty"`
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
	case retention == retentionmode.None:
		rec.Lost = lostNoTrace
	case in.UserBearer != "" || len(in.UserCredentials) > 0:
		if s.queueSealer == nil || !s.queueSealer.Enabled() {
			rec.Lost = lostNoKey
			break
		}
		pt, err := json.Marshal(queuedSecrets{UserBearer: in.UserBearer, UserCredentials: in.UserCredentials})
		if err != nil {
			slog.Error("queue: could not encode the run's secrets", "run_id", runID, "err", err)
			return
		}
		sealed, err := s.queueSealer.Seal(pt, queuedSecretsIdentity(tenantID, runID))
		if err != nil {
			slog.Error("queue: could not seal the run's secrets", "run_id", runID, "err", err)
			return
		}
		rec.Secrets = &sealed
	}
	if rec.Lost == "" {
		stored := in
		stored.UserBearer, stored.UserCredentials = "", nil
		rec.Input = &stored
		rec.ACLOrigin = aclOriginOf(ctx, runCallerACL(ctx)) // a recorded caller too: a trigger owner's set
	}
	b, err := json.Marshal(rec)
	if err != nil {
		slog.Error("queue: could not encode the run's request", "run_id", runID, "err", err)
		return
	}
	if err := s.store.SetQueuedInput(context.WithoutCancel(ctx), runID, b); err != nil && !errors.Is(err, store.ErrRunNotQueued) {
		slog.Error("queue: could not store the run's request", "run_id", runID, "err", err)
	}
}

// queuedRecordOf is the input a queued run is recorded with for a restart: the
// one it was started with, naming the agent and identity it resolved to — a
// continuation's are its session's, and the input that started it left them
// empty. A restart takes the identity from the row and a continuation's agent
// from its session again; the record says what the run was.
func queuedRecordOf(in runner.RunInput, agent, tenant, user string) runner.RunInput {
	in.Agent, in.TenantID, in.UserID = agent, tenant, user
	return in
}

// requestRunInput is the RunInput a POST /v1/runs request starts as after a
// restart: the conversion a configured run's start uses, so a request field
// that conversion carries is carried here too (TestRunDraft_CarriesEveryRequestField).
func requestRunInput(req runRequest) (runner.RunInput, error) {
	d, err := requestToDraft(req)
	if err != nil {
		return runner.RunInput{}, err
	}
	in := connector.SpawnRequestToRunInput(d.SpawnRunRequest)
	in.RunTimeoutSeconds = d.RunTimeoutSeconds
	in.Interactive = req.Interactive
	return in, nil
}

// messagesRunInput is the RunInput a POST /v1/sessions/{id}/messages request
// starts as after a restart: a continuation of sessionID by the session's
// agent, as its stored user. Through runRequest and JSON on purpose: every
// messagesRequest field is a runRequest field of the same name and type
// (TestMessagesRequest_EveryFieldIsARunRequestField), so the body is carried
// through the conversion a queued POST /v1/runs uses, and a field added to
// one but not the other fails that test instead of being dropped here.
func messagesRunInput(sessionID string, sess store.Session, body messagesRequest) (runner.RunInput, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return runner.RunInput{}, err
	}
	var req runRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return runner.RunInput{}, err
	}
	req.SessionID, req.Agent, req.TenantID, req.UserID = sessionID, sess.Agent, sess.TenantID, sess.UserID
	return requestRunInput(req)
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
	// The request records it too; either record withholds the user's scope.
	in.CredentialsNoUserScope = in.CredentialsNoUserScope || r.CredentialsNoUserScope
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
	restarted, failedIDs, _ := s.queueAgain(ctx, rows) // a held run is logged and audited; it waits for the next restart
	return restarted, len(failedIDs), nil
}

// requeueRestoredRuns queues again the queued runs a snapshot restore just
// inserted — those only, by id: every other queued row has a live waiter. A
// run that cannot be is failed and named in the warnings.
func (s *Server) requeueRestoredRuns(ctx context.Context, ids []string) (requeued int, warnings []string) {
	if s.store == nil || len(ids) == 0 {
		return 0, nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	rows, err := s.store.ListQueuedRuns(ctx, "", time.Now().Add(time.Second))
	if err != nil {
		return 0, []string{fmt.Sprintf("queued_runs: listing the restored queued runs failed, so none was queued again: %v", err)}
	}
	var restored []store.QueuedRun
	for _, q := range rows {
		if want[q.Run.ID] {
			restored = append(restored, q)
		}
	}
	requeued, failed, held := s.queueAgain(ctx, restored)
	for _, f := range failed {
		warnings = append(warnings, fmt.Sprintf("queued_run %s: not queued again and failed: %s", f[0], f[1]))
	}
	for _, h := range held {
		warnings = append(warnings, fmt.Sprintf("queued_run %s: not queued again, left queued: %s", h[0], h[1]))
	}
	return requeued, warnings
}

// queueAgain starts each of rows again, in order, or fails it with the reason
// it cannot be. failed holds (run id, reason) pairs.
//
// A run accepted under a caller's access-control set starts under that
// caller's set as it is now (queuedRunCtx). One whose set cannot be read is
// neither started nor failed: it stays queued, named in held, for a later
// restart.
func (s *Server) queueAgain(ctx context.Context, rows []store.QueuedRun) (started int, failed, held [][2]string) {
	for _, q := range rows {
		in, why := s.restartInput(q)
		if why != "" {
			if ferr := s.failQueuedRun(ctx, q.Run.ID, why); ferr != nil && !errors.Is(ferr, store.ErrRunNotQueued) {
				slog.Warn("queue: restart: could not fail run", "run_id", q.Run.ID, "err", ferr)
			}
			failed = append(failed, [2]string{q.Run.ID, why})
			continue
		}
		runCtx, err := s.queuedRunCtx(q, in.Agent)
		if err != nil {
			why := fmt.Sprintf("acl_unavailable: the access-control rules of the caller that queued it could not be read (%v); it stays queued rather than start without them", err)
			slog.Warn("queue: restart: run stays queued", "run_id", q.Run.ID, "reason", why)
			held = append(held, [2]string{q.Run.ID, why})
			continue
		}
		s.startQueuedAgain(runCtx, in)
		started++
	}
	return started, failed, held
}

// queuedRunCtx is the ctx a queued run starts on after a restart: with no
// record of a caller's set, a bare one, as before; with one, the caller's set
// compiled again now, for RunOnce to narrow the run by and to ask
// `run agent:<agent>` against (runCallerACL, admitPrincipalAgent).
func (s *Server) queuedRunCtx(q store.QueuedRun, agent string) (context.Context, error) {
	ctx := context.Background()
	var rec queuedRecord
	if json.Unmarshal(q.Input, &rec) != nil || rec.ACLOrigin == nil {
		return ctx, nil
	}
	ctx, eff, err := s.recordedCallerACL(ctx, rec.ACLOrigin, agent)
	if err != nil {
		return nil, err
	}
	return withRecordedCaller(ctx, *rec.ACLOrigin, eff), nil
}

// startQueuedAgain runs a restarted queued run in its own goroutine, and
// returns once it has its place in the queue (or has started, or failed), so
// the runs re-queue in their original order. A run RunOnce refuses before it
// reaches the queue is failed here: its row would otherwise stay queued with
// nothing to start it.
func (s *Server) startQueuedAgain(runCtx context.Context, in runner.RunInput) {
	placed := make(chan struct{})
	var once sync.Once
	mark := func() { once.Do(func() { close(placed) }) }
	go func() {
		defer mark()
		err := s.RunOnce(runCtx, in, runner.RunCallbacks{
			OnQueued:     mark,
			OnRegistered: func(string, string, string, string) { mark() },
		})
		if err == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ferr := s.failQueuedRun(ctx, in.QueuedRunID, "loomcycle restarted and the run could not be queued again: "+err.Error()); ferr != nil && !errors.Is(ferr, store.ErrRunNotQueued) {
			slog.Warn("queue: restart: could not fail run", "run_id", in.QueuedRunID, "err", ferr)
		}
	}()
	<-placed
}
