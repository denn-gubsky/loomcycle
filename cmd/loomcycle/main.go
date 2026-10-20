// Command loomcycle is the loomcycle sidecar.
//
// Usage:
//
//	loomcycle --config loomcycle.yaml
//
// Build identification: the buildVersion / buildCommit / buildTime vars
// are resolved automatically from Go's VCS stamp (embedded by `go build`
// since 1.18) and from the module's tagged version when present. A
// release script may still override any of them via -ldflags to inject
// explicit values, but the unattached default works for `go build`,
// `go install`, and CI without any wrapper tooling.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	webhookapi "github.com/denn-gubsky/loomcycle/internal/api/webhook"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/heartbeat"
	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	mcpsign "github.com/denn-gubsky/loomcycle/internal/mcp"
	"github.com/denn-gubsky/loomcycle/internal/metrics"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providerbuild"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	// RFC BF P2a — the resolver now builds providers via providers.NewDriver, not
	// each driver's New(), so these driver packages are imported ONLY for their
	// init() self-registration into the RFC BF driver registry. Blank imports keep
	// their factories registered; codejs + anthropic_oauth_dev stay named below
	// (still referenced directly: code-agent validation and the residual OAuth path).
	_ "github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	anthropic_oauth_dev "github.com/denn-gubsky/loomcycle/internal/providers/anthropic_oauth_dev"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/deepseek"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/gemini"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/llamacpp"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/mock"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/ollama"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/openai"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/vllm"
	"github.com/denn-gubsky/loomcycle/internal/search"
	// Deterministic stub embedder for runtime tests; its init() registers the
	// "stub" provider ONLY when LOOMCYCLE_EMBEDDER_STUB=1, so it is invisible
	// to a production binary that never sets the flag.
	"github.com/denn-gubsky/loomcycle/internal/changesub"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/stubembedder"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/retention"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/scheduler"
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storepostgres "github.com/denn-gubsky/loomcycle/internal/store/postgres"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/usage"

	googlegrpc "google.golang.org/grpc"

	loomgrpc "github.com/denn-gubsky/loomcycle/internal/api/grpc"
	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lcmcp "github.com/denn-gubsky/loomcycle/internal/api/mcp"
	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	mcphttp "github.com/denn-gubsky/loomcycle/internal/tools/mcp/http"
	mcpstdio "github.com/denn-gubsky/loomcycle/internal/tools/mcp/stdio"
)

// Build identification. Empty defaults are resolved at main()-entry
// from Go's VCS stamp via runtime/debug.ReadBuildInfo() — no external
// tooling required. ldflags overrides still win:
//
//	go build -ldflags "-X main.buildVersion=v0.8.14 \
//	                   -X main.buildCommit=$(git rev-parse --short HEAD) \
//	                   -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" ...
//
// Resolution precedence per var: ldflags override → runtime/debug →
// "unknown". A binary built outside a VCS-aware context (e.g. `go run`
// from an unpacked tarball) ends up with "unknown" — visible signal
// that the operator should rebuild from a real checkout.
var (
	buildVersion = ""
	buildCommit  = ""
	buildTime    = ""
)

// resolveBuildInfo reads Go's automatically-embedded VCS stamp and
// returns (version, commit, time). commit gets a "-dirty" suffix when
// the working tree was modified at build time. Empty strings mean the
// corresponding info wasn't available (binary built without VCS — e.g.
// `go test` by default does not embed VCS info, but `go build` and
// `go install` do).
// channelDebugEnabled reports whether the operator opted into the
// v0.12.7 channel publish/subscribe diagnostic log via the
// LOOMCYCLE_CHANNEL_DEBUG=1 env var. Read here (not at store package
// init time) so the env-var coupling stays in main.go where all the
// other env-var → Config bridging lives. See
// internal/tools/builtin/channel.go and
// internal/store/postgres/postgres.go for what gets logged.
func channelDebugEnabled() bool {
	return os.Getenv("LOOMCYCLE_CHANNEL_DEBUG") == "1"
}

// memoryChangesEnabled reports whether the RFC CD Part C change-data-capture
// feed is on. Default off — when off the store is NOT wrapped by the CDC
// decorator, so the default deployment is byte-identical and pays nothing.
func memoryChangesEnabled() bool {
	return os.Getenv("LOOMCYCLE_MEMORY_CHANGES_ENABLED") == "1"
}

// memoryChangesRetention is how long change-feed rows are kept before the
// retention sweeper prunes them. Default 24h; override with
// LOOMCYCLE_MEMORY_CHANGES_RETENTION_HOURS.
func memoryChangesRetention() time.Duration {
	if h := os.Getenv("LOOMCYCLE_MEMORY_CHANGES_RETENTION_HOURS"); h != "" {
		if n, err := strconv.Atoi(h); err == nil && n > 0 {
			return time.Duration(n) * time.Hour
		}
	}
	return 24 * time.Hour
}

// changeSubDeliveryTimeout bounds each outbound change-subscription POST.
const changeSubDeliveryTimeout = 30 * time.Second

// resolveChangeSubSecret resolves a subscription's secret_env NAME to its value,
// gated by the same credential allowlist config-load validation uses — so a
// callback signing key can never be one of loomcycle's own infra secrets.
// Returns only the env-var NAME in errors, never the value.
func resolveChangeSubSecret(name string) (string, error) {
	if !config.EnvNameCredentialSafe(name) {
		return "", fmt.Errorf("secret_env %q is not an allowed credential env var", name)
	}
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("secret_env %q is not set", name)
	}
	return v, nil
}

// changeSubDeliveryInterval is how often the RFC CD Part C push engine tails the
// feed. Default 5s; override with LOOMCYCLE_CHANGE_SUB_INTERVAL_MS.
func changeSubDeliveryInterval() time.Duration {
	if s := os.Getenv("LOOMCYCLE_CHANGE_SUB_INTERVAL_MS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 5 * time.Second
}

// resolveChangeSubscriptions turns the static change_subscriptions config into
// deliverable subscriptions, each with its OWN SSRF-guarded client — the
// callback host is private-allowlisted only when allow_private_host is set, so
// one subscription's opt-in never widens another's dial reach. Disabled entries
// are skipped.
func resolveChangeSubscriptions(cfg *config.Config) []changesub.Subscription {
	var out []changesub.Subscription
	for name, sub := range cfg.ChangeSubscriptions {
		if sub.Disabled {
			continue
		}
		var allow []string
		if sub.AllowPrivateHost {
			if u, err := url.Parse(sub.CallbackURL); err == nil && u.Hostname() != "" {
				allow = []string{u.Hostname()}
			}
		}
		out = append(out, changesub.Subscription{
			Name:        name,
			CallbackURL: sub.CallbackURL,
			TenantID:    sub.TenantID,
			Scope:       sub.Scope,
			Kinds:       sub.Kinds,
			SecretEnv:   sub.SecretEnv,
			Client:      netguard.NewGuardedClient(changeSubDeliveryTimeout, allow),
		})
	}
	return out
}

// asPostgresStore unwraps any store decorators (the CDC wrapper) and returns
// the concrete *storepostgres.Store when the backend is Postgres. Cluster
// coordination needs the concrete type, so the CDC wrapper must be transparent
// to it.
func asPostgresStore(s store.Store) (*storepostgres.Store, bool) {
	for s != nil {
		if pg, ok := s.(*storepostgres.Store); ok {
			return pg, true
		}
		u, ok := s.(interface{ Unwrap() store.Store })
		if !ok {
			return nil, false
		}
		s = u.Unwrap()
	}
	return nil, false
}

// backfillAgentDefSignFn computes the v0.9.x content_sha256 for an
// existing agent_defs row whose hash column is NULL/empty. The
// Definition JSONB is the mergedDef shape; agents.FromOverlay
// deserializes the content-bearing subset, then we layer the row's
// Name in (the agent name lives on the row column, not in JSONB).
func backfillAgentDefSignFn(name string, def []byte) (string, error) {
	content, err := agents.FromOverlay(def)
	if err != nil {
		return "", fmt.Errorf("decode agent_defs.definition: %w", err)
	}
	content.Name = name
	return agents.Sign(content), nil
}

// backfillSkillDefSignFn is the mirror against skill_defs rows.
func backfillSkillDefSignFn(name string, def []byte) (string, error) {
	content, err := skills.FromOverlay(def)
	if err != nil {
		return "", fmt.Errorf("decode skill_defs.definition: %w", err)
	}
	content.Name = name
	return skills.Sign(content), nil
}

// backfillMCPServerDefSignFn — v0.9.x mirror against mcp_server_defs rows.
func backfillMCPServerDefSignFn(name string, def []byte) (string, error) {
	content, err := mcpsign.FromOverlay(def)
	if err != nil {
		return "", fmt.Errorf("decode mcp_server_defs.definition: %w", err)
	}
	content.Name = name
	return mcpsign.Sign(content), nil
}

func resolveBuildInfo() (version, commit, builtAt string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", "", ""
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			builtAt = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return formatBuildInfo(info.Main.Version, rev, builtAt, dirty)
}

// formatBuildInfo normalises the raw VCS-stamp fields into the display
// shape used by --version and startup logs. Split out from
// resolveBuildInfo so it can be exhaustively unit-tested without
// depending on whether the test binary itself had VCS info embedded.
func formatBuildInfo(mainVersion, rev, builtAt string, dirty bool) (version, commit, ts string) {
	version = mainVersion
	if version == "(devel)" || version == "" {
		// `go build` from a local checkout reports "(devel)"; surface a
		// shorter form so operators can tell a release binary from a
		// developer build at a glance.
		version = "devel"
	}
	// Go's module proxy synthesises a "pseudo-version" for a commit
	// that has no semver tag yet. Shape:
	//   vX.Y.Z-0.YYYYMMDDHHMMSS-abcdef012345
	// (the "0." form means "before any vX.Y.Z tag"). Operators want
	// to see the meaningful base ("v0.8.21"), not the 40-char
	// timestamped suffix that's redundant with `commit` + `built`
	// fields. Strip it. Real prerelease tags like "v0.8.14-rc1" are
	// left alone — the regex only matches the precise pseudo shape.
	version = stripPseudoVersionSuffix(version)
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty && rev != "" {
		rev += "-dirty"
	}
	return version, rev, builtAt
}

// pseudoVersionRE matches Go's auto-synthesised pseudo-versions:
//
//	vMAJOR.MINOR.PATCH-0.YYYYMMDDHHMMSS-<12+ hex>
//
// (Go also emits a leading "0." for the pre-tag case; the v2-style
// post-tag pseudo-version is "<base>-<n>.YYYYMMDDHHMMSS-…" but our
// repo is sub-v2 so the 0.-prefixed form is the only one we see.)
// `(\+\w+)?` tolerates Go's module-level "+dirty" suffix that gets
// appended to the pseudo-version when the working tree has uncommitted
// changes — the version field still reports a clean semver, the
// dirty signal is preserved on the separate commit field.
var pseudoVersionRE = regexp.MustCompile(`^(v\d+\.\d+\.\d+)-0\.\d{14}-[0-9a-f]+(?:\+\w+)?$`)

func stripPseudoVersionSuffix(v string) string {
	if m := pseudoVersionRE.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	return v
}

// printVersion writes the one-line build identifier shared by the `version`
// subcommand and the --version flag. buildVersion/buildCommit/buildTime are
// resolved in main() before either caller runs.
func printVersion() {
	fmt.Printf("loomcycle version=%s commit=%s built=%s go=%s\n",
		buildVersion, buildCommit, buildTime, runtime.Version())
}

// mcpProxyHeaderTimeout reads LOOMCYCLE_MCP_UPSTREAM_HEADER_TIMEOUT_MS (the
// thin-client's time-to-response-headers bound; a stalled upstream must not
// hang a frame forever, and Claude Code's tool-idle timeout doesn't cover stdio
// servers). Unset / invalid → 0, letting NewProxyClient apply its default.
func mcpProxyHeaderTimeout() time.Duration {
	v := os.Getenv("LOOMCYCLE_MCP_UPSTREAM_HEADER_TIMEOUT_MS")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return time.Duration(n) * time.Millisecond
}

// mcpProxyReconnectDelays reads LOOMCYCLE_MCP_UPSTREAM_RECONNECT_ATTEMPTS and
// builds the thin-client's transport-error backoff schedule: an immediate first
// retry, then geometric waits (500ms doubling) capped at 4s. Unset → nil, so
// NewProxyClient applies its default schedule; 0 → an empty schedule (retry
// disabled — one attempt only).
func mcpProxyReconnectDelays() []time.Duration {
	v := os.Getenv("LOOMCYCLE_MCP_UPSTREAM_RECONNECT_ATTEMPTS")
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return nil
	}
	delays := make([]time.Duration, n)
	for i := 1; i < n; i++ {
		d := 500 * time.Millisecond * (1 << (i - 1))
		if d > 4*time.Second {
			d = 4 * time.Second
		}
		delays[i] = d
	}
	return delays
}

