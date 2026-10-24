package http

// erasure_execute.go — the HTTP face of POST /v1/_erasure.
//
// The operation lives in internal/erasure, including the confirm guard: that is
// a property of the erasure, not of HTTP, and leaving it to each transport would
// mean the newest one is the one missing it.
//
// What stays here is the DEFAULTING, which is genuinely a wire concern. dry_run
// is a *bool so an omitted field — or POST {} from a client with a buggy
// serialiser — means DO NOTHING. A plain bool would make the zero value
// destructive.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/erasure"
)

type erasureRequest struct {
	Subject string `json:"subject"`
	DryRun  *bool  `json:"dry_run"`
	Confirm string `json:"confirm"`
}

// handleErasureExecute serves POST /v1/_erasure.
func (s *Server) handleErasureExecute(w http.ResponseWriter, r *http.Request) {
	var req erasureRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "decode body: "+err.Error())
		return
	}
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no_store", "no store configured")
		return
	}
	tenant, ok := s.resolveErasureTenant(w, r)
	if !ok {
		return
	}
	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}

	res, err := s.ErasureExecute(r.Context(), erasure.ExecuteRequest{
		Tenant: tenant, Subject: req.Subject, DryRun: dryRun, Confirm: req.Confirm,
		SourceAddr: r.RemoteAddr,
	})
	switch {
	case errors.Is(err, erasure.ErrNoSubject):
		writeJSONError(w, http.StatusBadRequest, "missing_subject", "subject is required")
		return
	case errors.Is(err, erasure.ErrConfirmMismatch):
		writeJSONError(w, http.StatusBadRequest, "confirm_mismatch", err.Error())
		return
	case errors.Is(err, erasure.ErrAuditUnavailable):
		writeJSONError(w, http.StatusServiceUnavailable, "audit_unavailable",
			err.Error()+". Erasure cannot be undone, so it is not performed unless who did it "+
				"can be recorded — set LOOMCYCLE_AUDIT_LOG_PATH and retry.")
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "erasure_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ErasureExecute implements connector.Connector, and is the one path every
// transport's erasure takes — HTTP's handler included — so the audit pair is
// written whichever transport asked. Before it was, MCP and gRPC called the
// service directly and an erasure over either went unrecorded.
func (s *Server) ErasureExecute(ctx context.Context, req erasure.ExecuteRequest) (erasure.Result, error) {
	if s.store == nil {
		return erasure.Result{}, errors.New("no store configured")
	}
	// VALIDATE FIRST, so a request that was going to be rejected never writes an audit
	// record. A log of intents that never happened is noise in the one log an auditor
	// needs to trust — and reporting "audit unavailable" for a mistyped confirmation
	// would name the wrong problem.
	svc := s.erasureService()
	if err := svc.Validate(req); err != nil {
		return erasure.Result{}, err
	}

	// AUDIT BEFORE DELETING, and refuse if the record will not write.
	//
	// Every other consumer of this sink treats recording as best-effort — audit is
	// observability, not a transaction participant. Erasure is the exception, and
	// deliberately so: it is the one operation in this API that nothing can undo, so a
	// deployment that cannot say WHO erased WHICH subject does not get to perform the
	// deletion. Refusing is recoverable (configure the sink and retry); an unrecorded
	// erasure is not.
	//
	// A dry run writes no record: it removes nothing, and an audit log full of previews
	// is one an auditor stops reading.
	if !req.DryRun {
		if err := s.recordEraseIntent(ctx, req); err != nil {
			return erasure.Result{}, fmt.Errorf("%w: %v", erasure.ErrAuditUnavailable, err)
		}
	}

	res, err := svc.Execute(ctx, req)
	if err != nil {
		return res, err
	}
	// The outcome record is BEST-EFFORT, unlike the intent above. The deletion has
	// already happened; refusing now would report a failure for work that was done, and
	// the intent record already answers "who erased which subject, when". This one only
	// enriches it with what went.
	if !req.DryRun {
		s.recordEraseResult(ctx, req, res)
	}
	return res, nil
}

// recordEraseIntent writes the record that must exist before anything is deleted.
func (s *Server) recordEraseIntent(ctx context.Context, req erasure.ExecuteRequest) error {
	if s.eraseAudit == nil {
		return fmt.Errorf("no audit sink configured")
	}
	ev := audit.Event{Action: "erase_intent", TargetTenant: req.Tenant, TargetSubject: req.Subject}
	stampEraseActor(ctx, req, &ev)
	return s.eraseAudit.Record(ev)
}

// recordEraseResult appends what the erasure actually did.
func (s *Server) recordEraseResult(ctx context.Context, req erasure.ExecuteRequest, res erasure.Result) {
	if s.eraseAudit == nil {
		return
	}
	ev := audit.Event{
		Action: "erase_result", TargetTenant: req.Tenant, TargetSubject: req.Subject,
		ErasePlanes: res.Deleted, EraseRetained: res.Retained, EraseErrors: res.Errors,
	}
	stampEraseActor(ctx, req, &ev)
	if err := s.eraseAudit.Record(ev); err != nil {
		// Logged, not returned: the rows are gone either way, and the intent record
		// already carries who and what.
		slog.Error("audit: erase_result record failed", "subject", req.Subject, "tenant", req.Tenant, "err", err)
	}
}

// stampEraseActor fills WHO from the authenticated principal — never from the body.
func stampEraseActor(ctx context.Context, req erasure.ExecuteRequest, ev *audit.Event) {
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		ev.ActorTenant = p.TenantID
		ev.ActorSubject = p.Subject
		ev.ActorTokenSuffix = p.TokenSuffix
	}
	ev.SourceAddr = req.SourceAddr
}
