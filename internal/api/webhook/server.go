package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const defaultBodySizeLimitBytes = 1 << 20 // 1 MiB

// Receiver is the RFC H inbound-webhook HTTP handler. It is the
// security/trust boundary: it authenticates a raw external POST, guards
// against replay + overload, projects the payload through the Def's
// allowlisted mapping, then forks on delivery mode to either spawn an agent
// run or publish to a channel.
//
// The receiver does its OWN per-Def authentication (HMAC / bearer); it is
// therefore mounted WITHOUT the global bearer authMiddleware. Mounting it
// behind authMiddleware would force every external sender to also carry the
// operator's LOOMCYCLE_AUTH_TOKEN, defeating the per-webhook secret model.
type Receiver struct {
	store        lookup.WebhookStore
	cfg          *config.Config
	runner       runner.Runner
	publisher    channels.SystemPublisher
	runStateBus  *runstate.Bus
	envAllowlist map[string]bool
	logf         func(format string, args ...any)

	dedup   *dedupCache
	limiter *rateLimiter

	// recent is the per-webhook-name triage ring buffer (WH-5b). Holds the
	// last recentBufferCap invocations per name for the admin-gated
	// recent-deliveries endpoint. recentMu guards the map + every ring.
	recentMu sync.Mutex
	recent   map[string]*recentRing

	// now + getenv are injected so the timestamp-window, dedup-TTL,
	// rate-limit refill, and secret-resolution paths are deterministic in
	// tests. Production wiring uses time.Now + os.Getenv.
	now    func() time.Time
	getenv func(string) string
}

// Deps is the constructor input. runStateBus may be nil (disables ?sync);
// publisher may be nil (channel-delivery webhooks then 503). store may be
// nil (only yaml-defined webhooks resolve).
type Deps struct {
	Store        lookup.WebhookStore
	Cfg          *config.Config
	Runner       runner.Runner
	Publisher    channels.SystemPublisher
	RunStateBus  *runstate.Bus
	EnvAllowlist map[string]bool
	Logf         func(format string, args ...any)

	// Now + Getenv are test seams. Nil falls back to time.Now / os.Getenv.
	Now    func() time.Time
	Getenv func(string) string
}

// New constructs a Receiver. The dedup cache + rate limiter share the
// injected clock so tests advance both with one fake now().
func New(d Deps) *Receiver {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	getenv := d.Getenv
	if getenv == nil {
		getenv = osGetenv
	}
	logf := d.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Receiver{
		store:        d.Store,
		cfg:          d.Cfg,
		runner:       d.Runner,
		publisher:    d.Publisher,
		runStateBus:  d.RunStateBus,
		envAllowlist: d.EnvAllowlist,
		logf:         logf,
		dedup:        newDedupCache(now),
		limiter:      newRateLimiter(now),
		now:          now,
		getenv:       getenv,
	}
}

// Registrar is the minimal mux surface Mount needs. *http.ServeMux
// satisfies it directly; the HTTP server passes an adapter that wraps each
// handler in its recovery middleware. Declared as an interface so the
// receiver stays decoupled from the http package (no import cycle).
type Registrar interface {
	Handle(pattern string, handler http.Handler)
}

// Mount registers the receiver route. Mirrors a2a.Server.Mount. The route
// is NOT wrapped in admin/bearer auth — the receiver authenticates each
// request against the resolved Def's own secret.
func (rec *Receiver) Mount(reg Registrar) {
	reg.Handle("POST /v1/_webhooks/{name}", http.HandlerFunc(rec.handle))
}