// selectPresetNames resolves the RFC AQ embedded-preset selection: --preset
// flags, if any, override LOOMCYCLE_PRESETS (comma-separated, ordered). Empty /
// unset → no presets (opt-in default — exactly today's behaviour). Order is
// preserved; it becomes the base layer order of the config stack.
func selectPresetNames(flags []string, env string) []string {
	if len(flags) > 0 {
		return flags
	}
	var out []string
	for _, n := range strings.Split(env, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// processStartedAt is when this process began. A run row older than it was
// started by an earlier process.
var processStartedAt = time.Now()

func main() {
	resolveBuildIdentity()

	mcpMode, done := dispatchSubcommand()
	if done {
		return
	}
	b := &boot{mcpMode: mcpMode}
	// Cleanups registered by the phases run when main returns, last first —
	// the order the defers they replace ran in.
	defer b.runCleanups()

	upstream, showVersion := b.parseFlags()
	if showVersion {
		printVersion()
		return
	}

	// Identify ourselves first thing so an operator running a stale
	// binary spots it immediately — before any "but my code says X"
	// debugging spiral. Critical when development cycle is "git pull
	// && restart" without a rebuild step in between.
	log.Printf("loomcycle build: version=%s commit=%s time=%s", buildVersion, buildCommit, buildTime)

	// RFC R thin-client mode runs BEFORE any runtime boot — no config
	// load, no providers, no store, no listener, no second runtime. The
	// proxy needs only the upstream URL + bearer + stdio.
	if mcpMode && upstream != "" {
		runThinClient(upstream)
		return
	}

	b.loadConfig()
	b.initTelemetry()
	cfg := b.cfg
	otelShutdown := b.otelShutdown

	b.buildProviders()
	b.buildTools()
	b.addLocalAPITools()
	b.logToolPosture()
	b.buildMCP()
	b.openAndMigrateStore()
	// Boot-time checks that need the store belong here: after it is open and
	// migrated, before any subsystem is wired to it.

	b.wireMemory()
	b.wireCredentials()
	b.bindStoreToTools()
	b.loadMCPRegistry()
	b.addA2ATools()

	b.buildServer()
	b.openSQLMemory()
	b.wireSubstrateTools()
	b.wireAuth()
	b.wireServerCatalog()
	b.wireBuses()
	b.buildModelResolver()
	b.mountA2AServer()
	b.mountWebhooks()
	b.buildHTTPServer()

	pr := b.pr
	sem := b.sem
	helpSet := b.helpSet
	embedder := b.embedder
	channelScheduler := b.channelScheduler
	documentTool := b.documentTool
	interruptionTool := b.interruptionTool
	storeIface := b.storeIface
	accessFlusher := b.accessFlusher
	srv := b.srv
	expiredTokenRetention := b.expiredTokenRetention
	sqlMemMgr := b.sqlMemMgr
	sysPublisher := b.sysPublisher
	heartbeatRunner := b.heartbeatRunner
	heartbeatSpecs := b.heartbeatSpecs
	resolver := b.resolver
	a2aServer := b.a2aServer
	webhookReceiver := b.webhookReceiver
	httpServer := b.httpServer

	// Background goroutines (heartbeat sweeper + session-lock map GC)
	// share a single ctx tied to the signal handler below — graceful
	// shutdown cancels them alongside the HTTP server. We rely on the
	// goroutines to exit promptly on ctx.Done(); both are documented
	// to do so in their packages.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	b.addCleanup(bgCancel)

	// Webhook delivery counters: each replica adds what it counted every
	// half minute (never per delivery — the receiver answers unauthenticated
	// requests); the shutdown below flushes the last of them.
	// countFlusherDone closes when the flusher has returned; the final flush
	// below waits for it, so the two never run at once.
	countFlusherDone := make(chan struct{})
	if webhookReceiver != nil {
		go webhookReceiver.RunSweeper(bgCtx, webhookapi.SweepInterval)
		go func() {
			defer close(countFlusherDone)
			webhookReceiver.RunCountFlusher(bgCtx, webhookapi.CountFlushInterval)
		}()
	} else {
		close(countFlusherDone)
	}

	// v0.8.6 heartbeat runner — start AFTER bgCtx is available so
	// bgCancel() on SIGTERM (or a future pause hook) cancels the
	// heartbeat goroutines naturally.
	if heartbeatRunner != nil {
		log.Printf("system channels: starting %d heartbeat goroutine(s)", len(heartbeatSpecs))
		heartbeatRunner.Start(bgCtx)
	}

	// v0.12.0 multi-replica HA foundation. Activates only when the
	// operator sets LOOMCYCLE_REPLICA_ID. openStore already guards
	// against SQLite + cluster mode, so reaching here with a non-
	// empty ReplicaID guarantees a Postgres store.
	//
	// advisoryLock is declared outside the cluster block so the
	// sweeper launch sites below can pass it as nil in single-replica
	// mode (no locking) or the real *AdvisoryLock in cluster mode
	// (only one replica sweeps per tick). v0.12.4 Phase 5.
	var advisoryLock *coord.AdvisoryLock
	if cfg.Env.ReplicaID != "" {
		pgStore, ok := asPostgresStore(storeIface)
		if !ok {
			log.Fatalf("coord: REPLICA_ID set but store is not *storepostgres.Store (openStore guard bypassed?)")
		}
		bp, err := coord.NewPostgresBackplane(coord.PostgresBackplaneConfig{
			Pool:      pgStore.Pool(),
			DSN:       cfg.Storage.PgDSN,
			ReplicaID: cfg.Env.ReplicaID,
		})
		if err != nil {
			log.Fatalf("coord: backplane init: %v", err)
		}
		b.addCleanup(func() {
			if err := bp.Close(); err != nil {
				log.Printf("coord: backplane close: %v", err)
			}
		})
		replicaStore := coord.NewReplicaStore(pgStore.Pool())
		hostname, _ := os.Hostname()
		hb := coord.NewHeartbeat(replicaStore, coord.HeartbeatConfig{
			ReplicaID: cfg.Env.ReplicaID,
			Hostname:  hostname,
			Version:   buildVersion,
		})
		go hb.Run(bgCtx)
		// Wire the cluster view onto /healthz. Phase 1 ships the
		// backplane behind the interface but with no live publisher/
		// subscriber — Phase 2+ adds those.
		srv.SetCoord(bp, replicaStore, cfg.Env.ReplicaID)

		// v0.12.1 Phase 2: cluster-wide per-user fairness. Replaces the
		// in-process perUser counter inside Semaphore with a DB-backed
		// user_quotas atomic counter. No-op when MaxConcurrentRunsPerUser
		// is 0 (per-user check disabled); the wiring is unconditional
		// because WithUserQuotaStore is cheap and the gate is only
		// reached when perUserActive evaluates true.
		quotaStore := coord.NewUserQuotaStore(pgStore.Pool())
		sem.WithUserQuotaStore(quotaStore)

		// v0.12.2 Phase 3: cross-replica cancel coordinator. When a
		// cancel hits a replica that doesn't own the run, the registry
		// delegates to CancelCoordinator.CancelRemote which broadcasts
		// on the backplane + awaits an ack from the owning replica.
		// Two long-lived subscribers carry the wire side: one listens
		// for incoming cancel requests (RunCancelSubscriber), the other
		// receives acks (RunAckSubscriber).
		ackTimeout := time.Duration(cfg.Env.CancelAckTimeoutMs) * time.Millisecond
		cancelCoord, err := coord.NewCancelCoordinator(coord.CancelCoordinatorConfig{
			Backplane:    bp,
			ReplicaID:    cfg.Env.ReplicaID,
			Store:        pgStore,
			ReplicaStore: replicaStore,
			AckTimeout:   ackTimeout,
		})
		if err != nil {
			log.Fatalf("coord: cancel coordinator init: %v", err)
		}
		// A cancel for a run whose replica is recorded dead ends the run the
		// way the server ends any run no live loop holds.
		cancelCoord.SetOwnerGoneFinisher(srv.FinishRunOwnerGone)
		srv.CancelRegistry().SetClusterCanceller(cancelCoord)
		go cancelCoord.RunCancelSubscriber(bgCtx, srv.CancelRegistry())
		go cancelCoord.RunAckSubscriber(bgCtx)

		// Cross-replica operator steering (the twin of cancel): a
		// POST /v1/runs/{run_id}/input that lands on a replica not running the
		// target run routes over the backplane to the owning replica.
		if steerReg := srv.SteerRegistry(); steerReg != nil {
			steerCoord, err := coord.NewSteerCoordinator(coord.SteerCoordinatorConfig{
				Backplane:    bp,
				ReplicaID:    cfg.Env.ReplicaID,
				Store:        pgStore,
				ReplicaStore: replicaStore,
				AckTimeout:   ackTimeout,
			})
			if err != nil {
				log.Fatalf("coord: steer coordinator init: %v", err)
			}
			steerReg.SetClusterSteerer(steerCoord)
			go steerCoord.RunSteerSubscriber(bgCtx, steerReg)
			go steerCoord.RunSteerAckSubscriber(bgCtx)
		}

		// Cross-replica turn-cancel (RFC BH P3a; the twin of cancel/steer): a
		// POST /v1/runs/{run_id}/cancel that lands on a replica not running the
		// target run routes over the backplane to the owning replica by
		// runs.replica_id, so a turn-cancel works cluster-wide.
		if tcReg := srv.TurnCancelRegistry(); tcReg != nil {
			tcCoord, err := coord.NewTurnCancelCoordinator(coord.TurnCancelCoordinatorConfig{
				Backplane:    bp,
				ReplicaID:    cfg.Env.ReplicaID,
				Store:        pgStore,
				ReplicaStore: replicaStore,
				AckTimeout:   ackTimeout,
			})
			if err != nil {
				log.Fatalf("coord: turn-cancel coordinator init: %v", err)
			}
			tcReg.SetClusterCanceller(tcCoord)
			go tcCoord.RunTurnCancelSubscriber(bgCtx, tcReg)
			go tcCoord.RunTurnCancelAckSubscriber(bgCtx)
		}

		// v0.12.3 Phase 4: runstate + channel bus backplane fanout.
		// Both buses were constructed earlier (lines ~1003/1012) and
		// wired into the server via SetRunStateBus / SetChannelBus.
		// Here we add the cluster-mode fanout: every local Publish/
		// Notify also goes onto the backplane so remote replicas wake
		// their local subscribers (PostgresBackplane self-filter
		// prevents the originator from looping).
		if rsb := srv.RunStateBus(); rsb != nil {
			rsb.SetBackplane(bp)
			if err := rsb.SubscribeBackplane(bgCtx, bp); err != nil {
				log.Fatalf("coord: runstate bus subscribe: %v", err)
			}
		}
		if cb := srv.ChannelBus(); cb != nil {
			cb.SetBackplane(bp)
			if err := cb.SubscribeBackplane(bgCtx, bp); err != nil {
				log.Fatalf("coord: channel bus subscribe: %v", err)
			}
		}
		// RFC L Decision 11: flush the local auth-token cache when a peer
		// replica creates/rotates/retires a token.
		if err := srv.SubscribeTokenInvalidations(bgCtx, bp); err != nil {
			log.Fatalf("coord: token-cache invalidation subscribe: %v", err)
		}

		// v0.12.4 Phase 5: advisory-lock helper + replicas TTL sweeper.
		// AdvisoryLock gates the other sweepers below; ReplicasSweeper
		// reaps dead replicas + closes Phases 2 and 3 crash-safety gaps.
		advisoryLock = coord.NewAdvisoryLock(pgStore.Pool())
		replicasSweeper := coord.NewReplicasSweeper(pgStore.Pool(), coord.ReplicasSweeperConfig{
			Lock:       advisoryLock,
			Interval:   cfg.Env.ReplicasSweepInterval,
			StaleAfter: cfg.Env.ReplicasStaleAfter,
		})
		go replicasSweeper.Run(bgCtx)

		// v0.12.5 Phase 6a: cluster-wide session lock via
		// pg_try_advisory_lock. Replaces SessionLockMap so two
		// concurrent continuations on the same session_id ACROSS
		// REPLICAS both get the 409 ErrSessionBusy.
		pgSessionLock := runner.NewPgSessionLocker(pgStore.Pool())
		srv.SetPgSessionLocker(pgSessionLock)

		log.Printf("coord: cluster mode active — replica_id=%s heartbeat=30s backplane=postgres-listen-notify user_quotas=db-backed cancel_ack_timeout=%s bus_fanout=on singleton_sweepers=on session_lock=pg-advisory", cfg.Env.ReplicaID, ackTimeout)
	}

	// Heartbeat sweeper — must run AFTER the cluster block so it can
	// pick up the advisoryLock (v0.12.4 Phase 5). In single-replica
	// mode advisoryLock stays nil and the sweeper's lock-gating branch
	// is bypassed — behavior identical to v0.11.x.
	if cfg.Env.HeartbeatSweeperEnabled && storeIface != nil {
		hbCfg := heartbeat.Config{
			Interval:   cfg.Env.HeartbeatSweepInterval,
			StaleAfter: cfg.Env.HeartbeatStaleAfter,
		}
		// Only assign the interface field when we have a non-nil
		// pointer — assigning a typed-nil *coord.AdvisoryLock to the
		// interface would yield a non-nil interface value whose
		// underlying pointer is nil, causing the sweeper's nil-check
		// to incorrectly think a lock was wired.
		if advisoryLock != nil {
			hbCfg.AdvisoryLock = advisoryLock
			hbCfg.AdvisoryLockKey = coord.LockKeyHeartbeatSweeper
		}
		sweeper := heartbeat.New(storeIface, hbCfg)
		go sweeper.Run(bgCtx)
	} else {
		log.Printf("heartbeat: sweeper disabled (LOOMCYCLE_HEARTBEAT_SWEEPER=0 or no Store)")
	}

	// Channel hooks: the worker that decides the messages of channels that
	// carry hooks. Every replica runs one; per-message leases share the work.
	// With hooks off it still runs, to deliver unhooked the messages written
	// while they were on (by this replica before a restart, or by another):
	// nothing else would ever deliver them.
	hookOwner := cfg.Env.ReplicaID
	if hookOwner == "" {
		host, _ := os.Hostname()
		hookOwner = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	if cfg.Env.ChannelHooksEnabled && srv != nil && storeIface != nil {
		log.Printf("channel hooks: worker started (owner %s, concurrency %d, per channel %d, max wait %s)",
			hookOwner, cfg.Env.ChannelHooksConcurrency, cfg.Env.ChannelHooksPerChannel, cfg.Env.ChannelHooksMaxWait)
		go srv.NewChannelHookWorker(hookOwner, channelScheduler, interruptionTool).Run(bgCtx)
	} else {
		for name, ch := range cfg.Channels {
			if ch.HasHooks() {
				log.Printf("channel hooks: %q declares hooks, but channel hooks are off (LOOMCYCLE_CHANNEL_HOOKS=1 turns them on); they are skipped and its messages are delivered unhooked", name)
			}
		}
		if srv != nil && storeIface != nil {
			go srv.NewChannelHookWorker(hookOwner, channelScheduler, interruptionTool).RunUnhooked(bgCtx)
		}
	}

	// RFC DI D5: discard configured runs (drafts) older than their TTL. Each
	// discard is one guarded transaction, so every replica may run it without
	// a lock; LOOMCYCLE_CONFIGURED_RUN_TTL_MS=0 turns it off.
	if srv != nil && storeIface != nil && cfg.Env.ConfiguredRunTTL > 0 {
		go srv.RunConfiguredRunSweeper(bgCtx)
	}

	// Usage rollup-and-prune sweeper (RFC AV Phase 2b) — same placement
	// rationale as the heartbeat sweeper above: runs AFTER the cluster
	// block so it can pick up the advisoryLock. In single-replica mode
	// advisoryLock stays nil and the sweeper's lock-gating branch is
	// bypassed. Pruning is a compaction to daily usage_archive buckets,
	// not data loss — billing totals are preserved.
	// RFC BM Phase 2 reconciliation: when the retention sweeper is enabled AND its
	// effective (post-alias) chats mode is a delete-bearing one, the retention
	// sweeper owns aged-chat pruning — suppress the usage sweeper's legacy
	// aged-session archiver so a session is never cascade-deleted by both. The
	// usage-detail rollup is unaffected.
	// Only suppress the legacy usage archiver when the retention chats sweep will
	// ACTUALLY run — mirror retention's chatsPruneEnabled gate (export+prune needs
	// an ExportDir). Otherwise a misconfigured export+prune-without-dir would
	// disable BOTH paths, silently leaving aged chats unpruned.
	retentionChatsActive := cfg.Env.RetentionChatsMode == "prune" ||
		(cfg.Env.RetentionChatsMode == "export+prune" && cfg.Env.RetentionExportDir != "")
	retentionOwnsChats := cfg.Env.RetentionEnabled && retentionChatsActive
	if cfg.Env.UsageSweeperEnabled && storeIface != nil {
		uCfg := usage.Config{
			Interval:        cfg.Env.UsageSweepInterval,
			DetailRetention: cfg.Env.UsageDetailRetention,
			// RFC AV Phase 2b2 old-run archiver (opt-in; default OFF).
			RunRetention:       cfg.Env.UsageRunRetention,
			RunRetentionMode:   cfg.Env.UsageRunRetentionMode,
			ExportDir:          cfg.Env.UsageExportDir,
			DisableRunArchiver: retentionOwnsChats,
		}
		// Same typed-nil guard as the heartbeat block: only assign the
		// interface field from a non-nil pointer, or the sweeper's
		// nil-check would see a non-nil interface wrapping a nil pointer.
		if advisoryLock != nil {
			uCfg.AdvisoryLock = advisoryLock
			uCfg.AdvisoryLockKey = coord.LockKeyUsageSweeper
		}
		usageSweeper := usage.New(storeIface, uCfg)
		go usageSweeper.Run(bgCtx)
	} else {
		log.Printf("usage: sweeper disabled (LOOMCYCLE_USAGE_SWEEPER=0 or no Store)")
	}

	// RFC BM data-retention sweeper — same placement rationale as the usage
	// sweeper above (runs AFTER the cluster block so it can pick up the
	// advisoryLock). OPT-IN (default OFF): destructive (it deletes retired def
	// versions), unlike the lossless usage rollup. When the mode is
	// export+prune each version is written to the export dir before deletion.
	if cfg.Env.RetentionEnabled && storeIface != nil {
		rCfg := retention.Config{
			Interval:      cfg.Env.RetentionInterval,
			DefsMode:      cfg.Env.RetentionDefsMode,
			DefsMaxAge:    cfg.Env.RetentionDefsMaxAge,
			DefsKeepLastN: cfg.Env.RetentionDefsKeepLastN,
			// RFC BM Phase 2 aged-chat archiver (opt-in; default OFF, or inherited
			// from the legacy LOOMCYCLE_USAGE_RUN_RETENTION_* config in cfg.Load).
			ChatsMode:   cfg.Env.RetentionChatsMode,
			ChatsMaxAge: cfg.Env.RetentionChatsMaxAge,
			// The runtime's own maintenance chats age out faster, and are deleted
			// rather than exported. Same static-config resolution the consolidator
			// and History use, so all three agree on what "internal" means.
			ChatsInternalMaxAge: cfg.Env.RetentionChatsInternalMaxAge,
			InternalAgents:      cfg.InternalAgentNames(),
			// RFC BM Phase 3 retired-agent memory reclamation (opt-in; default OFF).
			MemMode:   cfg.Env.RetentionMemMode,
			MemMaxAge: cfg.Env.RetentionMemMaxAge,
			// RFC BL P4d class-aware memory-CONTENT prune (opt-in; default OFF).
			MemContentMode:     cfg.Env.RetentionMemContentMode,
			EmptyDossierMode:   cfg.Env.RetentionEmptyDossierMode,
			EmptyDossierMaxAge: cfg.Env.RetentionEmptyDossierMaxAge,
			MemContentMaxAge:   cfg.Env.RetentionMemContentMaxAge,
			ExportDir:          cfg.Env.RetentionExportDir,
		}
		// SQL-Memory scope reclamation needs the manager; nil (SQL Memory off) is
		// fine — the mem sweep then reclaims base memory + dirents only. Typed-nil
		// guard: only assign the interface field from a non-nil pointer.
		if sqlMemMgr != nil {
			rCfg.SQLMem = sqlMemMgr
			// The content prune needs the Document tool, which owns the chunk cascade —
			// the family stays off without it, so a deployment with no SQL Memory is
			// unaffected. Same typed-nil discipline as the interface assignments around it.
			if documentTool != nil {
				rCfg.ChunkPruner = documentTool
			}
		}
		// Same typed-nil guard as the usage block: only assign the interface field
		// from a non-nil pointer, or the sweeper's nil-check would see a non-nil
		// interface wrapping a nil pointer.
		if advisoryLock != nil {
			rCfg.AdvisoryLock = advisoryLock
			rCfg.AdvisoryLockKey = coord.LockKeyRetentionSweeper
		}
		retentionSweeper := retention.New(storeIface, rCfg)
		go retentionSweeper.Run(bgCtx)
	} else {
		log.Printf("retention: sweeper disabled (LOOMCYCLE_RETENTION_ENABLED != 1 or no Store)")
	}

	// RFC AW: seed the per-scope token-budget tracker from the RFC AV ledger
	// (month-to-date counters + the token_limits ceilings). Non-fatal — a
	// seeding fault leaves the counters empty (fail-open: budgets must never
	// block the runtime from starting). No-op when the server has no store.
	if err := srv.SeedLimits(bgCtx); err != nil {
		log.Printf("limits: seed failed (budgets start empty until next write): %v", err)
	}
	// RFC DT: seed the per-model throughput estimate from the ledger's latest
	// timed calls. Non-fatal — an empty estimate learns again from live calls,
	// and nothing depends on it yet (measure-only).
	if err := srv.SeedThroughput(bgCtx); err != nil {
		log.Printf("throughput: seed failed (estimate starts empty): %v", err)
	}

	// Identity Documents for config-declared principals. They exist because an operator
	// wrote them into the yaml, so they are established before anybody authenticates —
	// which makes them the case lazy provisioning serves worst, since their templates
	// should be waiting rather than appearing on some later run. Backgrounded: a hand-
	// written principal list is small, but it is one store round-trip each and boot
	// should not wait on a convenience. Best-effort inside.
	go srv.ProvisionDeclaredPrincipalDocs(bgCtx)

	// Expired-token sweeper (the retention was resolved with the auth wiring
	// above). One replica per cluster per tick; the delete is idempotent.
	if expiredTokenRetention > 0 && storeIface != nil {
		go runAdvisoryGatedSweeper(bgCtx, time.Hour, advisoryLock, coord.LockKeyExpiredTokenSweeper, "expired-token",
			func(ctx context.Context) {
				n, err := storeIface.OperatorTokenDefDeleteExpired(ctx, time.Now().Add(-expiredTokenRetention))
				if err != nil {
					log.Printf("expired-token sweep: %v", err)
					return
				}
				if n > 0 {
					log.Printf("expired-token sweep: deleted %d expired token row(s)", n)
				}
			})
	}

	// Memory tool TTL sweeper. Cheap periodic DELETE of expired rows.
	// The store also filters expired entries at read time, so this
	// goroutine only matters for keeping the table small over the
	// long haul. v0.12.4: gated behind advisoryLock so only one
	// replica per cluster sweeps per tick.
	if cfg.Env.MemorySweepInterval > 0 && storeIface != nil {
		go runAdvisoryGatedSweeper(bgCtx, cfg.Env.MemorySweepInterval, advisoryLock, coord.LockKeyMemorySweeper, "memory",
			func(ctx context.Context) {
				swept, err := storeIface.MemorySweep(ctx)
				if err != nil {
					log.Printf("memory sweep: %v", err)
					return
				}
				if swept > 0 {
					log.Printf("memory sweep: deleted %d expired row(s)", swept)
				}
			})
		log.Printf("memory: sweeper interval=%s", cfg.Env.MemorySweepInterval)
	} else {
		log.Printf("memory: sweeper disabled (LOOMCYCLE_MEMORY_SWEEP_MS=0 or no Store)")
	}

	// Drained consolidation-queue rows. An ack only stamps drained_at, so
	// without this the queue keeps every row ever enqueued, raw chat payload
	// included. Hourly and advisory-gated like the change-feed prune below.
	if ttl := cfg.Env.MemoryPendingDrainedTTL; ttl > 0 && storeIface != nil {
		go runAdvisoryGatedSweeper(bgCtx, time.Hour, advisoryLock, coord.LockKeyMemoryPendingSweeper, "memory_pending",
			func(ctx context.Context) {
				pruned, err := memory.PruneDrainedPending(ctx, storeIface, ttl, time.Now(), memory.PendingPruneBatch)
				if err != nil {
					log.Printf("memory_pending prune: %v", err)
				}
				if pruned > 0 {
					log.Printf("memory_pending prune: deleted %d row(s) drained more than %s ago", pruned, ttl)
				}
			})
		log.Printf("memory: drained-queue prune interval=1h ttl=%s", ttl)
	} else if storeIface != nil {
		log.Printf("memory: drained-queue prune disabled (LOOMCYCLE_MEMORY_PENDING_DRAINED_TTL_MS=0)")
	}

	// RFC CD Part C — prune the change-feed table so the opt-in feed stays
	// bounded. Only when the feed is enabled; the DELETE is idempotent and
	// advisory-gated so one replica prunes per tick.
	if memoryChangesEnabled() && storeIface != nil {
		window := memoryChangesRetention()
		go runAdvisoryGatedSweeper(bgCtx, time.Hour, advisoryLock, coord.LockKeyMemoryChangesSweeper, "memory_changes",
			func(ctx context.Context) {
				pruned, err := storeIface.PruneMemoryChanges(ctx, time.Now().Add(-window))
				if err != nil {
					log.Printf("memory_changes prune: %v", err)
					return
				}
				if pruned > 0 {
					log.Printf("memory_changes prune: deleted %d row(s) older than %s", pruned, window)
				}
			})
		log.Printf("memory: change-feed retention sweeper interval=1h window=%s", window)
	}

	// RFC CD Part C (push) — deliver change events to operator-declared HTTP
	// callbacks. Only when the feed is enabled AND subscriptions are declared;
	// advisory-gated so ONE replica delivers per tick (the persisted cursor is
	// shared). Each subscription's callback is dialed through its own
	// SSRF-guarded client (private hosts blocked unless allow_private_host).
	if memoryChangesEnabled() && storeIface != nil && len(cfg.ChangeSubscriptions) > 0 {
		subs := resolveChangeSubscriptions(cfg)
		if len(subs) > 0 {
			deliverer := changesub.New(storeIface, resolveChangeSubSecret, log.Printf)
			interval := changeSubDeliveryInterval()
			go runAdvisoryGatedSweeper(bgCtx, interval, advisoryLock, coord.LockKeyChangeSubDelivery, "change_subscriptions",
				func(ctx context.Context) { deliverer.RunOnce(ctx, subs) })
			log.Printf("memory: change-subscription push engine — %d subscription(s), interval=%s", len(subs), interval)
		}
	}

	// Dead-link reconciliation: the only layer that actually COLLECTS chunk
	// references nothing can reach.
	//
	// The other two layers are incomplete by construction. delete_chunk cleans every
	// reference, but the body delete runs after the transaction commits because it
	// is a different store — so a crash in that window leaves an orphan. And the
	// read-time guard HIDES an orphan, which is right for a read and is exactly why
	// nothing ever notices one is there. An out-of-band delete bypasses both.
	//
	// On by default, unlike retention, because everything it removes is unreachable:
	// it restores an invariant rather than enacting a policy.
	if cfg.Env.DeadLinkGCInterval > 0 && storeIface != nil && sqlMemMgr != nil {
		docTool := &builtin.Document{Store: storeIface, SqlMem: sqlMemMgr, Cfg: cfg}
		go runAdvisoryGatedSweeper(bgCtx, cfg.Env.DeadLinkGCInterval, advisoryLock, coord.LockKeyDeadLinkGC, "deadlink",
			func(ctx context.Context) {
				scopes, err := sqlMemMgr.ListScopes(ctx)
				if err != nil {
					log.Printf("deadlink gc: list scopes: %v", err)
					return
				}
				for _, sk := range scopes {
					// A run scope is dropped whole at run end, so reconciling it is
					// work with no beneficiary — and it has no durable body partition
					// to reconcile against.
					ms, ok := deadLinkMemoryScope(sk.Scope)
					if !ok {
						continue
					}
					rep, err := docTool.ReconcileDeadLinks(ctx, sk, ms, cfg.Env.DeadLinkGCDryRun)
					if err != nil {
						// Per-scope: one unreadable scope must not stop the sweep.
						log.Printf("deadlink gc: %s/%s/%s: %v", sk.Tenant, sk.Scope, sk.ScopeID, err)
						continue
					}
					if !rep.Empty() {
						verb := "collected"
						if cfg.Env.DeadLinkGCDryRun {
							verb = "would collect"
						}
						log.Printf("deadlink gc: %s %s", verb, rep)
					}
				}
			})
		log.Printf("deadlink gc: interval=%s dry_run=%v", cfg.Env.DeadLinkGCInterval, cfg.Env.DeadLinkGCDryRun)
	} else if storeIface != nil && sqlMemMgr == nil {
		log.Printf("deadlink gc: disabled (no SQL Memory — no chunk tables to reconcile)")
	} else {
		log.Printf("deadlink gc: disabled (LOOMCYCLE_DEADLINK_GC_MS=0 or no Store)")
	}

	// Channel tool TTL sweeper (v0.8.4). Same shape as MemorySweeper.
	if cfg.Env.ChannelsSweepInterval > 0 && storeIface != nil {
		go runAdvisoryGatedSweeper(bgCtx, cfg.Env.ChannelsSweepInterval, advisoryLock, coord.LockKeyChannelsSweeper, "channels",
			func(ctx context.Context) {
				swept, err := storeIface.ChannelSweepExpired(ctx)
				if err != nil {
					log.Printf("channels sweep: %v", err)
					return
				}
				if swept > 0 {
					log.Printf("channels sweep: deleted %d expired row(s)", swept)
				}
			})
		log.Printf("channels: sweeper interval=%s", cfg.Env.ChannelsSweepInterval)
	} else if storeIface != nil {
		log.Printf("channels: sweeper disabled (LOOMCYCLE_CHANNELS_SWEEP_MS=0)")
	}

	// v0.8.16 Interruption tool TTL sweeper. Marks pending rows
	// whose expires_at < now as timed_out.
	if cfg.Env.ChannelsSweepInterval > 0 && storeIface != nil {
		go runAdvisoryGatedSweeper(bgCtx, cfg.Env.ChannelsSweepInterval, advisoryLock, coord.LockKeyInterruptsSweeper, "interrupts",
			func(ctx context.Context) {
				swept, err := storeIface.InterruptSweepExpired(ctx)
				if err != nil {
					log.Printf("interrupts sweep: %v", err)
					return
				}
				if swept > 0 {
					log.Printf("interrupts sweep: timed_out %d expired row(s)", swept)
				}
			})
		log.Printf("interrupts: sweeper interval=%s", cfg.Env.ChannelsSweepInterval)
	}

	// v0.8.x process-resource metrics sampler. Default OFF;
	// operator opts in via LOOMCYCLE_METRICS_ENABLED=1. When
	// enabled, samples loomcycle's CPU + memory usage at
	// cfg.Env.MetricsSampleInterval cadence while at least one
	// agent run is active (the semaphore's Stats() is the idle
	// gate — no DB write, no /proc read while idle).
	// v0.8.17 PR 4: pause/resume/state HTTP endpoints. The Manager is
	// always constructed (cheap; one atomic + one channel) so the
	// endpoints respond rather than 503. Operator's default timeout
	// comes from cfg.Env.PauseDefaultTimeoutMs; 0 ⇒ pause.DefaultPauseTimeout.
	// pauseMgrRef stays nil unless storeIface is non-nil; the scheduler
	// branch below uses it to gate its tick on runtime state.
	var pauseMgrRef *pause.Manager
	if storeIface != nil {
		pauseDefault := time.Duration(cfg.Env.PauseDefaultTimeoutMs) * time.Millisecond
		pauseMgr := pause.NewManager(storeIface, pauseDefault)
		pauseMgrRef = pauseMgr
		srv.SetPauseManager(pauseMgr)
		if pauseDefault > 0 {
			log.Printf("pause: manager wired (default timeout=%s)", pauseDefault)
		} else {
			log.Printf("pause: manager wired (default timeout=%s)", pause.DefaultPauseTimeout)
		}

		// v0.12.3 Phase 4: cluster-wide pause/resume. The pause
		// manager's cluster paths read/write a singleton runtime_state
		// row + publish on `loomcycle.pause`. Only active in cluster
		// mode; single-replica mode keeps the v0.11.x in-process path
		// byte-identical.
		if cfg.Env.ReplicaID != "" {
			pgStoreForPause, ok := asPostgresStore(storeIface)
			if !ok {
				log.Fatalf("coord: pause cluster wiring requires Postgres store (REPLICA_ID set + non-Postgres store — should have been caught by openStore)")
			}
			rss := coord.NewRuntimeStateStore(pgStoreForPause.Pool())
			cacheTTL := time.Duration(cfg.Env.PauseCacheTTLMs) * time.Millisecond
			pauseMgr.SetRuntimeStateStore(rss, cacheTTL)
			// Reuse the same Backplane the cluster block constructed.
			// We can't directly reference `bp` here because it's
			// scoped to the cluster block above; instead read it via
			// srv's existing wiring (SetCoord stashed it).
			// Backplane is interface-typed; type-assert through the
			// known-only path via a separate fetch.
			//
			// Simpler approach: rather than re-fetch, do the pause
			// cluster wiring INSIDE the cluster block where `bp` is
			// in scope. We can't because pauseMgr isn't constructed
			// yet at that point. Solution: declare a package-private
			// hook on Server to retrieve the backplane.
			if bp := srv.Backplane(); bp != nil {
				pauseMgr.SetBackplane(bp)
				if err := pauseMgr.SubscribeBackplane(bgCtx, bp); err != nil {
					log.Fatalf("coord: pause backplane subscribe: %v", err)
				}
				log.Printf("coord: cluster pause/resume wired (cache_ttl=%s)", cacheTTL)
			}
		}
	}

	// F42 / RFC X Phase 2: re-dispatch any pause_state='paused' runs left in
	// the store at boot (crash recovery / restart on a restored data dir) by
	// reconstructing each run's loop from its transcript. One-shot, run after
	// the server is fully wired (providers/tools/cancel/steer/pause). In a
	// cluster, gate behind an advisory lock so exactly ONE replica resurrects
	// each run; single-replica runs it directly. The lock only orders booting
	// replicas: a run whose loop is still alive on another replica (parked at
	// a cluster pause) is left alone by ResumePausedRuns itself, which checks
	// the owner's heartbeat. Backgrounded so a long-lived resumed loop never
	// blocks the rest of boot.
	if storeIface != nil {
		// A no-trace run has nothing stored to resume from, so the ones this
		// process was running when it last stopped are lost. Say so on their
		// rows and release their idempotency keys, so a caller's retry runs
		// the work instead of being answered with a run that cannot finish.
		// Synchronous and first: nothing may resume or retry before it.
		if n, err := srv.FailLostNoTraceRuns(bgCtx, processStartedAt); err != nil {
			log.Printf("no-trace: could not fail the runs lost in the last stop: %v", err)
		} else if n > 0 {
			log.Printf("no-trace: failed %d run(s) lost in the last stop (not resumable); their idempotency keys are released", n)
		}
		// A run still queued for a slot when this process stopped lost its
		// wait with it. Each is queued again, in its old order, from the
		// request stored when it queued; one that cannot be (no request, or
		// sealed secrets this key cannot open) is failed with the reason and
		// its idempotency keys released.
		if n, failed, err := srv.RestartQueuedRuns(bgCtx, processStartedAt); err != nil {
			log.Printf("queue: could not restart the runs queued when loomcycle last stopped: %v", err)
		} else if n > 0 || failed > 0 {
			log.Printf("queue: %d run(s) queued when loomcycle last stopped are queued again; %d could not be and were failed", n, failed)
		}
		go func() {
			resume := func(ctx context.Context) error {
				n, warnings := srv.ResumePausedRuns(ctx)
				if n > 0 || len(warnings) > 0 {
					log.Printf("resume(boot): re-dispatched %d paused run(s); %d flagged", n, len(warnings))
				}
				return nil
			}
			if advisoryLock != nil {
				if _, err := advisoryLock.TryRun(bgCtx, coord.LockKeyResumePausedRuns, resume); err != nil {
					log.Printf("resume(boot): advisory-gated resume failed: %v", err)
				}
			} else {
				_ = resume(bgCtx)
			}
		}()
	}

	// RFC CY L5: the armed-subscription sweep. A PROMOTED team whose entry
	// state is a Starter runs on its own when its source channel has work.
	//
	// DEFAULT OFF, deliberately. This is the only part of the runtime that
	// starts agent runs unprompted — turning it on turns the definition plane
	// into a spend commitment, because every message on a subscribed source
	// becomes a wave of runs. An operator should arrive at that on purpose.
	//
	// The sweep needs no arm/disarm bookkeeping: each tick asks the store which
	// teams are promoted, so promote arms and retire/delete disarm with no
	// runtime state to leak or reconcile after a crash.
	if cfg.Env.TeamSubscriptions && srv != nil && storeIface != nil {
		if advisoryLock != nil {
			// Cluster singleton per TEAM (not per process): exactly one replica
			// drives a given team at a time, and the lock is held across the
			// whole walk so a second tick cannot start a concurrent one.
			srv.SetAdvisoryLock(advisoryLock)
		}
		tick := time.Duration(cfg.Env.TeamSubscriptionsTickSeconds) * time.Second
		go func() {
			t := time.NewTicker(tick)
			defer t.Stop()
			for {
				select {
				case <-bgCtx.Done():
					return
				case <-t.C:
					started, err := srv.SweepTeamSubscriptions(bgCtx)
					if err != nil {
						log.Printf("team-subscriptions: sweep: %v", err)
						continue
					}
					if started > 0 {
						log.Printf("team-subscriptions: started %d walk(s)", started)
					}
				}
			}
		}()
		log.Printf("team-subscriptions: enabled (tick=%s, cluster_gated=%v)", tick, advisoryLock != nil)
	}

	// RFC BL P1: reconcile the boot-time help-topic search index. The go:embed
	// help corpus is immutable in-process (no hot-reload), so this is one-shot at
	// boot. Content-hash gated — an unchanged corpus re-embeds nothing; only
	// changed/new sections are re-embedded and removed sections pruned. No-op
	// without an embedder or vector support (help search then degrades to an
	// in-memory substring scan). Backgrounded so a first-boot full embed never
	// blocks the rest of boot; advisory-gated in a cluster so exactly ONE replica
	// re-embeds. Non-fatal: on any error help search just degrades.
	// A static agent added since a team was written can share a name with one
	// of that team's own agents; say so at boot rather than at the first walk.
	// Backgrounded and bounded: it reads every active team, and how many there
	// are — and how many agents each declares — is the tenants' to decide, so
	// it must not be able to hold up boot.
	if storeIface != nil {
		go func() {
			ctx, cancel := context.WithTimeout(bgCtx, 30*time.Second)
			defer cancel()
			srv.LogTeamLocalNameClashes(ctx)
		}()
	}

	if storeIface != nil && helpSet != nil {
		go func() {
			reconcile := func(ctx context.Context) error {
				// Indexing the help docs is maintenance: behind live runs.
				stats, err := help.ReconcileIndex(lanegate.WithBackground(ctx), helpSet, storeIface, embedder)
				if err != nil {
					log.Printf("help index: reconcile failed (search degrades): %v", err)
					return nil // non-fatal — degrade, don't fail boot
				}
				switch {
				case stats.Degraded:
					log.Printf("help index: no embedder/vector support — query uses in-memory substring scan")
				case stats.Written > 0 || stats.Pruned > 0 || stats.Failed > 0:
					log.Printf("help index: reconciled (%d written, %d pruned, %d unchanged, %d failed)", stats.Written, stats.Pruned, stats.Unchanged, stats.Failed)
				default:
					log.Printf("help index: up to date (%d sections, 0 embed calls)", stats.Unchanged)
				}
				return nil
			}
			if advisoryLock != nil {
				if _, err := advisoryLock.TryRun(bgCtx, coord.LockKeyHelpIndexReconcile, reconcile); err != nil {
					log.Printf("help index: advisory-gated reconcile failed: %v", err)
				}
			} else {
				_ = reconcile(bgCtx)
			}
		}()
	}

	if cfg.Env.MetricsEnabled && storeIface != nil {
		metricsSampler := metrics.New(storeIface, sem, metrics.Config{
			Interval:      cfg.Env.MetricsSampleInterval,
			CollectSystem: cfg.Env.MetricsCollectSystem,
			ReplicaID:     cfg.Env.ReplicaID,
		})
		go metricsSampler.Run(bgCtx)
		srv.SetMetricsSampler(metricsSampler)
		log.Printf("metrics: sampler enabled (interval=%s, system=%v)",
			cfg.Env.MetricsSampleInterval, cfg.Env.MetricsCollectSystem)
		if cfg.Env.MetricsSweepInterval > 0 && cfg.Env.MetricsRetentionDays > 0 {
			retentionDays := cfg.Env.MetricsRetentionDays
			go runAdvisoryGatedSweeper(bgCtx, cfg.Env.MetricsSweepInterval, advisoryLock, coord.LockKeyMetricsSweeper, "metrics",
				func(ctx context.Context) {
					cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
					swept, err := storeIface.MetricsSweep(ctx, cutoff)
					if err != nil {
						log.Printf("metrics sweep: %v", err)
						return
					}
					if swept > 0 {
						log.Printf("metrics sweep: deleted %d expired row(s)", swept)
					}
				})
			log.Printf("metrics: sweeper enabled (retention=%dd, interval=%s)",
				cfg.Env.MetricsRetentionDays, cfg.Env.MetricsSweepInterval)
		} else {
			log.Printf("metrics: sweeper disabled (retention=0 or sweep interval=0)")
		}
	} else if cfg.Env.MetricsEnabled {
		log.Printf("metrics: sampler disabled (no Store backend)")
	} else {
		log.Printf("metrics: sampler disabled (LOOMCYCLE_METRICS_ENABLED=0)")
	}

	// v1.x RFC E scheduler runtime. Default OFF; operator opts in via
	// LOOMCYCLE_SCHEDULER_ENABLED=1. Sweeper goroutine fires due
	// schedules from the substrate (schedule_run_state table). NIL
	// MCPCaller is passed because mcp.call hook dispatch is wired in
	// a follow-up PR — channel.publish + memory.set hooks work today.
	if cfg.Env.SchedulerEnabled && storeIface != nil && srv != nil {
		envAllowlist := cfg.SchedulerCredentialEnvAllowlist()
		schedCfg := scheduler.Config{
			TickInterval: time.Duration(cfg.Env.SchedulerTickSeconds) * time.Second,
			EnvAllowlist: envAllowlist,
			// RFC BL P2 consolidation fan-out caps. Zero here means "use the
			// scheduler's documented defaults" (Config.defaults()), so an operator
			// who sets neither env var still gets 32 targets / 4 concurrent.
			MaxConsolidationTargets:     cfg.Env.MaxConsolidationTargets,
			MaxConsolidationConcurrency: cfg.Env.MaxConsolidationConcurrency,
			// The agents declared `internal:` — the fan-out neither reads their
			// sessions as evidence of new work nor hands them to a pass. Passed
			// as names because internal/scheduler takes no config dependency.
			InternalAgents: cfg.InternalAgentNames(),
			// The same gate the runtime enforces: while it is on, an
			// operator-layer sweep's runs in other tenants may not spend the
			// operator's provider key.
			OperatorKeyRestriction: cfg.Env.OperatorKeyRestriction,
			// Names this replica on the slots it claims. Every replica may run
			// the scheduler: the slot claim fires each slot once.
			ReplicaID: cfg.Env.ReplicaID,
		}
		// Materialize static yaml `scheduled_runs:` into the substrate so
		// they fire autonomously — symmetric with dynamically-created
		// schedules (which seed run_state on promoted create). The sweeper's
		// due-query is substrate-only, so without this a yaml-only schedule
		// never fires. Idempotent + fork-respecting; runs before the sweeper
		// so the first tick already sees the seeded rows. A minimal tool
		// instance suffices (bootstrap needs only Store + Cfg).
		bootSched := &builtin.ScheduleDef{Store: storeIface, Cfg: cfg}
		bootCtx := tools.WithRunIdentity(bgCtx, tools.RunIdentityValue{AgentID: "scheduler-bootstrap"})
		if n, err := bootSched.BootstrapStaticSchedules(bootCtx); err != nil {
			log.Printf("scheduler: static-schedule bootstrap: %v (continuing)", err)
		} else if n > 0 {
			log.Printf("scheduler: materialized %d static schedule(s) into the substrate", n)
		}
		// Read-only: name any stored schedule whose runs execute outside its
		// owning tenant (a non-admin could author one before the guard).
		if warns, err := bootSched.ForeignExecTenantWarnings(bootCtx); err != nil {
			log.Printf("scheduler: execution-tenant audit: %v (continuing)", err)
		} else {
			for _, warn := range warns {
				log.Printf("scheduler: WARNING: %s", warn)
			}
		}

		// srv satisfies runner.Runner via its RunOnce method (the same
		// seam HTTP + gRPC drive interactive runs through). pauseMgr is
		// guaranteed non-nil in this branch (storeIface != nil ⇒ pause
		// manager constructed earlier in this function).
		sched := scheduler.New(schedCfg, storeIface, srv, pauseMgrRef, nil, log.Printf)
		// Resolve on_complete: channel.publish to the channel's DECLARED scope
		// (F37 / RFC T) so a hook on a scope:global channel lands at global,
		// not under the run's user scope. Must be set before Start.
		// Adapt the server's channel def to the scheduler's DeclaredChannel:
		// the two packages meet here rather than importing each other for the
		// sake of one struct.
		sched.SetChannelScope(func(ctx context.Context, tenantID, name string) (scheduler.DeclaredChannel, bool) {
			def, ok := srv.ResolveChannelScope(ctx, tenantID, name)
			if !ok {
				return scheduler.DeclaredChannel{}, false
			}
			return scheduler.DeclaredChannel{
				Scope:       def.Scope,
				DefaultTTL:  def.DefaultTTL,
				MaxMessages: def.MaxMessages,
			}, true
		})
		// A schedule's channel writes go through the channel writer, which
		// decides from the channel's definition whether they are held.
		sched.SetChannelWriter(sysPublisher)
		// A `delivery: team` tick starts its walk through the server, which
		// owns the walk — the same instance that runs a tick's agent run.
		sched.SetTeamWalkStarter(srv)
		// RFC DZ: a concurrency_policy: replace slot cancels the schedule's
		// running run through the server, on whichever replica runs it.
		sched.SetRunCanceller(srv)
		// RFC BL P2 consolidation fan-out: the provider resolver decides
		// parallel-vs-serial dispatch (a local model runtime is serialized), and
		// the advisory lock makes exactly one replica per tick enumerate the
		// targets — otherwise every replica would dispatch a full fan-out and
		// burn N× the tokens. advisoryLock is nil in single-replica mode, which
		// leaves the fan-out correctly unguarded; guard the assignment so a
		// typed-nil never reaches the non-nil interface check.
		sched.SetProviderResolver(srv)
		if advisoryLock != nil {
			sched.SetFanoutCoordination(advisoryLock, coord.MemoryConsolidatorLockKey)
			// RFC DZ: one replica per tick finishes the scheduled runs whose
			// replica died. Only saves work — a run is finished once whoever
			// tries — so single-replica leaves it unguarded.
			sched.SetReconcileCoordination(advisoryLock, coord.LockKeyScheduleReconcile)
		}
		sched.Start(bgCtx)
		log.Printf("scheduler: enabled (tick=%ds, env_allowlist=%d names)",
			cfg.Env.SchedulerTickSeconds, len(cfg.Env.SchedulerEnvAllowlist))
	} else if cfg.Env.SchedulerEnabled {
		log.Printf("scheduler: disabled (no Store backend or no HTTP server)")
	} else {
		log.Printf("scheduler: disabled (LOOMCYCLE_SCHEDULER_ENABLED=0)")
	}

	// Boot summary of operator-declared channels. Mirrors the
	// "user_tiers: configured N — ..." line shape so operators see
	// the framework-primitive state at startup.
	if n := len(cfg.Channels); n > 0 {
		names := make([]string, 0, n)
		for name := range cfg.Channels {
			names = append(names, name)
		}
		sort.Strings(names)
		log.Printf("channels: configured %d — %s", n, strings.Join(names, " / "))
	}

	// Session-lock map GC. Defaults: prune entries idle ≥ 10 min, on
	// a 5-min tick. Disabled when both interval and max-idle resolve
	// to zero (operator can opt out by setting both to 0).
	sessionGCInterval := cfg.Env.SessionLockGCInterval
	if sessionGCInterval <= 0 {
		sessionGCInterval = 5 * time.Minute
	}
	sessionGCMaxIdle := cfg.Env.SessionLockMaxIdle
	if sessionGCMaxIdle <= 0 {
		sessionGCMaxIdle = 10 * time.Minute
	}
	go srv.RunSessionLockGC(bgCtx, sessionGCInterval, sessionGCMaxIdle)
	// RFC BK: idle-reap resident interactive sub-agents (per-replica; in-process
	// registry, no cluster coordination needed).
	go srv.RunResidentSweeper(bgCtx)

	// v0.8.15: LoomCycle MCP — when launched via `loomcycle mcp --config Y`,
	// expose the runtime as an MCP server over stdio alongside the HTTP
	// listener. Per the RFC, both surfaces run together; the HTTP server
	// keeps serving /healthz, /v1/*, and the Web UI while MCP clients
	// (Claude Code first) drive the same business logic through
	// Connector dispatch.
	//
	// The dynamic-agent TTL sweeper runs in BOTH default and mcp modes:
	// the dynamic_agents table is shared. operators who never use the
	// `register_agent` MCP tool simply have an empty table the sweeper
	// passes over with zero rows deleted.
	if cfg.Env.DynamicAgentSweepInterval > 0 {
		go runAdvisoryGatedSweeper(bgCtx, cfg.Env.DynamicAgentSweepInterval, advisoryLock, coord.LockKeyDynamicAgentSweeper, "dynamic_agents",
			func(ctx context.Context) {
				n, err := storeIface.DynamicAgentSweep(ctx)
				if err != nil {
					log.Printf("dynamic_agents sweep: %v", err)
					return
				}
				if n > 0 {
					log.Printf("dynamic_agents sweep: deleted %d expired row(s)", n)
				}
			})
		log.Printf("dynamic_agents: sweeper enabled (interval=%s)", cfg.Env.DynamicAgentSweepInterval)
	}

	// RFC AH Phase 2b ephemeral-volume crash-recovery sweeper. The inline
	// run-completion purge is the primary path; this backstops a host that
	// crashed before the inline purge ran. Each tick lists (root_run_id,
	// tenant_id) of TERMINAL-and-NOT-paused runs that still own ephemeral
	// rows, then runs the SAME fenced RemoveAll the inline purge uses +
	// deletes the rows. Cluster-gated so one replica per tick does the work.
	// Needs a dynamic_root to have anything to clean (no root → no ephemeral
	// volumes); skipped otherwise.
	if dynRoot, hasDynRoot := builtin.DynamicVolumeRoot(cfg); cfg.Env.EphemeralVolumeSweepInterval > 0 && storeIface != nil && hasDynRoot {
		go runAdvisoryGatedSweeper(bgCtx, cfg.Env.EphemeralVolumeSweepInterval, advisoryLock, coord.LockKeyEphemeralVolumeSweeper, "ephemeral_volumes",
			func(ctx context.Context) {
				cands, err := storeIface.EphemeralVolumeSweepCandidates(ctx)
				if err != nil {
					log.Printf("ephemeral_volumes sweep: candidates: %v", err)
					return
				}
				purged := 0
				for _, c := range cands {
					// Fenced RemoveAll of the per-run subtree (re-derived;
					// never a stored path). removed=false → already gone.
					if _, perr := builtin.PurgeEphemeralRunTree(dynRoot, c.RootRunID, "ephemeral sweeper"); perr != nil {
						log.Printf("ephemeral_volumes sweep: purge run=%s: %v", c.RootRunID, perr)
						// Still drop the rows below — a purge fault must not
						// wedge the row forever (the dir may be partly gone).
					}
					if _, derr := storeIface.EphemeralVolumeDeleteByRun(ctx, c.RootRunID); derr != nil {
						log.Printf("ephemeral_volumes sweep: delete rows run=%s: %v", c.RootRunID, derr)
						continue
					}
					purged++
				}
				if purged > 0 {
					log.Printf("ephemeral_volumes sweep: purged %d terminal run tree(s)", purged)
				}
			})
		log.Printf("ephemeral_volumes: sweeper enabled (interval=%s, dynamic_root=%s)", cfg.Env.EphemeralVolumeSweepInterval, dynRoot)
	}
	// Optional operator default for the spawn_run transport timeout
	// (RFC P). 0/unset → disabled (defer to the run's own budget).
	// Read outside the mcpMode block: the HTTP MCP transport below is
	// always wired (both server and mcp modes) and the RFC R thin client
	// (`mcp --upstream`) proxies to it, so it needs the same bound.
	mcpSpawnRunTimeoutMS := 0
	if v := os.Getenv("LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			mcpSpawnRunTimeoutMS = n
		}
	}
	if mcpMode {
		// Optional operator override for the stdio dispatch concurrency
		// cap (RFC O). 0/unset → the package default (16).
		mcpMaxConcurrentCalls := 0
		if v := os.Getenv("LOOMCYCLE_MCP_MAX_CONCURRENT_CALLS"); v != "" {
			if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
				mcpMaxConcurrentCalls = n
			}
		}
		mcpSrv := lcmcp.New(lcmcp.Config{
			Connector:          srv, // *http.Server satisfies connector.Connector
			Runner:             srv, // *http.Server satisfies runner.Runner (streaming)
			Store:              storeIface,
			Logf:               log.Printf, // log goes to stderr; never stdout (stdout is the JSON-RPC wire)
			ServerName:         "loomcycle",
			ServerVersion:      buildVersion,
			MaxConcurrentCalls: mcpMaxConcurrentCalls,
			SpawnRunTimeoutMS:  mcpSpawnRunTimeoutMS,
		})
		// Run the MCP stdio loop in a goroutine. When stdin closes
		// (Claude Code disconnects), Serve returns; we log and let
		// the HTTP listener keep serving until the process is sent
		// a signal. Operators who want MCP-disconnect = process-exit
		// can wrap loomcycle in a supervisor that watches for the
		// log line.
		go func() {
			// Sourced from the registry (lcmcp.MetaToolCount) so the count
			// can't drift — the hardcoded "20" went stale once the registry
			// grew past 40. Same rationale as the CLI help count.
			log.Printf("mcp: stdio server starting (%d tools registered)", lcmcp.MetaToolCount())
			if err := mcpSrv.Serve(bgCtx, os.Stdin, os.Stdout); err != nil {
				log.Printf("mcp: serve ended: %v", err)
			} else {
				log.Printf("mcp: stdio closed; HTTP listener continues")
			}
		}()
	}

	// v0.8.15.3: HTTP MCP transport. Always wired (both server and
	// mcp modes), per RFC decision C-open-question rationale:
	// service-to-service consumers don't use the stdio wrapper and
	// need a network endpoint. Security posture is identical to all
	// other /v1/* endpoints — bearer-authed via s.authMiddleware in
	// the server's Mux(). This /v1/_mcp endpoint is what the RFC R
	// thin client (`loomcycle mcp --upstream`) proxies to.
	mcpHTTPHandler := lcmcp.NewHTTPHandler(lcmcp.Config{
		Connector:     srv, // same *http.Server instance used by stdio MCP
		Runner:        srv,
		Store:         storeIface,
		Logf:          log.Printf,
		ServerName:    "loomcycle",
		ServerVersion: buildVersion,
		// RFC P spawn_run timeout must apply to the HTTP transport too —
		// the RFC R thin client (`mcp --upstream`) proxies to /v1/_mcp, so
		// without this the now-recommended topology would have no spawn_run
		// bound (the exp3 wedge class, one layer up). MaxConcurrentCalls is
		// intentionally NOT passed: it builds the stdio Serve loop's
		// semaphore and is meaningless here — the HTTP transport is already
		// per-request concurrent at the http.Server level, and run
		// concurrency is bounded downstream by the runtime semaphore +
		// per-tenant fairness.
		SpawnRunTimeoutMS: mcpSpawnRunTimeoutMS,
	})
	srv.SetMCPHTTPHandler(mcpHTTPHandler)
	// Background session sweeper. Reuses the dynamic-agent sweeper
	// pattern: bound to bgCtx, exits on shutdown, idempotent under
	// concurrent invocations. 5-minute cadence is well under the
	// 30-min session inactivity TTL — leaves at least ~25 min between
	// a session's last access and its earliest possible reaping.
	lcmcp.RunHTTPSessionSweeper(bgCtx, mcpHTTPHandler.Sessions(), 5*time.Minute, log.Printf)
	log.Printf("mcp: HTTP transport enabled at POST /v1/_mcp (session sweeper interval=5m, TTL=30m)")

	log.Printf("session-lock GC: interval=%s max_idle=%s", sessionGCInterval, sessionGCMaxIdle)

	// Periodic probe loop: re-runs the resolver's reachability +
	// model-list probe on the configured cadence so a provider
	// recovering from an outage shows up in the matrix without a
	// loomcycle restart. Default 15 minutes; clamped to [60s, 1h] in
	// the config loader. The first probe already ran inside
	// buildResolver above (synchronously, before traffic begins) —
	// this goroutine handles all subsequent rounds.
	probeInterval := cfg.Env.ResolveProbeInterval
	if probeInterval <= 0 {
		probeInterval = 15 * time.Minute
	}
	// v0.8.17: register the force-probe callback. The snapshot restore
	// handler (POST /v1/_snapshots/{id}/restore) calls
	// resolver.ForceProbe(ctx) to refresh the matrix before the
	// operator can call Resume — the pause-resume-snapshot RFC says
	// the resolver state is excluded from snapshots, so a fresh probe
	// closes the gap.
	resolver.SetForceProbeCallback(func(ctx context.Context) {
		runResolveProbeOnce(ctx, resolver, pr, cfg)
	})
	go runResolveProbeLoop(bgCtx, resolver, pr, cfg, probeInterval)
	log.Printf("resolve probe: interval=%s", probeInterval)

	// RFC CK — wire the runtime config-reload registry now that the resolver,
	// provider set, and background ctx exist. Each SectionReloader rebuilds its
	// subsystem IN PLACE from a validated candidate (the Server's handle pointers
	// never change, so the probe loop + all readers keep working). load re-runs
	// LoadLayers over the SAME captured layer stack (disk files re-read fresh).
	// POST /v1/_config/reload drives it.
	reloaders := []lchttp.SectionReloader{
		{
			// Provider set + resolver policy.
			Sections: []string{"providers", "models", "tiers", "provider_priority"},
			Reload: func(newCfg *config.Config) error {
				if err := pr.Reload(newCfg); err != nil {
					return err
				}
				resolver.ReloadPolicy(newCfg.ProviderPriority, convertTiers(newCfg.Tiers, newCfg.Models))
				// runResolveProbeOnce reads pr.currentCfg() (now the reloaded config),
				// so this immediately probes the possibly-changed provider set.
				runResolveProbeOnce(bgCtx, resolver, pr, newCfg)
				return nil
			},
		},
	}
	if cfg.Env.SchedulerEnabled {
		// Static scheduled_runs: re-materialize into the substrate (idempotent
		// add/change; a yaml-REMOVED schedule keeps its row by design — see
		// BootstrapStaticSchedules). The sweeper polls the store, so re-bootstrap
		// alone suffices; the scheduler's own tick/allowlist knobs stay boot-fixed.
		reloaders = append(reloaders, lchttp.SectionReloader{
			Sections: []string{"scheduled_runs"},
			Reload: func(newCfg *config.Config) error {
				bs := &builtin.ScheduleDef{Store: storeIface, Cfg: newCfg}
				_, err := bs.BootstrapStaticSchedules(bgCtx)
				return err
			},
		})
	}
	// Concurrency: global semaphore caps + per-user caps + the providers' call
	// lanes. Owns `providers` too because the lanes are built from
	// providers[*].max_concurrent / lanes / max_models_in_flight, and `models`
	// because a lane's model_caps key may be an alias. LAST, because it cannot
	// fail: a reloader that fails after it would leave its new caps live while
	// the config holder keeps the old ones, and a later reload back to the old
	// values would then see no change to apply.
	reloaders = append(reloaders, lchttp.SectionReloader{
		Sections: []string{"concurrency", "providers", "models"},
		Reload:   func(newCfg *config.Config) error { srv.ReloadConcurrency(newCfg); return nil },
	})
	// The reload LOADER re-runs the full boot assembly (re-glob siblings, re-read
	// CONFIG_DIR/CONFIG_FILES) rather than replaying the captured boot `layers`, so
	// a config or section file added/removed since boot takes effect on reload
	// without a restart. verbose=false: don't re-log the layering on every reload.
	srv.SetConfigReloader(func() (*config.Config, error) {
		reloadLayers, err := assembleConfigLayers(b.presetFlags, b.cfgPaths, false)
		if err != nil {
			return nil, err
		}
		return config.LoadLayers(reloadLayers...)
	}, reloaders...)

	go func() {
		log.Printf("loomcycle listening on %s", cfg.Env.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	// gRPC server. Optional; opt-in via LOOMCYCLE_GRPC_ADDR. Reuses
	// the same Store + cancel registry as the HTTP server so both
	// surfaces give identical answers. PR 1 of v0.5.5 implements
	// metadata RPCs only (Run/Continue stub to Unimplemented); PR 2
	// wires the streaming surface.
	var grpcSrv *googlegrpc.Server
	if cfg.Env.GrpcAddr != "" {
		grpcAdapter := loomgrpc.New(loomgrpc.Config{
			Store:     storeIface,
			CancelReg: srv.CancelRegistry(),
			Connector: srv, // v0.8.15: *http.Server satisfies connector.Connector
			Runner:    srv, // *http.Server satisfies runner.Runner (streaming)
			// RFC AW: share the HTTP server's token-budget tracker so the gRPC
			// TokenLimit RPC reads the identical live counters + reloads the same
			// cache the /v1/_limits handler uses.
			Limits:    srv.LimitsTracker(),
			AuthToken: cfg.Env.AuthToken,
			// RFC L: reuse the HTTP server's token resolution so gRPC
			// stamps the same authoritative principal (gRPC runs flow
			// authoritatively through RunOnce) and shares the open-mode
			// decision.
			PrincipalResolver: srv.ResolvePrincipal,
			AuthConfigured:    srv.AuthConfigured,
			BuildCommit:       buildCommit,
			BuildTime:         buildTime,
			// GetRun's `live` must see a team walk, which only the HTTP
			// server's walk table knows about.
			RunLive: srv.RunLive,
			// RFC EI P3: where a queued run waits, from this replica's queue.
			QueuePosition: srv.QueuePosition,
		})
		grpcSrv = googlegrpc.NewServer(
			googlegrpc.UnaryInterceptor(grpcAdapter.UnaryAuthInterceptor()),
			googlegrpc.StreamInterceptor(grpcAdapter.StreamAuthInterceptor()),
		)
		loomcyclepb.RegisterLoomcycleServer(grpcSrv, grpcAdapter)
		// v1.x RFC G — register the A2A gRPC binding on the same server.
		// gRPC is not a path-mounted http.Handler (it needs the HTTP/2
		// server), so the /a2a/grpc binding rides loomcycle's existing
		// grpc.Server; the AgentCard advertises the gRPC port.
		//
		// Call-site nil-guard mirrors the other two a2aServer uses above
		// (SetExtraMux at ~1426, PathTenantWrapper at ~1497): a2aServer is
		// nil whenever A2AServerEnabled is off. RegisterGRPC is itself
		// nil-receiver-safe, so this is defensive consistency rather than a
		// live-crash fix — it stops the gRPC path depending on the callee's
		// internal guard (exp7 C3).
		if a2aServer != nil {
			a2aServer.RegisterGRPC(grpcSrv)
		}
		grpcLis, err := net.Listen("tcp", cfg.Env.GrpcAddr)
		if err != nil {
			log.Fatalf("grpc listen %s: %v", cfg.Env.GrpcAddr, err)
		}
		grpcAdapter.MustLogStartupBanner(cfg.Env.GrpcAddr)
		go func() {
			// GracefulStop returns ErrServerStopped on the
			// normal shutdown path — don't log it as a serve
			// failure (would pollute operator logs and trip alert
			// rules watching for "error" tokens).
			if err := grpcSrv.Serve(grpcLis); err != nil && !errors.Is(err, googlegrpc.ErrServerStopped) {
				log.Printf("grpc serve: %v", err)
			}
		}()
	}

	// Graceful shutdown.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("shutting down…")
	bgCancel() // tear down sweeper + GC goroutines first
	if heartbeatRunner != nil {
		heartbeatRunner.Stop()
	}
	// v0.11.9 — stop the OAuth-dev refresher's background goroutine
	// before the process exits so an in-flight refresh HTTP call
	// either completes + persists OR cleanly aborts. Without this,
	// the OS hard-kills the goroutine and a partially-rotated token
	// could be lost (the on-disk token is atomic write, but the
	// fresh access token might never make it to the persist step).
	if pr.anthropicOAuthRefresher != nil {
		pr.anthropicOAuthRefresher.Stop()
	}
	if grpcSrv != nil {
		grpcSrv.GracefulStop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
	// RFC BL: stop the access-count flusher and flush the final window AFTER the
	// HTTP+gRPC servers have finished draining. In-flight Search requests
	// Record() access bumps right up to the end of that drain window, so the
	// flusher's final flush must run last to capture them — stopping it earlier
	// would leave those late bumps with no goroutine left to flush them. The
	// store closer is deferred (runs after main returns), so the store is still
	// open for this final write.
	accessFlusher.Stop()
	// The webhook delivery counters, for the same reason: the deliveries
	// answered during the drain are counted too. The store is still open.
	// bgCancel above stopped the periodic flusher; a flush it had in flight
	// finishes first (at most its own 10s timeout).
	<-countFlusherDone
	if webhookReceiver != nil {
		fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := webhookReceiver.FlushCounts(fctx); err != nil {
			log.Printf("webhooks: storing the last delivery counts failed: %v", err)
		}
		fcancel()
	}
	// Flush OTEL spans before exit. The OTLP exporter batches; without this,
	// in-flight spans never reach the collector. Give it its OWN 5s budget —
	// sharing the HTTP shutdown's ctx meant a slow in-flight request drain
	// could exhaust the deadline first, silently dropping the final spans.
	if otelShutdown != nil {
		otelCtx, otelCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer otelCancel()
		if err := otelShutdown(otelCtx); err != nil {
			log.Printf("otel: shutdown failed: %v", err)
		}
	}
}

// openStore resolves the operator's storage choice (sqlite default;
// postgres opt-in via storage.backend or LOOMCYCLE_STORAGE_BACKEND) and
// returns a ready Store + a Close-and-log closer.
//
// Errors propagate up to log.Fatalf in main; they cover both
// missing-config (postgres backend selected without a DSN) and
// dial/migration failures (postgres unreachable, schema not initialised
// when LOOMCYCLE_PG_AUTOMIGRATE=0).
// warnLegacyTenantMemory says loudly, once at boot, when memory rows are
// stranded at the legacy "" tenant.
//
// RFC BL added tenant_id to the `memory` table without backfilling the rows
// already in it, so every pre-upgrade row stayed at "" while reads became
// tenant-scoped. The failure is quiet in a specific and misleading way: Document
// keeps chunk BODIES in that table but chunk STRUCTURE in SQL Memory, which is
// not partitioned the same way, so an affected document still lists, still
// opens, and still exports as a complete heading tree with every section empty.
// It reads as corruption rather than as an access problem, which is exactly why
// it needs naming at boot rather than leaving an operator to discover it.
//
// Only the MIXED state warns. A deployment holding nothing but "" rows is fine —
// its reads go to "" too, which is the legacy/open-mode posture — so gating on
// the presence of real tenants keeps this quiet on every unaffected install.
// Best-effort: a failed probe must never block boot.
func warnLegacyTenantMemory(st store.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	legacy, tenants, err := st.MemoryLegacyTenantStats(ctx)
	if err != nil || legacy == 0 || len(tenants) == 0 {
		return
	}
	target := ""
	if len(tenants) == 1 {
		// The overwhelmingly common shape, and the one where we can name the
		// exact call rather than describing it.
		target = tenants[0]
	}
	msg := fmt.Sprintf("memory: WARNING — %d memory row(s) are stranded at the legacy \"\" tenant and are UNREADABLE from a tenant-scoped session (tenants holding rows: %s). "+
		"RFC BL added the tenant_id column without backfilling pre-existing rows. Symptoms: a Document created before the upgrade lists and exports as headings with EMPTY bodies (its structure lives in SQL Memory, which is partitioned differently), and an agent's pre-upgrade key/value memory reads as absent. "+
		"Nothing is deleted. Fix: POST /v1/_memory/repair-tenant (substrate:admin) — it reports by default, and applies with {\"dry_run\":false}; the Settings hub exposes it as Maintenance for shell-less deployments.",
		legacy, strings.Join(tenants, ", "))
	if target != "" {
		msg += fmt.Sprintf(" For this deployment: {\"tenant\":%q,\"dry_run\":false}.", target)
	}
	log.Print(msg)
}

func openStore(cfg *config.Config) (store.Store, func(), error) {
	switch cfg.Storage.Backend {
	case "sqlite", "":
		if cfg.Env.ReplicaID != "" {
			return nil, nil, errors.New(
				"LOOMCYCLE_REPLICA_ID is set but storage.backend is sqlite: " +
					"multi-replica requires Postgres (LISTEN/NOTIFY backplane + " +
					"shared replicas heartbeat table). Set storage.backend: postgres " +
					"+ storage.pg_dsn, or unset LOOMCYCLE_REPLICA_ID for single-replica mode")
		}
		if err := os.MkdirAll(cfg.Env.DataDir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("data dir: %w", err)
		}
		dbPath := filepath.Join(cfg.Env.DataDir, "loomcycle.db")
		st, err := storesqlite.Open(dbPath)
		if err != nil {
			return nil, nil, fmt.Errorf("sqlite open: %w", err)
		}
		st.SetChannelDebug(channelDebugEnabled())
		log.Printf("store: sqlite at %s", dbPath)
		warnLegacyTenantMemory(st)
		return st, func() { _ = st.Close() }, nil

	case "postgres":
		if cfg.Storage.PgDSN == "" {
			return nil, nil, fmt.Errorf("postgres backend selected but storage.pg_dsn / LOOMCYCLE_PG_DSN is empty")
		}
		st, err := storepostgres.Open(context.Background(), storepostgres.Config{
			DSN:             cfg.Storage.PgDSN,
			MaxOpenConns:    cfg.Storage.PgMaxOpenConns,
			MinIdleConns:    cfg.Storage.PgMinIdleConns,
			AutoMigrate:     cfg.Storage.PgAutoMigrate,
			PgvectorEnabled: cfg.Env.PgvectorEnabled,
			ChannelDebug:    channelDebugEnabled(),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("postgres open: %w", err)
		}
		log.Printf("store: postgres (automigrate=%v pgvector=%v)", cfg.Storage.PgAutoMigrate, cfg.Env.PgvectorEnabled)
		warnLegacyTenantMemory(st)
		return st, func() { _ = st.Close() }, nil

	default:
		return nil, nil, fmt.Errorf("unknown storage.backend %q (want \"sqlite\" or \"postgres\")", cfg.Storage.Backend)
	}
}

// providerResolver holds the constructed Provider instances keyed by the id
// agents reference in `provider:` (RFC BF P2a). newProviderResolver builds byID
// from cfg.Providers via the driver registry (providers.NewDriver) — a
// config-driven flip of the pre-P2a hardcoded per-provider fields. It is
// byte-identical when no operator `providers:` block is declared, because the
// embedded default-providers layer (cmd/loomcycle/embedded/providers.default.yaml)
// supplies the built-ins. A provider whose enabling env is absent is simply not
// in byID; Get reproduces the pre-P2a "not configured" error for it.
type providerResolver struct {
	// mu guards byID + cfg so a runtime config reload (RFC CK) can swap the
	// constructed provider set under Reload while Get reads it concurrently.
	mu   sync.RWMutex
	byID map[string]providers.Provider
	// cfg is retained so Get can tell a declared-but-disabled provider (a
	// bespoke "not configured" error) apart from a truly unknown id.
	cfg *config.Config
	// anthropic-oauth-dev remains a residual hardcoded path: its Refresher
	// lifecycle is main-owned (Start here, Stop at shutdown). The provider goes
	// into byID like any other; this field only carries the refresher handle for
	// teardown. See newProviderResolver.
	anthropicOAuthRefresher *anthropic_oauth_dev.Refresher
	// sched is the call scheduler every provider in byID is gated by: each one
	// is handed out wrapped in its call lane (lanegate.Wrap), so a holder of a
	// registry provider — the loop, compaction, the LLM gateway, the
	// image-describe route — cannot make a call the provider's max_concurrent
	// does not count. The Server reports and reloads this same instance.
	sched *concurrency.CallScheduler
}

// secretSink forwards secret values to a function set after the components
// holding the sink were built: providers are built before the server and its
// redactor exist (RFC EM). It is read at each use, never captured.
type secretSink struct{ f atomic.Pointer[func(string)] }

func (s *secretSink) set(f func(string)) { s.f.Store(&f) }

func (s *secretSink) register(v string) {
	if f := s.f.Load(); f != nil {
		(*f)(v)
	}
}

// buildProviderMap constructs every ENABLED provider from cfg.Providers via the
// RFC BF driver registry, returning the id→Provider map. Factored out of
// newProviderResolver so a runtime reload (providerResolver.Reload) rebuilds the
// exact same set WITHOUT re-running the one-time boot side effects
// (anthropic-oauth-dev refresher Start, the mock/code-js diagnostic logs). An
// unconstructable provider is an error (the reload is rejected, old set kept).
//
// Every provider is returned wrapped in its call lane, capped or not: the
// wrapper looks the cap up per call, so one a later reload adds applies to a
// provider built before it, and an uncapped call costs one map lookup.
func buildProviderMap(cfg *config.Config, sched *concurrency.CallScheduler) (map[string]providers.Provider, error) {
	byID := map[string]providers.Provider{}
	for id, pc := range cfg.Providers {
		if !providerEnabled(id, pc, cfg) {
			continue
		}
		p, err := providers.NewDriver(pc.Driver, providerbuild.DriverOptions(id, pc, cfg))
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", id, err)
		}
		byID[id] = lanegate.Wrap(id, p, sched)
	}
	return byID, nil
}

// newProviderResolver builds every enabled provider from cfg.Providers via the
// RFC BF driver registry (config-driven flip of the pre-P2a hardcoded per-provider
// construction). It is byte-identical when no operator `providers:` block is
// declared, because the embedded default-providers layer supplies the built-ins
// and providerbuild.DriverOptions/providerEnabled port the exact env behaviour. An
// unconstructable provider (bad driver/dialect/base_url) is a boot error.
func newProviderResolver(cfg *config.Config) (*providerResolver, error) {
	sched := newCallScheduler(cfg)
	byID, err := buildProviderMap(cfg, sched)
	if err != nil {
		return nil, err
	}
	pr := &providerResolver{byID: byID, cfg: cfg, sched: sched}

	// Residual per-provider boot diagnostics (operator-facing knob echoes, NOT
	// resolution logic) — kept id-keyed so the boot log is unchanged from pre-P2a
	// for the mock/code-js paths. anthropic-oauth-dev logs its own below.
	if _, ok := pr.byID["mock"]; ok {
		log.Printf("mock provider: enabled (LATENCY_MS=%q LATENCY_JITTER_MS=%q 429_RATE=%q 500_RATE=%q)",
			os.Getenv("LOOMCYCLE_MOCK_LATENCY_MS"),
			os.Getenv("LOOMCYCLE_MOCK_LATENCY_JITTER_MS"),
			os.Getenv("LOOMCYCLE_MOCK_429_RATE"),
			os.Getenv("LOOMCYCLE_MOCK_500_RATE"))
	}
	if _, ok := pr.byID["mock-stable"]; ok {
		log.Printf("mock-stable provider: enabled (failure injection always off; fallback target for [mock, mock-stable] tier policies)")
	}
	if _, ok := pr.byID["code-js"]; ok {
		log.Printf("code-js provider: enabled (root=%q deterministic=%v run_timeout=%s max_wall=%s abi=%s)",
			cfg.Env.CodeAgentsRoot, cfg.Env.CodeAgentsDeterministic, cfg.Env.CodeAgentsRunTimeout, cfg.Env.CodeAgentsMaxWall, codejs.ABIVersion)
	}

	// v0.11.9 — anthropic-oauth-dev stays a residual hardcoded path (main-owned
	// Refresher lifecycle; not a registry driver). Two gates: env var + tokens on
	// disk. The provider (when live) joins byID like any other; the refresher
	// handle is kept for shutdown teardown.
	if os.Getenv(anthropic_oauth_dev.EnvEnabled) == "1" {
		streamOpts := streamhttp.Options{
			HeaderTimeout: cfg.Env.ProviderHeaderTimeout,
			IdleTimeout:   cfg.Env.ProviderIdleTimeout,
		}
		storePath, pathErr := anthropic_oauth_dev.DefaultTokenStorePath()
		switch {
		case pathErr != nil:
			log.Printf("anthropic-oauth-dev: not registered — cannot resolve config dir: %v", pathErr)
		default:
			store := anthropic_oauth_dev.NewTokenStore(storePath)
			if _, loadErr := store.Load(); loadErr != nil {
				log.Printf("anthropic-oauth-dev: env var set but no tokens at %s — run `loomcycle anthropic login` to authorize", storePath)
			} else {
				refresher := anthropic_oauth_dev.NewRefresher(store, anthropic_oauth_dev.ExchangeOptions{}, log.Printf)
				refresher.Start(context.Background())
				pr.anthropicOAuthRefresher = refresher
				pr.byID["anthropic-oauth-dev"] = lanegate.Wrap("anthropic-oauth-dev", anthropic_oauth_dev.New(
					refresher, streamOpts, anthropic_oauth_dev.ResolveClaudeCodeVersion(), nil,
				), sched)
				log.Printf("anthropic-oauth-dev: registered (token expires %s; user-agent=claude-cli/%s)",
					refresher.Token().ExpiresAt.Format(time.RFC3339),
					anthropic_oauth_dev.ResolveClaudeCodeVersion())
				log.Printf("anthropic-oauth-dev: WARNING — reverse-engineered OAuth flow (Pi reference + loomcycle replication); NOT officially endorsed by Anthropic; subscription terms historically restrict programmatic use; operator carries all risk including account flag/revocation; no warranty/SLA/liability from loomcycle; see docs/PROVIDERS.md")
			}
		}
	}

	logProviderSummary(pr, cfg)
	return pr, nil
}

// providerEnabled reproduces the exact pre-P2a env gates: a keyed provider needs
// its api_key_env set; ollama-local needs a base URL that isn't the "disabled"
// opt-out; mock/mock-stable need LOOMCYCLE_MOCK_ENABLED=1; code-js needs
// CodeAgentsEnabled. anthropic-oauth-dev is intentionally NOT handled here — it is
// the residual hardcoded path in newProviderResolver.
func providerEnabled(id string, pc config.ProviderConfig, cfg *config.Config) bool {
	switch id {
	case "ollama-local":
		// The loader defaults OLLAMA_BASE_URL to http://localhost:11434; the opt-out
		// is the "disabled" sentinel, which always wins. Read the RESOLVED value
		// (cfg.Env), not os.Getenv, so the getenvDefault behaviour is preserved.
		// RFC CK: a YAML `providers.ollama-local.base_url` ALSO enables it, so a
		// `local` preset/bundle can carry the endpoint without relying on the env
		// var (the env still defaults to localhost, so existing deployments are
		// unchanged).
		if cfg.Env.OllamaBaseURL == "disabled" {
			return false
		}
		return cfg.Env.OllamaBaseURL != "" || pc.BaseURL != ""
	case "mock", "mock-stable":
		return os.Getenv("LOOMCYCLE_MOCK_ENABLED") == "1"
	case "code-js":
		return cfg.Env.CodeAgentsEnabled
	default:
		// Keyed provider (anthropic/openai/deepseek/gemini/ollama + any declared
		// 3rd-party naming an api_key_env): enabled iff that env var is set — the
		// exact pre-RFC-BF gate for the built-ins.
		if pc.APIKeyEnv != "" {
			return os.Getenv(pc.APIKeyEnv) != ""
		}
		// Keyless declared provider — a self-hosted OpenAI-compatible endpoint
		// with no auth (vLLM/llama.cpp/a second Ollama behind base_url), the RFC BF
		// headline case. Declaring it in `providers:` IS the opt-in, so it is
		// enabled; the driver calls base_url with no key. Without this a keyless
		// 3rd-party provider was silently treated as "not configured" and could
		// never be used. No built-in reaches here with an empty api_key_env
		// (ollama-local/mock/code-js are special-cased above), so this only newly
		// enables an operator-declared keyless provider.
		return true
	}
}

// newCallScheduler builds the providers' call lanes from cfg.Providers: one
// lane per (provider, model kind) that sets a cap — max_concurrent is the chat
// lane's — plus each provider's cap on models in flight. A slot is held for one
// model CALL, never for a run, and a wait for one does not time out. Returns a
// scheduler with nothing in it — an uncapped call then costs one map lookup —
// when no provider sets a cap (the default deployment).
func newCallScheduler(cfg *config.Config) *concurrency.CallScheduler {
	caps := lanegate.SchedulerConfig(cfg.Providers, cfg.Models)
	if len(caps) > 0 {
		log.Printf("providers: per-provider call caps enabled — %s (queue_depth=%d for requests that hold no run slot; a run's calls wait without timing out)",
			describeCallCaps(caps), cfg.Concurrency.ProviderQueueDepthOrDefault())
	}
	return concurrency.NewCallScheduler(caps)
}

// describeCallCaps renders the caps for the boot log, in a stable order:
// `ollama-local[chat=2 decision=1 models=1], openai[chat=8]`.
func describeCallCaps(caps map[string]concurrency.ProviderLanesConfig) string {
	ids := make([]string, 0, len(caps))
	for id := range caps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteString(", ")
		}
		pc := caps[id]
		kinds := make([]string, 0, len(pc.Lanes))
		for k := range pc.Lanes {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		parts := make([]string, 0, len(kinds)+1)
		for _, k := range kinds {
			parts = append(parts, fmt.Sprintf("%s=%d", k, pc.Lanes[k].MaxConcurrent))
		}
		if pc.MaxModelsInFlight > 0 {
			parts = append(parts, fmt.Sprintf("models=%d", pc.MaxModelsInFlight))
			if len(pc.ExemptKinds) > 0 {
				parts = append(parts, "models-exempt="+strings.Join(pc.ExemptKinds, ","))
			}
		}
		fmt.Fprintf(&b, "%s[%s]", id, strings.Join(parts, " "))
	}
	return b.String()
}

// notConfiguredError reproduces the pre-P2a "not configured" error for a
// declared-but-disabled provider so callers (and the probe) see the exact same
// message they did before the flip. Built-in ids keep their bespoke text; a
// declared 3rd-party id gets the generic "(set <api_key_env>)" form.
func notConfiguredError(id string, cfg *config.Config) error {
	switch id {
	case "ollama":
		return fmt.Errorf("ollama provider not configured (set OLLAMA_API_KEY for hosted ollama.com; use provider id \"ollama-local\" for a local-network Ollama)")
	case "ollama-local":
		return fmt.Errorf("ollama-local provider not configured (set OLLAMA_BASE_URL, or it's been opted out via OLLAMA_BASE_URL=disabled)")
	case "mock":
		return fmt.Errorf("mock provider not configured (set LOOMCYCLE_MOCK_ENABLED=1)")
	case "mock-stable":
		return fmt.Errorf("mock-stable provider not configured (set LOOMCYCLE_MOCK_ENABLED=1)")
	case "code-js":
		return fmt.Errorf("code-js provider not configured: code agents are disabled (set LOOMCYCLE_CODE_AGENTS_ENABLED=1)")
	case "anthropic-oauth-dev":
		return fmt.Errorf("anthropic-oauth-dev not registered (set LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1 + run `loomcycle anthropic login`)")
	default:
		env := cfg.Providers[id].APIKeyEnv
		if env == "" {
			return fmt.Errorf("%s provider not configured", id)
		}
		return fmt.Errorf("%s provider not configured (set %s)", id, env)
	}
}

// providerDisabledReason is the probe's short exclusion reason for a
// declared-but-disabled provider, byte-identical to the pre-P2a jobs slice.
func providerDisabledReason(id string, pc config.ProviderConfig) string {
	switch id {
	case "ollama-local":
		return "OLLAMA_BASE_URL not configured"
	case "mock", "mock-stable":
		return "LOOMCYCLE_MOCK_ENABLED not set"
	default:
		return fmt.Sprintf("%s not set", pc.APIKeyEnv)
	}
}

// logProviderSummary logs the config-sourced provider inventory at boot. Under
// RFC BF providers are fully config-driven; the embedded default-providers layer
// (cmd/loomcycle/embedded/providers.default.yaml) supplies the built-ins.
func logProviderSummary(pr *providerResolver, cfg *config.Config) {
	enabled := make([]string, 0, len(pr.byID))
	for id := range pr.byID {
		enabled = append(enabled, id)
	}
	sort.Strings(enabled)
	log.Printf("providers: %d configured, %d enabled (%s)", len(cfg.Providers), len(pr.byID), strings.Join(enabled, ", "))
}

// validateCodeAgents fails loud at startup for any statically-configured
// `provider: code-js` agent whose JS is missing or won't parse (RFC J
// Deliverable 3) — so a broken code-agent fails the boot, NOT the first
// scheduled fire. Two failure modes:
//   - code-js used but disabled → log.Fatalf pointing at the enable flag.
//   - code-js enabled but JS broken/missing → log.Fatalf naming the agent + path.
//
// Dynamic AgentDefs (created at runtime via the AgentDef tool) are not seen
// here; their JS validates on first Call and surfaces as an EventError.
func validateCodeAgents(cfg *config.Config, pr *providerResolver) {
	// nil interface → (nil,false): cp stays nil when code-js is disabled, matching
	// the pre-P2a pr.codeJS==nil path.
	cp, _ := providers.As[*codejs.Provider](pr.byID["code-js"])
	var broken []string
	for name, def := range cfg.Agents {
		if def.Provider != "code-js" {
			continue
		}
		if cp == nil {
			log.Fatalf("agent %q uses `provider: code-js` but code agents are disabled — set LOOMCYCLE_CODE_AGENTS_ENABLED=1", name)
		}
		// An inline `code:` body has no filesystem file — validate the body
		// itself (mirrors the run-time provider, which prefers the inline body
		// over agent_code/<name>/index.js). Only fall back to the filesystem
		// compile when no inline body is declared. Without this, a yaml code
		// agent shipped with an inline body and no host index.js — the whole
		// point of inline ingestion (no FS bind) — would fail the boot.
		var err error
		if def.Code != "" {
			if _, verr := codejs.Validate(def.Code); verr != nil {
				err = fmt.Errorf("code-agent %q: inline code_body: %w", name, verr)
			}
		} else {
			_, err = cp.Compile(name)
		}
		if err != nil {
			broken = append(broken, err.Error())
		}
	}
	if len(broken) > 0 {
		log.Fatalf("code-js agents failed to load:\n  - %s", strings.Join(broken, "\n  - "))
	}
	if cp != nil {
		log.Printf("code-js: validated %d static code-agent(s)", countCodeAgents(cfg))
	}
}

func countCodeAgents(cfg *config.Config) int {
	n := 0
	for _, def := range cfg.Agents {
		if def.Provider == "code-js" {
			n++
		}
	}
	return n
}

// Get returns the constructed provider for id, or the pre-P2a-identical error.
// A hit is a map lookup. On a miss: a declared-but-disabled provider (present in
// cfg.Providers, skipped by providerEnabled) — or the residual anthropic-oauth-dev
// — gets its bespoke "not configured" message; a truly unknown id gets the
// "unknown provider" error.
func (p *providerResolver) Get(id string) (providers.Provider, error) {
	p.mu.RLock()
	prov, ok := p.byID[id]
	cfg := p.cfg
	p.mu.RUnlock()
	if ok {
		return prov, nil
	}
	if id == "anthropic-oauth-dev" {
		return nil, notConfiguredError(id, cfg)
	}
	if _, declared := cfg.Providers[id]; declared {
		return nil, notConfiguredError(id, cfg)
	}
	return nil, fmt.Errorf("unknown provider %q", id)
}

// Reload rebuilds the constructed provider set from a freshly-loaded config
// (RFC CK config reload) and swaps it in under the write lock, so a changed
// base_url / options / timeout / added-or-removed provider takes effect on the
// next run without a restart. In-flight runs keep the provider they already
// resolved. The residual anthropic-oauth-dev provider (a main-owned refresher
// lifecycle, not declared in cfg.Providers) is carried over from the old set
// rather than reconstructed — its background token refresher keeps running.
// currentCfg returns the config the provider set was last built from (updated in
// place by Reload), read under the lock. The resolve-probe path reads this so a
// reloaded provider set (added/removed ids) is tracked without rewiring the
// periodic loop, which captured the boot cfg by value.
func (p *providerResolver) currentCfg() *config.Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

func (p *providerResolver) Reload(cfg *config.Config) error {
	next, err := buildProviderMap(cfg, p.sched)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if oauth, ok := p.byID["anthropic-oauth-dev"]; ok {
		next["anthropic-oauth-dev"] = oauth
	}
	p.byID = next
	p.cfg = cfg
	return nil
}

// buildResolver constructs the model-resolution matrix
// (resolve.Resolver) and runs the FIRST round of live probes
// synchronously before returning. Per the operator's directive:
// providers without API keys are explicitly marked Excluded so
// Snapshot() distinguishes "operator chose not to enable this
// provider" from "operator enabled but the probe failed".
//
// The first probe completes synchronously so traffic served on the
// HTTP/gRPC port immediately after this returns sees a populated
// matrix (or an explicit Excluded marker for unconfigured providers).
// Subsequent rounds run in a background goroutine started by main —
// see runResolveProbeLoop.
func buildResolver(cfg *config.Config, pr *providerResolver) *resolve.Resolver {
	libraryTiers := convertTiers(cfg.Tiers, cfg.Models)
	r := resolve.NewResolver(cfg.ProviderPriority, libraryTiers)
	// RFC BG: surface a silent model-pattern bump (a pattern resolving to a new
	// concrete model) in the operator log. Set before the first probe so an
	// initial resolution is recorded (first-seen never warns; only a MOVE does).
	r.SetLogf(log.Printf)

	// First-round probe — synchronous, so the matrix is hot before
	// the listener accepts traffic.
	runResolveProbeOnce(context.Background(), r, pr, cfg)
	return r
}

// runResolveProbeOnce performs one full sweep across all four
// providers: explicitly Excludes those without keys (visible in
// Snapshot), and live-probes the rest. Used by buildResolver for
// the synchronous startup probe and by runResolveProbeLoop for
// every periodic re-probe.
//
// Each provider's probe runs in its own goroutine with a 5-second
// deadline so a slow provider doesn't hold the whole sweep. The
// caller's ctx (typically bgCtx for the periodic loop, or
// context.Background for startup) bounds the total wait.
func runResolveProbeOnce(ctx context.Context, r *resolve.Resolver, pr *providerResolver, cfg *config.Config) {
	// RFC CK: probe the CURRENT provider set. pr.Reload updates pr.cfg in place, so
	// reading it here means the periodic loop + force-probe track a reloaded set
	// (added/removed provider ids) without rewiring — the loop captured the boot
	// cfg by value. At boot pr.cfg == the passed cfg, so this is a no-op then.
	cfg = pr.currentCfg()
	type probeJob struct {
		id         string             // provider id
		excluded   bool               // operator opted out
		exclReason string             // why excluded; surfaced in LastError
		provider   providers.Provider // nil when excluded
	}

	// RFC BF P2a — build the probe set from cfg.Providers (config-driven), deriving
	// excluded/reason from providerEnabled + providerDisabledReason (byte-identical
	// to the pre-P2a jobs slice). code-js is skipped: pre-P2a never probed it, so
	// its resolver-matrix presence is unchanged. Sorted for deterministic log order
	// (the reachable probes below run concurrently, so line order was never stable).
	// The residual anthropic-oauth-dev job (not in cfg.Providers) is appended — its
	// env/token gates are caught by pr.Get in the loop below.
	ids := make([]string, 0, len(cfg.Providers))
	for id := range cfg.Providers {
		if id == "code-js" {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	jobs := make([]probeJob, 0, len(ids)+1)
	for _, id := range ids {
		pc := cfg.Providers[id]
		excluded := !providerEnabled(id, pc, cfg)
		reason := ""
		if excluded {
			reason = providerDisabledReason(id, pc)
		}
		jobs = append(jobs, probeJob{id: id, excluded: excluded, exclReason: reason})
	}
	jobs = append(jobs, probeJob{id: "anthropic-oauth-dev"})
	for i := range jobs {
		if jobs[i].excluded {
			continue
		}
		if p, err := pr.Get(jobs[i].id); err == nil {
			jobs[i].provider = p
		} else {
			// Provider configured but resolver-Get failed. Mark
			// Excluded with the resolver error as the reason —
			// rare in practice (newProviderResolver wires every
			// driver whose key is set) but defensive.
			jobs[i].excluded = true
			jobs[i].exclReason = fmt.Sprintf("provider not registered: %v", err)
		}
	}

	var wg sync.WaitGroup
	for _, j := range jobs {
		if j.excluded {
			r.SetExcluded(j.id, j.exclReason)
			log.Printf("resolve probe: %s excluded (%s)", j.id, j.exclReason)
			continue
		}
		wg.Add(1)
		go func(j probeJob) {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			models, err := j.provider.ListModels(probeCtx)
			if err != nil {
				r.SetReachable(j.id, false, nil, err.Error())
				log.Printf("resolve probe: %s unreachable (%v)", j.id, err)
				return
			}
			r.SetReachable(j.id, true, models, "")
			log.Printf("resolve probe: %s reachable (%d models listed)", j.id, len(models))
		}(j)
	}
	wg.Wait()
}

// runResolveProbeLoop runs runResolveProbeOnce on the configured
// interval until ctx is cancelled. Started as a goroutine from main;
// shares bgCtx with the heartbeat sweeper and session-lock GC so
// SIGTERM tears all three down together.
func runResolveProbeLoop(ctx context.Context, r *resolve.Resolver, pr *providerResolver, cfg *config.Config, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runResolveProbeOnce(ctx, r, pr, cfg)
		}
	}
}

// convertTiers translates the config-package representation of library
// tier definitions into the resolver-package representation. Mirrors
// the per-agent helper in internal/api/http/server.go but operates on
// the top-level cfg.Tiers map.
func convertTiers(in map[string][]config.TierCandidate, models map[string]config.ModelRef) map[string][]resolve.Candidate {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]resolve.Candidate, len(in))
	for tier, cands := range in {
		conv := make([]resolve.Candidate, 0, len(cands))
		for _, c := range cands {
			// Expand model aliases (top-level models:) — mirrors the pin
			// path and the per-agent converter so a library tier candidate
			// can name an alias too. RFC BG: a pattern alias surfaces a glob
			// (pat) the resolver resolves against the live catalog.
			prov, mdl, pat := config.ExpandModelAlias(models, c.Provider, c.Model)
			conv = append(conv, resolve.Candidate{Provider: prov, Model: mdl, ModelPattern: pat})
		}
		out[tier] = conv
	}
	return out
}

// runAdvisoryGatedSweeper drives a per-tick fn behind an optional
// Postgres advisory lock. v0.12.4 Phase 5 — every TTL sweeper goes
// through this helper. When lock is nil (single-replica mode), tick
// runs unconditionally. When lock is set (cluster mode), only the
// replica that wins pg_try_advisory_lock for this tick actually runs
// the body. Lost-race ticks are silent.
//
// Replaces the v0.8.x runMemorySweeper / runChannelsSweeper /
// runInterruptsSweeper / runMetricsSweeper functions which all had
// the same ticker boilerplate. Each per-tick body is now a closure
// passed at call site.
func runAdvisoryGatedSweeper(
	ctx context.Context,
	interval time.Duration,
	lock *coord.AdvisoryLock,
	lockKey int64,
	name string,
	tick func(ctx context.Context),
) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if lock == nil {
				tick(ctx)
				continue
			}
			acquired, err := lock.TryRun(ctx, lockKey, func(ctx context.Context) error {
				tick(ctx)
				return nil
			})
			if err != nil {
				log.Printf("%s sweeper: advisory lock infra error: %v", name, err)
				continue
			}
			_ = acquired // silent lost race
		}
	}
}

