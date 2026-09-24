package grpc

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// interactive.go — RFC AI. The gRPC twins of the interactive-session HTTP
// endpoints. Both dispatch through the Connector (SteerRun / StreamRunEvents),
// which the HTTP *Server implements over the SAME in-process steer registry +
// store the HTTP handlers use — so a gRPC steer reaches an HTTP-started run and
// vice versa, and the tenant-ownership gate is enforced once, server-side.

// RunInput pushes an operator steering message into a live interactive run.
// Mirrors POST /v1/runs/{run_id}/input. The `source` is server-stamped (gRPC
// has no cookie → the API class), never trusted from the wire. Error mapping
// mirrors the HTTP codes: NotFound (no in-flight run / cross-tenant),
// ResourceExhausted (queue full — HTTP 429), Unavailable (steering off).
func (s *Server) RunInput(ctx context.Context, req *loomcyclepb.RunInputRequest) (*loomcyclepb.RunInputResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	runID := req.GetRunId()
	if runID == "" {
		return nil, status.Error(codes.InvalidArgument, "run_id is required")
	}
	if strings.TrimSpace(req.GetText()) == "" {
		return nil, status.Error(codes.InvalidArgument, "text is required")
	}
	// Apply the retune BEFORE delivering the text: the loop re-reads the run's
	// configuration when the operator's turn arrives, so the record has to be in
	// the store by the time the message wakes it. The other order applies the
	// change one turn late, which an operator reads as "it ignored me".
	if ov := overridesFromProto(protoOverrideFields{
		Model: req.GetModel(), Provider: req.GetProvider(), Tier: req.GetTier(), Effort: req.GetEffort(),
		MaxTokens: req.GetMaxTokens(), MaxIterations: req.GetMaxIterations(),
		MaxConcurrentChildren: req.GetMaxConcurrentChildren(),
		Unbounded:             req.UnboundedIterations, InjectToolGuide: req.InjectToolGuide,
		RetryAttempts: req.RetryAttempts, MemInject: req.MemoryInjectMaxTokens,
		MemIndex:    req.MemoryIndexMaxBytes,
		Interactive: req.Interactive, Interruption: req.GetInterruption(),
		Review: req.Review,
	}); !ov.IsZero() {
		switch err := s.connector.RetuneRun(ctx, runID, ov); {
		case errors.Is(err, connector.ErrRunNotInFlight):
			return nil, status.Error(codes.NotFound, err.Error())
		case err != nil:
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	delivered, err := s.connector.SteerRun(ctx, runID, req.GetText(), store.InterruptResolvedByAPI)
	switch {
	case errors.Is(err, connector.ErrRunNotInFlight):
		return nil, status.Error(codes.NotFound, err.Error())
	case errors.Is(err, connector.ErrSteerQueueFull):
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, connector.ErrSteeringUnavailable):
		return nil, status.Error(codes.Unavailable, err.Error())
	case err != nil:
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &loomcyclepb.RunInputResponse{RunId: runID, Delivered: delivered}, nil
}

// CancelTurn stops the CURRENT turn of a live interactive run and parks it at
// awaiting_input — the gRPC twin of POST /v1/runs/{run_id}/cancel (RFC BH). It
// dispatches through connector.CancelTurn, which owns the session-ownership gate
// + cross-replica owner-routing, so a gRPC turn-cancel reaches an HTTP-started
// run and vice versa. Error mapping mirrors the HTTP codes: Unavailable
// (turn-cancel off), NotFound (no in-flight / cross-tenant, opaque),
// FailedPrecondition (not mid-turn / not interactive).
func (s *Server) CancelTurn(ctx context.Context, req *loomcyclepb.CancelTurnRequest) (*loomcyclepb.CancelTurnResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	runID := req.GetRunId()
	if !validIdent(runID) {
		return nil, status.Error(codes.InvalidArgument, "run_id must match [A-Za-z0-9_-]{1,128}")
	}
	stopped, parked, err := s.connector.CancelTurn(ctx, runID, req.GetReason())
	switch {
	case errors.Is(err, connector.ErrTurnCancelUnavailable):
		return nil, status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, connector.ErrRunNotInFlight):
		return nil, status.Error(codes.NotFound, err.Error())
	case errors.Is(err, connector.ErrTurnNotMidTurn), errors.Is(err, connector.ErrTurnNotInteractive):
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	case err != nil:
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &loomcyclepb.CancelTurnResponse{RunId: runID, Stopped: stopped, Parked: parked}, nil
}

// ReviewRun delivers an operator's verdict on a run held for review — the gRPC
// twin of POST /v1/runs/{run_id}/review (RFC DJ). It dispatches through
// connector.ReviewRun, which owns the tenant and session-owner gate and routes
// the verdict to the replica that owns the run. gRPC has no cookie, so the
// source is always "api".
func (s *Server) ReviewRun(ctx context.Context, req *loomcyclepb.ReviewRunRequest) (*loomcyclepb.ReviewRunResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	runID := req.GetRunId()
	if !validIdent(runID) {
		return nil, status.Error(codes.InvalidArgument, "run_id must match [A-Za-z0-9_-]{1,128}")
	}
	delivered, err := s.connector.ReviewRun(ctx, runID, req.GetDecision(), req.GetFeedback(), store.InterruptResolvedByAPI)
	switch {
	case errors.Is(err, connector.ErrInvalidReviewDecision):
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, connector.ErrRunNotHeld):
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, connector.ErrRunNotInFlight):
		return nil, status.Error(codes.NotFound, err.Error())
	case errors.Is(err, connector.ErrSteerQueueFull):
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, connector.ErrSteeringUnavailable):
		return nil, status.Error(codes.Unavailable, err.Error())
	case err != nil:
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &loomcyclepb.ReviewRunResponse{RunId: runID, Decision: req.GetDecision(), Delivered: delivered}, nil
}