// handle is the shared front-half + delivery fork. The webhook NAME comes
// from the URL path (operator-addressable), never from the body — the body
// is fully attacker-controlled until the signature verifies.
func (rec *Receiver) handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")

	ctx, span := lcotel.Tracer().Start(ctx, "webhook.receive")
	defer span.End()
	span.SetAttributes(attribute.String("webhook.name", name))

	// 1. Resolve the active Def. Unknown name → 404.
	wd, ok := lookup.Webhook(ctx, rec.store, rec.cfg, name)
	if !ok {
		rec.finish(span, name, "", "rejected_unknown", "")
		writeError(w, http.StatusNotFound, "unknown_webhook", "")
		return
	}
	if !wd.Enabled {
		// A disabled Def is addressable but inert. 404 (not 403) so a
		// disabled webhook is indistinguishable from a never-registered one
		// to an external caller — no enumeration signal.
		rec.finish(span, name, "", "rejected_disabled", "")
		writeError(w, http.StatusNotFound, "unknown_webhook", "")
		return
	}

	// 2. Read the raw body under the Def's size limit. Raw bytes are what
	//    the signature is verified against — we never re-serialize.
	limit := wd.BodySizeLimitBytes
	if limit <= 0 {
		limit = defaultBodySizeLimitBytes
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(limit)))
	if err != nil {
		rec.finish(span, name, "", "rejected_body", "")
		writeError(w, http.StatusBadRequest, "bad_body", "")
		return
	}

	// 3. VERIFY BEFORE PARSE — signature over the raw bytes.
	if verr := verifySignature(wd.Auth, body, r.Header.Get, rec.envAllowlist, rec.getenv, rec.now); verr != nil {
		var ae *authError
		if errors.As(verr, &ae) && ae.verdict == verdictUnresolved {
			// Config-side failure (secret not allowlisted / unset). 503 names
			// the env var (NAME, not value) so the operator can fix it.
			rec.finish(span, name, "", verdictUnresolved, "")
			rec.logf("webhook %q: secret unresolvable (env=%q)", name, ae.secretEnv)
			writeSecretUnresolvable(w, ae.secretEnv)
			return
		}
		// Signature mismatch / replay-window / bad header / wrong bearer:
		// one opaque 401, NO body detail (no timing/oracle leak).
		rec.finish(span, name, "", verdictRejectedSig, "")
		rec.logf("webhook %q: signature rejected", name)
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}

	// 4. Replay guard (Layer-1, per-replica). delivery id from the Def's
	//    header or a body hash. A hit inside the TTL → 401 (same opaque
	//    posture as a sig failure — a replayed-but-valid request is still
	//    "not accepted").
	did := deliveryID(wd.Auth, body, r.Header.Get)
	if rec.dedup.seen(name, did) {
		// Replay of an already-ACCEPTED delivery (recorded on acceptance
		// below). Same opaque 401 posture as a sig failure.
		rec.finish(span, name, did, "rejected_replay", "")
		rec.logf("webhook %q: replay rejected (delivery_id seen within TTL)", name)
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}

	// 5. Project the payload through the allowlisted mapping. A malformed
	//    body or invalid mapping expression → 400.
	proj, perr := projectPayload(wd.PayloadMapping, body)
	if perr != nil {
		rec.finish(span, name, did, "rejected_mapping", "")
		rec.logf("webhook %q: payload mapping failed: %v", name, perr)
		writeError(w, http.StatusBadRequest, "bad_mapping", "")
		return
	}
	for _, mk := range proj.MissingKeys {
		// Missing optional fields are a tracing note, not a failure.
		rec.logf("webhook %q: payload_mapping target %q resolved to empty", name, mk)
	}

	// 6. Rate limit (per-Def token bucket). Exceeded → 429 + Retry-After.
	if okRate, retry := rec.limiter.allow(name, wd.RateLimit); !okRate {
		rec.finish(span, name, did, "rejected_rate", "")
		writeRetryAfter(w, retry)
		return
	}

	// Accepted at the trust boundary — fork on delivery mode.
	switch wd.Delivery {
	case "channel":
		rec.deliverChannel(ctx, w, span, name, did, wd, body)
	case "spawn", "":
		rec.deliverSpawn(ctx, w, span, name, did, wd, proj, r)
	default:
		rec.finish(span, name, did, "rejected_delivery", "")
		rec.logf("webhook %q: unknown delivery mode %q", name, wd.Delivery)
		writeError(w, http.StatusBadRequest, "bad_delivery", "")
	}
}

