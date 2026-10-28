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
	"log/slog"
	nethttp "net/http"
	"slices"
	"strings"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/api/memberpath"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/awaited"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/directory"
	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
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

	// liveRun reports whether a run is in flight on this replica, keyed on the
	// run — see Config.RunLive. Nil → the cancel registry alone.
	liveRun func(agentID, runID string) bool

	// queuePosition reports where a queued run waits — see Config.QueuePosition.
	// Nil → a queued run is read without a position.
	queuePosition func(agentID string) (position int, reason string, ok bool)

	// limits is the RFC AW token-budget tracker the TokenLimit RPC reads for
	// live month-to-date usage + reloads after a CRUD change. Shared with the
	// HTTP /v1/_limits handler (main.go wires it to srv.LimitsTracker()) so both
	// transports see the identical counters. Nil → used reads 0 + reload no-ops.
	limits tokenLimitTracker

	// usageDetailSince — see Config.UsageDetailSince. Nil → zero.
	usageDetailSince func() time.Time

	// authToken is the bearer token clients must present in the
	// `authorization` gRPC metadata header. Empty means open-mode
	// (matches the HTTP middleware's "no LOOMCYCLE_AUTH_TOKEN set"
	// behaviour). Compared in constant time.
	authToken string

	// principalResolver (RFC L) resolves a bearer → auth.Principal. Nil
	// = legacy-token-only auth with no principal stamping.
	principalResolver func(context.Context, string) (auth.Principal, bool)
	aclMode           authz.Mode
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
	Limits tokenLimitTracker
	// UsageDetailSince reports the oldest instant the per-call usage ledger
	// holds (zero when nothing is archived), for the UsageReport RPC's hour
	// buckets and detail_since. *internal/api/http.Server provides it; nil =
	// zero (every bucket allowed).
	UsageDetailSince func() time.Time
	AuthToken        string
	// ACLMode is the RFC EY rollout mode the gate evaluates under (compat
	// when unset); it mirrors the HTTP server's.
	ACLMode authz.Mode
	// PrincipalResolver resolves a raw bearer to an auth.Principal (RFC
	// L). Wired in main.go to the HTTP server's resolver so gRPC reuses
	// the identical token-substrate + legacy-fallback logic. When set,
	// the auth interceptors resolve the bearer and stamp the principal
	// into ctx (so gRPC-driven runs flow authoritatively through
	// RunOnce); when nil, they fall back to the legacy constant-time
	// AuthToken compare with no principal.
	PrincipalResolver func(context.Context, string) (auth.Principal, bool)
	// AuthConfigured reports whether auth is active (legacy secret set,
	// config principals declared, or an admin token exists). Wired alongside
	// PrincipalResolver so gRPC shares HTTP's open-mode decision: when it
	// returns false the interceptors pass through (dev). Nil → fall back to AuthToken != "".
	AuthConfigured func(context.Context) bool
	BuildCommit    string
	BuildTime      string
	// RunLive reports whether a run is in flight on this replica — HTTP's
	// RunLive, which also sees team walks (they are not in the cancel
	// registry). Used by GetRun. Nil → the cancel registry alone.
	RunLive func(agentID, runID string) bool
	// QueuePosition reports where a QUEUED run waits on this replica: its place
	// (1 = next to start) and why ("user_cap" | "global_cap"); ok=false when
	// this replica holds no wait for the run. The run semaphore's Position,
	// keyed on the run's agent id — the source the HTTP run read uses. Used by
	// the single-run reads (GetAgent, GetRun). Nil → no position is reported.
	QueuePosition func(agentID string) (position int, reason string, ok bool)
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
		usageDetailSince:  cfg.UsageDetailSince,
		liveRun:           cfg.RunLive,
		queuePosition:     cfg.QueuePosition,
		authToken:         cfg.AuthToken,
		principalResolver: cfg.PrincipalResolver,
		aclMode:           cfg.ACLMode,
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

// Config returns the instance-configuration report as JSON, at the caller's
// disclosure level. Mirrors GET /v1/config through the shared connector, so the
// two transports cannot answer the same question differently.
//
// The HTTP side's third, narrower `public` level does not exist here: it requires
// an unauthenticated read under LOOMCYCLE_PUBLIC_CONFIG, and gRPC authenticates
// before dispatch. A caller therefore gets `authenticated` or `admin` from its own
// scopes, and `view` in the payload names which.
func (s *Server) Config(ctx context.Context, _ *loomcyclepb.ConfigRequest) (*loomcyclepb.ConfigResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "config is not wired on this instance")
	}
	b, err := s.connector.Config(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "config: %v", err)
	}
	return &loomcyclepb.ConfigResponse{ConfigJson: b}, nil
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
		// No store, no run tree to walk: a token limited to certain agents is
		// refused (runTree fails closed).
		if !ok || !s.runTree(ctx).Reaches(entry.RunID) {
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
	// Tenant isolation (RFC L/N): fold a cross-tenant run — or, for an isolated
	// member, another user's — into the same opaque NotFound the HTTP
	// handleGetAgent returns. Agent ids are not secret, so the gate must not be
	// an existence oracle, and the reply carries the run's prompt and result.
	// A token limited to certain agents reads only runs in trees it could have
	// started; any other is the same NotFound.
	if p, ok := auth.PrincipalFromContext(ctx); !auth.OwnedRowVisible(p, ok, run.TenantID, run.UserID) || !s.runTree(ctx).ReachesKnown(runLink(run)) {
		return nil, status.Errorf(codes.NotFound, "no run found for agent_id %q", agentID)
	}
	_, live := s.cancelReg.Get(agentID)
	return s.singleRunProto(ctx, run, live), nil
}

// GetRun mirrors HTTP's GET /v1/runs/{run_id}: GetAgent addressed by the run
// itself. An agent id can name many runs — every walk of a team is filed
// under `team:<name>` — and GetAgent answers only the latest.
func (s *Server) GetRun(ctx context.Context, req *loomcyclepb.GetRunRequest) (*loomcyclepb.Agent, error) {
	runID := req.GetRunId()
	if !validIdent(runID) {
		return nil, status.Error(codes.InvalidArgument, "run_id must match [A-Za-z0-9_-]{1,128}")
	}
	if s.store == nil {
		// The cancel registry is keyed by agent id: without a store a run id
		// cannot be looked up.
		return nil, status.Errorf(codes.NotFound, "no run found for run_id %q (no store configured)", runID)
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return nil, status.Errorf(codes.NotFound, "no run found for run_id %q", runID)
		}
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	// The same fold as GetAgent: another tenant's run, or for an isolated
	// member another user's, is the NotFound a missing run gets.
	if p, ok := auth.PrincipalFromContext(ctx); !auth.OwnedRowVisible(p, ok, run.TenantID, run.UserID) || !s.runTree(ctx).ReachesKnown(runLink(run)) {
		return nil, status.Errorf(codes.NotFound, "no run found for run_id %q", runID)
	}
	return s.singleRunProto(ctx, run, s.runLive(run)), nil
}

// runLive reports whether THIS run is in flight. The injected check (HTTP's
// RunLive) also sees team walks, which are not in the cancel registry; without
// it only an ordinary run's registry entry — for this run id — counts.
func (s *Server) runLive(run store.Run) bool {
	if s.liveRun != nil {
		return s.liveRun(run.AgentID, run.ID)
	}
	e, ok := s.cancelReg.Get(run.AgentID)
	return ok && e.RunID == run.ID
}

// singleRunProto builds the single-run read for a run the caller has already
// been cleared to see: the awaited state, and the answer, spec and draft a
// listing leaves out (RFC DI). Shared by every RPC that reads one run, like
// HTTP's singleRunResponse.
func (s *Server) singleRunProto(ctx context.Context, run store.Run, live bool) *loomcyclepb.Agent {
	out := runToProto(run, live)
	s.fillAwaitedState(ctx, out)
	s.fillQueuePosition(out)
	out.Result = run.Result
	out.Spec = connector.ServedRunSpec(run.RunConfig)
	if run.Status == store.RunConfigured {
		if d, err := s.store.GetRunDraft(ctx, run.ID); err == nil {
			out.Draft = d
		}
	}
	return out
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
	statuses, awaitedStates, err := userAgentsFilter(req)
	if err != nil {
		return nil, err
	}
	p, ok := auth.PrincipalFromContext(ctx)
	// An isolated member lists only its own runs: another user's listing is
	// that of a user with no runs, answered before any scan, as HTTP does
	// (isolatedCrossUser) — a scan would read that user's rows, and its
	// `truncated` would say they exist.
	if auth.IsIsolated(p, ok) && req.GetUserId() != p.Subject {
		return &loomcyclepb.ListUserAgentsResponse{Agents: []*loomcyclepb.Agent{}}, nil
	}
	// The tenant goes into the query so it applies before the store's row
	// bound; "" = every tenant (admin / legacy / open).
	queryTenant, allTenants := grpcTenantScope(ctx)
	if allTenants {
		queryTenant = ""
	}
	// A token limited to certain agents lists only runs in trees it could have
	// started; the rest are left out (ReachesKnown is true, with no read, for
	// every other caller).
	tree := s.runTree(ctx)
	visible := func(r store.Run) bool {
		return auth.OwnedRowVisible(p, ok, r.TenantID, r.UserID) && tree.ReachesKnown(runLink(r))
	}
	// truncated describes the scan, which reads every row the query matches.
	// It is passed on only when visible can drop nothing the query matched:
	// never for a token limited to certain agents, and never for a scoped
	// caller whose tenant is "" (the store reads that as every tenant, and
	// visible narrows it). Otherwise it would say that rows the caller may not
	// see exist. The cost — such a caller whose own rows lie past the scan
	// bound is not told so — is the safe side.
	truncatedExact := !tree.Restricted() && (allTenants || queryTenant != "")
	if len(awaitedStates) > 0 {
		held, truncated, err := awaited.RunningInStates(ctx, s.store, queryTenant, req.GetUserId(), awaitedStates, visible, 0, userAgentsListMax)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "store: %v", err)
		}
		out := &loomcyclepb.ListUserAgentsResponse{Agents: make([]*loomcyclepb.Agent, 0, len(held)), Truncated: truncated && truncatedExact}
		for _, h := range held {
			_, live := s.cancelReg.Get(h.Run.AgentID)
			a := runToProto(h.Run, live)
			a.AwaitedState, a.AwaitedOn = h.State, h.On
			out.Agents = append(out.Agents, a)
		}
		return out, nil
	}
	var (
		runs      []store.Run
		truncated bool
	)
	if tree.Restricted() {
		// The allowlist is a filter the query cannot express: read on past
		// the rows it drops until the answer is full, as HTTP does.
		runs, truncated, err = store.CollectRunsByUser(ctx, s.store, queryTenant, req.GetUserId(), statuses, visible, 0, userAgentsListMax)
	} else {
		runs, err = s.store.ListActiveRunsByUser(ctx, queryTenant, req.GetUserId(), statuses...)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	out := &loomcyclepb.ListUserAgentsResponse{Agents: make([]*loomcyclepb.Agent, 0, len(runs)), Truncated: truncated && truncatedExact}
	for _, r := range runs {
		// Drop cross-tenant rows, and for an isolated member another user's rows,
		// mirroring HTTP handleListUserAgents (its isolatedCrossUser short-circuit
		// plus the principalTenantScope post-filter). Every row here carries the
		// requested user_id, so a cross-user list from an isolated member comes
		// back empty exactly as it does over HTTP. Still needed after the query's
		// own tenant filter: the store reads tenant "" as "all tenants", so a
		// non-admin principal whose tenant is "" gets every tenant's rows back
		// and must be narrowed here.
		if !visible(r) {
			continue
		}
		_, live := s.cancelReg.Get(r.AgentID)
		a := runToProto(r, live)
		s.fillAwaitedState(ctx, a)
		out.Agents = append(out.Agents, a)
	}
	return out, nil
}

