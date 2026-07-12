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
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	nethttp "net/http"
	"strings"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Server implements loomcyclepb.LoomcycleServer. The metadata RPCs
// dispatch through the connector.Connector interface (v0.8.15+) — in
// production this is the *internal/api/http.Server instance from
// main.go, so a cancel/get/list issued via gRPC reaches the same
// business logic the HTTP /v1/* endpoints use. The streaming RPCs
// (Run, Continue) keep using runner.Runner directly because Connector
// only exposes a blocking SpawnRun — gRPC streaming needs the
// callback-driven runner.RunOnce path.
//
// Future proto method additions (gRPC equivalents of the remaining
// Connector methods — RegisterAgent, ListAgents, Memory, Channel,
// AgentDef, Evaluation, Context, Pause/Snapshot) slot into the
// Connector dispatch pattern mechanically.
type Server struct {
	loomcyclepb.UnimplementedLoomcycleServer

	store     store.Store
	cancelReg *cancel.Registry
	// connector is the v0.8.15+ canonical operation surface. Used by
	// the unary metadata RPCs (GetAgent, CancelAgent). May be nil for
	// older callers that didn't supply one; the affected handlers
	// fall back to the legacy direct-store + cancelReg path.
	connector connector.Connector
	// runner is the wire-agnostic loop driver used by Run + Continue
	// streaming RPCs. Connector.SpawnRun is blocking-only; gRPC
	// streaming needs the callback-driven runner.RunOnce path so this
	// field stays alongside connector.
	runner runner.Runner

	// limits is the RFC AW token-budget tracker the TokenLimit RPC reads for
	// live month-to-date usage + reloads after a CRUD change. Shared with the
	// HTTP /v1/_limits handler (main.go wires it to srv.LimitsTracker()) so both
	// transports see the identical counters. Nil → used reads 0 + reload no-ops.
	limits tokenLimitTracker

	// authToken is the bearer token clients must present in the
	// `authorization` gRPC metadata header. Empty means open-mode
	// (matches the HTTP middleware's "no LOOMCYCLE_AUTH_TOKEN set"
	// behaviour). Compared in constant time.
	authToken string

	// principalResolver (RFC L) resolves a bearer → auth.Principal. Nil
	// = legacy-token-only auth with no principal stamping.
	principalResolver func(context.Context, string) (auth.Principal, bool)
	// authConfigured reports whether auth is active (open-mode decision).
	authConfigured func(context.Context) bool

	// Build identifiers (set at link time in main.go). Surfaced via
	// Health(). Empty fallbacks are fine — adapters don't depend on
	// these being populated.
	buildCommit string
	buildTime   string

	// startedAt is when the server began listening. Used by Health()
	// to report uptime.
	startedAt time.Time
}

// Config carries the server's construction-time inputs.
type Config struct {
	Store     store.Store
	CancelReg *cancel.Registry
	// Connector is the v0.8.15+ operation surface that unary metadata
	// RPCs dispatch through. *internal/api/http.Server satisfies it.
	// Optional — when nil, GetAgent/CancelAgent fall back to direct
	// store+cancelReg paths (preserves backwards compatibility).
	Connector connector.Connector
	// Runner is the wire-agnostic loop driver for the streaming RPCs.
	// *internal/api/http.Server also satisfies it. May be nil — Run +
	// Continue then return codes.Unimplemented (useful for tests that
	// don't need streaming).
	Runner runner.Runner
	// Limits is the RFC AW token-budget tracker the TokenLimit RPC uses for live
	// usage + cache reload. *internal/api/http.Server exposes it via
	// LimitsTracker(). May be nil — TokenLimit then reports used=0 and skips the
	// post-write reload (the store row is still persisted).
	Limits    tokenLimitTracker
	AuthToken string
	// PrincipalResolver resolves a raw bearer to an auth.Principal (RFC
	// L). Wired in main.go to the HTTP server's resolver so gRPC reuses
	// the identical token-substrate + legacy-fallback logic. When set,
	// the auth interceptors resolve the bearer and stamp the principal
	// into ctx (so gRPC-driven runs flow authoritatively through
	// RunOnce); when nil, they fall back to the legacy constant-time
	// AuthToken compare with no principal.
	PrincipalResolver func(context.Context, string) (auth.Principal, bool)
	// AuthConfigured reports whether auth is active (legacy secret set OR
	// an admin token exists). Wired alongside PrincipalResolver so gRPC
	// shares HTTP's open-mode decision: when it returns false the
	// interceptors pass through (dev). Nil → fall back to AuthToken != "".
	AuthConfigured func(context.Context) bool
	BuildCommit    string
	BuildTime      string
}