// spawnStdioMCP starts a stdio MCP child for one server entry. Env keys are
// sorted so process listings are deterministic across runs.
// mcpLookupView adapts *mcp.DynamicRegistry to lookup.MCPDynamicRegistry
// so the pool's build callback + the boot rehydrator can resolve through
// the shared lookup.MCPServer chain (RFC N). The dynamic registry stores
// mcp.DynamicMCPServerSpec; lookup wants the uniform lookup.MCPServerSpec.
// A dynamic spec carries no operator tools (the substrate doesn't
// record a narrowing), so Tools stays nil = allow-all.
type mcpLookupView struct{ reg *mcp.DynamicRegistry }

func (v mcpLookupView) Get(tenantID, name string) (lookup.MCPServerSpec, bool) {
	if v.reg == nil {
		return lookup.MCPServerSpec{}, false
	}
	s, ok := v.reg.Get(tenantID, name)
	if !ok {
		return lookup.MCPServerSpec{}, false
	}
	return lookup.MCPServerSpec{
		Transport: s.Transport, URL: s.URL, Headers: s.Headers,
		Command: s.Command, Args: s.Args, Env: s.Env, // stdio (F31)
		OperatorAuthored: s.OperatorAuthored,
		DefID:            s.DefID, Version: s.Version,
	}, true
}