// deliverChannel publishes the RAW payload to the Def's channel. No
// RunInput, no credential resolution — channel delivery is a pure relay.
func (rec *Receiver) deliverChannel(ctx context.Context, w http.ResponseWriter, span trace.Span, name, did string, wd config.Webhook, body []byte) {
	if rec.publisher == nil {
		rec.finish(span, name, did, "rejected_no_publisher", "")
		writeError(w, http.StatusServiceUnavailable, "channel_unavailable", "")
		return
	}
	// Publish under the global scope — a webhook is an operator-level
	// ingress, not bound to a user/agent keyspace. maxMessages/TTL default
	// to 0 (the channel's own config / store defaults govern retention).
	_, err := rec.publisher.PublishNow(ctx, wd.Channel, store.MemoryScopeGlobal, "",
		json.RawMessage(body), channels.SystemPublisherUserID, 0, 0)
	if err != nil {
		rec.finish(span, name, did, "rejected_publish", "")
		rec.logf("webhook %q: channel publish failed: %v", name, err)
		writeError(w, http.StatusServiceUnavailable, "channel_unavailable", "")
		return
	}
	// Accepted — record the delivery id now (not at the guard) so a publish
	// failure above stays retryable.
	rec.dedup.record(name, did)
	rec.finish(span, name, did, verdictAccepted, "")
	writeJSON(w, http.StatusAccepted, map[string]string{
		"webhook_name": name,
		"delivery_id":  did,
		"channel":      wd.Channel,
	})
}

// deliverSpawn builds a RunInput and drives runner.RunOnce. Default async
// (202 with the run id captured via OnRegistered). When ?sync=true AND the
// Def's sync_response is enabled, the request blocks on the run-state bus
// until this run reaches a terminal state or sync_response.timeout_ms.
func (rec *Receiver) deliverSpawn(ctx context.Context, w http.ResponseWriter, span trace.Span, name, did string, wd config.Webhook, proj projectResult, r *http.Request) {
	if rec.runner == nil {
		rec.finish(span, name, did, "rejected_no_runner", "")
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "")
		return
	}
	in := buildRunInput(wd, proj, rec.envAllowlist, rec.getenv, rec.logf)

	// user_tier may be projected from the (attacker-influenceable) payload
	// when the operator maps it; it selects the provider/model policy. The
	// webhook spawn path bypasses the HTTP handler's user_tier validation,
	// so an unknown tier would otherwise be silently dropped to the agent
	// default — surface it loudly instead, mirroring the HTTP 400 and the
	// never-silently-degrade contract. (Restricting WHICH valid tier the
	// payload may select is an operator-config concern noted in the docs.)
	if in.UserTier != "" && len(rec.cfg.UserTiers) > 0 {
		if _, ok := rec.cfg.UserTiers[in.UserTier]; !ok {
			rec.finish(span, name, did, "rejected_unknown_user_tier", "")
			writeError(w, http.StatusBadRequest, "unknown_user_tier", "")
			return
		}
	}

	// RFC H Decision 10 "Layer 2" durable dedup: stamp the run with the
	// delivery id so CreateRun persists it to runs.idempotency_key.
	in.IdempotencyKey = did

	// Layer-2 BEFORE-spawn check: if a run already carries this delivery
	// id (a redelivery that survived past the in-memory Layer-1 TTL, or
	// landed on a different replica), return the existing run as accepted
	// without spawning a duplicate. The store is nil for yaml-only
	// deployments; treat a lookup error as "not found" (fail open to the
	// spawn path — the unique index is the real backstop).
	if rec.store != nil && did != "" {
		if existing, ok, lerr := rec.store.RunByIdempotencyKey(ctx, did); lerr == nil && ok {
			rec.dedup.record(name, did)
			rec.finish(span, name, did, verdictAccepted, existing.ID)
			writeJSON(w, http.StatusAccepted, map[string]string{
				"webhook_name": name,
				"delivery_id":  did,
				"run_id":       existing.ID,
				"deduped":      "true",
			})
			return
		}
	}

	wantSync := r.URL.Query().Get("sync") == "true" && wd.SyncResponse.Enabled

	// userID selects the on_complete hook scope. It is the same value
	// buildRunInput stamped onto in.UserID (proj.Fields["user_id"]); read
	// from proj so the source is explicit at the call site.
	userID := proj.Fields["user_id"]

	if !wantSync {
		rec.spawnAsync(w, span, name, did, wd, in, userID)
		return
	}
	rec.spawnSync(ctx, w, span, name, did, wd, in, userID)
}

