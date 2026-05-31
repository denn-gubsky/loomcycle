package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	a2agrpc "github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"google.golang.org/grpc"

	bridge "github.com/denn-gubsky/loomcycle/internal/a2a"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// Authenticator authenticates an inbound A2A request from its headers
// and returns the principal name to attach to the SDK CallContext. It
// returns ("", false) for an unauthenticated request — the default-deny
// posture is the caller's (the bridge treats an unauthenticated User as
// anonymous run ownership; it does NOT widen any allowlist).
//
// This is the bearer-header check at the A2A frontier, reusing the same
// constant-time comparison loomcycle's HTTP authMiddleware uses. It is
// injected so tests can supply a deterministic fake.
type Authenticator func(h http.Header) (name string, ok bool)

// CardAndRunStore is the narrow store surface the A2A server needs: the
// active-server-card resolver path (lookup), the run-table reader the
// TaskStore is backed by, and the interrupt-row surface the
// INPUT_REQUIRED resume bridge resolves against. store.Store satisfies
// it; tests inject a small fake. Declared as an interface so this
// package doesn't drag in the full store.Store dependency for unit tests.
type CardAndRunStore interface {
	lookup.A2AServerCardStore
	bridge.RunReader
	bridge.InterruptStore
}

// Deps are the injected dependencies for the A2A server surface. All are
// constructor-required except ChannelNotify; the server does not reach
// into globals.
type Deps struct {
	Cfg   *config.Config
	Store CardAndRunStore     // resolves the active A2AServerCardDef + backs the TaskStore
	Conn  connector.Connector // cancel path for the executor
	Run   runner.Runner       // drives agent runs
	Auth  Authenticator       // frontier auth → principal (nil ⇒ all requests anonymous)

	// ChannelNotify wakes a run parked on an Interruption.ask, keyed by
	// "intr:<id>". It MUST be the SAME notification bus the Interruption
	// tool's blockWithHeartbeat waits on (main.go's channelBus.Notify) —
	// otherwise a resumed A2A run never unblocks. Nil ⇒ the INPUT_REQUIRED
	// resume bridge is disabled: a parked run still surfaces INPUT_REQUIRED
	// but a follow-up message starts a fresh run instead of resuming.
	ChannelNotify func(busKey string)
}

// Server is the mounted A2A surface. It owns the resolved card + the SDK
// RequestHandler; Mount adds its routes to an existing mux and
// GRPCHandler exposes the gRPC binding for registration on loomcycle's
// shared grpc.Server.
type Server struct {
	deps     Deps
	tenancy  string // "", "none", "host", or "path"
	baseURL  string
	cardName string

	// signEnvAllowlist gates which env var the active card's
	// sign_with_key_env may read, mirroring the scheduler / RFC F env
	// allowlist. Empty ⇒ no env var is readable ⇒ cards serve unsigned.
	signEnvAllowlist map[string]bool

	// handler is the SDK RequestHandler that all three bindings share.
	handler a2asrv.RequestHandler
	grpc    *a2agrpc.Handler

	// grpcEnabled is false when the gRPC binding must NOT be served — i.e.
	// under host/path tenancy, where the routed tenant is attached by the
	// HTTP-layer wrappers (hostTenantWrap / PathTenantWrapper) that the
	// gRPC transport bypasses entirely. Serving gRPC there would let a peer
	// spoof its tenant via the request body (the bridge falls back to the
	// body tenant when no routed tenant is stamped). We FAIL CLOSED: skip
	// the gRPC registration AND drop the gRPC interface from the advertised
	// card, rather than serve a body-spoofable tenancy boundary. (A proper
	// gRPC tenant interceptor is future work.)
	grpcEnabled bool
}