// refSubstituter resolves the references of one syntax ($cred:, $ghapp:) in s
// for an identity and a destination.
type refSubstituter interface {
	Substitute(ctx context.Context, tenantID, agentName, userID, s, targetURL string, register func(string)) (string, []string, error)
}

// substituteForRun resolves s's references with each substituter in turn, for
// the run identity on ctx. A run that may not use its user's own scope (a
// trigger whose definition's author could not act as that user) resolves for
// its agent and tenant only.
func substituteForRun(ctx context.Context, s, targetURL string, register func(string), subs ...refSubstituter) (string, []string, error) {
	ri := tools.RunIdentity(ctx)
	userID := ri.UserID
	if ri.NoUserScope() {
		userID = ""
	}
	var unresolved []string
	for _, sub := range subs {
		out, un, err := sub.Substitute(ctx, ri.TenantID, tools.AgentName(ctx), userID, s, targetURL, register)
		if err != nil {
			return "", nil, err
		}
		s, unresolved = out, append(unresolved, un...)
	}
	return s, unresolved, nil
}

// lateCredSubstitute returns a resolver that reads *ref when a request is
// built, so a client made before main assigns it still uses it. Until then it
// reports every $cred: and $ghapp: reference unresolved — the client drops or
// refuses the header — so a reference never goes out as written.
func lateCredSubstitute(ref *func(ctx context.Context, s, targetURL string) (string, []string, error)) func(ctx context.Context, s, targetURL string) (string, []string, error) {
	return func(ctx context.Context, s, targetURL string) (string, []string, error) {
		if f := *ref; f != nil {
			return f(ctx, s, targetURL)
		}
		return s, append(credential.RefNames(s), credential.GHAppRefNames(s)...), nil
	}
}