// New constructs a Server. Caller registers it with a *grpc.Server
// (in cmd/loomcycle/main.go) and starts the listener.
func New(cfg Config) *Server {
	return &Server{
		store:             cfg.Store,
		cancelReg:         cfg.CancelReg,
		connector:         cfg.Connector,
		runner:            cfg.Runner,
		limits:            cfg.Limits,
		authToken:         cfg.AuthToken,
		principalResolver: cfg.PrincipalResolver,
		authConfigured:    cfg.AuthConfigured,
		buildCommit:       cfg.BuildCommit,
		buildTime:         cfg.BuildTime,
		startedAt:         time.Now(),
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
	// Tenant isolation (RFC L/N): fold a cross-tenant run into the same opaque
	// NotFound the HTTP handleGetAgent returns via tenantStore.GetRunByAgentID —
	// agent ids are not secret, so the gate must not be an existence oracle.
	if !grpcTenantVisible(ctx, run.TenantID) {
		return nil, status.Errorf(codes.NotFound, "no run found for agent_id %q", agentID)
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

	// v0.8.15+ dispatch through Connector. The Connector method
	// already handles both "live in registry" (cascade-cancel) and
	// "in store but already ended" (idempotent no-op) paths — we just
	// translate its result into proto + gRPC status codes here.
	if s.connector != nil {
		res, err := s.connector.CancelRun(ctx, agentID, req.GetReason())
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return nil, status.Errorf(codes.NotFound, "no run found for agent_id %q", agentID)
			}
			return nil, status.Errorf(codes.Internal, "connector: %v", err)
		}
		// Cancelled=true means this call initiated the cancel (live in
		// registry); AlreadyEnded=true means the run was already
		// terminated. Both succeed; cancelled_count = 1+cascade on the
		// active path, 0 on the idempotent path.
		if res.Cancelled {
			return &loomcyclepb.CancelAgentResponse{
				CancelledCount: int32(1 + res.CascadeCount),
			}, nil
		}
		return &loomcyclepb.CancelAgentResponse{CancelledCount: 0}, nil
	}

	// Legacy direct path (preserves backwards compat when no
	// Connector was supplied — e.g., older callers, some tests).
	res, ok := s.cancelReg.Cancel(agentID, req.GetReason())
	if ok {
		return &loomcyclepb.CancelAgentResponse{
			CancelledCount: int32(1 + len(res.Cascaded)),
		}, nil
	}
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
		// Tenant isolation (RFC L/N): drop cross-tenant rows, mirroring the HTTP
		// handleListUserAgents post-filter driven by principalTenantScope.
		if !grpcTenantVisible(ctx, r.TenantID) {
			continue
		}
		_, live := s.cancelReg.Get(r.AgentID)
		out.Agents = append(out.Agents, runToProto(r, live))
	}
	return out, nil
}

// UsageReport is the gRPC twin of GET /v1/_usage (RFC AV): grouped token-usage +
// cost aggregation over the ledger ∪ archive. group_by is whitelist-validated;
// tenant scope mirrors principalTenantScope — an admin/legacy caller honors the
// optional wire tenant focus, a scoped principal is confined to its own tenant.
func (s *Server) UsageReport(ctx context.Context, req *loomcyclepb.UsageReportRequest) (*loomcyclepb.UsageReportResponse, error) {
	if s.store == nil {
		return &loomcyclepb.UsageReportResponse{}, nil
	}
	var q store.UsageQuery
	for _, g := range req.GetGroupBy() {
		if _, ok := store.UsageDimColumn(store.UsageDimension(g)); !ok {
			return nil, status.Errorf(codes.InvalidArgument, "unknown group_by dimension: %s (allowed: tenant,user,provider,model,source)", g)
		}
		q.GroupBy = append(q.GroupBy, store.UsageDimension(g))
	}
	if v := req.GetFromTime(); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "from_time must be RFC3339: %v", err)
		}
		q.From = t
	}
	if v := req.GetToTime(); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "to_time must be RFC3339: %v", err)
		}
		q.To = t
	}
	// Tenant scope: admin/legacy honor the wire focus; a scoped principal is
	// confined to its own tenant (req.tenant ignored).
	if tenantID, all := grpcTenantScope(ctx); all {
		q.TenantID = req.GetTenant()
	} else {
		q.TenantID = tenantID
	}
	rows, err := s.store.UsageReport(ctx, q)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "usage report: %v", err)
	}
	out := &loomcyclepb.UsageReportResponse{
		GroupBy: req.GetGroupBy(),
		Rows:    make([]*loomcyclepb.UsageAggregate, 0, len(rows)),
	}
	for _, r := range rows {
		out.Rows = append(out.Rows, &loomcyclepb.UsageAggregate{
			TenantId:            r.TenantID,
			UserId:              r.UserID,
			Provider:            r.Provider,
			Model:               r.Model,
			CredentialSource:    r.CredentialSource,
			InputTokens:         r.InputTokens,
			OutputTokens:        r.OutputTokens,
			CacheCreationTokens: r.CacheCreationTokens,
			CacheReadTokens:     r.CacheReadTokens,
			Cost:                r.Cost,
			Currency:            r.Currency,
			CallCount:           r.CallCount,
			UnpricedCalls:       r.UnpricedCalls,
		})
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
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return nil, status.Errorf(codes.NotFound, "session %q not found", sessionID)
		}
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	// Tenant isolation (RFC L/N): a transcript exposes the session's full
	// history, so gate it on the session's tenant exactly as the HTTP
	// handleTranscript does via tenantStore.GetSession — a cross-tenant session
	// folds into the same opaque NotFound (session ids are not secret).
	if !grpcTenantVisible(ctx, sess.TenantID) {
		return nil, status.Errorf(codes.NotFound, "session %q not found", sessionID)
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
		authedCtx, err := s.authenticate(ctx)
		if err != nil {
			return nil, err
		}
		if err := enforceScope(authedCtx, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(authedCtx, req)
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
		authedCtx, err := s.authenticate(ss.Context())
		if err != nil {
			return err
		}
		if err := enforceScope(authedCtx, info.FullMethod); err != nil {
			return err
		}
		// Carry the principal-bearing ctx into the stream handler.
		return handler(srv, &principalStream{ServerStream: ss, ctx: authedCtx})
	}
}