// New builds the A2A server from the active A2AServerCardDef named in
// config. It returns (nil, nil) when the surface is disabled so callers
// can unconditionally call New and skip mounting on a nil result.
//
// Resolution happens once at construction: the card's exposed_agents
// drive the skill→agent resolver baked into the executor, and the card
// metadata is captured for the served AgentCard. A config change
// requires a restart (same lifecycle as the static yaml card).
func New(ctx context.Context, deps Deps) (*Server, error) {
	if deps.Cfg == nil || !deps.Cfg.Env.A2AServerEnabled {
		return nil, nil
	}
	cardName := deps.Cfg.Env.A2AServerCardName
	card, ok := lookup.A2AServerCard(ctx, deps.Store, deps.Cfg, cardName)
	if !ok {
		return nil, fmt.Errorf("a2a server: active server card %q not found (yaml a2a_server_cards or A2AServerCardDef substrate)", cardName)
	}

	// Skill→agent resolver from exposed_agents. Built once; the map is
	// read-only after construction so it is safe to share across the
	// concurrent requests the SDK handler fans out.
	skillToAgent := make(map[string]string, len(card.ExposedAgents))
	var firstAgent string
	for _, e := range card.ExposedAgents {
		if e.SkillID == "" || e.AgentName == "" {
			continue
		}
		skillToAgent[e.SkillID] = e.AgentName
		if firstAgent == "" {
			firstAgent = e.AgentName
		}
	}
	if len(skillToAgent) == 0 {
		return nil, fmt.Errorf("a2a server: card %q exposes no usable agents (each exposed_agents entry needs skill_id + agent_name)", cardName)
	}

	taskStore := bridge.NewTaskStore(deps.Store)
	exec := bridge.NewExecutor(deps.Run, deps.Conn, deps.Store, firstAgent).
		WithAgentResolver(func(skillID string) (string, bool) {
			agent, ok := skillToAgent[skillID]
			return agent, ok
		})

	// INPUT_REQUIRED ↔ Interruption resume bridge. With a notify hook
	// wired (the SAME bus the Interruption tool waits on), a same-task
	// follow-up message RESOLVES the run's pending interruption and
	// resumes it; without it, a parked run still surfaces INPUT_REQUIRED
	// but a follow-up starts a fresh run. The resolver reuses the run
	// store's InterruptResolve + the channel bus's Notify — converging A2A
	// resume and HTTP resume on one mechanism (see internal/a2a
	// NewInterruptResolver).
	if deps.ChannelNotify != nil {
		exec = exec.WithInterruptionBridge(bridge.NewInterruptResolver(deps.Store, deps.ChannelNotify))
	}

	// The principal interceptor runs the frontier Authenticator and
	// stamps the SDK CallContext.User, which the bridge reads in
	// principalFromContext. This is how A2A binding requests get
	// authenticated without loomcycle's bearer authMiddleware (the
	// binding endpoints are NOT wrapped by it — see Mount).
	opts := []a2asrv.RequestHandlerOption{
		a2asrv.WithTaskStore(taskStore),
		a2asrv.WithCapabilityChecks(&a2asdk.AgentCapabilities{
			Streaming:         card.Capabilities.Streaming,
			PushNotifications: false,
		}),
		a2asrv.WithCallInterceptors(&principalInterceptor{auth: deps.Auth}),
	}
	handler := a2asrv.NewHandler(exec, opts...)

	allow := make(map[string]bool, len(deps.Cfg.Env.SchedulerEnvAllowlist))
	for _, n := range deps.Cfg.Env.SchedulerEnvAllowlist {
		allow[n] = true
	}

	// gRPC is served only when the routed tenant can be made authoritative
	// for it. Under host/path tenancy it cannot (the tenant is stamped by
	// the HTTP wrappers gRPC bypasses), so fail closed: no gRPC handler is
	// built, and writeCard drops the gRPC interface from the served card.
	tenancy := deps.Cfg.Env.A2ATenancyRouting
	grpcEnabled := tenancy != "host" && tenancy != "path"
	var grpcHandler *a2agrpc.Handler
	if grpcEnabled {
		grpcHandler = a2agrpc.NewHandler(handler)
	} else {
		log.Printf("a2a server: tenancy=%q — gRPC binding disabled (tenant cannot be derived from the gRPC transport); REST + JSON-RPC remain available", tenancy)
	}

	return &Server{
		deps:             deps,
		tenancy:          tenancy,
		baseURL:          deps.Cfg.Env.A2APublicBaseURL,
		cardName:         cardName,
		signEnvAllowlist: allow,
		handler:          handler,
		grpc:             grpcHandler,
		grpcEnabled:      grpcEnabled,
	}, nil
}

// GRPCHandler returns the gRPC binding so main.go can register it on the
// shared grpc.Server (gRPC is not a path-mounted http.Handler; it needs
// the HTTP/2 server). Nil-safe on a nil Server.
func (s *Server) RegisterGRPC(g *grpc.Server) {
	if s == nil || s.grpc == nil {
		return
	}
	s.grpc.RegisterWith(g)
}