// mcpPoolBuild returns the MCP pool's build callback: it resolves a server
// NAME for a tenant (static stdio first, then lookup.MCPServer's tenant-dynamic
// → static yaml → shared-dynamic order) and builds its client. credSubstitute
// is read at each request (lateCredSubstitute): main assigns it once the
// credential engine exists, after static servers were already dialed.
func mcpPoolBuild(cfg *config.Config, mcpDynView mcpLookupView,
	credSubstitute *func(ctx context.Context, s, targetURL string) (string, []string, error),
	logf func(format string, args ...any)) func(tenant, name string) (mcp.Caller, error) {
	// One warning per stored version per process: the pool rebuilds a client
	// on every evict and respawn, and a line per dial would bury the log.
	var warnedMu sync.Mutex
	warned := map[string]bool{}
	return func(tenant, name string) (mcp.Caller, error) {
		// Static stdio entries are yaml-only ground truth (lookup's
		// spec can't carry Command/Args/Env). A tenant never overrides
		// a stdio server — resolve it directly at the shared base.
		if srv, ok := cfg.MCPServers[name]; ok && srv.Transport == "stdio" {
			return spawnStdioMCP(name, srv)
		}
		// http / streamable-http: tenant-dynamic → static yaml →
		// shared-dynamic, via the shared resolver.
		spec, ok := lookup.MCPServer(cfg, mcpDynView, tenant, name)
		if !ok {
			return nil, fmt.Errorf("mcp_servers.%s: not in static yaml or dynamic registry (tenant=%q)", name, tenant)
		}
		// The expansions below read this process's environment, so only an
		// operator's definition should hold a ${NAME}: the yaml (static), or a
		// dynamic version stamped operator_authored. Authoring refuses the
		// rest; what reaches here unstamped is a row stored before that rule,
		// a restored older snapshot, or a hand edit.
		//
		// The default refuses it here, before any client exists or request is
		// sent. LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV=0 is the upgrade escape
		// hatch: it warns and still dials, so an operator who has not yet had
		// the boot-listed defs re-saved is not cut off mid-upgrade. Neither
		// message carries a value — only where the reference sits.
		if spec.Source != "static" && !spec.OperatorAuthored {
			if locs := mcp.EnvRefLocations(spec.URL, spec.Headers, spec.Command, spec.Args, spec.Env); len(locs) > 0 {
				fields := strings.Join(locs, ", ")
				if !cfg.Env.MCPDialUnattributedEnv {
					return nil, fmt.Errorf("mcp_servers.%s v%d (def_id %s, tenant=%q): %s holds a ${NAME} reference to the server environment, but this definition was not saved by an admin; refusing to dial it. "+
						"An admin acting in that tenant re-saves it (create with the same overlay) to restore it; LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV=0 dials it meanwhile",
						name, spec.Version, spec.DefID, tenant, fields)
				}
				key := tenant + "\x00" + name + "\x00" + spec.DefID
				warnedMu.Lock()
				first := !warned[key]
				warned[key] = true
				warnedMu.Unlock()
				if first {
					logf("mcp_servers: WARNING: %q v%d (def_id %s, tenant %q) expands %s from the server environment but was not saved by an admin; "+
						"dialing it only because LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV=0 — have an admin acting in that tenant re-save it (create with the same overlay), then unset the variable",
						name, spec.Version, spec.DefID, tenant, fields)
				}
			}
		}
		switch spec.Transport {
		case "http", "streamable-http":
			// F32: resolve ${LOOMCYCLE_*} at DIAL time, not persist time —
			// the stored def (and the registry spec) carry only the ${ref};
			// the live client gets the value. This mirrors yaml-load's
			// config.Load expansion at exactly the boundary that flattens any
			// inner ${LOOMCYCLE_*} before the request-time ${run.*}
			// substituter runs (see mcpserverdef.go buildDefinition). For a
			// yaml-static spec the values are already flat, so ExpandEnv is an
			// idempotent no-op; a pre-F32 baked row (resolved value, no ${})
			// is likewise a no-op → backward-compatible.
			hdrs := spec.Headers
			if len(hdrs) > 0 {
				hdrs = make(map[string]string, len(spec.Headers))
				for k, v := range spec.Headers {
					hdrs[k] = config.ExpandEnv(v)
				}
			}
			return mcphttp.New(mcphttp.Config{
				URL:     config.ExpandEnv(spec.URL),
				Headers: hdrs,
				// DNS-rebinding guard: OFF by default (MCP servers are commonly
				// on localhost/private-net), opt in via
				// LOOMCYCLE_MCP_ALLOW_PRIVATE_IPS=0. HTTPPrivateHostAllowlist
				// exempts specific internal MCP hosts when the block is on.
				BlockPrivateIPs:      !cfg.Env.MCPAllowPrivateIPs,
				PrivateHostAllowlist: cfg.Env.HTTPPrivateHostAllowlist,
				// RFC AR — resolve $cred:<name> header tokens per-request from
				// the tenant credential store (per-user tokens bind correctly on
				// this pooled client). Read at each request, not here: a static
				// server's client is built and dialed during boot, before main
				// assigns credSubstitute, and the pool keeps it — reading it
				// here left every static server without one, so a $cred: in
				// its headers went out as written.
				CredSubstitute: lateCredSubstitute(credSubstitute),
			})
		case "stdio":
			// F31: a runtime-registered stdio server. The substrate only
			// admits a stdio row when LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO is
			// set, so reaching here already means the operator opted in.
			// Spawned + lifecycle-managed (respawn on crash, teardown on
			// retire) by the same pool path as a yaml stdio server.
			return spawnStdio(name, spec.Command, spec.Args, spec.Env)
		default:
			return nil, fmt.Errorf("mcp_servers.%s: invalid transport %q (data corruption?)", name, spec.Transport)
		}
	}
}

