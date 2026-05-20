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
	"strings"
	"sync"
	"syscall"
	"time"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/cli"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/heartbeat"
	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/metrics"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	"github.com/denn-gubsky/loomcycle/internal/providers/deepseek"
	"github.com/denn-gubsky/loomcycle/internal/providers/gemini"
	"github.com/denn-gubsky/loomcycle/internal/providers/ollama"
	"github.com/denn-gubsky/loomcycle/internal/providers/openai"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storepostgres "github.com/denn-gubsky/loomcycle/internal/store/postgres"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"

	googlegrpc "google.golang.org/grpc"

	loomgrpc "github.com/denn-gubsky/loomcycle/internal/api/grpc"
	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lcmcp "github.com/denn-gubsky/loomcycle/internal/api/mcp"
	"github.com/denn-gubsky/loomcycle/internal/tools"
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
		case "health":
			os.Exit(cli.RunHealth(os.Args[2:], os.Stdout, os.Stderr))
		case "migrate":
			os.Exit(cli.RunMigrate(os.Args[2:], os.Stdout, os.Stderr))
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
		case "mcp":
			mcpMode = true
			// Strip "mcp" from os.Args so flag.Parse() below sees
			// the remaining flags (--config etc.) at index 1+.
			os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
		case "help", "-h", "--help":
			cli.PrintHelp(os.Stdout)
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

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		// Allow running with no YAML at all if a default agent can be derived
		// from defaults. Most callers will hit the same error path though.
		log.Fatalf("config: %v", err)
	}

	pr := newProviderResolver(cfg)
	sem := concurrency.New(
		cfg.Concurrency.MaxConcurrentRuns,
		cfg.Concurrency.MaxQueueDepth,
		cfg.Concurrency.QueueTimeout(),
	)
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
	memoryTool := &builtin.Memory{
		MaxValueBytes:     cfg.Env.MemoryMaxValueBytes,
		DefaultQuotaBytes: cfg.Env.MemoryMaxScopeBytes,
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
	mcpPool := mcp.NewPool(
		func(name string) (mcp.Caller, error) {
			srv, ok := cfg.MCPServers[name]
			if !ok {
				return nil, fmt.Errorf("mcp_servers.%s: not in config", name)
			}
			switch srv.Transport {
			case "stdio":
				return spawnStdioMCP(name, srv)
			case "http":
				return mcphttp.New(mcphttp.Config{
					URL:     srv.URL,
					Headers: srv.Headers,
				})
			default:
				return nil, fmt.Errorf("mcp_servers.%s: unknown transport %q", name, srv.Transport)
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
	)
	defer mcpPool.Close()

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
	// pain. See internal/tools/mcp/lazy.go for the state machine.
	mcpServerCfgs := make(map[string]mcp.ServerCfg, len(cfg.MCPServers))
	for name, srv := range cfg.MCPServers {
		mcpServerCfgs[name] = mcp.ServerCfg{AllowedTools: srv.AllowedTools}
	}
	mcpLazyResolver := mcp.NewLazyResolver(mcpPool, mcpServerCfgs, func(server string, count int) {
		log.Printf("mcp[%s]: lazy-registered %d tool(s) on first agent call (was skipped at boot)", server, count)
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
	// Memory tool depends on the Store; wire the live backend in now
	// that the adapter is open. This keeps the per-agent registration
	// at boot (allTools assembled once) and the tool's nil-Store
	// fallback for operators running without a configured store.
	memoryTool.Store = storeIface
	channelTool.Store = storeIface
	agentDefTool.Store = storeIface
	skillDefTool.Store = storeIface
	skillTool.Store = storeIface
	evaluationTool.Store = storeIface
	contextTool.Store = storeIface
	interruptionTool.Store = storeIface
	// Back-fill Context tool's catalog with the FINAL allTools slice
	// (including MCP-served tools registered above) so doc/tools ops
	// reflect the complete runtime catalog. Must happen AFTER every
	// allTools = append(...) line above.
	contextTool.Tools = allTools

	srv := lchttp.New(cfg, pr, allTools, sem, storeIface)
	// Surface the resolved build identifiers via /healthz so the Web UI
	// can render the running binary's real version instead of a stale
	// hard-coded string. Mirrors what gRPC's Health RPC has reported
	// since v0.5.5; HTTP only adopted this in v0.8.21.
	srv.SetBuildInfo(buildVersion, buildCommit, buildTime)
	srv.SetMCPFallback(mcpLazyResolver.Resolve)
	// v0.8.22: hand the static skills.Set to the server so the
	// per-run SkillDef resolver can fall back to static bodies when
	// no DB-active row exists for a skill name.
	srv.SetSkillSet(skillSet)

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

	httpServer := &http.Server{
		Addr:              cfg.Env.ListenAddr,
		Handler:           srv.Mux(),
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

	if cfg.Env.HeartbeatSweeperEnabled && storeIface != nil {
		sweeper := heartbeat.New(storeIface, heartbeat.Config{
			Interval:   cfg.Env.HeartbeatSweepInterval,
			StaleAfter: cfg.Env.HeartbeatStaleAfter,
		})
		go sweeper.Run(bgCtx)
	} else {
		log.Printf("heartbeat: sweeper disabled (LOOMCYCLE_HEARTBEAT_SWEEPER=0 or no Store)")
	}

	// Memory tool TTL sweeper. Cheap periodic DELETE of expired rows.
	// The store also filters expired entries at read time, so this
	// goroutine only matters for keeping the table small over the
	// long haul.
	if cfg.Env.MemorySweepInterval > 0 && storeIface != nil {
		go runMemorySweeper(bgCtx, storeIface, cfg.Env.MemorySweepInterval)
		log.Printf("memory: sweeper interval=%s", cfg.Env.MemorySweepInterval)
	} else {
		log.Printf("memory: sweeper disabled (LOOMCYCLE_MEMORY_SWEEP_MS=0 or no Store)")
	}

	// Channel tool TTL sweeper (v0.8.4). Same shape as MemorySweeper.
	// Reads filter expired rows regardless, so the sweeper is purely
	// for keeping the channel_messages table bounded.
	if cfg.Env.ChannelsSweepInterval > 0 && storeIface != nil {
		go runChannelsSweeper(bgCtx, storeIface, cfg.Env.ChannelsSweepInterval)
		log.Printf("channels: sweeper interval=%s", cfg.Env.ChannelsSweepInterval)
	} else if storeIface != nil {
		log.Printf("channels: sweeper disabled (LOOMCYCLE_CHANNELS_SWEEP_MS=0)")
	}

	// v0.8.16 Interruption tool TTL sweeper. Marks pending rows
	// whose expires_at < now as timed_out. Distinct from the
	// in-process timeout path (bus.Wait's own timer fires + the
	// tool calls InterruptFinish itself) — this sweeper catches
	// rows orphaned by a process crash mid-block, so the inbox
	// view doesn't keep showing dead interrupts as pending forever.
	// Cadence reuses the channels sweeper's interval — both are
	// "keep the substrate tables bounded" jobs at the same priority.
	if cfg.Env.ChannelsSweepInterval > 0 && storeIface != nil {
		go runInterruptsSweeper(bgCtx, storeIface, cfg.Env.ChannelsSweepInterval)
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
	if storeIface != nil {
		pauseDefault := time.Duration(cfg.Env.PauseDefaultTimeoutMs) * time.Millisecond
		pauseMgr := pause.NewManager(storeIface, pauseDefault)
		srv.SetPauseManager(pauseMgr)
		if pauseDefault > 0 {
			log.Printf("pause: manager wired (default timeout=%s)", pauseDefault)
		} else {
			log.Printf("pause: manager wired (default timeout=%s)", pause.DefaultPauseTimeout)
		}
	}

	if cfg.Env.MetricsEnabled && storeIface != nil {
		metricsSampler := metrics.New(storeIface, sem, metrics.Config{
			Interval:      cfg.Env.MetricsSampleInterval,
			CollectSystem: cfg.Env.MetricsCollectSystem,
		})
		go metricsSampler.Run(bgCtx)
		srv.SetMetricsSampler(metricsSampler)
		log.Printf("metrics: sampler enabled (interval=%s, system=%v)",
			cfg.Env.MetricsSampleInterval, cfg.Env.MetricsCollectSystem)
		if cfg.Env.MetricsSweepInterval > 0 && cfg.Env.MetricsRetentionDays > 0 {
			go runMetricsSweeper(bgCtx, storeIface,
				cfg.Env.MetricsRetentionDays, cfg.Env.MetricsSweepInterval)
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
		lcmcp.RunDynamicAgentSweeper(bgCtx, storeIface, cfg.Env.DynamicAgentSweepInterval, log.Printf)
		log.Printf("dynamic_agents: sweeper enabled (interval=%s)", cfg.Env.DynamicAgentSweepInterval)
	}
	if mcpMode {
		mcpSrv := lcmcp.New(lcmcp.Config{
			Connector:     srv, // *http.Server satisfies connector.Connector
			Runner:        srv, // *http.Server satisfies runner.Runner (streaming)
			Store:         storeIface,
			Logf:          log.Printf, // log goes to stderr; never stdout (stdout is the JSON-RPC wire)
			ServerName:    "loomcycle",
			ServerVersion: buildVersion,
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
			Store:       storeIface,
			CancelReg:   srv.CancelRegistry(),
			Connector:   srv, // v0.8.15: *http.Server satisfies connector.Connector
			Runner:      srv, // *http.Server satisfies runner.Runner (streaming)
			AuthToken:   cfg.Env.AuthToken,
			BuildCommit: buildCommit,
			BuildTime:   buildTime,
		})
		grpcSrv = googlegrpc.NewServer(
			googlegrpc.UnaryInterceptor(grpcAdapter.UnaryAuthInterceptor()),
			googlegrpc.StreamInterceptor(grpcAdapter.StreamAuthInterceptor()),
		)
		loomcyclepb.RegisterLoomcycleServer(grpcSrv, grpcAdapter)
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
	if grpcSrv != nil {
		grpcSrv.GracefulStop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !skipHTTP {
		_ = httpServer.Shutdown(ctx)
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
		if err := os.MkdirAll(cfg.Env.DataDir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("data dir: %w", err)
		}
		dbPath := filepath.Join(cfg.Env.DataDir, "loomcycle.db")
		st, err := storesqlite.Open(dbPath)
		if err != nil {
			return nil, nil, fmt.Errorf("sqlite open: %w", err)
		}
		log.Printf("store: sqlite at %s", dbPath)
		return st, func() { _ = st.Close() }, nil

	case "postgres":
		if cfg.Storage.PgDSN == "" {
			return nil, nil, fmt.Errorf("postgres backend selected but storage.pg_dsn / LOOMCYCLE_PG_DSN is empty")
		}
		st, err := storepostgres.Open(context.Background(), storepostgres.Config{
			DSN:          cfg.Storage.PgDSN,
			MaxOpenConns: cfg.Storage.PgMaxOpenConns,
			MinIdleConns: cfg.Storage.PgMinIdleConns,
			AutoMigrate:  cfg.Storage.PgAutoMigrate,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("postgres open: %w", err)
		}
		log.Printf("store: postgres (automigrate=%v)", cfg.Storage.PgAutoMigrate)
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
	return pr
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

// runMemorySweeper periodically deletes Memory rows whose TTL has
// expired. Read paths in the store filter expired rows out anyway so
// agents never see stale values; the sweeper just keeps the table
// bounded over time.
func runMemorySweeper(ctx context.Context, s store.Store, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			swept, err := s.MemorySweep(ctx)
			if err != nil {
				log.Printf("memory sweep: %v", err)
				continue
			}
			if swept > 0 {
				log.Printf("memory sweep: deleted %d expired row(s)", swept)
			}
		}
	}
}

// runInterruptsSweeper is the v0.8.16 mirror of runChannelsSweeper
// for the interrupts table. Transitions pending rows whose
// expires_at < now to status=timed_out. Distinct from the in-process
// timeout path (bus.Wait's own timer + the tool's InterruptFinish
// call) — this catches rows orphaned by a process crash mid-block
// so the user inbox doesn't show ghost-pending interrupts forever.
func runInterruptsSweeper(ctx context.Context, s store.Store, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			swept, err := s.InterruptSweepExpired(ctx)
			if err != nil {
				log.Printf("interrupts sweep: %v", err)
				continue
			}
			if swept > 0 {
				log.Printf("interrupts sweep: timed_out %d expired row(s)", swept)
			}
		}
	}
}

// runChannelsSweeper is the v0.8.4 mirror of runMemorySweeper for
// the channel_messages table. Same shape — read paths filter
// expired rows regardless, so the sweeper is bounded-table cleanup.
func runChannelsSweeper(ctx context.Context, s store.Store, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			swept, err := s.ChannelSweepExpired(ctx)
			if err != nil {
				log.Printf("channels sweep: %v", err)
				continue
			}
			if swept > 0 {
				log.Printf("channels sweep: deleted %d expired row(s)", swept)
			}
		}
	}
}

// runMetricsSweeper is the v0.8.x mirror of runChannelsSweeper for
// the process_samples table. Deletes rows whose sampled_at <
// (now - retentionDays). The retention guarantee is bounded-time,
// not bounded-rows; operators on slow disks should set
// MetricsRetentionDays to a smaller value if disk pressure shows up.
func runMetricsSweeper(ctx context.Context, s store.Store, retentionDays int, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
			swept, err := s.MetricsSweep(ctx, cutoff)
			if err != nil {
				log.Printf("metrics sweep: %v", err)
				continue
			}
			if swept > 0 {
				log.Printf("metrics sweep: deleted %d expired row(s)", swept)
			}
		}
	}
}

// spawnStdioMCP starts a stdio MCP child for one server entry. Env keys are
// sorted so process listings are deterministic across runs.
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