// authenticate is the shared auth path for both interceptors (RFC L).
// Open mode → pass through. Otherwise resolve the bearer to a principal
// (stamped into the returned ctx so gRPC runs flow authoritatively
// through RunOnce), falling back to the legacy constant-time AuthToken
// compare when no resolver is wired.
func (s *Server) authenticate(ctx context.Context) (context.Context, error) {
	// Open-mode decision, mirroring the HTTP middleware.
	if s.authConfigured != nil {
		if !s.authConfigured(ctx) {
			return ctx, nil
		}
	} else if s.authToken == "" {
		return ctx, nil
	}
	bearer, ok := bearerFromMetadata(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing authorization header")
	}
	if s.principalResolver != nil {
		p, ok := s.principalResolver(ctx, bearer)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "invalid bearer token")
		}
		return auth.WithPrincipal(ctx, p), nil
	}
	// Legacy-only fallback (no resolver wired — e.g. test harnesses).
	if !auth.CompareBearer(bearer, s.authToken) {
		return nil, status.Error(codes.Unauthenticated, "invalid bearer token")
	}
	return ctx, nil
}

// grpcMethodPrefix is the fully-qualified gRPC service path; every
// FullMethod is grpcMethodPrefix + "/" + RPC name.
const grpcMethodPrefix = "/loomcycle.v1.Loomcycle/"

// grpcConsumerScopes maps the NON-admin RPCs to their required scope.
// Everything not listed here defaults to substrate:admin (deny-by-default
// for a security gate — a newly-added admin/substrate RPC is protected even
// if someone forgets to map it; the cost is that a new CONSUMER RPC must be
// added here or it over-requires admin). Mirrors the HTTP requiredScopeFor
// intent: run create/cancel/continue need runs:create; reads need runs:read;
// the channel surface uses the channel scopes. Health is handled before this
// check (it bypasses auth entirely).
var grpcConsumerScopes = map[string]string{
	"Run":                 auth.ScopeRunsCreate,
	"Continue":            auth.ScopeRunsCreate,
	"RunInput":            auth.ScopeRunsCreate, // RFC AI — steering injects instructions (mutation)
	"StreamRun":           auth.ScopeRunsRead,   // RFC AI — pure read tail (mirrors handleRunStream)
	"SpawnRunBatch":       auth.ScopeRunsCreate,
	"CompactRun":          auth.ScopeRunsCreate,
	"CancelAgent":         auth.ScopeRunsCreate,
	"GetTranscript":       auth.ScopeRunsRead,
	"GetAgent":            auth.ScopeRunsRead,
	"ListUserAgents":      auth.ScopeRunsRead,
	"StreamUserRunStates": auth.ScopeRunsRead,
	"PublishChannel":      auth.ScopeChannelPublish,
	"AckChannel":          auth.ScopeChannelPublish,
	"SubscribeChannel":    auth.ScopeChannelRead,
	"PeekChannel":         auth.ScopeChannelRead,
	"ListChannels":        auth.ScopeChannelRead,
	// RFC AV: the usage/cost report is tenant-readable (the handler tenant-scopes
	// the aggregation), mirroring the HTTP /v1/_usage ScopeTenant gate.
	"UsageReport": auth.ScopeTenant,
	// RFC AW: token-budget management. The handler tenant-scopes reads + confines
	// writes to the caller's own tenant (operator-global + cross-tenant stay
	// admin-only), mirroring the HTTP /v1/_limits ScopeTenant gate.
	"TokenLimit": auth.ScopeTenant,
	// RFC AF: the tenant-confined substrate plane — the 8 def families + hook
	// management. ScopeTenant (substrate:admin still satisfies). substrateGRPCCtx
	// stamps the principal's authoritative tenant on the def-tools, and the hook
	// connector stamps + tenant-scopes register/list/delete, so a tenant operator
	// authors ONLY its own surface. OperatorTokenDef is DELIBERATELY absent — it
	// defaults to ScopeAdmin (token minting stays operator-only), mirroring the
	// HTTP /v1/_operatortokendef exclusion.
	"AgentDef":         auth.ScopeTenant,
	"SkillDef":         auth.ScopeTenant,
	"MCPServerDef":     auth.ScopeTenant,
	"ScheduleDef":      auth.ScopeTenant,
	"A2AServerCardDef": auth.ScopeTenant,
	"A2AAgentDef":      auth.ScopeTenant,
	"WebhookDef":       auth.ScopeTenant,
	"MemoryBackendDef": auth.ScopeTenant,
	"VolumeDef":        auth.ScopeTenant,
	// RFC AP TeamDef — team-workflow substrate. Same ScopeTenant posture as the
	// other def families; the HTTP /v1/_teamdef route is tenant-confined too.
	"TeamDef": auth.ScopeTenant,
	// RFC AL Path VFS + RFC AK Document — scope-aware, tenant-isolated tools
	// lifted to the wire. Same ScopeTenant posture as VolumeDef.
	"Path":         auth.ScopeTenant,
	"Document":     auth.ScopeTenant,
	"RegisterHook": auth.ScopeTenant,
	"ListHooks":    auth.ScopeTenant,
	"DeleteHook":   auth.ScopeTenant,
}