// userAgentsListMax bounds one ListUserAgents answer, as HTTP bounds its twin.
const userAgentsListMax = 100

// userAgentsFilter reads ListUserAgents' filters, with HTTP's semantics except
// the default: no status filter here has always meant every status.
//
// The single `status` field keeps its old reading (taken verbatim, "" → every
// status) so existing callers see no change; the repeated `statuses` is
// validated, and "all" alone in it is no filter. Setting both is refused rather
// than guessing which wins. awaited_states implies running, so any other status
// with it is refused.
func userAgentsFilter(req *loomcyclepb.ListUserAgentsRequest) ([]store.RunStatus, []string, error) {
	var statuses []store.RunStatus
	switch raw := req.GetStatuses(); {
	case req.GetStatus() != "" && len(raw) > 0:
		return nil, nil, status.Error(codes.InvalidArgument, "set status or statuses, not both")
	case req.GetStatus() != "":
		statuses = []store.RunStatus{store.RunStatus(req.GetStatus())}
	case len(raw) == 1 && raw[0] == "all":
	default:
		for _, v := range raw {
			if !store.IsRunStatus(store.RunStatus(v)) {
				return nil, nil, status.Errorf(codes.InvalidArgument, "statuses: %q is not a run status (or \"all\", alone)", v)
			}
			statuses = append(statuses, store.RunStatus(v))
		}
	}
	for _, v := range req.GetAwaitedStates() {
		if !slices.Contains(awaited.States, v) {
			return nil, nil, status.Errorf(codes.InvalidArgument, "awaited_states: %q is not one of %s", v, strings.Join(awaited.States, ", "))
		}
	}
	if len(req.GetAwaitedStates()) > 0 {
		if req.GetStatus() == "" && len(req.GetStatuses()) == 0 {
			return nil, req.GetAwaitedStates(), nil
		}
		// A filter of "all" left statuses nil; it takes in every other status.
		if len(statuses) == 0 || slices.ContainsFunc(statuses, func(st store.RunStatus) bool { return st != store.RunRunning }) {
			return nil, nil, status.Error(codes.InvalidArgument, "awaited_states lists running runs only; leave the status filter empty or running")
		}
	}
	return statuses, req.GetAwaitedStates(), nil
}

// ListWalkRuns mirrors HTTP's GET /v1/runs?walk_id=: one team walk's runs, the
// walk's own run and every member, oldest first, a page at a time.
//
// The gate is GetRun's: the caller must be allowed to read the walk's own run,
// so another tenant's walk — or, for an isolated member, another user's — is
// the NotFound an unknown walk gets. Each member is held to the same row rule.
// The walk's own row carries its result, as on HTTP.
func (s *Server) ListWalkRuns(ctx context.Context, req *loomcyclepb.ListWalkRunsRequest) (*loomcyclepb.ListWalkRunsResponse, error) {
	walkID := req.GetWalkId()
	if !validIdent(walkID) {
		return nil, status.Error(codes.InvalidArgument, "walk_id must match [A-Za-z0-9_-]{1,128}")
	}
	if l := req.GetLimit(); l < 0 || l > 1000 {
		return nil, status.Error(codes.InvalidArgument, "limit must be 0 (the default, 100) or 1..1000")
	}
	if c := req.GetCursor(); c != "" {
		if _, _, err := store.DecodeRunCursor(c); err != nil {
			return nil, status.Error(codes.InvalidArgument, "cursor must be a next_cursor this RPC returned")
		}
	}
	if s.store == nil {
		return nil, status.Errorf(codes.NotFound, "no walk found for walk_id %q (no store configured)", walkID)
	}
	p, ok := auth.PrincipalFromContext(ctx)
	walk, err := s.store.GetRun(ctx, walkID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return nil, status.Errorf(codes.NotFound, "no walk found for walk_id %q", walkID)
		}
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	// A token limited to certain agents reads only walks in trees it could have
	// started; members are in the walk's tree, so the per-row check below
	// passes them from what the tree has already read.
	tree := s.runTree(ctx)
	if !auth.OwnedRowVisible(p, ok, walk.TenantID, walk.UserID) || !tree.ReachesKnown(runLink(walk)) {
		return nil, status.Errorf(codes.NotFound, "no walk found for walk_id %q", walkID)
	}
	queryTenant, allTenants := grpcTenantScope(ctx)
	if allTenants {
		queryTenant = ""
	}
	runs, next, err := s.store.ListRunsByWalk(ctx, queryTenant, walkID, int(req.GetLimit()), req.GetCursor())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	out := &loomcyclepb.ListWalkRunsResponse{Agents: make([]*loomcyclepb.Agent, 0, len(runs)), NextCursor: next}
	for _, r := range runs {
		if !auth.OwnedRowVisible(p, ok, r.TenantID, r.UserID) || !tree.ReachesKnown(runLink(r)) {
			continue
		}
		a := runToProto(r, s.runLive(r))
		s.fillAwaitedState(ctx, a)
		if r.ID == walkID {
			a.Result = r.Result
		}
		out.Agents = append(out.Agents, a)
	}
	return out, nil
}

// ListChildRuns mirrors HTTP's GET /v1/runs?parent_run_id=: one run's direct
// children, of every status, oldest first, a page at a time.
//
// The gate is ListWalkRuns': the caller must be allowed to read the parent
// run, so another tenant's run — or, for an isolated member, another user's —
// is the NotFound an unknown run gets. Each child is held to the same row rule.
func (s *Server) ListChildRuns(ctx context.Context, req *loomcyclepb.ListChildRunsRequest) (*loomcyclepb.ListChildRunsResponse, error) {
	parentRunID := req.GetParentRunId()
	if !validIdent(parentRunID) {
		return nil, status.Error(codes.InvalidArgument, "parent_run_id must match [A-Za-z0-9_-]{1,128}")
	}
	if l := req.GetLimit(); l < 0 || l > 1000 {
		return nil, status.Error(codes.InvalidArgument, "limit must be 0 (the default, 100) or 1..1000")
	}
	if c := req.GetCursor(); c != "" {
		if _, _, err := store.DecodeRunCursor(c); err != nil {
			return nil, status.Error(codes.InvalidArgument, "cursor must be a next_cursor this RPC returned")
		}
	}
	if s.store == nil {
		return nil, status.Errorf(codes.NotFound, "no run found for parent_run_id %q (no store configured)", parentRunID)
	}
	p, ok := auth.PrincipalFromContext(ctx)
	parent, err := s.store.GetRun(ctx, parentRunID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return nil, status.Errorf(codes.NotFound, "no run found for parent_run_id %q", parentRunID)
		}
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	// The allowlist gate as ListWalkRuns': a run's children are in its tree.
	tree := s.runTree(ctx)
	if !auth.OwnedRowVisible(p, ok, parent.TenantID, parent.UserID) || !tree.ReachesKnown(runLink(parent)) {
		return nil, status.Errorf(codes.NotFound, "no run found for parent_run_id %q", parentRunID)
	}
	queryTenant, allTenants := grpcTenantScope(ctx)
	if allTenants {
		queryTenant = ""
	}
	runs, next, err := s.store.ListChildRuns(ctx, queryTenant, parentRunID, int(req.GetLimit()), req.GetCursor())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store: %v", err)
	}
	out := &loomcyclepb.ListChildRunsResponse{Agents: make([]*loomcyclepb.Agent, 0, len(runs)), NextCursor: next}
	for _, r := range runs {
		if !auth.OwnedRowVisible(p, ok, r.TenantID, r.UserID) || !tree.ReachesKnown(runLink(r)) {
			continue
		}
		a := runToProto(r, s.runLive(r))
		s.fillAwaitedState(ctx, a)
		out.Agents = append(out.Agents, a)
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
	params := store.UsageParams{
		GroupBy: req.GetGroupBy(),
		From:    req.GetFromTime(),
		To:      req.GetToTime(),
		Bucket:  req.GetBucket(),
		Top:     int(req.GetTop()),
		TopBy:   req.GetTopBy(),
	}
	for _, f := range req.GetFilters() {
		// tenant stays the focus field below, with its authority check, as
		// on HTTP: it is never a filter.
		if f.GetDimension() == string(store.UsageByTenant) {
			return nil, status.Errorf(codes.InvalidArgument, "tenant is not a filter; use the tenant field")
		}
		if params.Filters == nil {
			params.Filters = map[string][]string{}
		}
		params.Filters[f.GetDimension()] = append(params.Filters[f.GetDimension()], f.GetValues()...)
	}
	var detailSince time.Time
	if s.usageDetailSince != nil {
		detailSince = s.usageDetailSince()
	}
	q, err := store.ParseUsageQuery(params, detailSince)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
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
		GroupBy: make([]string, len(q.GroupBy)),
		Rows:    make([]*loomcyclepb.UsageAggregate, 0, len(rows)),
		Bucket:  string(q.Bucket),
	}
	for i, g := range q.GroupBy {
		out.GroupBy[i] = string(g)
	}
	if !detailSince.IsZero() {
		out.DetailSince = detailSince.UTC().Format(time.RFC3339)
	}
	for _, r := range rows {
		a := &loomcyclepb.UsageAggregate{
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
			Purpose:             r.Purpose,
			AudioSeconds:        r.AudioSeconds,
			Tier:                r.Tier,
			UserTier:            r.UserTier,
			Agent:               r.AgentID,
			Class:               r.Class,
			Other:               r.Other,
			InputChars:          r.InputChars,
		}
		if !r.Period.IsZero() {
			a.Period = r.Period.UTC().Format(time.RFC3339)
		}
		out.Rows = append(out.Rows, a)
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
	// folds into the same opaque NotFound (session ids are not secret). An
	// isolated member reads only its own sessions, as tenantStore.GetSession
	// confines it on HTTP.
	// A token limited to certain agents reads only sessions whose runs are all
	// in trees it could have started (sessionReachable); any other is the same
	// NotFound.
	if p, ok := auth.PrincipalFromContext(ctx); !auth.OwnedRowVisible(p, ok, sess.TenantID, sess.UserID) || !s.sessionReachable(ctx, sess) {
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
		Agent:         r.Agent,
		UserId:        r.UserID,
		ParentAgentId: r.ParentAgentID,
		ParentRunId:   r.ParentRunID,
		Status:        string(r.Status),
		StartedAt:     timestamppb.New(r.StartedAt),
		StopReason:    r.StopReason,
		Retention:     r.Retention,
		Error:         r.ErrorMsg,
		Usage: &loomcyclepb.AgentUsage{
			InputTokens:         int64(r.InputTokens),
			OutputTokens:        int64(r.OutputTokens),
			CacheCreationTokens: int64(r.CacheCreationTokens),
			CacheReadTokens:     int64(r.CacheReadTokens),
			Model:               r.Model,
			Provider:            r.Provider,
		},
		Live:          live,
		Interactive:   r.Interactive,
		ReplicaId:     r.ReplicaID,
		ParentContext: parentContextToProto(r.ParentContext),
	}
	if !r.CompletedAt.IsZero() {
		out.CompletedAt = timestamppb.New(r.CompletedAt)
	}
	if !r.LastHeartbeatAt.IsZero() {
		out.LastHeartbeatAt = timestamppb.New(r.LastHeartbeatAt)
	}
	return out
}