func spawnStdioMCP(name string, srv config.MCPServer) (mcp.Caller, error) {
	return spawnStdio(name, srv.Command, srv.Args, srv.Env)
}

// runStartMCPDiscoveryTimeout bounds the F33 run-start handshake of a referenced
// dynamic MCP server whose discovered_tools cache is empty. Kept well under the
// 30s boot-discovery budget: a run shouldn't stall long waiting on one peer, and
// if it times out the lazy first-call fallback still applies.
const runStartMCPDiscoveryTimeout = 10 * time.Second

// mcpActiveDefReader adapts the store to mcp.ActiveDefReader (F33), returning the
// active dynamic-MCP-server def JSON for (tenant, name). Keeps internal/tools/mcp
// free of an internal/store import.
type mcpActiveDefReader struct{ store store.Store }

func (r mcpActiveDefReader) ActiveMCPServerDef(ctx context.Context, tenant, name string) (json.RawMessage, bool) {
	row, err := r.store.MCPServerDefGetActive(ctx, tenant, name)
	if err != nil {
		return nil, false
	}
	// A retired active version keeps its pointer but is out of the registry,
	// so the pool dials the next tier for this name. Report it absent so the
	// enumerator advertises that tier's tools too, not the retired version's.
	if row.Retired {
		return nil, false
	}
	return row.Definition, true
}