// requiredScopeForRPC returns the scope a caller must hold for fullMethod.
// Unknown / unmapped methods → substrate:admin (deny-by-default). This is
// the gRPC analogue of the HTTP requiredScopeFor: PR2 stamped the principal
// over gRPC but never enforced per-RPC scope, so any valid token could reach
// every admin RPC (incl. OperatorTokenDef mint) — closed here.
func requiredScopeForRPC(fullMethod string) string {
	name := strings.TrimPrefix(fullMethod, grpcMethodPrefix)
	if sc, ok := grpcConsumerScopes[name]; ok {
		return sc
	}
	return auth.ScopeAdmin
}

// enforceScope rejects the call when a principal is stamped and lacks the
// RPC's required scope. No principal (open mode / legacy-only test harness)
// → skip, matching the HTTP middleware. PermissionDenied (not Unauthenticated)
// so the caller can tell "authenticated but unauthorized" — scope names are
// public, token state is not (mirrors the HTTP 403 + WWW-Authenticate).
func enforceScope(ctx context.Context, fullMethod string) error {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return nil
	}
	required := requiredScopeForRPC(fullMethod)
	if required != "" && !auth.HasScope(p.Scopes, required) {
		return status.Errorf(codes.PermissionDenied, "insufficient scope: %s required", required)
	}
	return nil
}

// bearerFromMetadata extracts the raw token (sans "Bearer ") from the
// gRPC `authorization` metadata header.
func bearerFromMetadata(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		return "", false
	}
	h := values[0]
	const pfx = "Bearer "
	if len(h) > len(pfx) && strings.EqualFold(h[:len(pfx)], pfx) {
		return h[len(pfx):], true
	}
	return "", false
}

// principalStream wraps a ServerStream to override Context() with the
// principal-bearing ctx produced by authenticate.
type principalStream struct {
	googlegrpc.ServerStream
	ctx context.Context
}

func (w *principalStream) Context() context.Context { return w.ctx }

// healthFullMethod is the gRPC method string for Loomcycle.Health.
// Hardcoded vs derived from the proto descriptor because it's a tiny
// well-known constant and the descriptor lookup at every interceptor
// call would be wasted work.
const healthFullMethod = "/loomcycle.v1.Loomcycle/Health"

// MustLogStartupBanner prints a one-line startup notice when the
// gRPC server starts listening. Symmetric with the HTTP banner in
// main.go.
func (s *Server) MustLogStartupBanner(addr string) {
	log.Printf("loomcycle gRPC listening on %s (auth=%v)", addr, s.authToken != "")
}

// ========================
// Streaming RPCs
// ========================

// Run server-streams events from a fresh agent run. Mirrors HTTP's
// POST /v1/runs. The gRPC stream's first message is always an Event
// of type "session" carrying the new session_id, followed by an
// Event of type "agent" carrying agent_id / run_id (parity with the
// HTTP SSE side-channel frames). After that, every providers.Event
// emitted by the loop becomes one Event message on the stream.
//
// Returning nil ends the stream cleanly; returning a status error
// surfaces the failure to the client.
func (s *Server) Run(req *loomcyclepb.RunRequest, stream loomcyclepb.Loomcycle_RunServer) error {
	if s.runner == nil {
		return status.Error(codes.Unimplemented, "Run streaming requires a runner; this Server was constructed without one")
	}
	if errMsg, ok := connector.ValidateUserCredentialsMap(req.GetUserCredentials()); !ok {
		return status.Error(codes.InvalidArgument, errMsg)
	}
	in := runInputFromProto(runInputProtoArgs{
		Agent:           req.GetAgent(),
		SessionID:       req.GetSessionId(),
		Segments:        req.GetSegments(),
		Tools:           req.GetTools(),
		AllowedHosts:    req.GetAllowedHosts(),
		WebSearchFilter: req.GetWebSearchFilter(),
		UserID:          req.GetUserId(),
		AgentID:         req.GetAgentId(),
		TenantID:        req.GetTenantId(),
		UserTier:        req.GetUserTier(),
		UserBearer:      req.GetUserBearer(),
		UserCredentials: req.GetUserCredentials(), // v1.x RFC F
		Sampling:        samplingFromProto(req.GetSampling()),
		Compaction:      compactionFromProto(req.GetCompaction()),
		Interactive:     req.GetInteractive(), // RFC AI
	})
	return s.driveStream(stream.Context(), stream, in)
}