// Mount adds the well-known AgentCard route and the REST + JSON-RPC
// binding routes to mux. These are ADDITIVE — they do not touch /v1/*,
// MCP, or the gRPC service. The binding endpoints are intentionally NOT
// wrapped in loomcycle's bearer authMiddleware: A2A auth happens inside
// the SDK handler via the principalInterceptor, mapping the peer's own
// credential to a run principal. The ?extended=true card variant IS
// gated behind the supplied admin-auth middleware.
//
// authMiddleware wraps only the extended-card surface; pass the same
// recoveryMiddleware(authMiddleware(...)) chain the rest of /v1/_* uses.
func (s *Server) Mount(mux *http.ServeMux, adminAuth func(http.Handler) http.Handler) {
	if s == nil {
		return
	}
	rest := a2asrv.NewRESTHandler(s.handler)
	jsonrpc := a2asrv.NewJSONRPCHandler(s.handler)
	card := http.HandlerFunc(s.handleAgentCard)

	// All three logical routes are registered at their CONCRETE paths
	// (no "/{tenant}" wildcard). An open first-segment wildcard would
	// collide with every subtree route the HTTP server already owns
	// (e.g. "GET /ui/"), which Go's ServeMux rejects as ambiguous.
	//
	// Tenant derivation differs by mode but never changes these mounts:
	//   - host/none: hostTenantWrap reads the Host header (or nothing).
	//   - path: the tenant segment is stripped by PathTenantWrapper at
	//     the http.Server.Handler level BEFORE the request reaches this
	//     mux, so the concrete paths still match. See PathTenantWrapper.
	mux.Handle("GET "+pathWellKnown, s.hostTenantWrap(card))
	// capBody bounds the binding request bodies. These endpoints are NOT
	// wrapped by the bearer authMiddleware (auth is an SDK CallInterceptor
	// that runs AFTER the transport decodes the body), and the SDK decodes
	// with an unbounded json.NewDecoder — so without this an UNAUTHENTICATED
	// client could stream a huge body that is fully buffered before the
	// interceptor rejects it. Mirrors the 1 MiB cap on every /v1/* route.
	// The SDK REST handler routes on paths RELATIVE to its mount point
	// (e.g. "POST /message:send", "GET /tasks/{id}"), so the "/a2a/v1"
	// prefix must be stripped before delegation — otherwise every REST
	// call lands as an unmatched path inside the handler and fails. The
	// JSON-RPC handler needs no stripping (it is a single endpoint with
	// no sub-routing). StripPrefix runs INSIDE hostTenantWrap so the
	// host-derived tenant is still attached.
	mux.Handle(pathREST+"/", s.hostTenantWrap(capBody(http.StripPrefix(pathREST, rest))))
	mux.Handle(pathJSONRPC, s.hostTenantWrap(capBody(jsonrpc)))

	if adminAuth != nil {
		mux.Handle("GET "+pathWellKnown+"/extended", adminAuth(http.HandlerFunc(s.handleExtendedCard)))
	}
}

// maxA2ABodyBytes bounds an inbound A2A binding request body, matching the
// 1 MiB cap loomcycle applies on every /v1/* route.
const maxA2ABodyBytes = 1 << 20

// capBody wraps a binding handler so the request body is bounded BEFORE the
// SDK's unbounded json.NewDecoder reads it — the binding endpoints are
// unauthenticated at the transport layer (auth is an SDK interceptor that
// runs after decode), so this bounds memory for an unauthenticated caller.
func capBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxA2ABodyBytes)
		next.ServeHTTP(w, r)
	})
}

// PathTenantWrapper wraps the fully-built server handler so path-mode
// tenancy can strip a leading "/{tenant}" segment when (and only when)
// the remaining path is an A2A route, attach the tenant as the routed
// (trust-boundary) tenant, and rewrite the URL so the inner mux's
// concrete A2A routes match. Non-A2A paths and non-path tenancy modes
// pass through untouched, so this is a no-op outside path mode.
//
// This sits OUTSIDE the mux because an open first-segment wildcard
// cannot coexist on the shared mux with the HTTP server's subtree
// routes. main.go wraps http.Server.Handler with this in path mode.
func (s *Server) PathTenantWrapper(inner http.Handler) http.Handler {
	if s == nil || s.tenancy != "path" {
		return inner
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, rest, ok := splitTenantPrefix(r.URL.Path)
		if !ok {
			// Not a tenant-prefixed A2A path (includes the bare-root
			// single-tenant fallback like /.well-known/...). Pass through.
			inner.ServeHTTP(w, r)
			return
		}
		r2 := r.Clone(bridge.WithRoutedTenant(r.Context(), tenant))
		r2.URL.Path = rest
		r2.URL.RawPath = ""
		inner.ServeHTTP(w, r2)
	})
}

