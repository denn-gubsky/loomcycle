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
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
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
	"syscall"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/agents"
	a2aapi "github.com/denn-gubsky/loomcycle/internal/api/a2a"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	webhookapi "github.com/denn-gubsky/loomcycle/internal/api/webhook"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/cli"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/heartbeat"
	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	mcpsign "github.com/denn-gubsky/loomcycle/internal/mcp"
	"github.com/denn-gubsky/loomcycle/internal/metrics"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	anthropic_oauth_dev "github.com/denn-gubsky/loomcycle/internal/providers/anthropic_oauth_dev"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/providers/deepseek"
	"github.com/denn-gubsky/loomcycle/internal/providers/gemini"
	mockprov "github.com/denn-gubsky/loomcycle/internal/providers/mock"
	"github.com/denn-gubsky/loomcycle/internal/providers/ollama"
	"github.com/denn-gubsky/loomcycle/internal/providers/openai"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
	// Deterministic stub embedder for runtime tests; its init() registers the
	// "stub" provider ONLY when LOOMCYCLE_EMBEDDER_STUB=1, so it is invisible
	// to a production binary that never sets the flag.
	_ "github.com/denn-gubsky/loomcycle/internal/providers/stubembedder"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/scheduler"
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storepostgres "github.com/denn-gubsky/loomcycle/internal/store/postgres"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"

	googlegrpc "google.golang.org/grpc"

	loomgrpc "github.com/denn-gubsky/loomcycle/internal/api/grpc"
	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lcmcp "github.com/denn-gubsky/loomcycle/internal/api/mcp"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	toolsa2a "github.com/denn-gubsky/loomcycle/internal/tools/a2a"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/localapi"
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