// Continue server-streams events from a continuation. Mirrors HTTP's
// POST /v1/sessions/{id}/messages. Same stream shape as Run; the
// "session" frame echoes the existing session_id (rather than
// announcing a new one) so adapters can use one decoder for both.
func (s *Server) Continue(req *loomcyclepb.ContinueRequest, stream loomcyclepb.Loomcycle_ContinueServer) error {
	if s.runner == nil {
		return status.Error(codes.Unimplemented, "Continue streaming requires a runner; this Server was constructed without one")
	}
	if req.GetSessionId() == "" {
		return status.Error(codes.InvalidArgument, "session_id is required for Continue")
	}
	if errMsg, ok := connector.ValidateUserCredentialsMap(req.GetUserCredentials()); !ok {
		return status.Error(codes.InvalidArgument, errMsg)
	}
	in := runInputFromProto(runInputProtoArgs{
		// Agent + TenantID + UserID omitted — server inherits from the
		// existing session per the HTTP wire's messagesRequest contract.
		SessionID:       req.GetSessionId(),
		Segments:        req.GetSegments(),
		Tools:           req.GetTools(),
		AllowedHosts:    req.GetAllowedHosts(),
		WebSearchFilter: req.GetWebSearchFilter(),
		AgentID:         req.GetAgentId(),
		UserTier:        req.GetUserTier(),
		UserBearer:      req.GetUserBearer(),
		UserCredentials: req.GetUserCredentials(), // v1.x RFC F
		Sampling:        samplingFromProto(req.GetSampling()),
		Compaction:      compactionFromProto(req.GetCompaction()),
		Interactive:     req.GetInteractive(), // RFC AI
	})
	return s.driveStream(stream.Context(), stream, in)
}

// SpawnRunBatch is the RFC Y external fan-out: spawn N fresh runs
// concurrently (mode "join") and return the combined index-aligned envelope.
// Dispatches to connector.SpawnRunBatch, which bounds concurrency on the
// per-user admission gate and captures per-child failures in-envelope. The RPC
// only errors on a malformed batch (over-cap / unsupported mode). Mirrors
// POST /v1/runs:batch.
func (s *Server) SpawnRunBatch(ctx context.Context, req *loomcyclepb.BatchSpawnRequest) (*loomcyclepb.BatchSpawnResult, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "SpawnRunBatch requires a connector; this Server was constructed without one")
	}
	spawns := make([]connector.SpawnRunRequest, 0, len(req.GetSpawns()))
	for _, sp := range req.GetSpawns() {
		if errMsg, ok := connector.ValidateUserCredentialsMap(sp.GetUserCredentials()); !ok {
			return nil, status.Error(codes.InvalidArgument, errMsg)
		}
		spawns = append(spawns, spawnRequestFromProto(sp))
	}
	res, err := s.connector.SpawnRunBatch(ctx, connector.BatchSpawnRequest{
		Spawns:    spawns,
		Mode:      req.GetMode(),
		TimeoutMS: int(req.GetTimeoutMs()),
	})
	if err != nil {
		// Malformed batch (empty / over-cap / unsupported mode) — a client
		// error, not Internal. Per-child run failures live inside res.
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	out := &loomcyclepb.BatchSpawnResult{Spawned: int32(res.Spawned)}
	for i := range res.Results {
		out.Results = append(out.Results, spawnResultToProto(res.Results[i]))
	}
	return out, nil
}

// CompactRun summarizes a run's conversation to free context. Dispatches to
// connector.CompactRun (keyed by run_id). Mirrors POST /v1/runs/{run_id}/compact;
// the connector's HTTP-status-bearing error maps to the matching gRPC code
// (mid-turn → FailedPrecondition, missing run → NotFound, …).
func (s *Server) CompactRun(ctx context.Context, req *loomcyclepb.CompactRunRequest) (*loomcyclepb.CompactRunResult, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "CompactRun requires a connector; this Server was constructed without one")
	}
	if !validIdent(req.GetRunId()) {
		return nil, status.Error(codes.InvalidArgument, "run_id must match [A-Za-z0-9_-]{1,128}")
	}
	res, err := s.connector.CompactRun(ctx, req.GetRunId())
	if err != nil {
		return nil, compactErrToStatus(err)
	}
	return &loomcyclepb.CompactRunResult{
		RunId:        res.RunID,
		Compacted:    res.Compacted,
		BeforeTokens: int32(res.BeforeTokens),
		AfterTokens:  int32(res.AfterTokens),
		Applied:      res.Applied,
	}, nil
}

// spawnRequestFromProto maps a proto RunRequest to a connector.SpawnRunRequest
// for the fan-out path. session_id is carried through but batch children are
// forced fresh by SpawnRunBatch. parent_context / metadata are not on the proto
// RunRequest (a pre-existing gRPC gap) so they stay nil.
func spawnRequestFromProto(req *loomcyclepb.RunRequest) connector.SpawnRunRequest {
	r := connector.SpawnRunRequest{
		Agent:           req.GetAgent(),
		SessionID:       req.GetSessionId(),
		TenantID:        req.GetTenantId(),
		Segments:        segmentsFromProto(req.GetSegments()),
		Tools:           req.GetTools(),
		WebSearchFilter: req.GetWebSearchFilter(),
		UserID:          req.GetUserId(),
		AgentID:         req.GetAgentId(),
		UserTier:        req.GetUserTier(),
		UserBearer:      req.GetUserBearer(),
		UserCredentials: req.GetUserCredentials(),
		Sampling:        samplingFromProto(req.GetSampling()),
		Compaction:      compactionFromProto(req.GetCompaction()),
	}
	if hosts := req.GetAllowedHosts(); hosts != nil {
		list := hosts.GetList()
		r.AllowedHosts = &list
	}
	return r
}