// splitTenantPrefix recognises a "/{tenant}<a2a-route>" path and returns
// (tenant, "<a2a-route>", true). It matches only when the segment after
// the tenant is one of the known A2A routes, so non-A2A first segments
// (e.g. /ui, /v1) are never mistaken for a tenant. Returns ok=false
// otherwise.
func splitTenantPrefix(p string) (tenant, rest string, ok bool) {
	if len(p) < 2 || p[0] != '/' {
		return "", "", false
	}
	seg, tail, found := strings.Cut(p[1:], "/")
	if !found || seg == "" {
		return "", "", false
	}
	rest = "/" + tail
	switch {
	case rest == pathWellKnown,
		rest == pathJSONRPC,
		rest == pathREST,
		strings.HasPrefix(rest, pathREST+"/"):
		return seg, rest, true
	default:
		return "", "", false
	}
}

// handleAgentCard serves the base (unauthenticated) AgentCard. The
// ?extended=true query is honoured here too for clients that prefer the
// query form over the /extended path, but only when the request carries
// valid admin auth — otherwise it silently serves the base card so an
// unauth'd ?extended=true never leaks the extended surface.
func (s *Server) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	tenant := s.tenantFromRequest(r)
	extended := r.URL.Query().Get("extended") == "true" && s.adminAuthed(r)
	s.writeCard(w, r, tenant, extended)
}

// handleExtendedCard serves the full card; the caller (Mount) has
// already wrapped it in admin auth, so reaching here means authorized.
func (s *Server) handleExtendedCard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.writeCard(w, r, s.tenantFromRequest(r), true)
}

// writeCard resolves the active card fresh per request (so a substrate
// edit is reflected without restart on the served metadata) and writes
// the generated AgentCard JSON. Cache-Control: max-age=300 per the
// slice spec. When the card declares sign_with_key_env and that var is
// allowlisted + holds a usable key, the served card is JWS-signed
// (A2A-6); otherwise it is served unsigned with a tracing line — card
// serving never fails on a signing problem.
func (s *Server) writeCard(w http.ResponseWriter, r *http.Request, tenant string, extended bool) {
	// Use the request context so a client disconnect / deadline cancels the
	// substrate card lookup (a DB query on the substrate path) instead of
	// running it to completion on a detached background context.
	card, ok := lookup.A2AServerCard(r.Context(), s.deps.Store, s.deps.Cfg, s.cardName)
	if !ok {
		http.Error(w, "a2a server card unavailable", http.StatusServiceUnavailable)
		return
	}
	base, prefix := s.cardURLAnchors(tenant)
	generated := buildAgentCard(card, base, prefix, extended, s.grpcEnabled)
	signCardIfConfigured(generated, card, s.signEnvAllowlist, log.Printf)

	// Marshal before writing any header so a (very unlikely) encode
	// failure surfaces as a clean 500 rather than a truncated 200 body.
	body, err := json.Marshal(generated)
	if err != nil {
		http.Error(w, "a2a card encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=300")
	if _, werr := w.Write(body); werr != nil {
		// Client hung up mid-write; nothing actionable here. The write
		// error is returned only for observability and recovery
		// middleware already covers panics.
		return
	}
}

// cardURLAnchors returns the (baseURL, pathPrefix) the AgentCard
// interface URLs are built from. Host-mode reflects the tenant in the
// host (the configured base URL already encodes it, or the operator
// fronts per-tenant subdomains), so the path prefix stays empty;
// path-mode prepends /{tenant}.
func (s *Server) cardURLAnchors(tenant string) (base, prefix string) {
	if s.tenancy == "path" && tenant != "" {
		return s.baseURL, "/" + tenant
	}
	return s.baseURL, ""
}

// adminAuthed reports whether the request carries valid admin bearer
// auth, reusing the frontier Authenticator. Used to gate the
// ?extended=true query form (the /extended path is gated by middleware).
func (s *Server) adminAuthed(r *http.Request) bool {
	if s.deps.Auth == nil {
		// No authenticator configured ⇒ open mode (dev). Treat as
		// authorized, matching authMiddleware's open-mode behaviour.
		return true
	}
	_, ok := s.deps.Auth(r.Header)
	return ok
}