// spawnStdio is the transport-spawn core shared by the static yaml stdio
// path (spawnStdioMCP) and the dynamic stdio path (the pool build
// callback's stdio case, F31).
//
// F39: command / args / env values are ExpandEnv'd HERE, at spawn. The static
// yaml path arrives pre-expanded (config-load ExpandEnvs the whole file), so
// this is an idempotent no-op for it; the DYNAMIC path stores raw ${...}
// placeholders (kept literal in the def per F32) that never went through the
// yaml-wide expansion, so without this a dynamic stdio server received the
// literal "${LOOMCYCLE_X}" (and clobbered the inherited real var, since
// cmd.Env appends def env AFTER the inherited environ). Expanding at spawn —
// not baking into the stored def — keeps the persisted content free of the
// resolved secret (F32). Uses the same allowlisted expander as yaml-load;
// ${run.*} late-binding tokens pass through verbatim as before.
func spawnStdio(name, command string, args []string, envMap map[string]string) (mcp.Caller, error) {
	expandedArgs := make([]string, len(args))
	for i, a := range args {
		expandedArgs[i] = config.ExpandEnv(a)
	}
	return mcpstdio.Spawn(mcpstdio.Config{
		Command: config.ExpandEnv(command),
		Args:    expandedArgs,
		Env:     expandedStdioEnv(envMap),
		OnStderr: func(line string) {
			log.Printf("mcp[%s]: %s", name, line)
		},
	})
}