// spawnResultToProto maps one connector.SpawnRunResult to its proto shape.
func spawnResultToProto(r connector.SpawnRunResult) *loomcyclepb.SpawnResult {
	return &loomcyclepb.SpawnResult{
		AgentId:    r.AgentID,
		RunId:      r.RunID,
		SessionId:  r.SessionID,
		Status:     r.Status,
		StopReason: r.StopReason,
		FinalText:  r.FinalText,
		Usage:      usageToProto(r.Usage),
		Error:      r.Error,
	}
}

// usageToProto maps a providers.Usage to the proto Usage message (nil-safe).
func usageToProto(u *providers.Usage) *loomcyclepb.Usage {
	if u == nil {
		return nil
	}
	return &loomcyclepb.Usage{
		InputTokens:         int64(u.InputTokens),
		OutputTokens:        int64(u.OutputTokens),
		CacheCreationTokens: int64(u.CacheCreationTokens),
		CacheReadTokens:     int64(u.CacheReadTokens),
		Model:               u.Model,
	}
}

// compactErrToStatus maps connector.CompactRun's HTTP-status-bearing error to
// the matching gRPC code (the error implements HTTPStatus() — see the http
// package's compactErr — so the mapping stays in sync with the REST surface
// without leaking the concrete type). Unrecognized errors map to Internal.
func compactErrToStatus(err error) error {
	var hse interface{ HTTPStatus() int }
	if errors.As(err, &hse) {
		switch hse.HTTPStatus() {
		case nethttp.StatusNotFound:
			return status.Error(codes.NotFound, err.Error())
		case nethttp.StatusConflict:
			return status.Error(codes.FailedPrecondition, err.Error())
		case nethttp.StatusServiceUnavailable, nethttp.StatusBadGateway:
			return status.Error(codes.Unavailable, err.Error())
		case nethttp.StatusTooManyRequests:
			return status.Error(codes.ResourceExhausted, err.Error())
		}
	}
	return status.Error(codes.Internal, err.Error())
}

// runStreamSink abstracts the two streaming server types
// (Loomcycle_RunServer and Loomcycle_ContinueServer) to one Send
// method so driveStream is generic across both RPC shapes.
type runStreamSink interface {
	Send(*loomcyclepb.Event) error
}

// driveStream translates the runner.RunCallbacks into stream.Send
// calls, runs the unified loop, and maps runner.ErrFoo → gRPC
// status codes on completion.
//
// The OnRegistered callback emits two synthetic Events on the stream
// — type="session" with the session_id in Text, and type="agent"
// with agent_id+run_id+session_id+parent_agent_id encoded into
// fields the proto Event already carries (Text=agent_id;
// StopReason=parent_agent_id for transport efficiency). Adapters
// that want a typed first frame can decode these by their type
// strings; the wire shape mirrors HTTP's "session" + "agent" SSE
// side-channel frames.
func (s *Server) driveStream(ctx context.Context, stream runStreamSink, in runner.RunInput) error {
	// Send error captured from OnEvent so we can fail the RPC if
	// the client disappears mid-stream. Without this the loop
	// would keep emitting events into a broken pipe.
	var sendErr error

	cb := runner.RunCallbacks{
		OnRegistered: func(agentID, runID, sessionID, parentAgentID string) {
			// "session" frame.
			if err := stream.Send(&loomcyclepb.Event{
				Type: "session",
				Text: sessionID,
			}); err != nil {
				sendErr = err
				return
			}
			// "agent" frame — pack the four IDs into the existing
			// Event fields so we don't need a new proto message.
			// Adapters parse: type=="agent", text=agent_id,
			// stop_reason=parent_agent_id, plus a JSON envelope in
			// `error` for run_id+session_id (using `error` as a
			// generic string carrier; sub-optimal but avoids a
			// proto change in PR 2).
			payload := agentFrameJSON(agentID, runID, sessionID, parentAgentID)
			// Capture the send error so the OnEvent guard fires
			// if the agent frame fails — otherwise the loop would
			// run for one full iteration emitting into a broken
			// pipe before discovering the client is gone.
			if err := stream.Send(&loomcyclepb.Event{
				Type:       "agent",
				Text:       agentID,
				StopReason: parentAgentID,
				Error:      payload,
			}); err != nil {
				sendErr = err
			}
		},
		OnEvent: func(ev providers.Event) {
			if sendErr != nil {
				return // skip emits once the stream is broken
			}
			if err := stream.Send(eventToProto(ev)); err != nil {
				sendErr = err
			}
		},
	}

	runErr := s.runner.RunOnce(ctx, in, cb)
	if sendErr != nil {
		// Stream broke mid-run — the runner kept going (correct, for
		// transcript persistence), but we surface to the client.
		return status.Errorf(codes.Canceled, "stream send failed: %v", sendErr)
	}
	return mapRunnerErr(runErr)
}

// runInputProtoArgs gathers the proto fields runInputFromProto reads.
// Struct-arg shape rather than positional parameters because the field
// set has grown past the comfortable positional limit (per-run policy
// fields TenantID/UserTier/UserBearer were added when gRPC reached
// HTTP wire parity).
type runInputProtoArgs struct {
	Agent           string
	SessionID       string
	Segments        []*loomcyclepb.PromptSegment
	Tools           []string
	AllowedHosts    *loomcyclepb.HostAllowlist
	WebSearchFilter string
	UserID          string
	AgentID         string
	TenantID        string
	UserTier        string
	UserBearer      string
	UserCredentials map[string]string  // v1.x RFC F per-tool named credentials
	Sampling        *config.Sampling   // v0.28.0 per-run sampling override
	Compaction      *config.Compaction // v0.32.0 per-run compaction override
	Interactive     bool               // RFC AI — park at end_turn for steering
}