// spawnAsync fires the run on a detached background ctx (so a client
// disconnect does NOT abort an accepted run) and returns 202 with the run
// id. We wait briefly for ONE of two signals before responding:
//
//   - OnRegistered fires → the run was admitted; 202 with the run id.
//   - RunOnce returns an error before OnRegistered → a setup-time rejection
//     (unknown agent / backpressure / runtime paused); 503, no run id.
//
// OnRegistered fires after the cancel-registry entry is in place but before
// the loop starts, so on the happy path it always precedes any meaningful
// work — the 202 carries a real id. The select also guards a (rare) hung
// setup with a timeout that surfaces 503 rather than hanging the request.
func (rec *Receiver) spawnAsync(w http.ResponseWriter, span trace.Span, name, did string, wd config.Webhook, in runner.RunInput, userID string) {
	registered := make(chan string, 1)
	setupErr := make(chan error, 1)
	var spawnedRunID, spawnedAgentID string
	cb := runner.RunCallbacks{
		OnRegistered: func(agentID, runID, _, _ string) {
			spawnedRunID, spawnedAgentID = runID, agentID
			select {
			case registered <- runID:
			default:
			}
		},
	}
	go func() {
		err := rec.runner.RunOnce(context.Background(), in, cb)
		if err != nil {
			rec.logf("webhook %q: async run failed: %v", name, err)
		}
		// Fire on_complete hooks AFTER the run returns. Only when the run was
		// actually admitted (OnRegistered fired → spawnedRunID set) and the
		// Def declares hooks. A setup-time rejection (err before OnRegistered)
		// never reaches a run completion, so no hooks fire. Detached ctx — the
		// request ctx is long gone. Best-effort; never affects the response.
		if err == nil && spawnedRunID != "" && len(wd.OnComplete) > 0 {
			rec.dispatchOnComplete(context.Background(), name, wd, spawnedRunID, spawnedAgentID, userID)
		}
		// Always signal completion so a setup-time error (which returns
		// before OnRegistered) unblocks the select below. A nil err after
		// OnRegistered already fired is harmless — the select will have
		// returned the run id already.
		setupErr <- err
	}()

	select {
	case runID := <-registered:
		// Run admitted = delivery accepted. Record the id now (not at the
		// guard) so a setup-time rejection below stays retryable.
		rec.dedup.record(name, did)
		rec.finish(span, name, did, verdictAccepted, runID)
		writeJSON(w, http.StatusAccepted, map[string]string{
			"webhook_name": name,
			"delivery_id":  did,
			"run_id":       runID,
		})
	case err := <-setupErr:
		// RunOnce returned with no error. The run WAS admitted whenever
		// OnRegistered fired (spawnedRunID set) — but that does NOT mean the
		// `registered` arm of this select won the race: a fast-completing run
		// makes both `registered` and `setupErr` ready at once, and Go's
		// select chooses at random, so we can land here even though a run id
		// exists. Recover it from spawnedRunID (set before RunOnce returned,
		// so the setupErr channel receive makes it visible here) rather than
		// reporting an empty run_id. Only a genuine setup rejection (err
		// returned BEFORE OnRegistered) leaves spawnedRunID empty.
		if err == nil {
			rec.dedup.record(name, did)
			rec.finish(span, name, did, verdictAccepted, spawnedRunID)
			resp := map[string]string{"webhook_name": name, "delivery_id": did}
			if spawnedRunID != "" {
				resp["run_id"] = spawnedRunID
			}
			writeJSON(w, http.StatusAccepted, resp)
			return
		}
		// RFC H Decision 10 concurrent-race: two deliveries with the same
		// id both passed the BEFORE-spawn Layer-2 check, and the unique
		// index let only one CreateRun win. The loser's RunOnce returns
		// ErrDuplicateIdempotencyKey (BEFORE its agent loop ran — no
		// double-execution). Re-look-up the winner and return it as
		// accepted (202), NOT a 503 — the delivery WAS processed, just by
		// the racing request.
		if errors.Is(err, store.ErrDuplicateIdempotencyKey) {
			if rec.store != nil && did != "" {
				if existing, ok, lerr := rec.store.RunByIdempotencyKey(context.Background(), did); lerr == nil && ok {
					rec.dedup.record(name, did)
					rec.finish(span, name, did, verdictAccepted, existing.ID)
					writeJSON(w, http.StatusAccepted, map[string]string{
						"webhook_name": name,
						"delivery_id":  did,
						"run_id":       existing.ID,
						"deduped":      "true",
					})
					return
				}
			}
			// Winner's row not visible yet (replication lag / nil store):
			// still accepted — the delivery was handled by the racing
			// request. Record so a retry doesn't re-spawn.
			rec.dedup.record(name, did)
			rec.finish(span, name, did, verdictAccepted, "")
			writeJSON(w, http.StatusAccepted, map[string]string{
				"webhook_name": name,
				"delivery_id":  did,
				"deduped":      "true",
			})
			return
		}
		// Setup-time rejection: the id was NOT recorded, so the sender's
		// retry of this delivery is processed rather than dropped.
		rec.finish(span, name, did, "rejected_spawn_setup", "")
		rec.spawnSetupErrorResponse(w, err)
	case <-time.After(5 * time.Second):
		rec.finish(span, name, did, "rejected_spawn_setup", "")
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "")
	}
}

