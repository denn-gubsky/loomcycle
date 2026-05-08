// Package grpc serves the gRPC equivalent of the HTTP+SSE surface.
//
// Both wire surfaces coexist on the same loomcycle process — an
// operator can run with HTTP only (default), gRPC only, or both
// (handy when migrating consumers from one to the other). The proto
// schema in proto/loomcycle.proto mirrors the HTTP+SSE shape 1:1; the
// methods in this package delegate to the same store / cancel
// registry / config the HTTP server uses.
//
// PR 1 of v0.5.5 implements the **metadata RPCs** only: GetAgent,
// CancelAgent, ListUserAgents, GetTranscript, Health. The streaming
// RPCs (Run, Continue) return Unimplemented so adapters can be coded
// against the full proto today, with the expectation that PR 2 lands
// the streaming wiring (which requires extracting the run loop from
// internal/api/http/server.go's handleRuns into a shared runner).
package grpc

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Server implements loomcyclepb.LoomcycleServer over the same backing
// state as internal/api/http.Server. The two are constructed from the
// same dependencies in cmd/loomcycle/main.go and intentionally do NOT
// share a struct — keeping them parallel rather than nested makes
// each surface separately testable and avoids cross-package import
// cycles.
type Server struct {
	loomcyclepb.UnimplementedLoomcycleServer

	store     store.Store
	cancelReg *cancel.Registry

	// authToken is the bearer token clients must present in the
	// `authorization` gRPC metadata header. Empty means open-mode
	// (matches the HTTP middleware's "no LOOMCYCLE_AUTH_TOKEN set"
	// behaviour). Compared in constant time.
	authToken string

	// Build identifiers (set at link time in main.go). Surfaced via
	// Health(). Empty fallbacks are fine — adapters don't depend on
	// these being populated.
	buildCommit string
	buildTime   string

	// startedAt is when the server began listening. Used by Health()
	// to report uptime.
	startedAt time.Time
}

// Config carries the server's construction-time inputs. Same shape
// the HTTP server takes.
type Config struct {
	Store       store.Store
	CancelReg   *cancel.Registry
	AuthToken   string
	BuildCommit string
	BuildTime   string
}

// New constructs a Server. Caller registers it with a *grpc.Server
// (in cmd/loomcycle/main.go) and starts the listener.
func New(cfg Config) *Server {
	return &Server{
		store:       cfg.Store,
		cancelReg:   cfg.CancelReg,
		authToken:   cfg.AuthToken,
		buildCommit: cfg.BuildCommit,
		buildTime:   cfg.BuildTime,
		startedAt:   time.Now(),
	}
}

// ========================
// Health
// ========================

// Health is the liveness probe. Unauthenticated, mirroring HTTP's
// /healthz exemption from the auth middleware. Returns build commit +
// build time + uptime so adapters running compatibility checks can
// log the runtime version they're talking to.
func (s *Server) Health(ctx context.Context, _ *loomcyclepb.HealthRequest) (*loomcyclepb.HealthResponse, error) {
	return &loomcyclepb.HealthResponse{
		Ok:            true,
		Commit:        s.buildCommit,
		Built:         s.buildTime,
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
	}, nil
}

// ========================
// Agent metadata
// ========================

// GetAgent mirrors HTTP's GET /v1/agents/{agent_id}. The two diverge
// in error vocabulary: HTTP returns 404 with a JSON body; gRPC returns
// codes.NotFound. The wire-stable string in the status message
// matches the HTTP error body so log correlation across the two
// surfaces stays clean.
func (s *Server) GetAgent(ctx context.Context, req *loomcyclepb.GetAgentRequest) (*loomcyclepb.Agent, error) {
	if !validIdent(req.GetAgentId()) {
		return nil, status.Error(codes.InvalidArgument, "agent_id must match [A-Za-z0-9_-]{1,128}")
	}
	agentID := req.GetAgentId()

	if s.store == nil {
		// No persistence — only live-in-registry answers are possible.
		entry, ok := s.cancelReg.Get(agentID)
		if !ok {
			return nil, status.Errorf(codes.NotFound, "no live run for %q (no store configured)", agentID)
		}
		return &loomcyclepb.Agent{
			AgentId:   agentID,
			RunId:     entry.RunID,
			SessionId: entry.SessionID,
			UserId:    entry.UserID,
			Status:    string(store.RunRunning),
			StartedAt: timestamppb.New(entry.StartedAt),
			Live:      true,
		}, nil
	}

	run, err := s.store.GetRunByAgentID(ctx, agentID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return nil, status.Errorf(codes.NotFound, "no run found for agent_id %q", agentID)
		}
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	_, live := s.cancelReg.Get(agentID)
	return runToProto(run, live), nil
}