// fillAwaitedState sets what a running run is blocked on — the rule the HTTP
// run read applies, so the two transports answer alike for the same run. It
// needs the run's events, which runToProto does not have.
func (s *Server) fillAwaitedState(ctx context.Context, a *loomcyclepb.Agent) {
	if s.store == nil || a.GetStatus() != string(store.RunRunning) {
		return
	}
	a.AwaitedState, a.AwaitedOn = awaited.ForRun(ctx, s.store, a.GetRunId())
}

// fillQueuePosition sets where a queued run waits. Only a queued run asks: the
// position lives in this replica's memory, not on the row, and a run that has
// started has none.
func (s *Server) fillQueuePosition(a *loomcyclepb.Agent) {
	if s.queuePosition == nil || a.GetStatus() != string(store.RunQueued) {
		return
	}
	if pos, reason, ok := s.queuePosition(a.GetAgentId()); ok {
		a.QueuePosition, a.QueueReason = int32(pos), reason
	}
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
		if err := enforceOp(authedCtx, info.FullMethod, req); err != nil {
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
		ctx = authz.WithTransport(authz.WithMode(ctx, s.aclMode), "grpc")
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
//
// A substrate:tenant entry is also the member path (RFC CB, RFC EZ Q1): a
// non-isolated principal without the scope reaches the RPC when its HTTP twin
// is on the HTTP member path — see enforceScope and internal/api/memberpath.
var grpcConsumerScopes = map[string]string{
	"Run":              auth.ScopeRunsCreate,
	"Continue":         auth.ScopeRunsCreate,
	"RunInput":         auth.ScopeRunsCreate, // RFC AI — steering injects instructions (mutation)
	"RetuneRun":        auth.ScopeRunsCreate, // changes a run's settings (mutation; mirrors POST .../retune)
	"CancelTurn":       auth.ScopeRunsCreate, // RFC BH — turn-cancel is a run mutation (mirrors POST .../cancel)
	"ReviewRun":        auth.ScopeRunsCreate, // RFC DJ — a verdict on a held run (mirrors POST .../review)
	"ResolveInterrupt": auth.ScopeRunsCreate, // RFC BH — resolve/decline steers the run (mirrors POST .../resolve)
	"StreamRun":        auth.ScopeRunsRead,   // RFC AI — pure read tail (mirrors handleRunStream)
	"SpawnRunBatch":    auth.ScopeRunsCreate,
	"CompactRun":       auth.ScopeRunsCreate,
	// Deleting a run tree removes stored records: its own scope, as on HTTP.
	// Not substrate:tenant-or-less by implication for a member or an isolated
	// user; the connector checks it again and requires read access to the root.
	"DeleteRunTree": auth.ScopeRunsDelete,
	// RFC DI configured runs: creating, editing, starting and discarding a
	// draft are run writes, the scope that creates a run (mirrors HTTP).
	"CreateConfiguredRun": auth.ScopeRunsCreate,
	"UpdateConfiguredRun": auth.ScopeRunsCreate,
	"StartConfiguredRun":  auth.ScopeRunsCreate,
	"DeleteConfiguredRun": auth.ScopeRunsCreate,
	"ReplaySession":       auth.ScopeRunsCreate,
	// A decision asked outside a run, and the models it may name: the scope that
	// creates a run, which is what a caller would otherwise start to get one
	// (mirrors POST /v1/_decide and GET /v1/_decide/models).
	"Decide":              auth.ScopeRunsCreate,
	"ListDecisionModels":  auth.ScopeRunsCreate,
	"CancelAgent":         auth.ScopeRunsCreate,
	"GetTranscript":       auth.ScopeRunsRead,
	"GetAgent":            auth.ScopeRunsRead,
	"GetRun":              auth.ScopeRunsRead,
	"ListUserAgents":      auth.ScopeRunsRead,
	"ListWalkRuns":        auth.ScopeRunsRead,
	"ListChildRuns":       auth.ScopeRunsRead,
	"StreamUserRunStates": auth.ScopeRunsRead,
	"PublishChannel":      auth.ScopeChannelPublish,
	"AckChannel":          auth.ScopeChannelPublish,
	"SubscribeChannel":    auth.ScopeChannelRead,
	"PeekChannel":         auth.ScopeChannelRead,
	"ListChannels":        auth.ScopeChannelRead,
	// The fan-in / fan-out twins of POST /v1/_channels/_await and _broadcast,
	// which ask substrate:tenant (the member path admits a member). Unlike the
	// per-channel RPCs above they have no /v1/users/{user_id}/channels twin, so
	// an isolated user is refused them on HTTP; a channel scope here would let
	// it through on gRPC. Unmapped, they defaulted to substrate:admin.
	"AwaitChannels":     auth.ScopeTenant,
	"BroadcastChannels": auth.ScopeTenant,
	// RFC AV: the usage/cost report is tenant-readable (the handler tenant-scopes
	// the aggregation), mirroring the HTTP /v1/_usage ScopeTenant gate.
	"UsageReport": auth.ScopeTenant,
	// Instance configuration. "" = ANY authenticated principal, which is the same
	// gate GET /v1/config gets from requiredScopeFor's GET default — the report is
	// self-narrowing (the handler emits only what the caller's own scopes allow),
	// so a scope floor would deny a caller a view it is entitled to. It MUST be
	// listed: an unmapped RPC defaults to substrate:admin, which would make the
	// gRPC surface stricter than the HTTP one for no reason.
	"Config": "",
	// RFC AW: token-budget management. The handler tenant-scopes reads + confines
	// writes to the caller's own tenant (operator-global + cross-tenant stay
	// admin-only), mirroring the HTTP /v1/_limits ScopeTenant gate.
	"TokenLimit": auth.ScopeTenant,
	// The timed override of an agent's no-trace mode: an admin for any tenant,
	// a tenant operator for its own. Its HTTP routes are carved out of the
	// member path, so a member is refused here too; the connector checks the
	// caller again.
	"RetentionOverride": auth.ScopeTenant,

	// RFC BL P5 erasure. ScopeTenant, mirroring the HTTP gate: answering a
	// data-subject request is the tenant operator's job, and both RPCs confine
	// themselves to the caller's own tenant (there is no wire tenant field).
	// The destructive half is guarded by dry_run defaulting true and confirm
	// having to equal the subject — not by requiring a broader scope, which an
	// admin token would satisfy anyway.
	// Directory reads. users/inspect are ScopeTenant — everything they aggregate is
	// already readable by this principal, and both take the tenant from the caller
	// with no wire field. tenants is ScopeAdmin AND re-checked in the handler,
	// because the LIST is cross-tenant information.
	"DirectoryUsers":   auth.ScopeTenant,
	"DirectoryInspect": auth.ScopeTenant,
	"DirectoryTenants": auth.ScopeAdmin,

	"ErasureReport":  auth.ScopeTenant,
	"ErasureExecute": auth.ScopeTenant,
	// RFC AF: the tenant-confined substrate plane — the 8 def families + hook
	// management. ScopeTenant (substrate:admin still satisfies). substrateGRPCCtx
	// stamps the principal's authoritative tenant on the def-tools, and the hook
	// connector stamps + tenant-scopes register/list/delete, so a tenant operator
	// authors ONLY its own surface. OperatorTokenDef is DELIBERATELY absent — it
	// defaults to ScopeAdmin (token minting stays operator-only), mirroring the
	// HTTP /v1/_operatortokendef exclusion.
	"AgentDef":          auth.ScopeTenant,
	"SkillDef":          auth.ScopeTenant,
	"MCPServerDef":      auth.ScopeTenant,
	"ScheduleDef":       auth.ScopeTenant,
	"A2AServerCardDef":  auth.ScopeTenant,
	"A2AAgentDef":       auth.ScopeTenant,
	"WebhookDef":        auth.ScopeTenant,
	"MemoryBackendDef":  auth.ScopeTenant,
	"DocumentSourceDef": auth.ScopeTenant,
	"ClientDef":         auth.ScopeTenant, // RFC FL app clients; confined like /v1/_clientdef
	"VolumeDef":         auth.ScopeTenant,
	// RFC AP TeamDef — team-workflow substrate. Same ScopeTenant posture as the
	// other def families; the HTTP /v1/_teamdef route is tenant-confined too.
	"TeamDef": auth.ScopeTenant,
	// A team's own channels, read by team and local name: the posture of the
	// HTTP routes under /v1/_teamdef/{team}/channels.
	"ListTeamChannels":   auth.ScopeTenant,
	"PeekTeamChannel":    auth.ScopeTenant,
	"ReleaseTeamChannel": auth.ScopeTenant,
	// HookDef — reusable hook definitions, authored like an AgentDef.
	"HookDef": auth.ScopeTenant,
	// RFC AL Path VFS + RFC AK Document — scope-aware, tenant-isolated tools
	// lifted to the wire. Same ScopeTenant posture as VolumeDef.
	"Path":     auth.ScopeTenant,
	"Document": auth.ScopeTenant,
	// RFC AR credential store. Route floor ScopeTenant (tenant operator + admin);
	// RFC CN additionally admits an isolated substrate:user caller via the
	// enforceScope self-service carve-out, confined to scope=user by the handler.
	"CredentialDef": auth.ScopeTenant,
	// RFC EP Crypto outside a run, mirroring POST /v1/_crypto. ScopeTenant,
	// and its route is carved out of the member path; it is deliberately NOT in
	// the credential self-service carve-out (a caller who can decrypt with the
	// tenant's keys reads whatever the tenant sealed) nor on the run plane.
	"Crypto": auth.ScopeTenant,
	// RFC BE History — scope-aware chat browse/annotate. ScopeTenant route
	// floor (a tenant operator may call it; a member on the member path); the
	// cross-tenant `global` scope is admin-gated inside the tool, and a member
	// is capped to its own chats (substrateGRPCCtx). The HTTP /v1/_history
	// route also admits an isolated user (runs:read); this RPC does not.
	"History": auth.ScopeTenant,
	// The Memory tool outside a run (RFC CD Part D), tenant-confined like the
	// /v1/_memory/* routes it parallels: scope and tenant come from the
	// principal (substrateGRPCUserCtx), never the wire. Unmapped, it defaulted
	// to substrate:admin, against its proto contract.
	"Memory": auth.ScopeTenant,
	// Evaluation outside a run: runs:read, as POST /v1/_evaluation and the MCP
	// `evaluation` tool. The floor admits an isolated user; which ops it may use
	// is tools.OffRunEvaluationPolicy (substrateGRPCCtx), not this gate.
	"Evaluation": auth.ScopeRunsRead,
	// RFC EZ access-control management, gated as the HTTP routes are: groups,
	// lists and bindings are the tenant operator's (the routes' ScopeTenant +
	// member carve-out, which the member path here reads through the twins),
	// and any authenticated principal may ask the authz questions about
	// itself ("" — the shared implementation confines a non-operator to its
	// own subject). The shared implementation re-checks every one of these.
	"GroupDef":   auth.ScopeTenant,
	"AclDef":     auth.ScopeTenant,
	"AclBinding": auth.ScopeTenant,
	"Authz":      "",
}

// requiredScopeForRPC returns the scope a caller must hold for fullMethod.
// Unknown / unmapped methods → substrate:admin (deny-by-default). This is
// the gRPC analogue of the HTTP requiredScopeFor: PR2 stamped the principal
// over gRPC but never enforced per-RPC scope, so any valid token could reach
// every admin RPC (incl. OperatorTokenDef mint) — closed here.
// grpcRunPlaneRPCs is the run plane on this transport: the whole of what a
// credential with an agent allowlist may call. Decide is deliberately absent —
// it reaches a model with no agent, so no allowlist can admit it — and so is
// every definition, channel, memory and admin RPC.
var grpcRunPlaneRPCs = map[string]bool{
	"Run": true, "Continue": true, "SpawnRunBatch": true,
	"RunInput": true, "RetuneRun": true, "CancelTurn": true, "ReviewRun": true,
	"ResolveInterrupt": true, "CompactRun": true, "CancelAgent": true,
	"CreateConfiguredRun": true, "UpdateConfiguredRun": true,
	"StartConfiguredRun": true, "DeleteConfiguredRun": true,
	"ReplaySession": true,
	"StreamRun":     true, "GetTranscript": true, "GetAgent": true, "GetRun": true,
	"ListUserAgents": true, "ListWalkRuns": true, "ListChildRuns": true,
	"StreamUserRunStates": true,
	"Config":              true,
}

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
	// A credential limited to certain agents is held to the run plane, however
	// wide its scopes (auth.Restrictions.RunPlaneOnly). Default-deny: an RPC
	// added later is refused to such a credential until it is listed.
	if p.Restrictions.RunPlaneOnly() && !grpcRunPlaneRPCs[strings.TrimPrefix(fullMethod, grpcMethodPrefix)] {
		return status.Error(codes.PermissionDenied, "this token is limited to starting and following runs of its allowed agents")
	}
	// RFC FA D16: a user-management credential reaches only the HTTP user
	// routes; no RPC is on its list.
	if auth.IsAuthManager(p) {
		return status.Error(codes.PermissionDenied, "an "+auth.ScopeAuthManage+" token reaches only the user-management HTTP routes")
	}
	// RFC FA P6: a SCIM credential reaches only /scim/v2 over HTTP.
	if auth.IsSCIMProvisioner(p) {
		return status.Error(codes.PermissionDenied, "a "+auth.ScopeSCIMProvision+" token reaches only the /scim/v2 HTTP routes")
	}
	required := requiredScopeForRPC(fullMethod)
	if required != "" && !auth.HasScope(p.Scopes, required) {
		// RFC CB member path, as on HTTP (RFC EZ Q1): a NON-ISOLATED principal
		// without substrate:tenant reaches the RPC when its HTTP twin is on the
		// HTTP member path. The answer comes from the twin route through the
		// one carve-out list both transports read (memberpath), so the two
		// cannot drift; an RPC whose operations differ there is held to each
		// call's own operation by enforceOp. Every handler behind it confines
		// the caller to its own tenant, as the HTTP handlers do.
		memberOK := memberpath.RPC(strings.TrimPrefix(fullMethod, grpcMethodPrefix), required) && !auth.IsIsolated(p, ok)
		// RFC CN: the credential-store RPC admits an isolated substrate:user caller
		// for self-service of its OWN scope=user tokens — the handler
		// (credential.ConstrainToUserScope) confines it to scope=user before the
		// tool. tenant/agent scope still needs substrate:tenant.
		selfOK := isCredentialSelfServiceRPC(fullMethod) && auth.IsIsolated(p, ok)
		if !(memberOK || selfOK) {
			return status.Errorf(codes.PermissionDenied, "insufficient scope: %s required", required)
		}
	}
	// RFC EY: the RPC's type-level ACL question, after the scope gate.
	return enforceACL(ctx, p, fullMethod)
}

// enforceACL asks the engine the RPC's type-level question under the mode
// stamped on ctx (authz.Evaluate). A refusal is PermissionDenied with the
// `acl_denied` text; an enforce-mode request with no compiled set is
// Unavailable, so the client retries rather than reads it as forbidden.
func enforceACL(ctx context.Context, p auth.Principal, fullMethod string) error {
	surface, governed := authz.GRPCSurface(strings.TrimPrefix(fullMethod, grpcMethodPrefix))
	if !governed {
		return nil
	}
	return enforceACLSurface(ctx, p, surface)
}

// enforceOp holds a call of an RPC that carries its operation in the request
// (memberpath.ByOp — TokenLimit's list/set/delete) to that operation's own HTTP
// twin: a member admitted to the RPC because one operation is on the member
// path is refused the others, and every caller is asked the twin route's own
// ACL question (the RPC-level question is the family's weakest verb). An op
// the twin table does not know is refused to a member and left to the handler
// for anyone else. Streaming RPCs carry no op and are never op-keyed.
func enforceOp(ctx context.Context, fullMethod string, req any) error {
	rpc := strings.TrimPrefix(fullMethod, grpcMethodPrefix)
	if !memberpath.ByOp(rpc) {
		return nil
	}
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return nil
	}
	op := ""
	if r, isOp := req.(interface{ GetOp() string }); isOp {
		op = r.GetOp()
	}
	twin, known := memberpath.OpTwin(rpc, op)
	if required := requiredScopeForRPC(fullMethod); !auth.HasScope(p.Scopes, required) {
		if !known || !memberpath.Route(required, twin.Method, twin.Path) {
			return status.Errorf(codes.PermissionDenied, "insufficient scope: %s required", required)
		}
	}
	if !known {
		return nil
	}
	surface, governed := authz.HTTPSurface(twin.Method, twin.Path)
	if !governed {
		return nil
	}
	return enforceACLSurface(ctx, p, surface)
}