// ResolveInterrupt resolves a pending interruption — answer or decline — the
// gRPC twin of POST /v1/runs/{run_id}/interrupts/{interrupt_id}/resolve (RFC
// BH). It dispatches through connector.ResolveInterrupt (tenant gate +
// validation + persist + wake). `resolved_by` is server-stamped "api" when the
// caller leaves it empty (gRPC has no cookie), never wire-trusted for a webui
// attribution. Error mapping: InvalidArgument (bad kind/disposition/answer),
// NotFound (unknown / cross-tenant, opaque), FailedPrecondition (already
// terminal / expired), Unavailable (no store).
func (s *Server) ResolveInterrupt(ctx context.Context, req *loomcyclepb.ResolveInterruptRequest) (*loomcyclepb.ResolveInterruptResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	runID := req.GetRunId()
	interruptID := req.GetInterruptId()
	if !validIdent(runID) || !validIdent(interruptID) {
		return nil, status.Error(codes.InvalidArgument, "run_id / interrupt_id must match [A-Za-z0-9_-]{1,128}")
	}
	resolvedBy := req.GetResolvedBy()
	if resolvedBy == "" {
		// gRPC has no cookie → the API attribution class (mirrors RunInput).
		resolvedBy = store.InterruptResolvedByAPI
	}
	st, err := s.connector.ResolveInterrupt(ctx, runID, interruptID, req.GetKind(), req.GetAnswer(), resolvedBy, req.GetDisposition())
	switch {
	case errors.Is(err, connector.ErrInterruptStoreUnavailable):
		return nil, status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, connector.ErrInterruptNotFound):
		return nil, status.Error(codes.NotFound, err.Error())
	case errors.Is(err, connector.ErrInterruptAlreadyTerminal), errors.Is(err, connector.ErrInterruptExpired):
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, connector.ErrInterruptUnsupportedKind),
		errors.Is(err, connector.ErrInterruptUnsupportedDisposition),
		errors.Is(err, connector.ErrInterruptDeclineWithAnswer),
		errors.Is(err, connector.ErrInterruptInvalidAnswer):
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case err != nil:
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &loomcyclepb.ResolveInterruptResponse{InterruptId: interruptID, Status: st}, nil
}

// StreamRun re-attaches to a run's event stream by run_id, replaying from
// from_seq then live-tailing. Mirrors GET /v1/runs/{run_id}/stream. The
// operator's own turns are replayed too (connector.StreamRunEvents / RFC AI S1),
// so a cold gRPC client reconstructs the whole conversation. Tenant-ownership is
// gated inside the connector (cross-tenant → NotFound, opaque). As-built note:
// unlike the HTTP endpoint, this does NOT emit a leading side-channel `agent`
// metadata frame — the caller already holds run_id; if it needs the run's
// agent_id (e.g. for CancelAgent) it calls GetAgent. (Deviation from RFC AI
// fork D, taken to keep the run metadata fetch tenant-gated through the single
// connector entry rather than a raw store read in the handler.)
func (s *Server) StreamRun(req *loomcyclepb.StreamRunRequest, stream loomcyclepb.Loomcycle_StreamRunServer) error {
	if s.connector == nil {
		return status.Error(codes.Unavailable, "connector not wired")
	}
	runID := req.GetRunId()
	if runID == "" {
		return status.Error(codes.InvalidArgument, "run_id is required")
	}
	visit := func(ev providers.Event) error {
		return stream.Send(eventToProto(ev))
	}
	err := s.connector.StreamRunEvents(stream.Context(), runID, req.GetFromSeq(), visit)
	switch {
	case errors.Is(err, connector.ErrRunNotInFlight):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, connector.ErrSteeringUnavailable):
		return status.Error(codes.Unavailable, err.Error())
	case err != nil:
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}