// expandedStdioEnv renders an stdio env map as sorted KEY=VALUE strings,
// ExpandEnv'ing each VALUE (F39). Sorted for a deterministic child env
// regardless of map iteration order. Pure (no spawn) so the expansion is
// unit-testable. See spawnStdio for the static-vs-dynamic rationale + F32.
func expandedStdioEnv(envMap map[string]string) []string {
	keys := make([]string, 0, len(envMap))
	for k := range envMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+config.ExpandEnv(envMap[k]))
	}
	return env
}

// applyToolsFilter — thin wrapper over mcp.ApplyToolsFilter.
// Lives in the mcp package so both the boot-time path here and the
// lazy-retry path (mcp.LazyResolver) share one implementation.
func applyToolsFilter(descs []mcp.ToolDescriptor, allowed []string) []mcp.ToolDescriptor {
	return mcp.ApplyToolsFilter(descs, allowed)
}

// bootstrapMemoryEntries walks cfg.Memory.Entries and writes any
// missing rows to the substrate. Idempotent: existing rows (matched
// by the (scope, scopeID, key) tuple) are left alone so runtime
// updates from the Memory tool / admin endpoints are preserved across
// reboots — yaml is a starting state, not a re-baseline.
//
// When an entry sets embed: true and the operator wired
// memory.embedder yaml, the embedding is computed synchronously and
// upserted. Boot prints a per-entry log line so the operator sees the
// time cost of embedding many entries.
func bootstrapMemoryEntries(ctx context.Context, cfg *config.Config, st store.Store, embedder providers.Embedder) {
	t0 := time.Now()
	loaded, skipped, failed := 0, 0, 0
	for i, e := range cfg.Memory.Entries {
		scope := store.MemoryScope(strings.TrimSpace(e.Scope))
		if scope == "" {
			scope = store.MemoryScopeGlobal
		}
		switch scope {
		case store.MemoryScopeGlobal, store.MemoryScopeAgent, store.MemoryScopeUser:
		default:
			log.Printf("memory.entries[%d]: skipping — invalid scope %q (must be global|agent|user)", i, e.Scope)
			failed++
			continue
		}
		key := strings.TrimSpace(e.Key)
		if key == "" {
			log.Printf("memory.entries[%d]: skipping — empty key", i)
			failed++
			continue
		}
		if _, err := st.MemoryGet(ctx, "", scope, e.ScopeID, key); err == nil {
			// Row already present — yaml seeding is a one-time
			// bootstrap, never an overwrite. This is the line that
			// makes the loader safe to re-run on every boot.
			//
			// Non-nil err here covers BOTH "row missing"
			// (ErrNotFound — the expected path on a fresh row) AND
			// transient store errors (e.g. a dropped DB connection).
			// We deliberately don't distinguish: the MemorySet
			// attempt below will fail loudly on a broken store and
			// the failure gets logged + counted in `failed`. The
			// alternative — bailing out on every Get error — would
			// mean a flaky DB at boot leaves the entries unseeded
			// without retry. Fail-on-Set lets a recovered store on
			// the next boot pick up where we left off.
			skipped++
			continue
		}
		valBytes, marshalErr := json.Marshal(e.Value)
		if marshalErr != nil {
			log.Printf("memory.entries[%d] (%s/%s/%s): skipping — value marshal failed: %v",
				i, scope, e.ScopeID, key, marshalErr)
			failed++
			continue
		}
		if err := st.MemorySet(ctx, "", scope, e.ScopeID, key, valBytes, 0); err != nil {
			log.Printf("memory.entries[%d] (%s/%s/%s): set failed: %v",
				i, scope, e.ScopeID, key, err)
			failed++
			continue
		}
		loaded++
		if !e.Embed {
			continue
		}
		// embed:true is best-effort — the k/v row stays even if the
		// embedding fails, matching the v0.9.0 Memory.set posture.
		if embedder == nil {
			log.Printf("memory.entries[%d] (%s/%s/%s): embed requested but memory.embedder is not configured — k/v written without embedding",
				i, scope, e.ScopeID, key)
			continue
		}
		if !st.SupportsVectors() {
			log.Printf("memory.entries[%d] (%s/%s/%s): embed requested but store does not support vectors — k/v written without embedding",
				i, scope, e.ScopeID, key)
			continue
		}
		te0 := time.Now()
		vecs, embedErr := embedder.Embed(ctx, []string{string(valBytes)})
		if embedErr != nil || len(vecs) != 1 {
			log.Printf("memory.entries[%d] (%s/%s/%s): embed failed: %v",
				i, scope, e.ScopeID, key, embedErr)
			continue
		}
		// Dimension is the OBSERVED width, never embedder.Dimension(): a driver
		// backed by a static (model → dim) table returns 0 for any model outside
		// it, and a stored 0 alongside a real vector poisons every later search
		// in this scope with dimension_mismatch. len(vecs[0]) is always true.
		if err := st.MemoryEmbedSet(ctx, "", scope, e.ScopeID, key, store.MemoryEmbedding{
			Provider:  embedder.Provider(),
			Model:     embedder.Model(),
			Dimension: len(vecs[0]),
			Vector:    vecs[0],
			EmbedText: string(valBytes),
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			log.Printf("memory.entries[%d] (%s/%s/%s): embed write failed: %v",
				i, scope, e.ScopeID, key, err)
			continue
		}
		log.Printf("memory.entries[%d] (%s/%s/%s): seeded + embedded (%s)",
			i, scope, e.ScopeID, key, time.Since(te0).Round(time.Millisecond))
	}
	log.Printf("memory.entries: bootstrap complete — loaded=%d skipped=%d failed=%d elapsed=%s",
		loaded, skipped, failed, time.Since(t0).Round(time.Millisecond))
}

// buildSearchProviders assembles the RFC BB search catalog from config: the
// provider registry, the fallback resolver (explicit search_priority, else the
// enabled set sorted for determinism), and the operator host-key map. Returns
// (nil, nil, nil) when no providers are configured — WebSearch then refuses with
// "not configured". config.Validate has already checked the enabled set, so a
// BuildRegistry error here is a bug (logged, WebSearch disabled).
//
// Back-compat: pre-RFC-BB, WebSearch worked on BRAVE_API_KEY alone. With no
// explicit search_providers, default to a single Brave provider when its key is
// set so existing deployments keep working with no config change.
func buildSearchProviders(cfg *config.Config) (*search.Registry, *search.Resolver, map[string]string) {
	hostKeys := map[string]string{}
	var specs []search.ProviderSpec
	var ids []string
	if len(cfg.SearchProviders) == 0 {
		if cfg.Env.BraveAPIKey != "" {
			specs = []search.ProviderSpec{{ID: "brave"}}
			hostKeys["brave"] = cfg.Env.BraveAPIKey
			ids = []string{"brave"}
		}
	} else {
		for id, spc := range cfg.SearchProviders {
			specs = append(specs, search.ProviderSpec{ID: id, BaseURL: spc.BaseURL})
			if k := cfg.SearchHostKey(id); k != "" {
				hostKeys[id] = k
			}
			ids = append(ids, id)
		}
	}
	if len(specs) == 0 {
		return nil, nil, nil
	}
	reg, err := search.BuildRegistry(specs)
	if err != nil {
		log.Printf("search: registry build error: %v (WebSearch disabled)", err)
		return nil, nil, nil
	}
	priority := cfg.SearchPriority
	if len(priority) == 0 {
		sort.Strings(ids)
		priority = ids
	}
	return reg, search.NewResolver(priority), hostKeys
}

// deadLinkMemoryScope maps a SQL-Memory scope onto the Memory scope its chunk
// bodies live in, and reports whether the scope is worth reconciling at all.
//
// `run` is excluded: an ephemeral scope is dropped whole at run end, so collecting
// its orphans is work with no beneficiary — and it has no durable body partition to
// reconcile against, so guessing one risks deleting another scope's bodies. The same
// exclusion the retention content-prune makes, for the same reason.
func deadLinkMemoryScope(scope string) (store.MemoryScope, bool) {
	switch scope {
	case "agent":
		return store.MemoryScopeAgent, true
	case "user":
		return store.MemoryScopeUser, true
	case "tenant":
		return store.MemoryScopeTenant, true
	}
	return "", false
}