// CancelAgent mirrors HTTP's POST /v1/agents/{agent_id}/cancel.
// Idempotent: cancelling an already-terminated run returns the
// existing terminal status rather than NotFound.
func (s *Server) CancelAgent(ctx context.Context, req *loomcyclepb.CancelAgentRequest) (*loomcyclepb.CancelAgentResponse, error) {
	if !validIdent(req.GetAgentId()) {
		return nil, status.Error(codes.InvalidArgument, "agent_id must match [A-Za-z0-9_-]{1,128}")
	}
	agentID := req.GetAgentId()

	res, ok := s.cancelReg.Cancel(agentID, req.GetReason())
	if ok {
		// The cascade walk captured this agent + every descendant; the
		// proto's cancelled_count reports the total.
		return &loomcyclepb.CancelAgentResponse{
			CancelledCount: int32(1 + len(res.Cascaded)),
		}, nil
	}

	// Not in registry — either already terminated or never existed.
	if s.store == nil {
		return nil, status.Errorf(codes.NotFound, "no live or terminated run for %q (no store configured)", agentID)
	}
	if _, err := s.store.GetRunByAgentID(ctx, agentID); err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return nil, status.Errorf(codes.NotFound, "no run found for agent_id %q", agentID)
		}
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	// Idempotent: agent exists in the store but is no longer live —
	// cancelled_count = 0 signals "this call did not initiate the
	// cancel" but the RPC succeeds (mirrors HTTP's 200 with
	// cancelled=false).
	return &loomcyclepb.CancelAgentResponse{CancelledCount: 0}, nil
}

// ListUserAgents mirrors HTTP's GET /v1/users/{user_id}/agents?status=...
func (s *Server) ListUserAgents(ctx context.Context, req *loomcyclepb.ListUserAgentsRequest) (*loomcyclepb.ListUserAgentsResponse, error) {
	if !validIdent(req.GetUserId()) {
		return nil, status.Error(codes.InvalidArgument, "user_id must match [A-Za-z0-9_-]{1,128}")
	}
	if s.store == nil {
		return &loomcyclepb.ListUserAgentsResponse{}, nil
	}
	runs, err := s.store.ListActiveRunsByUser(ctx, req.GetUserId(), store.RunStatus(req.GetStatus()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	out := &loomcyclepb.ListUserAgentsResponse{Agents: make([]*loomcyclepb.Agent, 0, len(runs))}
	for _, r := range runs {
		_, live := s.cancelReg.Get(r.AgentID)
		out.Agents = append(out.Agents, runToProto(r, live))
	}
	return out, nil
}

// ========================
// Transcript
// ========================

// GetTranscript mirrors HTTP's GET /v1/sessions/{id}/transcript.
// Returns every event for the session in seq order. Payload is raw
// JSON bytes (matches HTTP); adapters decode via providers.Event's
// existing JSON shape.
func (s *Server) GetTranscript(ctx context.Context, req *loomcyclepb.GetTranscriptRequest) (*loomcyclepb.Transcript, error) {
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "transcript requires persistence (Store not configured)")
	}
	if _, err := s.store.GetSession(ctx, sessionID); err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return nil, status.Errorf(codes.NotFound, "session %q not found", sessionID)
		}
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	events, err := s.store.GetTranscript(ctx, sessionID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	out := &loomcyclepb.Transcript{Events: make([]*loomcyclepb.TranscriptEvent, 0, len(events))}
	for _, e := range events {
		out.Events = append(out.Events, &loomcyclepb.TranscriptEvent{
			Seq:       e.Seq,
			SessionId: e.SessionID,
			RunId:     e.RunID,
			Ts:        timestamppb.New(e.Timestamp),
			Type:      e.Type,
			Payload:   e.Payload,
		})
	}
	return out, nil
}

// ========================
// Helpers
// ========================