// enforceACLSurface asks the engine one surface's question for p.
func enforceACLSurface(ctx context.Context, p auth.Principal, surface authz.Surface) error {
	res := authz.EvaluateSurface(ctx, p.Effective, surface, true)
	switch {
	case res.Unavailable:
		return status.Error(codes.Unavailable, "acl_unavailable: access control could not be evaluated; retry")
	case res.Diverged:
		slog.Warn("authz: acl_shadow_diverged", "tenant", p.TenantID, "subject", p.Subject, "action", surface.Action,
			"resource", surface.Type+":"+surface.Name, "outcome", res.Decision.Outcome.String(), "transport", "grpc")
	case !res.Allowed:
		slog.Warn("authz: acl_denied", "tenant", p.TenantID, "subject", p.Subject, "action", surface.Action,
			"resource", surface.Type+":"+surface.Name, "outcome", res.Decision.Outcome.String(), "transport", "grpc")
		return status.Errorf(codes.PermissionDenied, "acl_denied: %s %s:%s", surface.Action, surface.Type, surface.Name)
	}
	return nil
}

// RPCAdmits reports whether an authenticated principal p gets past the legacy
// gates of rpc for a call of operation op ("" for an RPC that carries none):
// the scope gate with its member and self-service paths, the run-plane
// restriction and enforceOp — under compat with no access-control rows, so the
// ACL adds nothing. It is what the cross-transport parity test
// (internal/api/acltest) compares with the HTTP twin's http.RouteAdmits; the
// interceptors run the same functions.
func RPCAdmits(p auth.Principal, rpc, op string) bool {
	p.Effective = nil
	ctx := authz.WithMode(auth.WithPrincipal(context.Background(), p), authz.ModeCompat)
	fullMethod := grpcMethodPrefix + rpc
	if enforceScope(ctx, fullMethod) != nil {
		return false
	}
	return enforceOp(ctx, fullMethod, opRequest(op)) == nil
}

// opRequest is a request carrying only an op, for RPCAdmits.
type opRequest string

func (o opRequest) GetOp() string { return string(o) }

// isCredentialSelfServiceRPC reports whether fullMethod is the RFC AR credential
// store RPC — the one RFC CN opens to an isolated substrate:user caller (see
// enforceScope). The handler confines such a caller to scope=user.
func isCredentialSelfServiceRPC(fullMethod string) bool {
	return strings.TrimPrefix(fullMethod, grpcMethodPrefix) == "CredentialDef"
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
	// The same open-mode decision authenticate makes: an admin operator token
	// turns auth on with no legacy secret, so the secret alone misreports it.
	authOn := s.authToken != ""
	if s.authConfigured != nil {
		authOn = s.authConfigured(context.Background())
	}
	slog.Info("loomcycle gRPC listening on "+addr, "auth", authOn)
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
	if err := validateAdmission(req.GetAdmission()); err != nil {
		return err
	}
	added, err := hooksFromProto(req.GetHooksJson())
	if err != nil {
		return err
	}
	in := runInputFromProto(runInputProtoArgs{
		Agent:            req.GetAgent(),
		SessionID:        req.GetSessionId(),
		Segments:         req.GetSegments(),
		Tools:            req.GetTools(),
		AllowedHosts:     req.GetAllowedHosts(),
		WebSearchFilter:  req.GetWebSearchFilter(),
		UserID:           req.GetUserId(),
		AgentID:          req.GetAgentId(),
		TenantID:         req.GetTenantId(),
		UserTier:         req.GetUserTier(),
		UserBearer:       req.GetUserBearer(),
		UserCredentials:  req.GetUserCredentials(), // v1.x RFC F
		Sampling:         samplingFromProto(req.GetSampling()),
		ToolChoice:       toolChoiceFromProto(req.GetToolChoice()),
		OutputFormat:     outputFormatFromProto(req.GetOutputFormat()),
		Routing:          routingFromProto(req.GetRouting()),
		Compaction:       compactionFromProto(req.GetCompaction()),
		Interactive:      req.GetInteractive(), // RFC AI
		Review:           req.GetReview(),      // RFC DJ — hold for an operator's verdict
		ReviewTTLSeconds: int(req.GetReviewTtlSeconds()),
		Metadata:         metadataFromProto(req.GetMetadata()),
		Context:          contextFromProto(req.GetContext()),
		ParentContext:    parentContextFromProto(req.GetParentContext()),
		Interruption:     interruptionFromProto(req.GetInterruption()),
		MaxContextTokens: int(req.GetMaxContextTokens()), // RFC CJ per-run context-window override
		IdempotencyKey:   req.GetIdempotencyKey(),
		MaxWallSeconds:   int(req.GetMaxWallSeconds()),
		Retention:        req.GetRetention(),
		Admission:        req.GetAdmission(),
		// RFC DC per-run overrides. One helper for all three call sites, so a
		// typed gRPC caller and an HTTP one get the same answer from the same
		// validation — and so adding a field means editing one place.
		Model:                 req.GetModel(),
		Provider:              req.GetProvider(),
		Tier:                  req.GetTier(),
		Effort:                req.GetEffort(),
		MaxTokens:             int(req.GetMaxTokens()),
		MaxIterations:         int(req.GetMaxIterations()),
		UnboundedIterations:   req.UnboundedIterations,
		MaxConcurrentChildren: int(req.GetMaxConcurrentChildren()),
		RetryAttempts:         int32PtrToInt(req.RetryAttempts),
		MemoryInjectMaxTokens: int32PtrToInt(req.MemoryInjectMaxTokens),
		MemoryIndexMaxBytes:   int32PtrToInt(req.MemoryIndexMaxBytes),
		InjectToolGuide:       req.InjectToolGuide,
	})
	in.Hooks, in.ToolHooks = added.Hooks, added.ToolHooks
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
	added, err := hooksFromProto(req.GetHooksJson())
	if err != nil {
		return err
	}
	in := runInputFromProto(runInputProtoArgs{
		// Agent + TenantID + UserID omitted — server inherits from the
		// existing session per the HTTP wire's messagesRequest contract.
		SessionID:        req.GetSessionId(),
		Segments:         req.GetSegments(),
		Tools:            req.GetTools(),
		AllowedHosts:     req.GetAllowedHosts(),
		WebSearchFilter:  req.GetWebSearchFilter(),
		AgentID:          req.GetAgentId(),
		UserTier:         req.GetUserTier(),
		UserBearer:       req.GetUserBearer(),
		UserCredentials:  req.GetUserCredentials(), // v1.x RFC F
		Sampling:         samplingFromProto(req.GetSampling()),
		ToolChoice:       toolChoiceFromProto(req.GetToolChoice()),
		OutputFormat:     outputFormatFromProto(req.GetOutputFormat()),
		Routing:          routingFromProto(req.GetRouting()),
		Compaction:       compactionFromProto(req.GetCompaction()),
		Interactive:      req.GetInteractive(), // RFC AI
		Review:           req.GetReview(),      // RFC DJ — hold for an operator's verdict
		ReviewTTLSeconds: int(req.GetReviewTtlSeconds()),
		Metadata:         metadataFromProto(req.GetMetadata()),
		Context:          contextFromProto(req.GetContext()),
		ParentContext:    parentContextFromProto(req.GetParentContext()),
		Interruption:     interruptionFromProto(req.GetInterruption()),
		MaxContextTokens: int(req.GetMaxContextTokens()), // RFC CJ per-continuation context-window override
		// RFC DC per-run overrides. One helper for all three call sites, so a
		// typed gRPC caller and an HTTP one get the same answer from the same
		// validation — and so adding a field means editing one place.
		Model:                 req.GetModel(),
		Provider:              req.GetProvider(),
		Tier:                  req.GetTier(),
		Effort:                req.GetEffort(),
		MaxTokens:             int(req.GetMaxTokens()),
		MaxIterations:         int(req.GetMaxIterations()),
		UnboundedIterations:   req.UnboundedIterations,
		MaxConcurrentChildren: int(req.GetMaxConcurrentChildren()),
		RetryAttempts:         int32PtrToInt(req.RetryAttempts),
		MemoryInjectMaxTokens: int32PtrToInt(req.MemoryInjectMaxTokens),
		MemoryIndexMaxBytes:   int32PtrToInt(req.MemoryIndexMaxBytes),
		InjectToolGuide:       req.InjectToolGuide,
	})
	in.Hooks, in.ToolHooks = added.Hooks, added.ToolHooks
	// Its session is held in every replica, as the HTTP continuation routes
	// hold it: two replicas must not take turns on one session at once.
	in.SessionLockAcrossReplicas = true
	return s.driveStream(stream.Context(), stream, in)
}