// runInputFromProto maps the proto request fields into the
// runner.RunInput shared between Run and Continue.
func runInputFromProto(a runInputProtoArgs) runner.RunInput {
	in := runner.RunInput{
		Agent:           a.Agent,
		SessionID:       a.SessionID,
		Segments:        segmentsFromProto(a.Segments),
		Tools:           a.Tools,
		WebSearchFilter: a.WebSearchFilter,
		UserID:          a.UserID,
		AgentID:         a.AgentID,
		TenantID:        a.TenantID,
		UserTier:        a.UserTier,
		UserBearer:      a.UserBearer,
		UserCredentials: a.UserCredentials, // v1.x RFC F per-tool named credentials
		Sampling:        a.Sampling,        // v0.28.0 per-run sampling override
		Compaction:      a.Compaction,      // v0.32.0 per-run compaction override
		Interactive:     a.Interactive,     // RFC AI — park at end_turn for steering
	}
	if a.AllowedHosts != nil {
		// Proto3 message-type field present → caller did supply a
		// list (possibly empty). Mirrors HTTP's *[]string distinction
		// between nil (no narrowing) and []string{} (deny-all).
		list := a.AllowedHosts.GetList()
		in.AllowedHosts = &list
	}
	return in
}

// samplingFromProto maps the proto Sampling message to *config.Sampling,
// preserving field presence (proto3 `optional` → a nil pointer means
// "inherit"; an explicit 0.0 temperature stays 0.0, not unset). Returns nil
// when the whole message is absent.
func samplingFromProto(p *loomcyclepb.Sampling) *config.Sampling {
	if p == nil {
		return nil
	}
	s := &config.Sampling{Stop: p.GetStop()}
	if p.Temperature != nil {
		v := *p.Temperature
		s.Temperature = &v
	}
	if p.TopP != nil {
		v := *p.TopP
		s.TopP = &v
	}
	if p.TopK != nil {
		v := int(*p.TopK)
		s.TopK = &v
	}
	if p.FrequencyPenalty != nil {
		v := *p.FrequencyPenalty
		s.FrequencyPenalty = &v
	}
	if p.PresencePenalty != nil {
		v := *p.PresencePenalty
		s.PresencePenalty = &v
	}
	if p.Seed != nil {
		v := int(*p.Seed)
		s.Seed = &v
	}
	return s
}

// compactionFromProto maps the proto Compaction message to *config.Compaction,
// preserving field presence (an absent message = inherit; enabled=false is an
// explicit "off", not unset). Returns nil when the message is absent.
func compactionFromProto(p *loomcyclepb.Compaction) *config.Compaction {
	if p == nil {
		return nil
	}
	c := &config.Compaction{}
	if p.Enabled != nil {
		v := *p.Enabled
		c.Enabled = &v
	}
	if p.TargetPercentage != nil {
		v := int(*p.TargetPercentage)
		c.TargetPercentage = &v
	}
	if p.KeepLastN != nil {
		v := int(*p.KeepLastN)
		c.KeepLastN = &v
	}
	if p.KeepFirst != nil {
		v := *p.KeepFirst
		c.KeepFirst = &v
	}
	if p.AutocompactAtPct != nil {
		v := int(*p.AutocompactAtPct)
		c.AutoCompactAtPct = &v
	}
	if p.Model != nil {
		v := *p.Model
		c.Model = &v
	}
	return c
}

func segmentsFromProto(segs []*loomcyclepb.PromptSegment) []loop.PromptSegment {
	out := make([]loop.PromptSegment, 0, len(segs))
	for _, s := range segs {
		blocks := make([]loop.PromptContentBlock, 0, len(s.GetContent()))
		for _, b := range s.GetContent() {
			blk := loop.PromptContentBlock{
				Type:      b.GetType(),
				Text:      b.GetText(),
				Cacheable: b.GetCacheable(),
				MediaType: b.GetMediaType(),
			}
			// gRPC carries raw image bytes (RFC AT); the loop works in base64
			// internally — uniform with the HTTP/JSON wire and what every
			// driver emits. Encode at the boundary. Empty for non-image blocks.
			if d := b.GetData(); len(d) > 0 {
				blk.Data = base64.StdEncoding.EncodeToString(d)
			}
			blocks = append(blocks, blk)
		}
		out = append(out, loop.PromptSegment{
			Role:    s.GetRole(),
			Content: blocks,
		})
	}
	return out
}