// runToProto converts a store.Run into the proto Agent message,
// matching the HTTP runToAgentResponse byte-for-byte where they
// overlap (timestamps as TIMESTAMPTZ → Timestamp, status as string).
func runToProto(r store.Run, live bool) *loomcyclepb.Agent {
	out := &loomcyclepb.Agent{
		AgentId:       r.AgentID,
		RunId:         r.ID,
		SessionId:     r.SessionID,
		UserId:        r.UserID,
		ParentAgentId: r.ParentAgentID,
		Status:        string(r.Status),
		StartedAt:     timestamppb.New(r.StartedAt),
		StopReason:    r.StopReason,
		Error:         r.ErrorMsg,
		Usage: &loomcyclepb.AgentUsage{
			InputTokens:         int64(r.InputTokens),
			OutputTokens:        int64(r.OutputTokens),
			CacheCreationTokens: int64(r.CacheCreationTokens),
			CacheReadTokens:     int64(r.CacheReadTokens),
			Model:               r.Model,
		},
		Live: live,
	}
	if !r.CompletedAt.IsZero() {
		out.CompletedAt = timestamppb.New(r.CompletedAt)
	}
	if !r.LastHeartbeatAt.IsZero() {
		out.LastHeartbeatAt = timestamppb.New(r.LastHeartbeatAt)
	}
	return out
}

// validIdent matches [A-Za-z0-9_-]{1,128} — same charset the HTTP
// surface accepts for agent_id / user_id. Inlined here rather than
// imported from internal/api/http to avoid a back-edge import (HTTP
// → grpc would create a cycle if the helper lived elsewhere).
func validIdent(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// ========================
// Auth interceptor
// ========================

// UnaryAuthInterceptor enforces bearer-token auth on every unary RPC
// EXCEPT Health. Mirrors the HTTP middleware's behaviour:
//   - Empty AuthToken → open mode (matches LOOMCYCLE_AUTH_TOKEN unset).
//   - Token mismatch → codes.Unauthenticated.
//   - Constant-time compare prevents a timing oracle.
//
// Caller wires this via grpc.UnaryInterceptor when constructing the
// *grpc.Server in main.go.
func (s *Server) UnaryAuthInterceptor() googlegrpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *googlegrpc.UnaryServerInfo, handler googlegrpc.UnaryHandler) (any, error) {
		if info.FullMethod == healthFullMethod {
			return handler(ctx, req)
		}
		if s.authToken == "" {
			return handler(ctx, req)
		}
		if err := checkBearer(ctx, s.authToken); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamAuthInterceptor is the streaming-RPC equivalent of
// UnaryAuthInterceptor. The Run/Continue methods are streaming and
// will need this once PR 2 wires them up; landing the interceptor
// now keeps main.go's grpc.NewServer call symmetric with the unary
// side and means PR 2 doesn't need to touch wiring code.
func (s *Server) StreamAuthInterceptor() googlegrpc.StreamServerInterceptor {
	return func(srv any, ss googlegrpc.ServerStream, info *googlegrpc.StreamServerInfo, handler googlegrpc.StreamHandler) error {
		if info.FullMethod == healthFullMethod {
			return handler(srv, ss)
		}
		if s.authToken == "" {
			return handler(srv, ss)
		}
		if err := checkBearer(ss.Context(), s.authToken); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// healthFullMethod is the gRPC method string for Loomcycle.Health.
// Hardcoded vs derived from the proto descriptor because it's a tiny
// well-known constant and the descriptor lookup at every interceptor
// call would be wasted work.
const healthFullMethod = "/loomcycle.v1.Loomcycle/Health"

// checkBearer validates the `authorization: Bearer <token>` metadata
// header in constant time.
func checkBearer(ctx context.Context, want string) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization header")
	}
	got := values[0]
	expected := "Bearer " + want
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid bearer token")
	}
	return nil
}

// MustLogStartupBanner prints a one-line startup notice when the
// gRPC server starts listening. Symmetric with the HTTP banner in
// main.go.
func (s *Server) MustLogStartupBanner(addr string) {
	log.Printf("loomcycle gRPC listening on %s (auth=%v)", addr, s.authToken != "")
}

// ========================
// Streaming RPC stubs (PR 2)
// ========================

// Run server-streams events from a fresh agent run. PR 2 wires this
// into the same loop runner the HTTP handleRuns drives; PR 1 stubs
// it so adapters can compile against the full proto without the gRPC
// surface refusing-to-implement at the type level.
func (s *Server) Run(req *loomcyclepb.RunRequest, stream loomcyclepb.Loomcycle_RunServer) error {
	return status.Error(codes.Unimplemented, "Run streaming lands in v0.5.5 PR 2 — use HTTP+SSE for now")
}

// Continue server-streams events from a continuation. Same PR 2
// caveat as Run.
func (s *Server) Continue(req *loomcyclepb.ContinueRequest, stream loomcyclepb.Loomcycle_ContinueServer) error {
	return status.Error(codes.Unimplemented, "Continue streaming lands in v0.5.5 PR 2 — use HTTP+SSE for now")
}