// SpawnRunBatch is the RFC Y external fan-out: spawn N fresh runs
// concurrently and return the combined index-aligned envelope — once they
// settle (mode "join") or once they have started (mode "detach"). Dispatches
// to connector.SpawnRunBatch, which bounds concurrency on the per-user
// admission gate and captures per-child failures in-envelope. The RPC only
// errors on a malformed batch (over-cap / unsupported mode / timeout_ms with
// detach). Mirrors POST /v1/runs:batch.
func (s *Server) SpawnRunBatch(ctx context.Context, req *loomcyclepb.BatchSpawnRequest) (*loomcyclepb.BatchSpawnResult, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "SpawnRunBatch requires a connector; this Server was constructed without one")
	}
	spawns := make([]connector.SpawnRunRequest, 0, len(req.GetSpawns()))
	for _, sp := range req.GetSpawns() {
		if errMsg, ok := connector.ValidateUserCredentialsMap(sp.GetUserCredentials()); !ok {
			return nil, status.Error(codes.InvalidArgument, errMsg)
		}
		if err := validateAdmission(sp.GetAdmission()); err != nil {
			return nil, err
		}
		spawn := spawnRequestFromProto(sp)
		added, err := hooksFromProto(sp.GetHooksJson())
		if err != nil {
			return nil, err
		}
		spawn.Hooks, spawn.ToolHooks = added.Hooks, added.ToolHooks
		spawns = append(spawns, spawn)
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

// DeleteRunTree mirrors DELETE /v1/runs/{run_id}/tree and DELETE
// /v1/sessions/{session_id}?cascade=children: a run (or every run of a
// session) with its descendants and what is stored about them. The
// authorization, the walk and the delete are the connector's, shared with
// HTTP; the interceptor has already required runs:delete.
func (s *Server) DeleteRunTree(ctx context.Context, req *loomcyclepb.DeleteRunTreeRequest) (*loomcyclepb.DeleteRunTreeResult, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "DeleteRunTree requires a connector; this Server was constructed without one")
	}
	res, err := s.connector.DeleteRunTree(ctx, connector.DeleteRunTreeRequest{
		RunID: req.GetRunId(), SessionID: req.GetSessionId(),
	})
	if err != nil {
		return nil, deleteRunTreeErrToStatus(err)
	}
	out := &loomcyclepb.DeleteRunTreeResult{
		RunId:       res.RunID,
		SessionId:   res.SessionID,
		DeletedRuns: int32(res.DeletedRuns),
		Counts:      make(map[string]int32, len(res.Counts)),
		Kept:        res.Kept,
	}
	for table, n := range res.Counts {
		out.Counts[table] = int32(n)
	}
	for _, nr := range res.NotReached {
		item := &loomcyclepb.RunTreeNotReached{Name: nr.Name, CountIsFloor: nr.CountIsFloor, Reason: nr.Reason}
		if nr.Count != nil {
			n := int32(*nr.Count)
			item.Count = &n
		}
		out.NotReached = append(out.NotReached, item)
	}
	return out, nil
}

// deleteRunTreeErrToStatus maps the connector's refusals by their HTTP status,
// so the two transports cannot disagree about which refusal is which.
func deleteRunTreeErrToStatus(err error) error {
	var hse interface{ HTTPStatus() int }
	if errors.As(err, &hse) {
		switch hse.HTTPStatus() {
		case nethttp.StatusBadRequest:
			return status.Error(codes.InvalidArgument, err.Error())
		case nethttp.StatusForbidden:
			return status.Error(codes.PermissionDenied, err.Error())
		case nethttp.StatusConflict, nethttp.StatusUnprocessableEntity:
			return status.Error(codes.FailedPrecondition, err.Error())
		case nethttp.StatusServiceUnavailable:
			return status.Error(codes.Unavailable, err.Error())
		}
	}
	return status.Error(codes.Internal, err.Error())
}

// ReplaySession mirrors POST /v1/sessions/{id}/replay (RFC BJ Phase 4): copy a
// source session's transcript into a new session bound to a target agent.
func (s *Server) ReplaySession(ctx context.Context, req *loomcyclepb.ReplaySessionRequest) (*loomcyclepb.ReplaySessionResult, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "ReplaySession requires a connector; this Server was constructed without one")
	}
	if !validIdent(req.GetSessionId()) {
		return nil, status.Error(codes.InvalidArgument, "session_id must match [A-Za-z0-9_-]{1,128}")
	}
	if strings.TrimSpace(req.GetAgent()) == "" {
		return nil, status.Error(codes.InvalidArgument, "agent is required")
	}
	res, err := s.connector.ReplaySession(ctx, connector.ReplaySessionRequest{
		SourceSessionID: req.GetSessionId(),
		Agent:           strings.TrimSpace(req.GetAgent()),
		Compress:        req.GetCompress(),
	})
	if err != nil {
		return nil, replayErrToStatus(err)
	}
	return &loomcyclepb.ReplaySessionResult{
		NewSessionId: res.NewSessionID,
		SeedRunId:    res.SeedRunID,
		EventsCopied: int32(res.EventsCopied),
		Compacted:    res.Compacted,
	}, nil
}

// replayErrToStatus maps a ReplaySession error (carrying an HTTPStatus) to a gRPC
// status. Adds InvalidArgument (400) to the compact mapping — replay validates
// the target agent name + empty-source in the core.
func replayErrToStatus(err error) error {
	var hse interface{ HTTPStatus() int }
	if errors.As(err, &hse) {
		switch hse.HTTPStatus() {
		case nethttp.StatusBadRequest:
			return status.Error(codes.InvalidArgument, err.Error())
		case nethttp.StatusNotFound:
			return status.Error(codes.NotFound, err.Error())
		case nethttp.StatusConflict:
			return status.Error(codes.FailedPrecondition, err.Error())
		case nethttp.StatusServiceUnavailable, nethttp.StatusBadGateway:
			return status.Error(codes.Unavailable, err.Error())
		case nethttp.StatusTooManyRequests:
			return status.Error(codes.ResourceExhausted, err.Error())
		case nethttp.StatusForbidden:
			return status.Error(codes.PermissionDenied, err.Error())
		}
	}
	return status.Error(codes.Internal, err.Error())
}

// spawnRequestFromProto maps a proto RunRequest to a connector.SpawnRunRequest
// for the fan-out path. session_id is carried through but batch children are
// forced fresh by SpawnRunBatch. parent_context / metadata are not on the proto
// RunRequest (a pre-existing gRPC gap) so they stay nil.
// hooksFromProto reads a request's hooks_json. A malformed value is refused:
// dropping it would start the run without gates its caller asked for.
func hooksFromProto(b []byte) (hooks.Additions, error) {
	var a hooks.Additions
	if len(b) == 0 {
		return a, nil
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return hooks.Additions{}, status.Errorf(codes.InvalidArgument, "hooks_json: %v", err)
	}
	return a, nil
}

// validateAdmission refuses an `admission` value the runner does not know.
// Refused here rather than passed on: an unknown value read as the default
// would queue a run whose caller asked never to wait.
func validateAdmission(v string) error {
	if !runner.ValidAdmission(v) {
		return status.Errorf(codes.InvalidArgument, "admission must be %q or %q, got %q",
			runner.AdmissionWait, runner.AdmissionReject, v)
	}
	return nil
}

func spawnRequestFromProto(req *loomcyclepb.RunRequest) connector.SpawnRunRequest {
	r := connector.SpawnRunRequest{
		Agent:            req.GetAgent(),
		SessionID:        req.GetSessionId(),
		TenantID:         req.GetTenantId(),
		Segments:         segmentsFromProto(req.GetSegments()),
		Tools:            req.GetTools(),
		WebSearchFilter:  req.GetWebSearchFilter(),
		UserID:           req.GetUserId(),
		AgentID:          req.GetAgentId(),
		UserTier:         req.GetUserTier(),
		UserBearer:       req.GetUserBearer(),
		UserCredentials:  req.GetUserCredentials(),
		Sampling:         samplingFromProto(req.GetSampling()),
		ToolChoice:       toolChoiceFromProto(req.GetToolChoice()),
		OutputFormat:     outputFormatFromProto(req.GetOutputFormat()),
		Routing:          routingFromProto(req.GetRouting()),
		Compaction:       compactionFromProto(req.GetCompaction()),
		MaxContextTokens: int(req.GetMaxContextTokens()), // RFC CJ per-run context-window override
		IdempotencyKey:   req.GetIdempotencyKey(),
		MaxWallSeconds:   int(req.GetMaxWallSeconds()),
		Retention:        req.GetRetention(),
		Admission:        req.GetAdmission(),
		// The same four Run maps: a configured run or a batch child built from
		// this request must not silently lose them
		// (TestSpawnRequestFromProto_MapsEveryFieldRunMaps guards the set).
		Metadata:      metadataFromProto(req.GetMetadata()),
		Context:       contextFromProto(req.GetContext()),
		ParentContext: parentContextFromProto(req.GetParentContext()),
		Interruption:  interruptionFromProto(req.GetInterruption()),
		// RFC DC per-run overrides. One helper for all three call sites, so a
		// typed gRPC caller and an HTTP one get the same answer from the same
		// validation — and so adding a field means editing one place.
		Model:                 req.GetModel(),
		Provider:              req.GetProvider(),
		Tier:                  req.GetTier(),
		Effort:                req.GetEffort(),
		MaxTokens:             int(req.GetMaxTokens()),
		MaxIterations:         int(req.GetMaxIterations()),
		UnboundedIterations:   req.UnboundedIterations,
		MaxConcurrentChildren: int(req.GetMaxConcurrentChildren()),
		RetryAttempts:         int32PtrToInt(req.RetryAttempts),
		MemoryInjectMaxTokens: int32PtrToInt(req.MemoryInjectMaxTokens),
		MemoryIndexMaxBytes:   int32PtrToInt(req.MemoryIndexMaxBytes),
		InjectToolGuide:       req.InjectToolGuide,
	}
	if req.GetReview() {
		review := true
		r.Review = &review
	}
	r.ReviewTTLSeconds = int(req.GetReviewTtlSeconds())
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

		Deduplicated: r.Deduplicated,
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
			payload := agentFrameJSON(agentID, runID, sessionID, parentAgentID, false)
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
	if dup := (*runner.DuplicateRunError)(nil); errors.As(runErr, &dup) {
		return s.streamExistingRun(ctx, stream, dup)
	}
	if sendErr != nil {
		// Stream broke mid-run — the runner kept going (correct, for
		// transcript persistence), but we surface to the client.
		return status.Errorf(codes.Canceled, "stream send failed: %v", sendErr)
	}
	return mapRunnerErr(runErr)
}