// eventToProto maps a providers.Event onto the proto Event message.
// The proto fields mirror providers.Event 1:1 — see proto/loomcycle.proto.
func eventToProto(ev providers.Event) *loomcyclepb.Event {
	out := &loomcyclepb.Event{
		Type:       string(ev.Type),
		Text:       ev.Text,
		Error:      ev.Error,
		IsError:    ev.IsError,
		StopReason: ev.StopReason,
	}
	if ev.ToolUse != nil {
		out.ToolUse = &loomcyclepb.ToolUse{
			Id:    ev.ToolUse.ID,
			Name:  ev.ToolUse.Name,
			Input: ev.ToolUse.Input,
		}
	}
	if ev.Usage != nil {
		out.Usage = &loomcyclepb.Usage{
			InputTokens:         int64(ev.Usage.InputTokens),
			OutputTokens:        int64(ev.Usage.OutputTokens),
			CacheCreationTokens: int64(ev.Usage.CacheCreationTokens),
			CacheReadTokens:     int64(ev.Usage.CacheReadTokens),
			Model:               ev.Usage.Model,
		}
	}
	if ev.Retry != nil {
		out.Retry = &loomcyclepb.Retry{
			Provider: ev.Retry.Provider,
			Attempt:  int32(ev.Retry.Attempt),
			WaitMs:   ev.Retry.WaitMs,
			Reason:   ev.Retry.Reason,
		}
	}
	// RFC AW — the token-budget crossing payload on type=limit frames. Type is
	// already set to "limit" from ev.Type above (mirrors how Usage is mapped).
	if ev.Limit != nil {
		out.Limit = &loomcyclepb.LimitInfo{
			Scope:    ev.Limit.Scope,
			ScopeId:  ev.Limit.ScopeID,
			Severity: ev.Limit.Severity,
			Window:   ev.Limit.Window,
			Used:     ev.Limit.Used,
			Limit:    ev.Limit.Limit,
			Message:  ev.Limit.Message,
		}
	}
	if ev.HostWidening != nil {
		out.HostWidening = &loomcyclepb.HostWidening{
			ToolCallId: ev.HostWidening.ToolCallID,
			ToolName:   ev.HostWidening.ToolName,
			Url:        ev.HostWidening.URL,
			HookOwner:  ev.HostWidening.HookOwner,
			HookName:   ev.HostWidening.HookName,
			HostsAdded: ev.HostWidening.HostsAdded,
		}
	}
	// RFC AI interactive payloads — previously dropped on the gRPC wire.
	if ev.AwaitingInput != nil {
		out.AwaitingInput = &loomcyclepb.AwaitingInput{
			SinceTurn: int32(ev.AwaitingInput.SinceTurn),
		}
	}
	if ev.UserInput != nil {
		out.UserInput = &loomcyclepb.UserInput{
			Text:   ev.UserInput.Text,
			Source: ev.UserInput.Source,
			SeenAt: ev.UserInput.SeenAt,
		}
	}
	return out
}

// agentFrameJSON encodes the four registration IDs into a compact
// JSON object stored in the synthetic "agent" Event's Error field.
// Adapters decode this on the client side.
//
// Uses encoding/json — fires once per run (not per event), so the
// allocation cost is negligible and the previous hand-rolled
// concat was a fragile micro-optimisation: a future caller passing
// an ID with `"` or `\` would have produced malformed JSON.
func agentFrameJSON(agentID, runID, sessionID, parentAgentID string) string {
	b, err := json.Marshal(struct {
		AgentID       string `json:"agent_id"`
		RunID         string `json:"run_id"`
		SessionID     string `json:"session_id"`
		ParentAgentID string `json:"parent_agent_id"`
	}{agentID, runID, sessionID, parentAgentID})
	if err != nil {
		// json.Marshal on a struct of strings cannot fail in
		// practice — there's no UnmarshalJSON on string and no
		// channel/func in the type. Fall back to an empty
		// envelope; adapters tolerate this (test_drive_stream
		// covers a malformed envelope).
		return "{}"
	}
	return string(b)
}

// mapRunnerErr converts a runner.ErrFoo (or wrapped variant) into a
// gRPC status error. Preserves the underlying message so adapter
// logs can correlate with HTTP-side error bodies.
func mapRunnerErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, runner.ErrUnknownAgent):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, runner.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, runner.ErrUnknownProvider):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, runner.ErrSessionRequired):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, runner.ErrSessionNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, runner.ErrSessionBusy):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, runner.ErrAgentIDInUse):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, runner.ErrBackpressure),
		errors.Is(err, runner.ErrPerUserQuotaExhausted):
		// Both backpressure flavors share ResourceExhausted on the
		// gRPC wire. HTTP distinguishes the two via the JSON body's
		// `code` field + Retry-After header; gRPC consumers branch on
		// the error message if they need to distinguish.
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, runner.ErrRuntimePaused):
		// Runtime-wide pause in effect (RFC X) — new runs rejected until
		// resume. Unavailable mirrors the HTTP 503 gate.
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, runner.ErrTokenLimitExceeded):
		// Per-scope token budget hard cap reached (RFC AW). ResourceExhausted
		// mirrors the HTTP 429 refusal + the backpressure/quota flavors above.
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, resolve.ErrOperatorKeyRestricted),
		errors.Is(err, providers.ErrOperatorKeyForbidden):
		// RFC AX: the run's principal may not spend the operator's provider key
		// and has no keyable provider of its own (routing refusal) or reached
		// the driver backstop on a pinned agent. PermissionDenied mirrors the
		// HTTP 403; the message names the way out (own key / the scope).
		return status.Error(codes.PermissionDenied, err.Error())
	default:
		return status.Errorf(codes.Internal, "runner: %v", err)
	}
}