// overridesFromProto maps the wire override fields onto the connector shape.
//
// ONE function for both RunInput and RetuneRun. They carry the same twelve
// fields at different numbers, and two hand-written mappings of one list is the
// exact shape that left the steer path with none of them in the first place.
// protoOverrideFields is what the caller fills in. A STRUCT rather than the
// positional parameter list this started as: it reached twelve parameters, six of
// them *bool / *int32, and adding two more would have made a transposed pair
// compile silently. Keyed fields cannot be swapped by accident.
type protoOverrideFields struct {
	Model, Provider, Tier, Effort                   string
	MaxTokens, MaxIterations, MaxConcurrentChildren int32
	Unbounded, InjectToolGuide, Interactive, Review *bool
	RetryAttempts, MemInject, MemIndex              *int32
	Interruption                                    *loomcyclepb.Interruption
}

func overridesFromProto(f protoOverrideFields) connector.RunOverrides {
	toInt := func(p *int32) *int {
		if p == nil {
			return nil
		}
		v := int(*p)
		return &v
	}
	return connector.RunOverrides{
		Model: f.Model, Provider: f.Provider, Tier: f.Tier, Effort: f.Effort,
		MaxTokens: int(f.MaxTokens), MaxIterations: int(f.MaxIterations),
		UnboundedIterations: f.Unbounded, MaxConcurrentChildren: int(f.MaxConcurrentChildren),
		RetryAttempts: toInt(f.RetryAttempts), MemoryInjectMaxTokens: toInt(f.MemInject),
		MemoryIndexMaxBytes: toInt(f.MemIndex), InjectToolGuide: f.InjectToolGuide,
		// nil stays nil: "the caller said nothing" must reach the runner as
		// inherit-the-definition, never as a block that DISABLES what it allows.
		Interactive:  f.Interactive,
		Interruption: interruptionFromProto(f.Interruption),
		Review:       f.Review,
	}
}

// RetuneRun changes a run's per-run overrides without delivering a turn — the
// gRPC twin of POST /v1/runs/{run_id}/retune. Error mapping mirrors the HTTP
// codes: NotFound (no in-flight run, and equally a cross-tenant one),
// InvalidArgument (no override supplied — the HTTP 422).
func (s *Server) RetuneRun(ctx context.Context, req *loomcyclepb.RetuneRunRequest) (*loomcyclepb.RetuneRunResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unavailable, "connector not wired")
	}
	runID := req.GetRunId()
	if runID == "" {
		return nil, status.Error(codes.InvalidArgument, "run_id is required")
	}
	ov := overridesFromProto(protoOverrideFields{
		Model: req.GetModel(), Provider: req.GetProvider(), Tier: req.GetTier(), Effort: req.GetEffort(),
		MaxTokens: req.GetMaxTokens(), MaxIterations: req.GetMaxIterations(),
		MaxConcurrentChildren: req.GetMaxConcurrentChildren(),
		Unbounded:             req.UnboundedIterations, InjectToolGuide: req.InjectToolGuide,
		RetryAttempts: req.RetryAttempts, MemInject: req.MemoryInjectMaxTokens,
		MemIndex:    req.MemoryIndexMaxBytes,
		Interactive: req.Interactive, Interruption: req.GetInterruption(),
		Review: req.Review,
	})
	if ov.IsZero() {
		return nil, status.Error(codes.InvalidArgument, "at least one override is required")
	}
	switch err := s.connector.RetuneRun(ctx, runID, ov); {
	case errors.Is(err, connector.ErrRunNotInFlight):
		return nil, status.Error(codes.NotFound, err.Error())
	case err != nil:
		// A refused override (outside what the definition allows) is the
		// caller's mistake, not the server's.
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &loomcyclepb.RetuneRunResponse{RunId: runID, Retuned: true}, nil
}