// streamExistingRun answers a Run whose idempotency_key a run already held:
// nothing was started, and the stream carries that run instead. It opens with
// the same `session` and `agent` frames a fresh run does — the agent frame
// marked deduplicated — and then the run's events from its first, ending when
// the run does. The client leaving ends the stream and nothing else: this
// request did not start the run.
func (s *Server) streamExistingRun(ctx context.Context, stream runStreamSink, dup *runner.DuplicateRunError) error {
	if s.connector == nil {
		return status.Error(codes.Unavailable, "connector not wired")
	}
	if err := stream.Send(&loomcyclepb.Event{Type: "session", Text: dup.SessionID}); err != nil {
		return status.Errorf(codes.Canceled, "stream send failed: %v", err)
	}
	if err := stream.Send(&loomcyclepb.Event{
		Type:  "agent",
		Text:  dup.AgentID,
		Error: agentFrameJSON(dup.AgentID, dup.RunID, dup.SessionID, "", true),
	}); err != nil {
		return status.Errorf(codes.Canceled, "stream send failed: %v", err)
	}
	err := s.connector.StreamRunEvents(ctx, dup.RunID, 0, func(ev providers.Event) error {
		return stream.Send(eventToProto(ev))
	})
	switch {
	case errors.Is(err, connector.ErrRunNotInFlight):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, connector.ErrSteeringUnavailable):
		return status.Error(codes.Unavailable, err.Error())
	case err != nil:
		return status.Errorf(codes.Canceled, "stream send failed: %v", err)
	}
	return nil
}

// runInputProtoArgs gathers the proto fields runInputFromProto reads.
// Struct-arg shape rather than positional parameters because the field
// set has grown past the comfortable positional limit (per-run policy
// fields TenantID/UserTier/UserBearer were added when gRPC reached
// HTTP wire parity).
type runInputProtoArgs struct {
	Agent            string
	SessionID        string
	Segments         []*loomcyclepb.PromptSegment
	Tools            []string
	AllowedHosts     *loomcyclepb.HostAllowlist
	WebSearchFilter  string
	UserID           string
	AgentID          string
	TenantID         string
	UserTier         string
	UserBearer       string
	UserCredentials  map[string]string            // v1.x RFC F per-tool named credentials
	Sampling         *config.Sampling             // v0.28.0 per-run sampling override
	ToolChoice       *config.ToolChoice           // RFC DI per-run tool_choice
	OutputFormat     *config.OutputFormat         // RFC DI per-run answer schema
	Routing          *config.AgentRouting         // the run's own routing: block (queue-aware start)
	Compaction       *config.Compaction           // v0.32.0 per-run compaction override
	Interactive      bool                         // RFC AI — park at end_turn for steering
	Review           bool                         // RFC DJ — hold for an operator's verdict when done
	ReviewTTLSeconds int                          // RFC DJ — end an unreviewed hold as rejected after this long
	Interruption     *config.AgentInterruptionACL // per-run override of whether the agent may ask a human
	Metadata         map[string]any               // non-secret structured metadata handed to the run
	Context          *config.Context              // per-run layered-context / retention override
	ParentContext    *store.ParentContext         // opaque caller-tracking lineage
	MaxContextTokens int                          // RFC CJ per-run context-window override (0 = inherit agent def)
	IdempotencyKey   string                       // the caller's key, as sent (Run only)
	MaxWallSeconds   int                          // the run's own lifetime limit (Run only)
	Retention        string                       // "none" = a no-trace run
	Admission        string                       // "wait" (default) queues a run that cannot start; "reject" refuses it

	// RFC DC per-run overrides. Routing selects within what the definition
	// declares; the budget knobs are raisable except MaxConcurrentChildren,
	// which may only be lowered; the tuning pointers keep "turn it off"
	// expressible. Validation lives in the HTTP layer both transports share, so
	// a gRPC caller is refused for exactly the same reasons.
	Model                 string
	Provider              string
	Tier                  string
	Effort                string
	MaxTokens             int
	MaxIterations         int
	UnboundedIterations   *bool
	MaxConcurrentChildren int
	RetryAttempts         *int
	MemoryInjectMaxTokens *int
	MemoryIndexMaxBytes   *int
	InjectToolGuide       *bool
}