func main() {
	// Resolve build identifiers FIRST so subcommands (validate / agents
	// / health / migrate) and --version see the same auto-resolved
	// values. ldflags overrides win — release scripts that explicitly
	// set buildVersion/buildCommit/buildTime get exactly those.
	autoVersion, autoCommit, autoTime := resolveBuildInfo()
	if buildVersion == "" {
		buildVersion = autoVersion
	}
	if buildCommit == "" {
		buildCommit = autoCommit
	}
	if buildTime == "" {
		buildTime = autoTime
	}
	if buildVersion == "" {
		buildVersion = "unknown"
	}
	if buildCommit == "" {
		buildCommit = "unknown"
	}
	if buildTime == "" {
		buildTime = "unknown"
	}

	// Subcommand dispatch BEFORE flag parsing — let `loomcycle
	// validate ...` flow into the CLI surface without colliding with
	// the server's own --config flag.
	//
	// First non-flag arg is the subcommand keyword. If it's one of
	// the known subcommands, hand off to internal/cli and exit.
	// Otherwise fall through to the server entry point (preserves
	// backwards compat: `loomcycle --config foo.yaml` still works).
	//
	// The `mcp` subcommand is a special case (v0.8.15+): it starts
	// the SAME server setup as the default flow, plus a stdio MCP
	// listener reading os.Stdin / writing os.Stdout. CRITICAL: in
	// mcp mode, no code may write to os.Stdout outside the MCP loop
	// — stdout is the JSON-RPC wire. The stdlib `log` package
	// defaults to os.Stderr (so log lines are safe); any future
	// `fmt.Println` / direct os.Stdout writes elsewhere in the
	// binary would corrupt the protocol.
	mcpMode := false
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "validate":
			os.Exit(cli.RunValidate(os.Args[2:], os.Stdout, os.Stderr))
		case "agents":
			os.Exit(cli.RunAgents(os.Args[2:], os.Stdout, os.Stderr))
		case "anthropic":
			// v0.11.9 — OAuth-dev subcommands (login / status / logout).
			// Gated by LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1 inside
			// the subcommand handler; without the env var, the
			// subcommand prints a clear error pointing at docs.
			os.Exit(cli.RunAnthropic(os.Args[2:], os.Stdout, os.Stderr))
		case "health":
			os.Exit(cli.RunHealth(os.Args[2:], os.Stdout, os.Stderr))
		case "migrate":
			os.Exit(cli.RunMigrate(os.Args[2:], os.Stdout, os.Stderr))
		case "operator-token":
			// RFC L — mint / rotate / retire / show / list auth tokens
			// against the running instance's admin endpoint.
			os.Exit(cli.RunOperatorToken(os.Args[2:], os.Stdout, os.Stderr))
		// v0.8.17 runtime admin subcommands. Each is a thin HTTP
		// client to the corresponding /v1/_pause / _resume / _state /
		// _snapshots endpoint on the running instance addressed by
		// $LOOMCYCLE_BASE_URL.
		case "pause":
			os.Exit(cli.RunPause(os.Args[2:], os.Stdout, os.Stderr))
		case "resume":
			os.Exit(cli.RunResume(os.Args[2:], os.Stdout, os.Stderr))
		case "state":
			os.Exit(cli.RunState(os.Args[2:], os.Stdout, os.Stderr))
		case "snapshot":
			os.Exit(cli.RunSnapshot(os.Args[2:], os.Stdout, os.Stderr))
		case "snapshots":
			// `snapshots <verb>` dispatch — list / export / delete.
			if len(os.Args) < 3 {
				fmt.Fprintln(os.Stderr, "loomcycle: error: usage: loomcycle snapshots <list|export|delete> ...")
				os.Exit(2)
			}
			switch os.Args[2] {
			case "list":
				os.Exit(cli.RunSnapshotsList(os.Args[3:], os.Stdout, os.Stderr))
			case "export":
				os.Exit(cli.RunSnapshotsExport(os.Args[3:], os.Stdout, os.Stderr))
			case "delete":
				os.Exit(cli.RunSnapshotsDelete(os.Args[3:], os.Stdout, os.Stderr))
			default:
				fmt.Fprintf(os.Stderr, "loomcycle: error: unknown snapshots subcommand %q (want: list / export / delete)\n", os.Args[2])
				os.Exit(2)
			}
		case "restore":
			os.Exit(cli.RunRestore(os.Args[2:], os.Stdout, os.Stderr))
		case "hash":
			os.Exit(cli.RunHash(os.Args[2:], os.Stdout, os.Stderr))
		case "mcp":
			// v0.12.x: `loomcycle mcp install` prints copy-paste config
			// snippets for Claude Code / Claude Desktop. Must be checked
			// BEFORE the mcpMode strip below — otherwise "install" leaks
			// into the server's flag set and is mis-parsed as a positional.
			if len(os.Args) >= 3 && os.Args[2] == "install" {
				os.Exit(cli.RunMCPInstall(os.Args[3:], os.Stdout, os.Stderr))
			}
			mcpMode = true
			// Strip "mcp" from os.Args so flag.Parse() below sees
			// the remaining flags (--config etc.) at index 1+.
			os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
		case "init":
			os.Exit(cli.RunInit(os.Args[2:], os.Stdout, os.Stderr))
		case "doctor":
			os.Exit(cli.RunDoctor(os.Args[2:], os.Stdout, os.Stderr))
		case "mcp-registry":
			// v1.x RFC C1: curated MCP server recipe library.
			// Seven verbs (list / show / append-to-config / add /
			// remove / enable / disable) dispatched inside the Run
			// helper. Reads bundled recipes + LOOMCYCLE_MCP_RECIPES_ROOT
			// overlay; emits yaml into the operator's loomcycle.yaml
			// via append-to-config.
			os.Exit(cli.RunMCPRegistry(os.Args[2:], os.Stdout, os.Stderr))
		case "import":
			// v1.x RFC C2: Claude Code repo ingestion. Currently the
			// only subverb is `claude-code`; future subverbs cover
			// other source shapes (plain .mcp.json directories, etc.).
			// Reads a .claude/ tree and emits agents:/mcp_servers:/skill
			// fragments into the operator's loomcycle.yaml.
			os.Exit(cli.RunImport(os.Args[2:], os.Stdout, os.Stderr))
		case "memory-eval":
			// v1.x RFC I (MR-5): scores the memory ranker/dedup against a
			// dataset of (query, expected_recall) tuples — precision@k,
			// recall@k, duplication_rate. The gating tool for ranker PRs.
			os.Exit(cli.RunMemoryEval(os.Args[2:], os.Stdout, os.Stderr))
		case "help", "-h", "--help":
			cli.PrintHelp(os.Stdout, lcmcp.MetaToolCount())
			return
		}
	}

	cfgPath := flag.String("config", "loomcycle.yaml", "path to config YAML")
	showVersion := flag.Bool("version", false, "print build identifier and exit")
	// v0.8.15.2: suppress the HTTP listener. Useful when the daemon
	// (`loomcycle.sh`, binds 127.0.0.1:8787 by default) is already
	// running on the same host AND Claude Code spawns `loomcycle mcp`
	// alongside it. Without --no-http the mcp subcommand collides on
	// the port. Only honoured in mcp mode — in server mode it warns
	// and starts HTTP normally so CI scripts conditionally passing the
	// flag don't hard-fail.
	noHTTP := flag.Bool("no-http", false, "suppress the HTTP listener (only honoured in `mcp` subcommand mode)")
	flag.Parse()

	// Resolve --no-http: takes effect only when mcpMode is true. In
	// server mode the flag is ignored with a visible warning per RFC
	// decision B2 — silently dropping it would be confusing; failing
	// hard would break CI scripts that pass the flag conditionally.
	skipHTTP := false
	if *noHTTP {
		if mcpMode {
			skipHTTP = true
		} else {
			log.Printf("note: --no-http only takes effect in `loomcycle mcp` mode; flag ignored, HTTP listener will start normally")
		}
	}

	if *showVersion {
		fmt.Printf("loomcycle version=%s commit=%s built=%s go=%s\n",
			buildVersion, buildCommit, buildTime, runtime.Version())
		return
	}

	// Identify ourselves first thing so an operator running a stale
	// binary spots it immediately — before any "but my code says X"
	// debugging spiral. Critical when development cycle is "git pull
	// && restart" without a rebuild step in between.
	log.Printf("loomcycle build: version=%s commit=%s time=%s", buildVersion, buildCommit, buildTime)

	// v0.11.1 auto-discovery: when --config wasn't overridden AND
	// the default file doesn't exist, walk the XDG paths. The
	// search list is the same one `loomcycle doctor` uses, so the
	// two stay in lockstep. Explicit --config /path is unchanged.
	resolvedCfg, found := resolveConfigPath(*cfgPath)
	if !found {
		fmt.Fprintln(os.Stderr, "loomcycle: no config found at any of:")
		for _, p := range configAutoDiscoveryPaths() {
			fmt.Fprintf(os.Stderr, "    %s\n", p)
		}
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Run `loomcycle init` to create one, or pass --config <path> to use an existing file.")
		os.Exit(1)
	}
	// Auto-load <configdir>/auth.env (companion to `loomcycle init
	// --with-token`) BEFORE config.Load reads os.Getenv, so a persisted
	// LOOMCYCLE_AUTH_TOKEN is in scope without a shell-rc edit. Set-if-unset
	// (real env wins), so an explicit shell export still overrides it.
	if authEnvPath, n, err := cli.LoadAuthEnv(resolvedCfg); err != nil {
		log.Printf("auth.env: %v (continuing without it)", err)
	} else if n > 0 {
		log.Printf("auth.env: loaded %d var(s) from %s (a shell export overrides them)", n, authEnvPath)
	}
	cfg, err := config.Load(resolvedCfg)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// v0.10.0 OpenTelemetry tracer bootstrap. No-op when
	// LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT is unset, so zero runtime
	// cost on deployments that haven't opted in. When set, every
	// otel.Tracer(...).Start(ctx, ...) call across the codebase emits
	// to the configured OTLP/HTTP collector.
	otelShutdown, otelErr := lcotel.Init(lcotel.Config{
		Endpoint:       cfg.Env.OTELExporterEndpoint,
		Headers:        cfg.Env.OTELExporterHeaders,
		ServiceName:    cfg.Env.OTELServiceName,
		ServiceVersion: buildVersion,
		SamplerRatio:   cfg.Env.OTELTracesSamplerRatio,
	})
	if otelErr != nil {
		log.Fatalf("otel: %v", otelErr)
	}
	if cfg.Env.OTELExporterEndpoint != "" {
		log.Printf("otel: tracer enabled — endpoint=%s service=%s sampler=traceidratio@%g",
			cfg.Env.OTELExporterEndpoint, cfg.Env.OTELServiceName, cfg.Env.OTELTracesSamplerRatio)
	} else {
		log.Printf("otel: disabled (set LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT to enable)")
	}

	pr := newProviderResolver(cfg)
	validateCodeAgents(cfg, pr)
	sem := concurrency.New(
		cfg.Concurrency.MaxConcurrentRuns,
		cfg.Concurrency.MaxQueueDepth,
		cfg.Concurrency.QueueTimeout(),
	).WithPerUserCap(cfg.Concurrency.MaxConcurrentRunsPerUser)
	if cfg.Concurrency.MaxConcurrentRunsPerUser > 0 {
		log.Printf("concurrency: per-user cap enabled — max_concurrent_runs_per_user=%d (global cap=%d)",
			cfg.Concurrency.MaxConcurrentRunsPerUser, cfg.Concurrency.MaxConcurrentRuns)
	}
	// All built-in tools are SANDBOXED: each refuses every call until the
	// operator explicitly enables it via env. We register every tool and
	// log a note for any that's still disabled — that way the model sees
	// a clear "tool refused" error instead of a confusing "unknown tool",
	// and operators see at startup which tools they've configured.
	// Strip loopback aliases (localhost, 127.0.0.1, etc.) from the
	// operator's static allowlist. Belt-and-braces — the IP-level
	// guard at dial time also rejects loopback, but stripping here
	// means loopback never appears in the tool's effective list and
	// operators don't get fooled by seeing "localhost" listed.
	//
	// PrivateHostAllowlist acts as the exemption: if the operator
	// has explicitly opted in to a loopback host via that env var,
	// it survives the strip on the main allowlist too. This is what
	// lets a single "localhost" entry mean "agents may reach this
	// loopback host AND the IP-private check is lifted for it".
	staticHosts := builtin.StripLocalhostAliases(
		cfg.Env.HTTPHostAllowlist,
		cfg.Env.HTTPPrivateHostAllowlist,
	)
	httpTool := &builtin.HTTP{
		HostAllowlist:        staticHosts,
		PrivateHostAllowlist: cfg.Env.HTTPPrivateHostAllowlist,
	}
	// Skill tool reuses the same name→body registry that the static
	// bundling path (Approach A in internal/config) uses. Loading once
	// at boot keeps the in-memory map authoritative — SIGHUP-style
	// hot-reload of skills is a future enhancement.
	skillSet, err := skills.LoadSet(cfg.Env.SkillsRoot)
	if err != nil {
		log.Fatalf("skills: %v", err)
	}
	if cfg.Env.SkillsRoot != "" {
		log.Printf("skills: loaded %d from %s", len(skillSet.Names()), cfg.Env.SkillsRoot)
	}
	// v0.8.8 help topics — bundled defaults overlaid with operator
	// .md files from LOOMCYCLE_HELP_ROOT. Always non-nil after this
	// load; bundled-only deployments get the default topic set.
	helpSet, err := help.LoadSet(cfg.Env.HelpRoot)
	if err != nil {
		log.Fatalf("help: %v", err)
	}
	if cfg.Env.HelpRoot != "" {
		log.Printf("help: loaded %d topics (filesystem overlay at %s)", len(helpSet.Names()), cfg.Env.HelpRoot)
	} else {
		log.Printf("help: loaded %d bundled topics (no LOOMCYCLE_HELP_ROOT overlay)", len(helpSet.Names()))
	}
	if cfg.Env.AgentsRoot != "" {
		// Agents-from-MD discovery happened inside config.Load (must run
		// before resolveSystemPromptFiles so the merged map flows through
		// the existing pipeline). Log the count post-merge so operators
		// see the final cfg.Agents size, which may include yaml-only
		// entries on top of the discovered ones.
		log.Printf("agents: discovered from %s — total in cfg.Agents (after yaml merge): %d", cfg.Env.AgentsRoot, len(cfg.Agents))
	}
	if len(cfg.UserTiers) > 0 {
		// v0.8.2 — log configured user_tier policies at boot so
		// operators see what's available. "default" surfaces first
		// when present (it's the required entry per validation); the
		// rest sort lexicographically.
		names := make([]string, 0, len(cfg.UserTiers))
		for n := range cfg.UserTiers {
			if n != "default" {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		if _, hasDefault := cfg.UserTiers["default"]; hasDefault {
			names = append([]string{"default"}, names...)
		}
		log.Printf("user_tiers: configured %d — %s", len(names), strings.Join(names, " / "))
	}

	allTools := []tools.Tool{
		&builtin.Read{Root: cfg.Env.ReadRoot},
		&builtin.Write{Root: cfg.Env.WriteRoot},
		&builtin.Edit{Root: cfg.Env.WriteRoot},
		// Grep + Glob ride the Read sandbox (read-only content/path
		// search). NotebookEdit rides the Write sandbox (it mutates
		// .ipynb files atomically, same posture as Edit).
		&builtin.Grep{Root: cfg.Env.ReadRoot},
		&builtin.Glob{Root: cfg.Env.ReadRoot},
		&builtin.NotebookEdit{Root: cfg.Env.WriteRoot},
		httpTool,
		&builtin.WebFetch{HTTP: httpTool},
		&builtin.WebSearch{APIKey: cfg.Env.BraveAPIKey},
		&builtin.Bash{Enabled: cfg.Env.BashEnabled, Cwd: cfg.Env.BashCwd},
		// SkillTool's Store is late-bound below (so DB-active SkillDef
		// rows override the static Set body). Nil-Store before
		// late-binding falls back to the static Set; the assignment
		// after storeIface is constructed switches the resolution
		// order to DB-first.
	}
	skillTool := &builtin.SkillTool{Set: skillSet}
	allTools = append(allTools, skillTool)
	// Memory tool — wired post-store so it can grab the live Store
	// reference. Registered unconditionally; access is gated per-agent
	// via memory_scopes yaml + the Memory.Store==nil branch when the
	// runtime hasn't configured a store backend.
	// v0.9.0 Vector Memory: construct the embedder from
	// cfg.Memory.Embedder when the operator yaml set one. Held in
	// the local `embedder` var so the admin /v1/_memory/reembed
	// endpoint (PR 4) can reach it too. When the embedder block is
	// unset, vector ops refuse with embedder_not_configured at the
	// tool layer.
	embedder, err := buildEmbedder(cfg)
	if err != nil {
		log.Fatalf("embedder: %v", err)
	}
	if embedder != nil {
		log.Printf("embedder: %s/%s (dim=%d)", embedder.Provider(), embedder.Model(), embedder.Dimension())
	}

	// RFC I MR-4: the env-allowlist gate for the mem9 backend's X-API-Key.
	// Same allowlist the scheduler + webhooks use (cfg.Env.SchedulerEnvAllowlist).
	memoryEnvAllowlist := make(map[string]bool, len(cfg.Env.SchedulerEnvAllowlist))
	for _, name := range cfg.Env.SchedulerEnvAllowlist {
		memoryEnvAllowlist[name] = true
	}

	memoryTool := &builtin.Memory{
		MaxValueBytes:     cfg.Env.MemoryMaxValueBytes,
		DefaultQuotaBytes: cfg.Env.MemoryMaxScopeBytes,
		Embedder:          embedder,
		// RFC I MR-3b: resolves a per-agent memory_backend NAME to its
		// MemoryBackendDef (static yaml or dynamic substrate Def).
		Cfg: cfg,
		// RFC I MR-4: gates which env vars the mem9 backend may read for
		// its X-API-Key. Reuses the scheduler/webhook env allowlist — no
		// new credential surface (Decision 10).
		EnvAllowlist: memoryEnvAllowlist,
	}
	allTools = append(allTools, memoryTool)

	// Channel tool (v0.8.4) — persistent inter-agent message bus.
	// One Bus instance per process so in-process subscribers waiting
	// in long-poll mode get sub-millisecond notification. Same
	// Store==nil branch as Memory: tool registered unconditionally,
	// declines all ops when no store configured. Per-agent ACL via
	// `channels:` yaml + the operator-declared top-level `channels:`
	// block.
	channelBus := channels.NewBus()
	// v0.8.6 scheduler — arms time.AfterFunc timers for deferred
	// publishes so long-poll subscribers wake at visible_at. Bounded
	// by LOOMCYCLE_CHANNELS_MAX_PENDING_DEFERRED (default 10000).
	channelScheduler := channels.NewScheduler(channelBus, cfg.Env.ChannelsMaxPendingDeferred)
	channelTool := &builtin.Channel{
		Bus:           channelBus,
		Scheduler:     channelScheduler,
		MaxValueBytes: cfg.Env.ChannelsMaxValueBytes,
		LongPollCapMS: cfg.Env.ChannelsLongPollCapMS,
	}
	allTools = append(allTools, channelTool)

	// AgentDef tool (v0.8.5). Per-agent default-deny via
	// agent_def_scopes yaml; the tool itself is registered globally
	// and the policy gate runs inside Execute.
	agentDefTool := &builtin.AgentDef{
		Cfg:                 cfg,
		MaxDefinitionBytes:  cfg.Env.AgentDefMaxDefinitionBytes,
		MaxDescriptionBytes: cfg.Env.AgentDefMaxDescriptionBytes,
		MaxCodeBytes:        cfg.Env.AgentDefMaxCodeBytes,
	}
	allTools = append(allTools, agentDefTool)

	// SkillDef tool (v0.8.22). Mirror of AgentDef for runtime skill
	// mutation. Same default-deny gate (skill_def_scopes yaml). Set
	// is shared with the SkillTool above so static-name guard +
	// fork-bootstrap see the same static skills.
	skillDefTool := &builtin.SkillDef{
		Set:                 skillSet,
		MaxBodyBytes:        cfg.Env.SkillDefMaxBodyBytes,
		MaxDescriptionBytes: cfg.Env.SkillDefMaxDescriptionBytes,
	}
	allTools = append(allTools, skillDefTool)

	// MCPServerDef tool construction is deferred until after the pool
	// + dynamic registry are built (operator-admin-only — NOT appended
	// to allTools; wired via SetMCPServerDefTool after the pool exists).

	// Evaluation tool (v0.8.5). Selection half of the substrate;
	// emitter_role is derived server-side from the caller's RunIdentity
	// vs the target run's identity, and per-agent yaml `evaluation_scopes`
	// gate which roles may submit + whether read ops are allowed.
	evaluationTool := &builtin.Evaluation{
		MaxJudgementBytes: cfg.Env.EvaluationMaxJudgementBytes,
		MaxRationaleBytes: cfg.Env.EvaluationMaxRationaleBytes,
	}
	allTools = append(allTools, evaluationTool)

	// Interruption tool (v0.8.16) — human-in-the-loop primitive. Three
	// ops (ask / notify / cancel) gated by per-agent
	// `interruption: {enabled, kinds, max_pending}` yaml. The tool
	// blocks via channels.Bus, so it reuses the same bus instance the
	// Channel tool's long-poll subscribe uses. SystemPublisher (set
	// below alongside Store) carries the external `_system/interrupts/
	// pending` / `resolved` signal for non-run consumers (Web UI inbox,
	// Slack notifier).
	interruptionTool := &builtin.Interruption{
		Bus:               channelBus,
		Backend:           cfg.Interruption.Backend,
		DefaultTimeout:    time.Duration(cfg.Interruption.DefaultTimeoutMS) * time.Millisecond,
		MaxTimeout:        time.Duration(cfg.Interruption.MaxTimeoutMS) * time.Millisecond,
		HeartbeatInterval: time.Duration(cfg.Interruption.HeartbeatIntervalMS) * time.Millisecond,
		MaxPendingPerRun:  cfg.Interruption.MaxPendingPerRun,
	}
	allTools = append(allTools, interruptionTool)

	// Context tool (v0.8.7). Read-only runtime introspection. The Tools
	// field is back-filled with the FULL allTools slice after every
	// other registration (MCP + localapi) so doc/tools ops reflect the
	// complete catalog. Including Context itself in the catalog is
	// intentional — agents introspecting "what tools do I have" should
	// see Context, and `doc(name="Context")` should work. Cfg + Store
	// power the v0.8.7 PR 2 substrate-coupled ops (agents / lineage /
	// evaluations); Store is late-bound below alongside the other
	// substrate tools.
	contextTool := &builtin.Context{Cfg: cfg, Help: helpSet}
	allTools = append(allTools, contextTool)

	// Local API MCP gateway (v0.4.0+). When `local_api.spec` is set
	// in loomcycle.yaml, parse the OpenAPI spec and register one tool
	// per operation. Each tool forwards calls to local_api.base_url
	// with the agent's `bearer` field as Authorization. Replaces the
	// curl-shaped HTTP-tool pattern Phase B agents currently use.
	if cfg.LocalAPI.SpecPath != "" {
		laTools, laWarns, err := localapi.Build(localapi.Config{
			SpecPath:       cfg.LocalAPI.SpecPath,
			BaseURL:        cfg.LocalAPI.BaseURL,
			ToolNamePrefix: cfg.LocalAPI.ToolNamePrefix,
		}, cfg.ConfigDir())
		if err != nil {
			log.Printf("local-api gateway disabled: %v", err)
		} else {
			for _, w := range laWarns {
				log.Printf("local-api: %s", w)
			}
			log.Printf("local-api: registered %d tools from %s", len(laTools), cfg.LocalAPI.SpecPath)
			allTools = append(allTools, laTools...)
		}
	}
	if cfg.Env.ReadRoot == "" {
		log.Printf("note: Read tool is registered but disabled — set LOOMCYCLE_READ_ROOT to enable")
	}
	if cfg.Env.WriteRoot == "" {
		log.Printf("note: Write + Edit tools are registered but disabled — set LOOMCYCLE_WRITE_ROOT to enable")
	}
	if len(staticHosts) == 0 && !cfg.Env.HTTPCallerAuthoritative {
		log.Printf("note: HTTP + WebFetch tools are registered but disabled — set LOOMCYCLE_HTTP_HOST_ALLOWLIST to enable (or LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE=1 to delegate the allowlist to the caller)")
	}
	if cfg.Env.HTTPCallerAuthoritative {
		log.Printf("note: HTTP_CALLER_AUTHORITATIVE=1 — caller's allowed_hosts is the sole policy; operator's static list is fallback only")
	}
	if len(cfg.Env.HTTPPrivateHostAllowlist) > 0 {
		log.Printf("note: HTTP_PRIVATE_HOST_ALLOWLIST=%v — these hosts may resolve to private IPs (e.g. localhost callbacks)", cfg.Env.HTTPPrivateHostAllowlist)
	}
	if cfg.Env.BraveAPIKey == "" {
		log.Printf("note: WebSearch tool is registered but disabled — set BRAVE_API_KEY to enable")
	}
	if cfg.Env.BashEnabled {
		log.Printf("WARNING: Bash tool is enabled (LOOMCYCLE_BASH_ENABLED=1). This is NOT a true sandbox — run loomcycle inside a container or VM if you expose this to untrusted prompts.")
		if cfg.Env.BashCwd == "" {
			log.Printf("note: Bash is enabled but has no cwd; every call will refuse — set LOOMCYCLE_BASH_CWD")
		}
	} else {
		log.Printf("note: Bash tool is registered but disabled — set LOOMCYCLE_BASH_ENABLED=1 to enable (NOT a true sandbox; see docs)")
	}

	// Per-agent tool policy is default-deny: an agent with no allowed_tools
	// in YAML gets ZERO tools at the dispatcher. Warn at startup so
	// operators don't discover this only when an agent inexplicably can't
	// do anything. We log per-agent rather than once so the operator sees
	// which definitions are affected.
	for name, def := range cfg.Agents {
		if len(def.AllowedTools) == 0 {
			log.Printf("note: agent %q has no allowed_tools — it will see zero tools (intentional default-deny; add allowed_tools to its YAML to expose tools)", name)
		}
	}

	// MCP: spawn declared servers (stdio or http), discover their tools,
	// register each as `mcp__{server}__{tool}` alongside the built-ins.
	// Failures to spawn or handshake are logged and the server is skipped —
	// the other servers still come up.
	// v0.9.x dynamic MCP server registration substrate. In-process
	// registry consulted by the pool's build callback on every name
	// not found in the static yaml map. Mutated by the MCPServerDef
	// substrate tool's create / promote / retire ops; loaded from DB
	// at boot below so previous-deployment registrations survive
	// restart.
	dynamicMCPRegistry := mcp.NewDynamicRegistry()

	// RFC N: the pool's build callback resolves through lookup.MCPServer
	// with the run's tenant so a per-tenant dynamic registration dials its
	// OWN URL — not a shared one of the same name. A tenant-scoped dynamic
	// shadow takes precedence; otherwise the static yaml base resolves;
	// otherwise a shared ("") dynamic registration. stdio is yaml-only and
	// resolved straight from cfg (lookup.MCPServerSpec omits stdio fields).
	mcpDynView := mcpLookupView{dynamicMCPRegistry}
	mcpPool := mcp.NewPool(
		func(tenant, name string) (mcp.Caller, error) {
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
			switch spec.Transport {
			case "http", "streamable-http":
				return mcphttp.New(mcphttp.Config{
					URL:     spec.URL,
					Headers: spec.Headers,
				})
			default:
				return nil, fmt.Errorf("mcp_servers.%s: invalid transport %q for non-stdio resolution (data corruption?)", name, spec.Transport)
			}
		},
		func(c mcp.Caller) {
			// Both stdio.Client and http.Client implement Close() error.
			// A future transport that doesn't gets logged so the leak is
			// at least visible to operators.
			type closer interface{ Close() error }
			cl, ok := c.(closer)
			if !ok {
				log.Printf("mcp pool: teardown skipped (%T does not implement Close)", c)
				return
			}
			_ = cl.Close()
		},
		// RFC N tenantOf: the pool cache is keyed by (tenant, name). The
		// tenant is the authoritative principal's tenant carried on the run
		// ctx via RunIdentity (set from applyPrincipal / inherited by
		// sub-agents). RunIdentity, not the http auth package, keeps the
		// pool free of an import cycle. "" = shared/legacy tenant.
		func(ctx context.Context) string {
			return tools.RunIdentity(ctx).TenantID
		},
	)
	defer mcpPool.Close()

	// v0.9.x MCPServerDef tool — operator-admin-only. NOT appended to
	// allTools; wired into the HTTP server via SetMCPServerDefTool
	// below. Registry + Pool wired so create/promote installs entries
	// into both the DB AND the in-process registry the pool's build
	// callback consults; Pool is used for evict on retire / promote-
	// replaces and for the `rediscover` op's tools/list refresh.
	mcpServerDefTool := &builtin.MCPServerDef{
		Cfg:                 cfg,
		Registry:            dynamicMCPRegistry,
		Pool:                mcpPool,
		MaxDefinitionBytes:  cfg.Env.AgentDefMaxDefinitionBytes,
		MaxDescriptionBytes: cfg.Env.AgentDefMaxDescriptionBytes,
	}

	// Initialise each server, apply per-server allowed_tools filter, and
	// register the resulting tools alongside the built-ins.
	//
	// Budget: 30s SHARED across all servers, not per-server. With many
	// servers configured, per-server timeouts can serially stack up past
	// K8s liveness/readiness deadlines and put the pod in a restart loop
	// before /healthz ever serves once. A shared ctx caps total startup
	// blocking; remaining servers after the budget exits are skipped with
	// a clear log line — they can be retried on demand via the lazy
	// pool resolution path on the first agent run that needs them.
	//
	// GetWithRetry handles the chicken-and-egg start order: when an MCP
	// server lives behind a dependency that boots concurrently with
	// loomcycle (e.g. a Next.js dev server compiling its /api/mcp route
	// on first request), the first handshake attempt may fail with
	// ECONNREFUSED or a 404. GetWithRetry backs off (500ms, 1s, 2s, 4s,
	// 8s, 16s) until success or ctx exhaustion. Each retry logs.
	//
	// v0.8.15.1: skip the eager init entirely in `loomcycle mcp` mode.
	// The consumer (Claude Code, custom MCP orchestrator) expects the
	// stdio JSON-RPC loop to answer initialize / tools/list within
	// its short discovery timeout — well under the ~32s exponential-
	// backoff budget GetWithRetry can spend per unreachable upstream.
	// The lazy resolver wired below handles "server wasn't in pool at
	// boot" cleanly since v0.8.1; the first agent call that needs an
	// mcp__<server>__<tool> tool triggers the handshake there. Server
	// mode keeps eager init — fail-fast is correct when the operator
	// is starting a long-lived daemon.
	if mcpMode {
		log.Printf("mcp mode: skipping eager upstream MCP init for %d server(s); lazy resolver will handshake on first agent call", len(cfg.MCPServers))
	} else {
		mcpInitCtx, mcpInitCancel := context.WithTimeout(context.Background(), 30*time.Second)
		for name, srv := range cfg.MCPServers {
			_, descs, err := mcpPool.GetWithRetry(mcpInitCtx, name, log.Printf)
			if err != nil {
				log.Printf("mcp[%s]: skipped — %v", name, err)
				continue
			}
			filtered := applyAllowedToolsFilter(descs, srv.AllowedTools)
			for _, d := range filtered {
				allTools = append(allTools, mcp.NewTool(mcpPool, name, d))
			}
			log.Printf("mcp[%s]: ready, %d/%d tools registered (transport=%s)",
				name, len(filtered), len(descs), srv.Transport)
		}
		mcpInitCancel()
	}

	// Lazy MCP recovery — when an agent calls a tool from a server that
	// failed initial handshake (skipped above), this resolver tries one
	// fresh pool.Get on the agent's call path. On success the server's
	// tools are memoised in the resolver and dispatched. Subsequent
	// calls hit the cache without re-handshaking.
	//
	// Without this, an MCP peer that's down at loomcycle boot stays
	// invisible for the lifetime of the process — operators have to
	// notice the "skipped" log line and restart loomcycle by hand.
	// In a server environment where peers (jobs-search-web, other MCP
	// services) restart independently, that's recurring operational
	// pain. See internal/tools/mcp/lazy.go for the state machine. Membership +
	// per-server allowed_tools resolve through the shared lookup.MCPServer
	// (static cfg.MCPServers → dynamic registry), so cfg + the registry are
	// passed straight through.
	mcpLazyResolver := mcp.NewLazyResolver(mcpPool, cfg, dynamicMCPRegistry, func(server string, count int) {
		log.Printf("mcp[%s]: lazy-registered %d tool(s) on first agent call", server, count)
	}, 0)

	// Storage: SQLite (default, compact installs) or Postgres
	// (production, hundreds of concurrent agents). Both adapters
	// implement the same store.Store interface; they're tested against
	// a shared contract suite in CI so they can't drift silently.
	storeIface, storeCloser, err := openStore(cfg)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer storeCloser()

	// v0.9.x content_sha256 backfill — populate the new column on
	// every NULL/empty row left over from a pre-v0.9.x upgrade OR
	// restored from a pre-v0.9.x snapshot. Idempotent + fast: a
	// second boot scans zero NULL rows. Failures here don't fatal
	// the start — the column just stays NULL and `verify` returns
	// matches=false until the row is naturally re-touched via
	// AgentDef set/fork.
	bfCtx, bfCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if n, err := storeIface.BackfillAgentDefContentSHA256(bfCtx, backfillAgentDefSignFn); err != nil {
		log.Printf("agent_defs: backfill content_sha256 partial — %v (rows updated before error: %d)", err, n)
	} else if n > 0 {
		log.Printf("agent_defs: backfilled %d rows with content_sha256", n)
	}
	if n, err := storeIface.BackfillSkillDefContentSHA256(bfCtx, backfillSkillDefSignFn); err != nil {
		log.Printf("skill_defs: backfill content_sha256 partial — %v (rows updated before error: %d)", err, n)
	} else if n > 0 {
		log.Printf("skill_defs: backfilled %d rows with content_sha256", n)
	}
	// v0.9.x mcp_server_defs backfill — shares the same 30-second
	// bfCtx so the cumulative "boot blocked on slow store" wait is
	// bounded. Documented best-effort posture as for the other two.
	if n, err := storeIface.BackfillMCPServerDefContentSHA256(bfCtx, backfillMCPServerDefSignFn); err != nil {
		log.Printf("mcp_server_defs: backfill content_sha256 partial — %v (rows updated before error: %d)", err, n)
	} else if n > 0 {
		log.Printf("mcp_server_defs: backfilled %d rows with content_sha256", n)
	}
	// v0.9.x system_prompt_base backfill — fills the field that PR #186's
	// read-side normalizer derives at runtime. Closes the on-disk data
	// gap for legacy rows; new rows persist the field directly via the
	// write-side mergedDef.normalize() (agentdef.go). Idempotent: a
	// second boot after a full backfill finds zero rows missing the
	// field. Shares the same bfCtx — bounded boot wait.
	if n, err := storeIface.BackfillAgentDefSystemPromptBase(bfCtx); err != nil {
		log.Printf("agent_defs: backfill system_prompt_base partial — %v (rows updated before error: %d)", err, n)
	} else if n > 0 {
		log.Printf("agent_defs: backfilled %d rows with system_prompt_base", n)
	}
	bfCancel()

	// v0.11.5 yaml-static memory entries — pre-seed the substrate from
	// the operator-yaml `memory.entries:` block. Idempotent: each
	// (scope, scope_id, key) tuple is skipped if a row already exists
	// (preserves runtime updates from operators / agents that may have
	// rewritten the value between boots). Synchronous: a slow embedder
	// will delay boot — logged below per-entry so the cost is visible.
	if len(cfg.Memory.Entries) > 0 {
		bootstrapMemoryEntries(context.Background(), cfg, storeIface, embedder)
	}

	// Memory tool depends on the Store; wire the live backend in now
	// that the adapter is open. This keeps the per-agent registration
	// at boot (allTools assembled once) and the tool's nil-Store
	// fallback for operators running without a configured store.
	memoryTool.Store = storeIface
	// RFC I MR-2: this sets the operator-DEFAULT backend (the path taken
	// when an agent has no memory_backend, plus the unconditional fallback
	// for unresolved / not-yet-wired backends). Constructed here, post-store,
	// so both deps are concrete — the embedder was built above (nil when
	// unconfigured, which the backend handles by refusing vector ops exactly
	// as before). MR-3b routes PER-AGENT named backends per-call from
	// memory.go backend(ctx) via memoryTool.Cfg; MR-4's Mem9 plugs into that
	// switch. This default assignment stays.
	memoryTool.Backend = inprocess.New(storeIface, embedder)
	channelTool.Store = storeIface
	// Wire the pool-stats accessor when the backend is Postgres so
	// the Channel tool's subscribe-race diagnostic log can correlate
	// retry fires with pool exhaustion. SQLite stores and test builds
	// leave it nil; the diagnostic log path then reports zeros for
	// the pool fields and the retry behavior is unchanged. The
	// PoolStatsFn capture lives inside execSubscribe at the case
	// <-waker: arm — see internal/tools/builtin/channel.go.
	if pg, ok := storeIface.(*storepostgres.Store); ok && pg != nil {
		channelTool.PoolStatsFn = func() (total, acquired, idle int32) {
			st := pg.Pool().Stat()
			return st.TotalConns(), st.AcquiredConns(), st.IdleConns()
		}
	}
	agentDefTool.Store = storeIface
	skillDefTool.Store = storeIface
	skillTool.Store = storeIface
	evaluationTool.Store = storeIface
	contextTool.Store = storeIface
	interruptionTool.Store = storeIface
	mcpServerDefTool.Store = storeIface

	// v0.9.x — load active mcp_server_defs rows into the in-process
	// registry so previous-deployment registrations survive a restart.
	// The pool's build callback consults this registry on first agent
	// call (after a tool-not-in-frozen-allTools fallthrough via the
	// existing v0.8.1 lazy resolver). Errors are logged + non-fatal —
	// a corrupted row blocks only its own server from being callable.
	if names, err := storeIface.MCPServerDefListNames(context.Background()); err == nil {
		for _, ns := range names {
			if ns.ActiveDefID == "" {
				continue
			}
			// Skip SHARED-tenant names that collide with a static yaml
			// `mcp_servers:` entry — yaml is ground truth for the "" tenant,
			// so a shared registry entry of that name would be unreachable
			// (lookup.MCPServer's "" path is static → shared-dynamic) and
			// only inflate diagnostics. A per-TENANT row (ns.TenantID != "")
			// that shares the name is a legitimate RFC N override (the
			// tenant-dynamic pass shadows the static base), so it is NOT
			// skipped. Mirrors the execCreate refusal, which only refuses
			// over yaml within the same tenant scope.
			if _, ok := cfg.MCPServers[ns.Name]; ok && ns.TenantID == "" {
				log.Printf("mcp_server_defs: skipping shared %q at boot — name collides with static yaml entry (yaml takes precedence)", ns.Name)
				continue
			}
			active, err := storeIface.MCPServerDefGet(context.Background(), ns.ActiveDefID)
			if err != nil {
				log.Printf("mcp_server_defs: load active %q (tenant=%q): %v", ns.Name, ns.TenantID, err)
				continue
			}
			// Skip retired active rows. SetRetired leaves the active overlay
			// pointing at the retired def_id (matches AgentDef/SkillDef
			// semantics — the substrate doesn't auto-clear the pointer).
			// Rehydrating a retired spec would silently revive a name the
			// operator explicitly retired.
			if active.Retired {
				log.Printf("mcp_server_defs: skipping %q (tenant=%q) at boot — active row is retired (def_id=%s)", ns.Name, ns.TenantID, active.DefID)
				continue
			}
			var ov struct {
				Transport string            `json:"transport"`
				URL       string            `json:"url"`
				Headers   map[string]string `json:"headers"`
			}
			if err := json.Unmarshal(active.Definition, &ov); err != nil {
				log.Printf("mcp_server_defs: parse active %q (tenant=%q): %v", ns.Name, ns.TenantID, err)
				continue
			}
			// RFC N: carry the def's tenant onto the registry entry so it is
			// keyed by (tenant, name) and only resolved for that tenant's runs.
			dynamicMCPRegistry.Set(mcp.DynamicMCPServerSpec{
				TenantID: active.TenantID, Name: active.Name, Transport: ov.Transport, URL: ov.URL, Headers: ov.Headers,
			})
		}
		if size := dynamicMCPRegistry.Size(); size > 0 {
			log.Printf("mcp_server_defs: loaded %d active registration(s) into pool registry", size)
		}
	} else {
		log.Printf("mcp_server_defs: list active failed at boot: %v (dynamic MCP servers will be empty until first registration)", err)
	}

	// v1.x RFC G — outbound A2A: register one synthetic
	// `a2a__<peer>__<skill>` tool per (operator-registered peer,
	// expected_skill) pair, mirroring the static MCP registration above.
	// They land in allTools and are filtered per-agent by `allowed_tools`
	// exactly like `mcp__<server>__<tool>` tools. Gated behind the same
	// LOOMCYCLE_A2A_ENABLED master switch as the server surface: with A2A
	// disabled, no outbound tools are registered. The per-call resolver is
	// lookup.A2AAgent (yaml > active substrate def) so a substrate fork of
	// a registered peer is picked up without a restart.
	if cfg.Env.A2AServerEnabled {
		a2aResolve := func(ctx context.Context, name string) (config.A2AAgent, bool) {
			return lookup.A2AAgent(ctx, storeIface, cfg, name)
		}
		a2aTools := toolsa2a.RegisterTools(context.Background(), cfg, storeIface, a2aResolve, nil, log.Printf)
		if len(a2aTools) > 0 {
			allTools = append(allTools, a2aTools...)
			log.Printf("a2a: registered %d outbound peer tool(s)", len(a2aTools))
		}
	}

	// Back-fill Context tool's catalog with the FINAL allTools slice
	// (including MCP-served tools registered above) so doc/tools ops
	// reflect the complete runtime catalog. Must happen AFTER every
	// allTools = append(...) line above.
	contextTool.Tools = allTools

	srv := lchttp.New(cfg, pr, allTools, sem, storeIface)
	// v0.9.x — wire the MCPServerDef substrate tool. NOT in allTools
	// (operator-admin-only); reached via Connector.MCPServerDef + the
	// admin endpoint + the LoomCycle MCP meta-tool.
	srv.SetMCPServerDefTool(mcpServerDefTool)
	// v1.x — wire the ScheduleDef substrate tool. Same operator-admin-
	// only posture; reached via Connector.ScheduleDef + the admin
	// endpoint + the future LoomCycle MCP meta-tool. Tool needs only
	// the store + cfg (no MCP-pool dependency) so this could move
	// earlier — kept next to MCPServerDef for substrate-wiring locality.
	srv.SetScheduleDefTool(&builtin.ScheduleDef{
		Store:               storeIface,
		Cfg:                 cfg,
		MaxDefinitionBytes:  cfg.Env.AgentDefMaxDefinitionBytes,
		MaxDescriptionBytes: cfg.Env.AgentDefMaxDescriptionBytes,
	})
	// v1.x RFC G — wire the two A2A substrate tools. Same operator-admin-
	// only posture as ScheduleDef; reached via Connector + the admin
	// endpoints + the LoomCycle MCP meta-tools. Identical Store + Cfg +
	// byte-cap construction.
	srv.SetA2AServerCardDefTool(&builtin.A2AServerCardDef{
		Store:               storeIface,
		Cfg:                 cfg,
		MaxDefinitionBytes:  cfg.Env.AgentDefMaxDefinitionBytes,
		MaxDescriptionBytes: cfg.Env.AgentDefMaxDescriptionBytes,
	})
	srv.SetA2AAgentDefTool(&builtin.A2AAgentDef{
		Store:               storeIface,
		Cfg:                 cfg,
		MaxDefinitionBytes:  cfg.Env.AgentDefMaxDefinitionBytes,
		MaxDescriptionBytes: cfg.Env.AgentDefMaxDescriptionBytes,
	})
	// v1.x RFC H — wire the WebhookDef substrate tool. Same operator-
	// admin-only posture as the A2A substrate tools; reached via
	// Connector.WebhookDef + the admin endpoint + the LoomCycle MCP
	// meta-tool. Identical Store + Cfg + byte-cap construction.
	srv.SetWebhookDefTool(&builtin.WebhookDef{
		Store:               storeIface,
		Cfg:                 cfg,
		MaxDefinitionBytes:  cfg.Env.AgentDefMaxDefinitionBytes,
		MaxDescriptionBytes: cfg.Env.AgentDefMaxDescriptionBytes,
	})
	// RFC I MR-3a — wire the MemoryBackendDef substrate tool. Same
	// operator-admin-only posture as WebhookDef; reached via
	// Connector.MemoryBackendDef + the admin endpoint + the LoomCycle
	// MCP meta-tool. Identical Store + Cfg + byte-cap construction.
	// Nothing consumes the Def yet — the per-agent routing + factory
	// land in MR-3b.
	srv.SetMemoryBackendDefTool(&builtin.MemoryBackendDef{
		Store:               storeIface,
		Cfg:                 cfg,
		MaxDefinitionBytes:  cfg.Env.AgentDefMaxDefinitionBytes,
		MaxDescriptionBytes: cfg.Env.AgentDefMaxDescriptionBytes,
	})
	// RFC L OSS multi-tenant authorization — wire the OperatorTokenDef
	// substrate tool (auth-token minting/rotation/retirement). Audit sink
	// is a file when LOOMCYCLE_AUDIT_LOG_PATH is set, else a NopSink. The
	// rotation grace defaults to 24h (override via
	// LOOMCYCLE_OPERATOR_TOKEN_ROTATION_GRACE_SECONDS). Nothing consumes
	// the tokens yet — the auth-middleware switch lands in the next PR.
	var tokenAudit audit.Sink = audit.NopSink{}
	if cfg.Env.AuditLogPath != "" {
		fs, aerr := audit.NewFileSink(cfg.Env.AuditLogPath)
		if aerr != nil {
			log.Fatalf("audit: %v", aerr)
		}
		tokenAudit = fs
		log.Printf("audit: OperatorTokenDef mutations → %s", cfg.Env.AuditLogPath)
	}
	graceSecs := 0
	if v := os.Getenv("LOOMCYCLE_OPERATOR_TOKEN_ROTATION_GRACE_SECONDS"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n >= 0 {
			graceSecs = n
		}
	}
	srv.SetOperatorTokenDefTool(&builtin.OperatorTokenDef{
		Store:                storeIface,
		Pepper:               cfg.Env.OperatorTokenPepper,
		Audit:                tokenAudit,
		RotationGraceSeconds: graceSecs,
		LegacyTokenSet:       cfg.Env.AuthToken != "",
	})
	// RFC L Decision 11: per-replica auth-token resolution cache. Default
	// 30s TTL; LOOMCYCLE_AUTH_CACHE_TTL_SECONDS=0 disables it (direct
	// lookup per request — immediate revocation). A token mutation
	// flushes it locally + (in cluster mode) broadcasts a flush.
	authCacheTTL := 30
	if v := os.Getenv("LOOMCYCLE_AUTH_CACHE_TTL_SECONDS"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n >= 0 {
			authCacheTTL = n
		}
	}
	srv.EnableTokenCache(time.Duration(authCacheTTL) * time.Second)
	// Surface the resolved build identifiers via /healthz so the Web UI
	// can render the running binary's real version instead of a stale
	// hard-coded string. Mirrors what gRPC's Health RPC has reported
	// since v0.5.5; HTTP only adopted this in v0.8.21.
	srv.SetBuildInfo(buildVersion, buildCommit, buildTime)
	srv.SetMCPFallback(mcpLazyResolver.Resolve)
	// v0.9.x: expose Pool's cached tools/list snapshots to the unified
	// /v1/_library/mcp-servers endpoint so static MCP servers' tools
	// surface alongside substrate-side discovered_tools. The closure
	// marshals into the substrate-mirror shape ({name, description,
	// input_schema}) so the wire stays uniform across static + dynamic.
	srv.SetMCPPoolInspector(func(name string) json.RawMessage {
		// Static yaml MCP servers live at the shared "" tenant in the
		// (tenant, name)-keyed pool. The Library introspection endpoint
		// surfaces those operator-blessed servers' cached tool lists.
		descs := mcpPool.PeekTools("", name)
		if descs == nil {
			return nil
		}
		type td struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			InputSchema json.RawMessage `json:"input_schema,omitempty"`
		}
		out := make([]td, len(descs))
		for i, d := range descs {
			out[i] = td{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema}
		}
		b, _ := json.Marshal(out)
		return b
	})
	// Post-boot tool advertising: enumerate the substrate-registered tools
	// (dynamic MCP servers' discovered tools + A2A peer skills) so a run's
	// catalog includes them WITHOUT a restart — symmetric with boot-time
	// tools. Run-creation folds this into the candidate set before the
	// allowed_tools filter. MCP: each dynamic-registry server's persisted
	// discovered_tools (set by rediscover). A2A: re-enumerate static +
	// substrate (the static dups collapse in the by-name filter).
	//
	// RFC N §3 tenant boundary: enumerate ONLY the names visible to the
	// run's tenant (NamesForTenant = own + shared), and read each name's
	// active def with the tenant→shared precedence. NET EFFECT: a run in
	// tenant A's candidate set contains ONLY A's + shared MCP tools, never
	// tenant B's.
	//
	// RFC N FIX 2-mcp: the tenant is the run's AUTHORITATIVE tenant passed
	// in by the server, NOT derived from ctx here. candidateTools runs at
	// the entry sites before WithRunIdentity is stamped, so a ctx-derived
	// tenant would be "" for non-HTTP-principal spawn surfaces and the run
	// would advertise the wrong tenant's MCP tools. The tenant remains
	// authoritative (the server derived it from the principal / session /
	// parent RunIdentity), never from the wire.
	srv.SetDynamicToolEnumerator(func(ctx context.Context, tenant string) []tools.Tool {
		var out []tools.Tool
		for _, name := range dynamicMCPRegistry.NamesForTenant(tenant) {
			// Resolve the active def with the same precedence as
			// lookup.MCPServer: the run's tenant first, then the shared ""
			// base. A tenant-owned name's tools come from the tenant's def;
			// a shared name's from the "" def. A run never reads another
			// tenant's row.
			row, gerr := storeIface.MCPServerDefGetActive(ctx, tenant, name)
			if gerr != nil && tenant != "" {
				row, gerr = storeIface.MCPServerDefGetActive(ctx, "", name)
			}
			if gerr != nil {
				continue
			}
			var def lookup.SubstrateMCPServer
			if json.Unmarshal(row.Definition, &def) != nil {
				continue
			}
			for _, dt := range def.DiscoveredTools {
				out = append(out, mcp.NewTool(mcpPool, name, mcp.ToolDescriptor{
					Name:        dt.Name,
					Description: dt.Description,
					InputSchema: dt.InputSchema,
				}))
			}
		}
		if cfg.Env.A2AServerEnabled {
			resolve := func(ctx context.Context, name string) (config.A2AAgent, bool) {
				return lookup.A2AAgent(ctx, storeIface, cfg, name)
			}
			out = append(out, toolsa2a.RegisterTools(ctx, cfg, storeIface, resolve, nil, log.Printf)...)
		}
		return out
	})
	// v0.8.22: hand the static skills.Set to the server so the
	// per-run SkillDef resolver can fall back to static bodies when
	// no DB-active row exists for a skill name.
	srv.SetSkillSet(skillSet)
	// v0.9.0: hand the embedder to the server so the
	// /v1/_memory/reembed + /v1/_memory/embed_stats admin endpoints
	// have a backing object. Nil-safe — endpoints return 503 when
	// no embedder is configured.
	srv.SetEmbedder(embedder)

	// v0.8.6 SystemPublisher — backs the POST /v1/_channels/_system/...
	// admin endpoint AND the cadence/event-hook publishers added in
	// PR 3. Wires the same Bus + Scheduler as the agent tool so
	// long-poll subscribers wake whether the publish came from an
	// agent, an internal goroutine, or the admin endpoint.
	sysPublisher := &channels.StorePublisher{
		Store:     storeIface,
		Bus:       channelBus,
		Scheduler: channelScheduler,
	}
	srv.SetSystemPublisher(sysPublisher)
	// Wire the SystemPublisher onto the Interruption tool too — same
	// instance, so _system/interrupts/* publishes from inside the
	// tool wake the same Channel long-poll subscribers.
	interruptionTool.SystemPublisher = sysPublisher
	// v0.8.16 — wire the same Bus to the server so the resolve
	// handler can wake the blocked tool's bus.Wait via the
	// "intr:<id>" key. Without this the resolve writes the row but
	// the tool re-checks storage only when its own timer fires.
	srv.SetInterruptionBus(channelBus)
	// v0.9.x — same Bus also drives the Channel CRUD subscribe path
	// (Connector + HTTP /v1/_channels/{name}/subscribe). Wire-side
	// subscribers wake on the same Notify() the in-band tool would.
	srv.SetChannelBus(channelBus)

	// v0.9.x n8n RFC Phase 0 — run-state pub/sub bus that backs the
	// GET /v1/users/{user_id}/agents/stream SSE endpoint. One
	// instance per process; subscribers (typically the SSE handler)
	// register per user_id; the finishRun* paths publish state
	// transitions. Decoupled from channelBus so a slow run-state
	// consumer can't stall the channel notification path.
	runStateBus := runstate.NewBus()
	srv.SetRunStateBus(runStateBus)

	// v0.8.6 heartbeat runner — one goroutine per `_system/heartbeat-*`
	// (or any other `publisher: system` + `period:` channel) declared
	// in operator yaml. Construction happens here; Start is deferred
	// to AFTER bgCtx is created below so the runner observes the same
	// shared shutdown context as the sweepers + session-lock GC.
	// That way bgCancel() on SIGTERM tears the heartbeat goroutines
	// down naturally, and any future v0.8.9 pause path that cancels
	// bgCtx pauses heartbeats without a separate hook.
	heartbeatChannels := make(map[string]struct {
		Period      string
		Publisher   string
		DefaultTTL  int
		MaxMessages int
	}, len(cfg.Channels))
	for name, ch := range cfg.Channels {
		heartbeatChannels[name] = struct {
			Period      string
			Publisher   string
			DefaultTTL  int
			MaxMessages int
		}{
			Period:      ch.Period,
			Publisher:   ch.Publisher,
			DefaultTTL:  ch.DefaultTTL,
			MaxMessages: ch.MaxMessages,
		}
	}
	heartbeatSpecs, err := channels.LoadHeartbeatSpecs(heartbeatChannels)
	if err != nil {
		log.Fatalf("heartbeat specs: %v", err)
	}
	var heartbeatRunner *channels.HeartbeatRunner
	if len(heartbeatSpecs) > 0 {
		heartbeatRunner = channels.NewHeartbeatRunner(sysPublisher, buildCommit, heartbeatSpecs)
	}

	// Build the model-resolution matrix (resolve.Resolver). Providers
	// without API keys are MARKED EXCLUDED so Snapshot() shows the
	// distinct "no key configured" state — the resolver skips them
	// like unreachable providers but operators can tell the two
	// apart in logs and dashboards. Providers with keys are probed
	// live (GET /v1/models or /api/tags) and seeded from the
	// response. The probe goroutine started below re-runs the probe
	// on the configured cadence to catch recoveries.
	resolver := buildResolver(cfg, pr)
	srv.SetResolver(resolver)

	// v1.x RFC G — A2A server surface (well-known AgentCard + REST /
	// JSON-RPC / gRPC binding mounts + multi-tenant routing). Default
	// OFF; New returns (nil, nil) when LOOMCYCLE_A2A_ENABLED != 1. The
	// *http.Server (srv) is BOTH the runner.Runner and the
	// connector.Connector the bridge needs, plus the run-table reader.
	// Mount must be registered BEFORE srv.Mux() is called below; gRPC
	// registration happens on the shared grpc.Server further down.
	var a2aServer *a2aapi.Server
	if cfg.Env.A2AServerEnabled {
		authToken := cfg.Env.AuthToken
		var a2aAuth a2aapi.Authenticator
		if authToken != "" {
			// Reuse the same constant-time bearer check as the HTTP
			// authMiddleware. The peer's bearer authenticates it as the
			// principal; the name is opaque here (run attribution only).
			a2aAuth = func(h http.Header) (string, bool) {
				got := h.Get("Authorization")
				if got == "" {
					return "", false
				}
				if auth.CompareBearer(got, "Bearer "+authToken) {
					return "a2a-peer", true
				}
				return "", false
			}
		}
		a2aServer, err = a2aapi.New(context.Background(), a2aapi.Deps{
			Cfg:   cfg,
			Store: storeIface,
			Conn:  srv,
			Run:   srv,
			Auth:  a2aAuth,
			// Same bus the Interruption tool waits on + the HTTP resolve
			// handler notifies (SetInterruptionBus(channelBus) above), so an
			// A2A INPUT_REQUIRED follow-up wakes the parked run on the same
			// "intr:<id>" key.
			ChannelNotify: channelBus.Notify,
		})
		if err != nil {
			log.Fatalf("a2a server: %v", err)
		}
		if a2aServer != nil {
			srv.SetExtraMux(a2aServer.Mount)
			log.Printf("a2a: server surface enabled (card=%q tenancy=%q)", cfg.Env.A2AServerCardName, cfg.Env.A2ATenancyRouting)
		}
	}

	// v1.x RFC H inbound-webhook receiver. Default OFF; operator opts in
	// via LOOMCYCLE_WEBHOOKS_ENABLED=1. The receiver is the trust boundary:
	// it authenticates each external POST against the resolved WebhookDef's
	// own HMAC/bearer secret (gated by the SAME env allowlist the scheduler
	// uses — the shared RFC F trigger-credential gate), then spawns a run
	// (srv as runner) or publishes to a channel (sysPublisher). ?sync runs
	// block on runStateBus. The route mounts WITHOUT the global bearer auth.
	//
	// Registered BEFORE srv.Mux() below — the SetWebhookMux hook fires
	// inside Mux(), same ordering constraint as SetExtraMux for A2A.
	if cfg.Env.WebhooksEnabled && storeIface != nil && srv != nil {
		webhookAllowlist := make(map[string]bool, len(cfg.Env.SchedulerEnvAllowlist))
		for _, name := range cfg.Env.SchedulerEnvAllowlist {
			webhookAllowlist[name] = true
		}
		rec := webhookapi.New(webhookapi.Deps{
			Store:        storeIface,
			Cfg:          cfg,
			Runner:       srv,
			Publisher:    sysPublisher,
			RunStateBus:  runStateBus,
			EnvAllowlist: webhookAllowlist,
			Logf:         log.Printf,
		})
		srv.SetWebhookMux(func(reg lchttp.MuxRegistrar, adminAuth func(http.Handler) http.Handler) {
			// Receiver POST: unauthed (per-WebhookDef secret). Triage
			// endpoints (recent-deliveries / test): admin-bearer gated.
			rec.Mount(reg)
			rec.MountAdmin(reg, adminAuth)
		})
		log.Printf("webhooks: enabled (receiver mounted at POST /v1/_webhooks/{name}, env_allowlist=%d names)",
			len(cfg.Env.SchedulerEnvAllowlist))
	} else if cfg.Env.WebhooksEnabled {
		log.Printf("webhooks: disabled (no Store backend or no HTTP server)")
	} else {
		log.Printf("webhooks: disabled (LOOMCYCLE_WEBHOOKS_ENABLED=0)")
	}

	// In path-mode A2A tenancy, the leading /{tenant} segment is stripped
	// before the mux sees the request (an open first-segment wildcard
	// cannot coexist with the HTTP server's subtree routes). The wrapper
	// is a no-op outside path mode and when A2A is disabled.
	var rootHandler http.Handler = srv.Mux()
	if a2aServer != nil {
		rootHandler = a2aServer.PathTenantWrapper(rootHandler)
	}

	httpServer := &http.Server{
		Addr:              cfg.Env.ListenAddr,
		Handler:           rootHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	if cfg.Env.AuthToken == "" {
		log.Printf("WARNING: LOOMCYCLE_AUTH_TOKEN is not set; /v1 routes are unauthenticated (dev mode)")
	}

	// Background goroutines (heartbeat sweeper + session-lock map GC)
	// share a single ctx tied to the signal handler below — graceful
	// shutdown cancels them alongside the HTTP server. We rely on the
	// goroutines to exit promptly on ctx.Done(); both are documented
	// to do so in their packages.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

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
		pgStore, ok := storeIface.(*storepostgres.Store)
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
		defer func() {
			if err := bp.Close(); err != nil {
				log.Printf("coord: backplane close: %v", err)
			}
		}()
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
		srv.CancelRegistry().SetClusterCanceller(cancelCoord)
		go cancelCoord.RunCancelSubscriber(bgCtx, srv.CancelRegistry())
		go cancelCoord.RunAckSubscriber(bgCtx)

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

		// v0.12.5 Phase 6b: cluster-wide hook registry. Persists
		// registrations to the hooks table; backplane events keep
		// each replica's in-process cache current. The inner Registry
		// preserves the operator-yaml host-widen permit list (CLAUDE.md
		// rule #8 — frozen at boot, never DB-derived).
		innerReg := hooks.NewRegistryWithPermissions(cfg.Hooks.PermitHostWiden.Owners)
		dbReg, err := hooks.NewDBBackedRegistry(innerReg, pgStore, bp, cfg.Env.ReplicaID)
		if err != nil {
			log.Fatalf("coord: hook db registry init: %v", err)
		}
		if err := dbReg.LoadFromDB(bgCtx); err != nil {
			log.Printf("coord: hook db registry initial load: %v (continuing with empty cache)", err)
		}
		go dbReg.RunBackplaneConsumer(bgCtx)
		srv.SetHookRegistry(dbReg)

		log.Printf("coord: cluster mode active — replica_id=%s heartbeat=30s backplane=postgres-listen-notify user_quotas=db-backed cancel_ack_timeout=%s bus_fanout=on singleton_sweepers=on session_lock=pg-advisory hooks=db-backed", cfg.Env.ReplicaID, ackTimeout)
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
			pgStoreForPause, ok := storeIface.(*storepostgres.Store)
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
		envAllowlist := make(map[string]bool, len(cfg.Env.SchedulerEnvAllowlist))
		for _, name := range cfg.Env.SchedulerEnvAllowlist {
			envAllowlist[name] = true
		}
		schedCfg := scheduler.Config{
			TickInterval: time.Duration(cfg.Env.SchedulerTickSeconds) * time.Second,
			FireTimeout:  time.Duration(cfg.Env.SchedulerFireTimeoutSeconds) * time.Second,
			EnvAllowlist: envAllowlist,
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

		// srv satisfies runner.Runner via its RunOnce method (the same
		// seam HTTP + gRPC drive interactive runs through). pauseMgr is
		// guaranteed non-nil in this branch (storeIface != nil ⇒ pause
		// manager constructed earlier in this function).
		sched := scheduler.New(schedCfg, storeIface, srv, pauseMgrRef, nil, log.Printf)
		sched.Start(bgCtx)
		log.Printf("scheduler: enabled (tick=%ds, fire_timeout=%ds, env_allowlist=%d names)",
			cfg.Env.SchedulerTickSeconds, cfg.Env.SchedulerFireTimeoutSeconds,
			len(cfg.Env.SchedulerEnvAllowlist))
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
	if mcpMode {
		// Optional operator override for the stdio dispatch concurrency
		// cap (RFC O). 0/unset → the package default (16).
		mcpMaxConcurrentCalls := 0
		if v := os.Getenv("LOOMCYCLE_MCP_MAX_CONCURRENT_CALLS"); v != "" {
			if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
				mcpMaxConcurrentCalls = n
			}
		}
		// Optional operator default for the spawn_run transport timeout
		// (RFC P). 0/unset → disabled (defer to the run's own budget).
		mcpSpawnRunTimeoutMS := 0
		if v := os.Getenv("LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS"); v != "" {
			if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
				mcpSpawnRunTimeoutMS = n
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
			log.Printf("mcp: stdio server starting (20 tools registered)")
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
	// the server's Mux(). Disabled implicitly by --no-http (no HTTP
	// listener = no /v1/_mcp route reachable).
	mcpHTTPHandler := lcmcp.NewHTTPHandler(lcmcp.Config{
		Connector:     srv, // same *http.Server instance used by stdio MCP
		Runner:        srv,
		Store:         storeIface,
		Logf:          log.Printf,
		ServerName:    "loomcycle",
		ServerVersion: buildVersion,
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

	if skipHTTP {
		// v0.8.15.2: --no-http suppresses the HTTP listener. The
		// httpServer object stays constructed (other subsystems may
		// hold references for routing internals); we just never call
		// ListenAndServe. Shutdown below is a no-op on a non-listening
		// server but we guard it anyway.
		log.Printf("--no-http: HTTP listener suppressed; %s remains free for other processes", cfg.Env.ListenAddr)
	} else {
		go func() {
			log.Printf("loomcycle listening on %s", cfg.Env.ListenAddr)
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("listen: %v", err)
			}
		}()
	}

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
			AuthToken: cfg.Env.AuthToken,
			// RFC L: reuse the HTTP server's token resolution so gRPC
			// stamps the same authoritative principal (gRPC runs flow
			// authoritatively through RunOnce) and shares the open-mode
			// decision.
			PrincipalResolver: srv.ResolvePrincipal,
			AuthConfigured:    srv.AuthConfigured,
			BuildCommit:       buildCommit,
			BuildTime:         buildTime,
		})
		grpcSrv = googlegrpc.NewServer(
			googlegrpc.UnaryInterceptor(grpcAdapter.UnaryAuthInterceptor()),
			googlegrpc.StreamInterceptor(grpcAdapter.StreamAuthInterceptor()),
		)
		loomcyclepb.RegisterLoomcycleServer(grpcSrv, grpcAdapter)
		// v1.x RFC G — register the A2A gRPC binding on the same server.
		// gRPC is not a path-mounted http.Handler (it needs the HTTP/2
		// server), so the /a2a/grpc binding rides loomcycle's existing
		// grpc.Server; the AgentCard advertises the gRPC port. Nil-safe
		// when the A2A surface is disabled.
		a2aServer.RegisterGRPC(grpcSrv)
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
	if !skipHTTP {
		_ = httpServer.Shutdown(ctx)
	}
	// Flush OTEL spans before exit. The OTLP exporter batches; without
	// this, in-flight spans never reach the collector. Same 5s deadline
	// as the HTTP shutdown — exporters honor ctx.
	if otelShutdown != nil {
		if err := otelShutdown(ctx); err != nil {
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
		return st, func() { _ = st.Close() }, nil

	default:
		return nil, nil, fmt.Errorf("unknown storage.backend %q (want \"sqlite\" or \"postgres\")", cfg.Storage.Backend)
	}
}

// providerResolver constructs Provider instances at startup based on which
// env vars are set. A provider that wasn't configured returns a clear error
// when an agent tries to use it (rather than failing the whole startup).
type providerResolver struct {
	anthropic providers.Provider
	openai    providers.Provider
	// ollama = hosted ollama.com (Bearer auth via OLLAMA_API_KEY).
	// ollamaLocal = local-network endpoint (no auth, default
	// localhost:11434). Both wrap the same internal driver; only the
	// providerID, auth header, and base URL differ. See v0.8.3 split.
	ollama      providers.Provider
	ollamaLocal providers.Provider
	deepseek    providers.Provider
	gemini      providers.Provider
	// v0.11.9 OAuth-dev provider. Registered only when
	// LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1 AND a token file exists.
	// nil otherwise — Get("anthropic-oauth-dev") returns a clear error
	// pointing at `loomcycle anthropic login`.
	anthropicOAuthDev       providers.Provider
	anthropicOAuthRefresher *anthropic_oauth_dev.Refresher
	// v0.12.8 — synthetic mock provider for cost-free stress testing.
	// Registered only when LOOMCYCLE_MOCK_ENABLED=1. Drives the
	// canonical 3-agent circuit-stress pipeline with no real-LLM
	// cost; injection knobs (LOOMCYCLE_MOCK_429_RATE, 500_RATE,
	// LATENCY_MS, LATENCY_JITTER_MS) exercise the resolver matrix
	// + runtime-fallback paths under load. See
	// internal/providers/mock/driver.go.
	mock providers.Provider

	// v0.12.9 — companion `mock-stable` provider. Same driver
	// shape, failure rates pinned at zero regardless of env. Lets
	// operators configure `[mock, mock-stable]` as a tier candidate
	// list with `fallback_on_error: true`, so a 429 on the primary
	// escalates to the stable variant under tryProviderFallback —
	// exercising the recovery path against a known-good target.
	mockStable providers.Provider

	// RFC J — synthetic code-js provider (operator-authored JS via goja).
	// Registered only when LOOMCYCLE_CODE_AGENTS_ENABLED=1; nil otherwise,
	// so Get("code-js") returns a clear "code agents are disabled" error.
	codeJS providers.Provider
}

// buildEmbedder turns cfg.Memory.Embedder into a constructed
// providers.Embedder, sourcing the API key + base URL from the same
// env vars the chat-completion drivers use. Returns (nil, nil) when
// no embedder is configured — the Memory tool refuses vector ops
// with embedder_not_configured in that case.
//
// Per-embedder yaml knobs (timeout_ms, batch_size) override the
// env-var defaults when set; the env-var fallback gives operators
// a single-place override for many embedders without touching yaml.
func buildEmbedder(cfg *config.Config) (providers.Embedder, error) {
	provider := cfg.Memory.Embedder.Provider
	if provider == "" {
		return nil, nil
	}

	// Reuse the chat-completion driver's auth + base URL. Embedders
	// hit the same provider account, so a separate set of env vars
	// would be operator friction without benefit.
	//
	// EXCEPTION: the `anthropic` embedder slot is a Voyage AI proxy
	// (v0.10.2) — Anthropic has no native embeddings API and points
	// users at Voyage. The operator yaml stays `provider: anthropic`
	// for ergonomics, but the underlying auth is the separate
	// VOYAGE_API_KEY env var routed to cfg.Env.VoyageAPIKey.
	var apiKey, baseURL string
	switch provider {
	case "openai":
		apiKey, baseURL = cfg.Env.OpenAIAPIKey, ""
	case "gemini":
		apiKey, baseURL = cfg.Env.GeminiAPIKey, cfg.Env.GeminiBaseURL
	case "anthropic":
		apiKey, baseURL = cfg.Env.VoyageAPIKey, ""
		if apiKey == "" {
			log.Printf("memory.embedder: provider=anthropic uses Voyage AI; set VOYAGE_API_KEY or Embed() calls will fail at 401")
		}
	}

	timeoutMs := cfg.Memory.Embedder.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = cfg.Env.MemoryEmbedTimeoutMs
	}
	batchSize := cfg.Memory.Embedder.BatchSize
	if batchSize == 0 {
		batchSize = cfg.Env.MemoryEmbedBatchSize
	}

	return providers.NewEmbedder(provider, providers.EmbedderOptions{
		APIKey:    apiKey,
		BaseURL:   baseURL,
		Model:     cfg.Memory.Embedder.Model,
		Timeout:   time.Duration(timeoutMs) * time.Millisecond,
		BatchSize: batchSize,
	})
}

func newProviderResolver(cfg *config.Config) *providerResolver {
	pr := &providerResolver{}
	streamOpts := streamhttp.Options{
		HeaderTimeout: cfg.Env.ProviderHeaderTimeout,
		IdleTimeout:   cfg.Env.ProviderIdleTimeout,
	}
	if cfg.Env.AnthropicAPIKey != "" {
		pr.anthropic = anthropic.New(cfg.Env.AnthropicAPIKey, "", streamOpts, nil)
	}
	if cfg.Env.OpenAIAPIKey != "" {
		pr.openai = openai.New(cfg.Env.OpenAIAPIKey, "", streamOpts, nil)
	}
	// Hosted ollama.com — opts in via OLLAMA_API_KEY. Bearer-auth on the
	// same /api/chat wire shape as local Ollama; the driver is shared,
	// only the providerID + auth header + base URL differ.
	// OLLAMA_CLOUD_BASE_URL overrides the public endpoint for staged
	// rollouts or vendor mirrors.
	if cfg.Env.OllamaAPIKey != "" {
		pr.ollama = ollama.New("ollama", cfg.Env.OllamaAPIKey, cfg.Env.OllamaCloudBaseURL, streamOpts, nil).
			WithNumCtx(cfg.Env.OllamaNumCtx)
	}
	// Local-network Ollama — no API key, local trust model. The loader
	// defaults OLLAMA_BASE_URL to http://localhost:11434, so this is
	// effectively always-on. Operators disable it via
	// OLLAMA_BASE_URL=disabled (or an empty string in shell env).
	if cfg.Env.OllamaBaseURL != "" && cfg.Env.OllamaBaseURL != "disabled" {
		pr.ollamaLocal = ollama.New("ollama-local", "", cfg.Env.OllamaBaseURL, streamOpts, nil).
			WithNumCtx(cfg.Env.OllamaLocalNumCtx)
	}
	// DeepSeek opts in via DEEPSEEK_API_KEY. Optional DEEPSEEK_BASE_URL
	// overrides the public endpoint for self-hosted OpenAI-compatible
	// mirrors. Same on/off semantics as Anthropic + OpenAI.
	if cfg.Env.DeepSeekAPIKey != "" {
		pr.deepseek = deepseek.New(cfg.Env.DeepSeekAPIKey, cfg.Env.DeepSeekBaseURL, streamOpts, nil)
	}
	// Gemini opts in via GEMINI_API_KEY. Optional GEMINI_BASE_URL
	// points at a Vertex AI Gemini endpoint instead of the public
	// generativelanguage.googleapis.com surface.
	if cfg.Env.GeminiAPIKey != "" {
		pr.gemini = gemini.New(cfg.Env.GeminiAPIKey, cfg.Env.GeminiBaseURL, streamOpts, nil)
	}

	// v0.12.8 — synthetic mock provider. Single gate: LOOMCYCLE_MOCK_ENABLED=1.
	// Registers as provider id "mock" with four model variants
	// (mock-researcher / mock-editor / mock-evaluator / mock-generic)
	// for the canonical circuit-stress 3-agent pipeline. Failure
	// injection knobs (LOOMCYCLE_MOCK_429_RATE etc.) are read by the
	// driver itself; logged here so the boot output makes the active
	// scenario obvious.
	if os.Getenv("LOOMCYCLE_MOCK_ENABLED") == "1" {
		pr.mock = mockprov.New()
		pr.mockStable = mockprov.NewStableProvider()
		log.Printf("mock provider: enabled (LATENCY_MS=%q LATENCY_JITTER_MS=%q 429_RATE=%q 500_RATE=%q)",
			os.Getenv("LOOMCYCLE_MOCK_LATENCY_MS"),
			os.Getenv("LOOMCYCLE_MOCK_LATENCY_JITTER_MS"),
			os.Getenv("LOOMCYCLE_MOCK_429_RATE"),
			os.Getenv("LOOMCYCLE_MOCK_500_RATE"))
		log.Printf("mock-stable provider: enabled (failure injection always off; fallback target for [mock, mock-stable] tier policies)")
	}

	// v0.11.9 — anthropic-oauth-dev. Two gates: env var + tokens on
	// disk. Without either, the provider is absent from the resolver
	// matrix and any agent yaml that pins `provider: anthropic-oauth-dev`
	// fails resolution at request time with a clear error.
	if os.Getenv(anthropic_oauth_dev.EnvEnabled) == "1" {
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
				pr.anthropicOAuthDev = anthropic_oauth_dev.New(
					refresher, streamOpts, anthropic_oauth_dev.ResolveClaudeCodeVersion(), nil,
				)
				log.Printf("anthropic-oauth-dev: registered (token expires %s; user-agent=claude-cli/%s)",
					refresher.Token().ExpiresAt.Format(time.RFC3339),
					anthropic_oauth_dev.ResolveClaudeCodeVersion())
				log.Printf("anthropic-oauth-dev: WARNING — reverse-engineered OAuth flow (Pi reference + loomcycle replication); NOT officially endorsed by Anthropic; subscription terms historically restrict programmatic use; operator carries all risk including account flag/revocation; no warranty/SLA/liability from loomcycle; see docs/PROVIDERS.md")
			}
		}
	}

	// RFC J — synthetic code-js provider. Single gate:
	// LOOMCYCLE_CODE_AGENTS_ENABLED=1. Runs operator-authored JS via goja
	// instead of an LLM. Registered as provider id "code-js"; resolves each
	// agent's code from agent_code/<name>/index.js under the configured root.
	if cfg.Env.CodeAgentsEnabled {
		pr.codeJS = codejs.New(codejs.Config{
			CodeRoot:      cfg.Env.CodeAgentsRoot,
			Deterministic: cfg.Env.CodeAgentsDeterministic,
			RunTimeout:    cfg.Env.CodeAgentsRunTimeout,
			Logf:          log.Printf,
		})
		log.Printf("code-js provider: enabled (root=%q deterministic=%v run_timeout=%s abi=%s)",
			cfg.Env.CodeAgentsRoot, cfg.Env.CodeAgentsDeterministic, cfg.Env.CodeAgentsRunTimeout, codejs.ABIVersion)
	}

	return pr
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
	cp, _ := pr.codeJS.(*codejs.Provider)
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

func (p *providerResolver) Get(id string) (providers.Provider, error) {
	switch id {
	case "anthropic":
		if p.anthropic == nil {
			return nil, fmt.Errorf("anthropic provider not configured (set ANTHROPIC_API_KEY)")
		}
		return p.anthropic, nil
	case "openai":
		if p.openai == nil {
			return nil, fmt.Errorf("openai provider not configured (set OPENAI_API_KEY)")
		}
		return p.openai, nil
	case "ollama":
		if p.ollama == nil {
			return nil, fmt.Errorf("ollama provider not configured (set OLLAMA_API_KEY for hosted ollama.com; use provider id \"ollama-local\" for a local-network Ollama)")
		}
		return p.ollama, nil
	case "ollama-local":
		if p.ollamaLocal == nil {
			return nil, fmt.Errorf("ollama-local provider not configured (set OLLAMA_BASE_URL, or it's been opted out via OLLAMA_BASE_URL=disabled)")
		}
		return p.ollamaLocal, nil
	case "deepseek":
		if p.deepseek == nil {
			return nil, fmt.Errorf("deepseek provider not configured (set DEEPSEEK_API_KEY)")
		}
		return p.deepseek, nil
	case "gemini":
		if p.gemini == nil {
			return nil, fmt.Errorf("gemini provider not configured (set GEMINI_API_KEY)")
		}
		return p.gemini, nil
	case "anthropic-oauth-dev":
		if p.anthropicOAuthDev == nil {
			return nil, fmt.Errorf("anthropic-oauth-dev not registered (set LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1 + run `loomcycle anthropic login`)")
		}
		return p.anthropicOAuthDev, nil
	case "mock":
		if p.mock == nil {
			return nil, fmt.Errorf("mock provider not configured (set LOOMCYCLE_MOCK_ENABLED=1)")
		}
		return p.mock, nil
	case "mock-stable":
		if p.mockStable == nil {
			return nil, fmt.Errorf("mock-stable provider not configured (set LOOMCYCLE_MOCK_ENABLED=1)")
		}
		return p.mockStable, nil
	case "code-js":
		if p.codeJS == nil {
			return nil, fmt.Errorf("code-js provider not configured: code agents are disabled (set LOOMCYCLE_CODE_AGENTS_ENABLED=1)")
		}
		return p.codeJS, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", id)
	}
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
	libraryTiers := convertTiers(cfg.Tiers)
	r := resolve.NewResolver(cfg.ProviderPriority, libraryTiers)

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
	type probeJob struct {
		id         string             // provider id
		excluded   bool               // operator opted out
		exclReason string             // why excluded; surfaced in LastError
		provider   providers.Provider // nil when excluded
	}

	jobs := []probeJob{
		{id: "anthropic", excluded: cfg.Env.AnthropicAPIKey == "",
			exclReason: "ANTHROPIC_API_KEY not set"},
		{id: "openai", excluded: cfg.Env.OpenAIAPIKey == "",
			exclReason: "OPENAI_API_KEY not set"},
		{id: "deepseek", excluded: cfg.Env.DeepSeekAPIKey == "",
			exclReason: "DEEPSEEK_API_KEY not set"},
		{id: "gemini", excluded: cfg.Env.GeminiAPIKey == "",
			exclReason: "GEMINI_API_KEY not set"},
		{id: "ollama", excluded: cfg.Env.OllamaAPIKey == "",
			exclReason: "OLLAMA_API_KEY not set"},
		{id: "ollama-local", excluded: cfg.Env.OllamaBaseURL == "" || cfg.Env.OllamaBaseURL == "disabled",
			exclReason: "OLLAMA_BASE_URL not configured"},
		// v0.11.10 anthropic-oauth-dev. The provider self-registers
		// only when LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1 AND a
		// token file is present (cf. lines ~1775-1810). Gate the
		// probe on the resolver actually having the provider —
		// pr.Get below catches both the env-var-off case (returns
		// the canonical "not registered" error) and the
		// resolver-misconfigured case.
		{id: "anthropic-oauth-dev"},
		// v0.12.8 — synthetic mock provider. Excluded unless the
		// operator opted in via LOOMCYCLE_MOCK_ENABLED=1. The probe
		// itself is trivial (ListModels returns a fixed slice) so
		// the matrix populates instantly when enabled.
		{id: "mock", excluded: os.Getenv("LOOMCYCLE_MOCK_ENABLED") != "1",
			exclReason: "LOOMCYCLE_MOCK_ENABLED not set"},
		// v0.12.9 — companion stable variant for fallback testing.
		// Same gate as `mock` — when one is enabled, both are.
		{id: "mock-stable", excluded: os.Getenv("LOOMCYCLE_MOCK_ENABLED") != "1",
			exclReason: "LOOMCYCLE_MOCK_ENABLED not set"},
	}
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
func convertTiers(in map[string][]config.TierCandidate) map[string][]resolve.Candidate {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]resolve.Candidate, len(in))
	for tier, cands := range in {
		conv := make([]resolve.Candidate, 0, len(cands))
		for _, c := range cands {
			conv = append(conv, resolve.Candidate{Provider: c.Provider, Model: c.Model})
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
// A dynamic spec carries no operator allowed_tools (the substrate doesn't
// record a narrowing), so AllowedTools stays nil = allow-all.
type mcpLookupView struct{ reg *mcp.DynamicRegistry }

func (v mcpLookupView) Get(tenantID, name string) (lookup.MCPServerSpec, bool) {
	if v.reg == nil {
		return lookup.MCPServerSpec{}, false
	}
	s, ok := v.reg.Get(tenantID, name)
	if !ok {
		return lookup.MCPServerSpec{}, false
	}
	return lookup.MCPServerSpec{Transport: s.Transport, URL: s.URL, Headers: s.Headers}, true
}

func spawnStdioMCP(name string, srv config.MCPServer) (mcp.Caller, error) {
	keys := make([]string, 0, len(srv.Env))
	for k := range srv.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+srv.Env[k])
	}
	return mcpstdio.Spawn(mcpstdio.Config{
		Command: srv.Command,
		Args:    srv.Args,
		Env:     env,
		OnStderr: func(line string) {
			log.Printf("mcp[%s]: %s", name, line)
		},
	})
}

// applyAllowedToolsFilter — thin wrapper over mcp.ApplyAllowedToolsFilter.
// Lives in the mcp package so both the boot-time path here and the
// lazy-retry path (mcp.LazyResolver) share one implementation.
func applyAllowedToolsFilter(descs []mcp.ToolDescriptor, allowed []string) []mcp.ToolDescriptor {
	return mcp.ApplyAllowedToolsFilter(descs, allowed)
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
		if _, err := st.MemoryGet(ctx, scope, e.ScopeID, key); err == nil {
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
		if err := st.MemorySet(ctx, scope, e.ScopeID, key, valBytes, 0); err != nil {
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
		if err := st.MemoryEmbedSet(ctx, scope, e.ScopeID, key, store.MemoryEmbedding{
			Provider:  embedder.Provider(),
			Model:     embedder.Model(),
			Dimension: embedder.Dimension(),
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
