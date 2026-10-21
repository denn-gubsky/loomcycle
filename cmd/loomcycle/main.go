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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	mcpsign "github.com/denn-gubsky/loomcycle/internal/mcp"
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
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storepostgres "github.com/denn-gubsky/loomcycle/internal/store/postgres"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"

	"github.com/denn-gubsky/loomcycle/internal/tools"
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
	setupLogging()
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

	// The boot phases, in the order their side effects must happen: each
	// phase's log lines, goroutines and listeners come after the previous
	// phase's. See boot.go for what they hand one another.
	b.loadConfig()    // 1. config
	b.initTelemetry() //    and tracing

	b.buildProviders() // 2. providers
	b.buildTools()     // 3. built-in tools
	b.addLocalAPITools()
	b.logToolPosture()
	b.buildMCP()            // 4. MCP client pool
	b.openAndMigrateStore() // 5. store
	// Boot-time checks that need the store belong here: after it is open and
	// migrated, before any subsystem is wired to it.

	b.wireMemory() // 6. the store and credentials, into the tools
	b.wireCredentials()
	b.bindStoreToTools()
	b.loadMCPRegistry()
	b.addA2ATools()

	b.buildServer() // 7. the HTTP server and its wiring
	b.openSQLMemory()
	b.wireSubstrateTools()
	b.wireAuth()
	b.wireServerCatalog()
	b.wireBuses()
	b.buildModelResolver()
	b.mountA2AServer()
	b.mountWebhooks()
	b.buildHTTPServer()

	b.startBackground() // 8. background work and triggers
	b.startCluster()
	b.startRunSweepers()
	b.seedServerState()
	b.startStoreSweepers()
	b.startPauseAndResume()
	b.startTeamSubscriptions()
	b.startBootReconciles()
	b.startMetrics()
	b.startScheduler()
	b.logChannelSummary()
	b.startSessionGC()
	b.startVolumeAndAgentSweepers()

	b.startMCPSurfaces() // 9. MCP surfaces, resolver probe, config reload
	b.logSessionLockGC()
	b.startResolveProbe()
	b.wireConfigReload()

	b.startListeners()  // 10. listeners
	b.waitAndShutdown() // 11. block until a signal, then shut down
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