// runInputFromProto maps the proto request fields into the
// runner.RunInput shared between Run and Continue.
func runInputFromProto(a runInputProtoArgs) runner.RunInput {
	in := runner.RunInput{
		Model:                 a.Model,
		Provider:              a.Provider,
		Tier:                  a.Tier,
		Effort:                a.Effort,
		MaxTokens:             a.MaxTokens,
		MaxIterations:         a.MaxIterations,
		UnboundedIterations:   a.UnboundedIterations,
		MaxConcurrentChildren: a.MaxConcurrentChildren,
		RetryAttempts:         a.RetryAttempts,
		MemoryInjectMaxTokens: a.MemoryInjectMaxTokens,
		MemoryIndexMaxBytes:   a.MemoryIndexMaxBytes,
		InjectToolGuide:       a.InjectToolGuide,
		Agent:                 a.Agent,
		SessionID:             a.SessionID,
		Segments:              segmentsFromProto(a.Segments),
		Tools:                 a.Tools,
		WebSearchFilter:       a.WebSearchFilter,
		UserID:                a.UserID,
		AgentID:               a.AgentID,
		TenantID:              a.TenantID,
		UserTier:              a.UserTier,
		UserBearer:            a.UserBearer,
		UserCredentials:       a.UserCredentials, // v1.x RFC F per-tool named credentials
		Sampling:              a.Sampling,        // v0.28.0 per-run sampling override
		ToolChoice:            a.ToolChoice,      // RFC DI per-run tool_choice
		OutputFormat:          a.OutputFormat,    // RFC DI per-run answer schema
		Routing:               a.Routing,         // the run's own routing: block
		Compaction:            a.Compaction,      // v0.32.0 per-run compaction override
		Interactive:           a.Interactive,     // RFC AI — park at end_turn for steering
		Review:                a.Review,          // RFC DJ — hold for an operator's verdict when done
		ReviewTTLSeconds:      a.ReviewTTLSeconds,
		Metadata:              a.Metadata,
		Context:               a.Context,
		ParentContext:         a.ParentContext,
		Interruption:          a.Interruption,
		MaxContextTokens:      a.MaxContextTokens, // RFC CJ per-run context-window override
		// As sent; RunOnce scopes and stores it.
		ClientIdempotencyKey: a.IdempotencyKey,
		MaxWallSeconds:       a.MaxWallSeconds,
		Retention:            a.Retention,
		Admission:            a.Admission,
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
// toolChoiceFromProto maps the wire message to config.ToolChoice; nil stays nil
// (inherit the agent's). Validation happens in RunOnce, like the HTTP path.
func toolChoiceFromProto(p *loomcyclepb.ToolChoice) *config.ToolChoice {
	if p == nil {
		return nil
	}
	return &config.ToolChoice{Mode: p.GetMode(), Name: p.GetName(), Until: p.GetUntil()}
}

// routingFromProto maps the wire message to config.AgentRouting. nil stays nil
// (the agent's block); a present, empty message is kept as an empty block,
// which is the run turning queue-aware routing off. Validation happens in
// RunOnce against the run's effective definition, like the HTTP path.
func routingFromProto(p *loomcyclepb.RunRouting) *config.AgentRouting {
	if p == nil {
		return nil
	}
	return &config.AgentRouting{QueueAware: p.GetQueueAware(), SwitchAfterMS: int(p.GetSwitchAfterMs())}
}

// outputFormatFromProto maps the wire message to config.OutputFormat; nil stays
// nil (inherit the agent's). The schema travels as JSON bytes, like metadata. A
// schema that is not a JSON object is passed on EMPTY so RunOnce's validation
// refuses it — the same InvalidArgument as any other bad format, rather than a
// second error path here.
func outputFormatFromProto(p *loomcyclepb.OutputFormat) *config.OutputFormat {
	if p == nil {
		return nil
	}
	of := &config.OutputFormat{Type: p.GetType(), Name: p.GetName()}
	if len(p.GetSchema()) > 0 {
		var schema map[string]any
		if err := json.Unmarshal(p.GetSchema(), &schema); err == nil {
			of.Schema = schema
		}
	}
	return of
}

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
				Kind:      b.GetKind(),
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
	// RFC DC: a typed gRPC client must see a retune as a retune. A field that
	// stops at the Go struct is invisible here, which is the exact way RFC DA
	// shipped a field its TypeScript client could not read.
	if ev.Override != nil {
		out.Override = &loomcyclepb.OverrideInfo{
			Source:    ev.Override.Source,
			FromModel: ev.Override.FromModel,
			ToModel:   ev.Override.ToModel,
			Fields:    ev.Override.Fields,
		}
	}
	// The admission payload on type=queued / type=admitted frames, and on
	// call_queued / call_admitted: without it a typed client sees that a run
	// or call waits but not why, for which provider, or for how long.
	if ev.Admission != nil {
		out.Admission = &loomcyclepb.AdmissionInfo{
			Reason:   ev.Admission.Reason,
			Position: int32(ev.Admission.Position),
			WaitMs:   ev.Admission.WaitMs,
			Provider: ev.Admission.Provider,
			Kind:     ev.Admission.Kind,
		}
	}
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
	if f := ev.File; f != nil {
		out.File = &loomcyclepb.FileCapturedInfo{
			Ref: f.Ref, MediaType: f.MediaType, Size: f.Size, Sha256: f.SHA256, Origin: f.Origin, RunId: f.RunID,
		}
	}
	if tr := ev.Transcription; tr != nil {
		out.Transcription = &loomcyclepb.TranscriptionInfo{
			Ref: tr.Ref, Out: tr.Out, Purpose: tr.Purpose, Engine: tr.Engine, Provider: tr.Provider, Model: tr.Model,
			Pages: int32(tr.Pages), Seconds: tr.Seconds,
			InputTokens: int64(tr.InputTokens), OutputTokens: int64(tr.OutputTokens),
			// proto3 optional, so an unpriced pass (nil) stays apart from a free one.
			Cost:     tr.Cost,
			Currency: tr.Currency, Status: tr.Status, DryRun: tr.DryRun, RunId: tr.RunID, Error: tr.Error,
		}
	}
	if hd := ev.HookDecision; hd != nil {
		out.HookDecision = &loomcyclepb.HookDecision{
			Hook: hd.Hook, Phase: hd.Phase, ToolUseId: hd.ToolUseID, ToolName: hd.ToolName,
			Decision: hd.Decision, FailMode: hd.FailMode, Reason: hd.Reason,
			UpdatedInput: hd.UpdatedInput, AdditionalContext: hd.AdditionalContext,
			Channel: hd.Channel, MessageId: hd.MessageID,
		}
	}
	if ev.CapabilityInert != nil {
		out.CapabilityInert = &loomcyclepb.CapabilityInertInfo{
			Tool:    ev.CapabilityInert.Tool,
			Gate:    ev.CapabilityInert.Gate,
			Message: ev.CapabilityInert.Message,
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
	if ev.SubagentHold != nil {
		out.SubagentHold = &loomcyclepb.SubagentHold{
			Subagent:      ev.SubagentHold.Subagent,
			SubagentRunId: ev.SubagentHold.SubagentRunID,
			State:         ev.SubagentHold.State,
			HeldBy:        ev.SubagentHold.HeldBy,
			Round:         int32(ev.SubagentHold.Round),
			ExpiresAt:     ev.SubagentHold.ExpiresAt,
			Status:        ev.SubagentHold.Status,
		}
	}
	if ev.AwaitingReview != nil {
		out.AwaitingReview = &loomcyclepb.AwaitingReview{
			SinceTurn: int32(ev.AwaitingReview.SinceTurn),
			Round:     int32(ev.AwaitingReview.Round),
			ExpiresAt: ev.AwaitingReview.ExpiresAt,
			HeldBy:    ev.AwaitingReview.HeldBy,
		}
	}
	if ev.AwaitingInput != nil {
		out.AwaitingInput = &loomcyclepb.AwaitingInput{
			SinceTurn: int32(ev.AwaitingInput.SinceTurn),
		}
	}
	if ev.AwaitingChildren != nil {
		out.AwaitingChildren = &loomcyclepb.AwaitingChildren{
			ChildRunIds: ev.AwaitingChildren.ChildRunIDs,
			SinceTurn:   int32(ev.AwaitingChildren.SinceTurn),
		}
	}
	if td := ev.TeamDecision; td != nil {
		out.TeamDecision = &loomcyclepb.TeamDecision{
			State: td.State, Visit: int32(td.Visit), Edge: td.Edge, Next: td.Next, Answer: td.Answer,
		}
	}
	if ev.ChildrenNote != nil {
		out.ChildrenNote = &loomcyclepb.ChildrenNote{
			Text:        ev.ChildrenNote.Text,
			ChildRunIds: ev.ChildrenNote.ChildRunIDs,
		}
	}
	if ev.UserInput != nil {
		out.UserInput = &loomcyclepb.UserInput{
			Text:   ev.UserInput.Text,
			Source: ev.UserInput.Source,
			SeenAt: ev.UserInput.SeenAt,
		}
	}
	// A terminal run failure's classification. Without it a gRPC stream
	// consumer sees the same error frame for a transient concurrency cap and
	// an exhausted budget, which want opposite responses. The status details
	// already carry this for a call that FAILS the RPC; this is the streamed
	// half.
	// A failed tool call's too: its classification and correct call format.
	out.ErrorInfo = errorInfoToProto(ev.ErrorInfo)
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
func agentFrameJSON(agentID, runID, sessionID, parentAgentID string, deduplicated bool) string {
	b, err := json.Marshal(struct {
		AgentID       string `json:"agent_id"`
		RunID         string `json:"run_id"`
		SessionID     string `json:"session_id"`
		ParentAgentID string `json:"parent_agent_id"`
		// Deduplicated: the request's idempotency_key was already held, and
		// this stream is that existing run's.
		Deduplicated bool `json:"deduplicated,omitempty"`
	}{agentID, runID, sessionID, parentAgentID, deduplicated})
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
// mapRunnerErr turns a runner error into the gRPC status a caller sees, then
// attaches google.rpc error details so conditions that SHARE a code stay
// distinguishable. codes.ResourceExhausted alone cannot separate a transient
// concurrency cap from an exhausted token budget, and those want opposite
// behaviour from the caller.
func mapRunnerErr(err error) error {
	if err == nil {
		return nil
	}
	return withErrorDetails(status.Convert(runnerErrStatus(err)), err)
}

// runnerErrStatus is the code-and-message mapping, unchanged. Kept separate
// from the decoration above so the codes a caller branches on stay easy to
// read and to diff.
func runnerErrStatus(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, runner.ErrAgentNotAllowed), errors.Is(err, runner.ErrOverrideNotAllowed):
		// The caller's token is limited to other agents, or locked out of the
		// override it set. PermissionDenied mirrors the HTTP 403.
		return status.Error(codes.PermissionDenied, err.Error())
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
	case errors.Is(err, runner.ErrRunNotConfigured):
		// RFC DI: a start of a draft that has already started or been
		// discarded — the state changed under the caller.
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, runner.ErrDraftChanged):
		// A read-modify-write conflict: the draft was edited while this start
		// waited. Starting again runs the edited draft.
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, runner.ErrRateLimited),
		errors.Is(err, runner.ErrBackpressure),
		errors.Is(err, runner.ErrPerUserQuotaExhausted):
		// All backpressure flavors share ResourceExhausted on the gRPC
		// wire — the global queue and the per-user cap. HTTP distinguishes
		// them via the JSON body's `code` field + Retry-After header; gRPC
		// consumers branch on
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
	case errors.Is(err, runner.ErrRetentionUnsupported):
		// The request is well formed and cannot be served by a no-trace run:
		// it needs the run's content stored. Mirrors the HTTP 400/409.
		return status.Error(codes.FailedPrecondition, err.Error())
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

// --- RFC BL P5: subject erasure (gRPC twins of GET/POST /v1/_erasure) --------

// erasureTenant resolves the tenant an erasure call may act in.
//
// Unlike UsageReport there is no wire tenant to honor, deliberately: a subject
// id is only unique within one tenant, so accepting one would let a caller
// enumerate — or erase — a subject belonging to somebody else. An admin
// therefore acts in its OWN tenant here; the cross-tenant focus that HTTP offers
// via ?tenant= is an explicit, auditable query parameter, and quietly inheriting
// it from a gRPC principal would be a different thing entirely.
func erasureTenant(ctx context.Context) string {
	tenantID, _ := grpcTenantScope(ctx)
	return tenantID
}

// ErasureReport is the gRPC twin of GET /v1/_erasure. Read-only.
func (s *Server) ErasureReport(ctx context.Context, req *loomcyclepb.ErasureReportRequest) (*loomcyclepb.ErasureReportResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "ErasureReport requires a connector; this Server was constructed without one")
	}
	if req.GetSubject() == "" {
		return nil, status.Error(codes.InvalidArgument, "subject is required")
	}
	rep, err := s.connector.ErasureReport(ctx, erasureTenant(ctx), req.GetSubject())
	if err != nil {
		return nil, erasureErrToStatus(err)
	}
	return &loomcyclepb.ErasureReportResponse{
		Tenant:         rep.Tenant,
		Subject:        rep.Subject,
		Tier1Covered:   erasureTierToProto(rep.Tier1),
		Tier2Uncovered: erasureTierToProto(rep.Tier2),
		Tier3Residue:   erasureResidueToProto(rep.Tier3),
		Notes:          rep.Notes,
		Errors:         rep.Errors,
		Access:         erasureAccessToProto(rep.Access),
	}, nil
}

// ErasureExecute is the gRPC twin of POST /v1/_erasure.
//
// dry_run is proto3 `optional`, so an UNSET field is distinguishable from an
// explicit false — which is what lets the default be "do nothing". A plain bool
// would make a zero-valued request destructive.
func (s *Server) ErasureExecute(ctx context.Context, req *loomcyclepb.ErasureExecuteRequest) (*loomcyclepb.ErasureExecuteResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "ErasureExecute requires a connector; this Server was constructed without one")
	}
	if req.GetSubject() == "" {
		return nil, status.Error(codes.InvalidArgument, "subject is required")
	}
	dryRun := true
	if req.DryRun != nil {
		dryRun = req.GetDryRun()
	}
	res, err := s.connector.ErasureExecute(ctx, erasure.ExecuteRequest{
		Tenant:  erasureTenant(ctx),
		Subject: req.GetSubject(),
		DryRun:  dryRun,
		Confirm: req.GetConfirm(),
	})
	if err != nil {
		return nil, erasureErrToStatus(err)
	}
	deleted := make(map[string]int32, len(res.Deleted))
	for k, v := range res.Deleted {
		deleted[k] = int32(v)
	}
	return &loomcyclepb.ErasureExecuteResponse{
		Tenant:   res.Tenant,
		Subject:  res.Subject,
		DryRun:   res.DryRun,
		Deleted:  deleted,
		Retained: res.Retained,
		Residue:  erasureResidueToProto(res.Residue),
		Errors:   res.Errors,
		Notes:    res.Notes,
		Access:   erasureAccessToProto(res.Access),
	}, nil
}

// erasureErrToStatus maps the shared service's sentinels onto gRPC codes, so a
// client sees the same refusal HTTP returns as a 400 rather than an opaque
// Internal.
func erasureErrToStatus(err error) error {
	switch {
	case errors.Is(err, erasure.ErrNoSubject):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, erasure.ErrConfirmMismatch):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, erasure.ErrAuditUnavailable):
		// HTTP's 503: nothing was deleted, and configuring the sink and
		// retrying is the way out.
		return status.Error(codes.Unavailable, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// erasureAccessToProto carries the names; a token travels as def id + name,
// the only identity erasure.TokenRef has.
func erasureAccessToProto(a erasure.Access) *loomcyclepb.ErasureAccess {
	out := &loomcyclepb.ErasureAccess{Groups: a.Groups, Lists: a.Lists}
	for _, t := range a.Tokens {
		out.Tokens = append(out.Tokens, &loomcyclepb.ErasureToken{DefId: t.DefID, Name: t.Name})
	}
	return out
}

func erasureTierToProto(t erasure.Tier) *loomcyclepb.ErasureTier {
	return &loomcyclepb.ErasureTier{Counts: t.Counts, Total: t.Total}
}

func erasureResidueToProto(r erasure.Residue) *loomcyclepb.ErasureResidue {
	return &loomcyclepb.ErasureResidue{
		Rows:             int32(r.Rows),
		Scopes:           r.Scopes,
		SessionsExamined: int32(r.SessionsExamined),
		Truncated:        r.Truncated,
	}
}

// --- directory: who is here, and what is held for them -----------------------

// DirectoryUsers is the gRPC twin of GET /v1/_users. Tenant from the caller.
func (s *Server) DirectoryUsers(ctx context.Context, _ *loomcyclepb.DirectoryUsersRequest) (*loomcyclepb.DirectoryUsersResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "DirectoryUsers requires a connector; this Server was constructed without one")
	}
	tenant := erasureTenant(ctx)
	rows, err := s.connector.DirectoryUsers(ctx, tenant)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &loomcyclepb.DirectoryUsersResponse{Tenant: tenant}
	for _, r := range rows {
		out.Users = append(out.Users, directoryUserToProto(r))
	}
	return out, nil
}