// spawnSetupErrorResponse maps a runner setup-time sentinel to the wire
// status. Mirrors the HTTP run-endpoint mapping so a webhook-spawned run
// fails the same way an interactive run would.
func (rec *Receiver) spawnSetupErrorResponse(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runner.ErrUnknownAgent), errors.Is(err, runner.ErrUnknownProvider), errors.Is(err, runner.ErrInvalidArgument):
		writeError(w, http.StatusBadRequest, "invalid_run", "")
	case errors.Is(err, runner.ErrBackpressure), errors.Is(err, runner.ErrPerUserQuotaExhausted):
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "")
	default:
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "")
	}
}

// spawnSync subscribes to the run-state bus BEFORE starting the run, then
// blocks until this run's agent reaches a terminal state or the timeout.
func (rec *Receiver) spawnSync(ctx context.Context, w http.ResponseWriter, span trace.Span, name, did string, wd config.Webhook, in runner.RunInput, userID string) {
	if rec.runStateBus == nil {
		// Def asked for sync but the runtime has no bus — fail closed (503)
		// rather than silently degrade to async (Decision 9: never silently
		// degrade).
		rec.finish(span, name, did, "rejected_no_bus", "")
		writeError(w, http.StatusServiceUnavailable, "sync_unavailable", "")
		return
	}
	timeout := time.Duration(wd.SyncResponse.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	sub := rec.runStateBus.Subscribe(in.UserID)
	defer sub.Close()

	registered := make(chan struct {
		agentID string
		runID   string
	}, 1)
	var spawnedRunID, spawnedAgentID string
	cb := runner.RunCallbacks{
		OnRegistered: func(agentID, runID, _, _ string) {
			spawnedRunID, spawnedAgentID = runID, agentID
			select {
			case registered <- struct {
				agentID string
				runID   string
			}{agentID, runID}:
			default:
			}
		},
	}
	go func() {
		err := rec.runner.RunOnce(context.Background(), in, cb)
		if err != nil {
			rec.logf("webhook %q: sync run failed: %v", name, err)
		}
		// Fire on_complete hooks AFTER the run returns, same posture as the
		// async path. OnRegistered runs before the loop, so spawnedRunID is
		// set by the time RunOnce returns nil. Detached ctx + best-effort.
		if err == nil && spawnedRunID != "" && len(wd.OnComplete) > 0 {
			rec.dispatchOnComplete(context.Background(), name, wd, spawnedRunID, spawnedAgentID, userID)
		}
	}()

	// Learn our agent_id (the bus events are keyed by it) before matching.
	var ourAgentID, ourRunID string
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	select {
	case reg := <-registered:
		ourAgentID, ourRunID = reg.agentID, reg.runID
		// Run admitted = delivery accepted; record so a later terminal-wait
		// timeout does not leave the (now-running) delivery retryable.
		rec.dedup.record(name, did)
	case <-deadline.C:
		rec.finish(span, name, did, "timeout", "")
		writeError(w, http.StatusGatewayTimeout, "sync_timeout", "")
		return
	case <-ctx.Done():
		rec.finish(span, name, did, "client_gone", "")
		return
	}

	for {
		select {
		case evt := <-sub.C:
			if evt.AgentID != ourAgentID {
				continue
			}
			if isTerminal(evt.Status) {
				rec.finish(span, name, did, verdictAccepted, ourRunID)
				writeJSON(w, http.StatusOK, map[string]string{
					"webhook_name": name,
					"delivery_id":  did,
					"run_id":       ourRunID,
					"agent_id":     ourAgentID,
					"status":       evt.Status,
				})
				return
			}
		case <-deadline.C:
			rec.finish(span, name, did, "timeout", "")
			writeError(w, http.StatusGatewayTimeout, "sync_timeout", "")
			return
		case <-ctx.Done():
			rec.finish(span, name, did, "client_gone", "")
			return
		}
	}
}

// isTerminal reports whether a run status is a terminal state. Mirrors the
// store's RunStatus transitions (running → completed | failed | cancelled).
func isTerminal(status string) bool {
	switch store.RunStatus(status) {
	case store.RunCompleted, store.RunFailed, store.RunCancelled:
		return true
	default:
		return false
	}
}

// finish stamps the verdict on the span AND records a triage entry in the
// per-name recent-deliveries ring (WH-5b). accepted = Ok span status;
// everything else = Error status with the verdict as the description. We
// never put a secret or signature value on the span or the ring (only the
// verdict label, the delivery id, and — for accepted spawns — the run id).
//
// did is "" for rejects that fail before the delivery id is derived (unknown
// name, disabled, body read, signature). runID is "" except for accepted
// spawn deliveries. The ring is bounded per name, so recording on every
// terminal point (including rejects) cannot grow memory without limit.
func (rec *Receiver) finish(s trace.Span, name, did, verdict, runID string) {
	s.SetAttributes(attribute.String("webhook.verdict", verdict))
	if verdict == verdictAccepted {
		s.SetStatus(codes.Ok, "")
	} else {
		s.SetStatus(codes.Error, verdict)
	}
	rec.recordDelivery(name, did, verdict, runID)
}

// --- HTTP response helpers ---

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError emits a minimal {"error":code} body. detail is included only
// when non-empty — the signature-failure path passes "" so no information
// leaks.
func writeError(w http.ResponseWriter, status int, code, detail string) {
	body := map[string]string{"error": code}
	if detail != "" {
		body["detail"] = detail
	}
	writeJSON(w, status, body)
}

// writeSecretUnresolvable emits the 503 with the env-var NAME (never value).
func writeSecretUnresolvable(w http.ResponseWriter, secretEnv string) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error":      "secret_unresolvable",
		"secret_env": secretEnv,
	})
}

// writeRetryAfter emits 429 + Retry-After (whole seconds, min 1).
func writeRetryAfter(w http.ResponseWriter, retry time.Duration) {
	secs := int(retry.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, http.StatusTooManyRequests, "rate_limited", "")
}

// osGetenv is the production env reader. Indirected so the Receiver's
// getenv field defaults to it while tests inject a map-backed reader.
func osGetenv(name string) string { return os.Getenv(name) }