// DirectoryInspect is the gRPC twin of GET /v1/_users/{subject}.
func (s *Server) DirectoryInspect(ctx context.Context, req *loomcyclepb.DirectoryInspectRequest) (*loomcyclepb.DirectoryInspectResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "DirectoryInspect requires a connector; this Server was constructed without one")
	}
	if req.GetSubject() == "" {
		return nil, status.Error(codes.InvalidArgument, "subject is required")
	}
	ins, err := s.connector.DirectoryInspect(ctx, erasureTenant(ctx), req.GetSubject())
	if err != nil {
		if errors.Is(err, directory.ErrNoSubject) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &loomcyclepb.DirectoryInspectResponse{
		Tenant:   ins.Tenant,
		Subject:  ins.Subject,
		Activity: directoryUserToProto(ins.Activity),
		Chats:    int32(ins.Chats),
		Memory:   ins.Memory,
		Usage:    &loomcyclepb.DirectoryUsage{Calls: ins.Usage.Calls, Cost: ins.Usage.Cost},
		Errors:   ins.Errors,
		Notes:    ins.Notes,
	}
	// Left UNSET when the plane was not examined, so a client can tell that apart
	// from "examined, found none" — the same reason the HTTP shape omits it.
	if ins.Documents != nil {
		n := int32(*ins.Documents)
		out.Documents = &n
	}
	if ins.Budget != nil {
		out.Budget = &loomcyclepb.DirectoryBudget{
			SoftLimit: ins.Budget.SoftLimit,
			HardLimit: ins.Budget.HardLimit,
		}
	}
	return out, nil
}

// DirectoryTenants is the gRPC twin of GET /v1/_tenants.
//
// The admin check is re-asserted HERE as well as in the per-RPC scope table,
// mirroring the HTTP handler: the reason is easy to lose in a table, and the
// consequence of losing it is a confined principal learning which tenants exist.
func (s *Server) DirectoryTenants(ctx context.Context, _ *loomcyclepb.DirectoryTenantsRequest) (*loomcyclepb.DirectoryTenantsResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "DirectoryTenants requires a connector; this Server was constructed without one")
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok && !auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return nil, status.Error(codes.PermissionDenied,
			"listing tenants requires an operator-admin token: the list of tenants is itself cross-tenant information")
	}
	rows, err := s.connector.DirectoryTenants(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &loomcyclepb.DirectoryTenantsResponse{
		Notes: []string{
			"derived from runs, so a tenant that has never started a run does not appear — " +
				"an empty list means no ACTIVITY, not no tenants.",
		},
	}
	for _, r := range rows {
		out.Tenants = append(out.Tenants, &loomcyclepb.DirectoryTenant{
			Tenant: r.Tenant, Users: int32(r.Users), Runs: int32(r.Runs),
		})
	}
	return out, nil
}

func directoryUserToProto(r directory.UserRow) *loomcyclepb.DirectoryUser {
	return &loomcyclepb.DirectoryUser{
		Subject:       r.Subject,
		RunningCount:  int32(r.RunningCount),
		TotalCount:    int32(r.TotalCount),
		LastStartedAt: r.LastStartedAt,
	}
}

// int32PtrToInt converts a proto3 optional int32 to the *int the RunInput
// overrides use. nil stays nil: for every field that takes one, the ZERO is
// meaningful (no retries, inject nothing), so absence and zero must not
// collapse.
func int32PtrToInt(p *int32) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

// interruptionFromProto maps the per-run interruption override onto the config
// shape. nil in stays nil out — "the caller said nothing" must reach the runner
// as inherit-the-definition, not as a zero-valued block that disables it.
func interruptionFromProto(in *loomcyclepb.Interruption) *config.AgentInterruptionACL {
	if in == nil {
		return nil
	}
	return &config.AgentInterruptionACL{
		Enabled:    in.GetEnabled(),
		Kinds:      in.GetKinds(),
		MaxPending: int(in.GetMaxPending()),
	}
}

// metadataFromProto decodes the run's non-secret structured metadata.
//
// BYTES carrying canonical JSON, because the value is map[string]any by
// definition. A malformed body is DROPPED rather than failing the run: this is
// an advisory channel an agent reads as a prompt block, and refusing an
// otherwise-valid run over it would be a worse trade than starting without it.
// The decode error is logged so it is not invisible.
func metadataFromProto(b []byte) map[string]any {
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		slog.Warn("grpc: run metadata is not a JSON object and was dropped", "err", err)
		return nil
	}
	return m
}

// contextFromProto maps the per-run layered-context block.
//
// nil in stays nil out — "the caller said nothing" must reach the runner as
// inherit-the-agent's-block, never as a zero-valued Context that would override
// it with emptiness. Each scalar is a pointer on both sides for the same reason:
// keep_last_n 0 means keep none, which is not the same as unset.
func contextFromProto(in *loomcyclepb.Context) *config.Context {
	if in == nil {
		return nil
	}
	out := &config.Context{
		Mode:            in.Mode,
		KeepLastN:       intPtrFrom32(in.KeepLastN),
		Reasoning:       in.Reasoning,
		RecapMaxChars:   intPtrFrom32(in.RecapMaxChars),
		Model:           in.Model,
		AutoRecapAtPct:  intPtrFrom32(in.AutorecapAtPct),
		OnInvalidPatch:  in.OnInvalidPatch,
		MaxPatchRetries: intPtrFrom32(in.MaxPatchRetries),
		Recall:          in.Recall,
		HarvestToMemory: in.HarvestToMemory,
	}
	if len(in.StateSchema) > 0 {
		var sch map[string]any
		if err := json.Unmarshal(in.StateSchema, &sch); err != nil {
			// Unlike metadata, this one MATTERS: a stateful run validates every
			// patch against it, so silently dropping it would turn a validated
			// mode into an unvalidated one. Left nil and logged; the loop's own
			// handling of an absent schema then applies.
			slog.Warn("grpc: context.state_schema is not a JSON object and was dropped", "err", err)
		} else {
			out.StateSchema = sch
		}
	}
	return out
}

func intPtrFrom32(p *int32) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

// parentContextFromProto maps the caller's opaque tracking lineage. It reuses
// the EXISTING ParentContext message rather than a second one: the lineage a
// caller sends and the lineage an event echoes are the same thing, and two
// declarations of it would be free to drift.
func parentContextFromProto(in *loomcyclepb.ParentContext) *store.ParentContext {
	if in == nil {
		return nil
	}
	out := &store.ParentContext{
		RootAgentRunID: in.GetRootAgentRunId(),
		FunctionKey:    in.GetFunctionKey(),
		TierAtRun:      in.GetTierAtRun(),
	}
	// Normalise an all-empty block to nil so the echo surfaces omit it, matching
	// handleRuns and handleSpawnRun.
	if out.IsZero() {
		return nil
	}
	return out
}

// --- Configured runs (RFC DI): create now, start later. ---
//
// The rules are the connector's (shared with HTTP and MCP); these handlers map
// its refusals — which carry an HTTP status — to gRPC codes.

func (s *Server) CreateConfiguredRun(ctx context.Context, req *loomcyclepb.RunRequest) (*loomcyclepb.ConfiguredRun, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "CreateConfiguredRun requires a connector; this Server was constructed without one")
	}
	if err := validateAdmission(req.GetAdmission()); err != nil {
		return nil, err
	}
	spawn := spawnRequestFromProto(req)
	added, err := hooksFromProto(req.GetHooksJson())
	if err != nil {
		return nil, err
	}
	spawn.Hooks, spawn.ToolHooks = added.Hooks, added.ToolHooks
	if req.GetInteractive() {
		interactive := true
		spawn.Interactive = &interactive
	}
	res, err := s.connector.CreateConfiguredRun(ctx, connector.ConfiguredRunRequest{SpawnRunRequest: spawn})
	if err != nil {
		return nil, configuredErrToStatus(err)
	}
	return configuredRunToProto(res), nil
}

func (s *Server) UpdateConfiguredRun(ctx context.Context, req *loomcyclepb.UpdateConfiguredRunRequest) (*loomcyclepb.ConfiguredRun, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "UpdateConfiguredRun requires a connector; this Server was constructed without one")
	}
	res, err := s.connector.UpdateConfiguredRun(ctx, req.GetRunId(), req.GetPatch())
	if err != nil {
		return nil, configuredErrToStatus(err)
	}
	return configuredRunToProto(res), nil
}

func (s *Server) StartConfiguredRun(req *loomcyclepb.StartConfiguredRunRequest, stream loomcyclepb.Loomcycle_StartConfiguredRunServer) error {
	if s.connector == nil {
		return status.Error(codes.Unimplemented, "StartConfiguredRun requires a connector; this Server was constructed without one")
	}
	ctx := stream.Context()
	in, err := s.connector.ConfiguredRunInput(ctx, req.GetRunId(), connector.RunSecrets{
		UserBearer: req.GetUserBearer(), UserCredentials: req.GetUserCredentials(),
	})
	if err != nil {
		return configuredErrToStatus(err)
	}
	// The same driver Run uses: session + agent frames, then the events, and
	// the runner's refusals mapped to codes. An admission refusal returns
	// before OnRegistered, so the caller sees no event and the draft is left.
	return s.driveStream(ctx, stream, in)
}

func (s *Server) DeleteConfiguredRun(ctx context.Context, req *loomcyclepb.DeleteConfiguredRunRequest) (*loomcyclepb.DeleteConfiguredRunResponse, error) {
	if s.connector == nil {
		return nil, status.Error(codes.Unimplemented, "DeleteConfiguredRun requires a connector; this Server was constructed without one")
	}
	if err := s.connector.DeleteConfiguredRun(ctx, req.GetRunId()); err != nil {
		return nil, configuredErrToStatus(err)
	}
	return &loomcyclepb.DeleteConfiguredRunResponse{Deleted: true}, nil
}

func configuredRunToProto(r connector.ConfiguredRun) *loomcyclepb.ConfiguredRun {
	return &loomcyclepb.ConfiguredRun{
		RunId: r.RunID, AgentId: r.AgentID, SessionId: r.SessionID, Status: r.Status, Draft: r.Draft,
	}
}

// configuredErrToStatus maps a configured-run refusal (carrying an HTTP
// status) to the gRPC code the same refusal gets elsewhere on this surface,
// with the refusal's own code as the ErrorInfo reason. The code alone is not
// enough: ResourceExhausted is both configured_run_cap, which needs the caller
// to start or discard a draft, and rate_limited, which clears by waiting.
func configuredErrToStatus(err error) error {
	var hse interface{ HTTPStatus() int }
	if !errors.As(err, &hse) {
		return mapRunnerErr(err)
	}
	var code codes.Code
	switch hse.HTTPStatus() {
	case nethttp.StatusBadRequest, nethttp.StatusUnprocessableEntity:
		code = codes.InvalidArgument
	case nethttp.StatusForbidden:
		// agent_not_allowed / override_not_allowed. The refusal wraps no
		// runner sentinel, so the mapRunnerErr fallback filed it as Internal.
		code = codes.PermissionDenied
	case nethttp.StatusNotFound:
		code = codes.NotFound
	case nethttp.StatusConflict:
		code = codes.FailedPrecondition
	case nethttp.StatusTooManyRequests:
		code = codes.ResourceExhausted
	case nethttp.StatusServiceUnavailable:
		code = codes.Unavailable
	default:
		return mapRunnerErr(err)
	}
	return withErrorDetails(status.New(code, err.Error()), err)
}
