package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	lcmcp "github.com/denn-gubsky/loomcycle/internal/api/mcp"
	"github.com/denn-gubsky/loomcycle/internal/cli"
)

// resolveBuildIdentity fills buildVersion / buildCommit / buildTime.
func resolveBuildIdentity() {
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
}

// dispatchSubcommand runs a CLI subcommand and exits, or reports how the
// runtime should boot. done means main must return without booting (help,
// version); mcpMode means `loomcycle mcp` was asked for, with "mcp" already
// stripped from os.Args.
func dispatchSubcommand() (mcpMode, done bool) {
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
		case "presets":
			// RFC AQ — list the embedded presets/bundles, or `presets show <name>`
			// to print one's YAML. Read-only introspection of the embedded base.
			os.Exit(cli.RunPresets(os.Args[2:], os.Stdout, os.Stderr))
		case "env-template":
			// RFC AQ — print the embedded .env.insecure.example (non-secret env
			// catalogue) so it's reachable without a source checkout.
			os.Exit(cli.RunEnvTemplate(os.Args[2:], os.Stdout, os.Stderr))
		case "truenas-questions":
			// RFC AR Phase 2 — generate the catalog form's env-options block from
			// the embedded env catalogue (keeps the form in lockstep, no drift).
			os.Exit(cli.RunTrueNASQuestions(os.Args[2:], os.Stdout, os.Stderr))
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
		case "memory-eval-live":
			// RFC BL P4a — the memory extraction eval against a REAL model. Scores
			// the SHIPPED extractor prompt's judgement per ability (extraction,
			// property, temporal, update, abstention) and gates on updates +
			// abstention. Not hermetic: it needs a provider and spends tokens.
			os.Exit(cli.RunMemoryEvalLive(os.Args[2:], os.Stdout, os.Stderr))
		case "memory-eval-judge":
			// The write-time JUDGE against a real model: does it admit claims their
			// quotes plainly carry, and refuse the ones that say more than their
			// quote does. Gated on FALSE REFUSALS — the judge decides which facts
			// stop being returned, so its failures are the invisible kind.
			os.Exit(cli.RunMemoryEvalJudge(os.Args[2:], os.Stdout, os.Stderr))
		case "memory-calibrate":
			// Measures the consolidation similarity bands against the
			// operator's OWN embedder. Cosine scale is a property of the
			// embedding model, so a shipped default is right for at most one
			// model — and wrong SILENTLY, in both directions. Exits non-zero
			// when no threshold can separate duplicates from related facts.
			os.Exit(cli.RunMemoryCalibrate(os.Args[2:], os.Stdout, os.Stderr))
		case "help", "-h", "--help":
			cli.PrintHelp(os.Stdout, lcmcp.MetaToolCount())
			return false, true
		case "version", "-v":
			// Real `version` subcommand (mirrors --version). Prints + exits;
			// it must NEVER fall through to booting the runtime.
			printVersion()
			return false, true
		default:
			// A non-flag first arg that isn't a known subcommand is a
			// mistake — e.g. `loomcycle version` historically fell through
			// here and BOOTED the runtime, silently binding the port. Reject
			// it loudly. Flags (--config / --version / --upstream) are NOT
			// subcommands: they start with "-" and fall through to flag
			// parsing below (preserving `loomcycle --config foo.yaml`).
			if !strings.HasPrefix(os.Args[1], "-") {
				fmt.Fprintf(os.Stderr, "loomcycle: error: unknown subcommand %q\n", os.Args[1])
				fmt.Fprintln(os.Stderr, "run `loomcycle help` for subcommands, or `loomcycle --config <file>` to start the runtime.")
				os.Exit(2)
			}
		}
	}
	return mcpMode, false
}

// parseFlags parses the server flags into b and returns the --upstream URL
// and whether --version was given.
func (b *boot) parseFlags() (string, bool) {
	// --config is repeatable (RFC AN config layering): each flag adds a layer,
	// merged left→right with last-wins. No flag → the XDG default below.
	flag.Func("config", "path to config YAML (repeatable; layers deep-merge left→right, last wins)", func(v string) error {
		b.cfgPaths = append(b.cfgPaths, v)
		return nil
	})
	// --preset is the repeatable flag equivalent of LOOMCYCLE_PRESETS (RFC AQ):
	// each names an embedded preset/bundle layered as the BASE of the stack, in
	// order. Flags, if any, override the env selection.
	flag.Func("preset", "embedded preset/bundle to layer as a base (repeatable; e.g. base, document-agent)", func(v string) error {
		b.presetFlags = append(b.presetFlags, v)
		return nil
	})
	showVersion := flag.Bool("version", false, "print build identifier and exit")
	// RFC R thin-client mode: `loomcycle mcp --upstream <url>` runs as a
	// stdio↔/v1/_mcp proxy to an authoritative runtime, with NO local
	// runtime. The single-runtime invariant — a second loomcycle process
	// is a control client, never a second runtime. This replaced the old
	// `--no-http` flag (removed in v0.23.0), which "muted the listener"
	// while still booting a full second runtime — the anti-pattern that
	// caused the cross-process interruption hang (F15) and rogue-runtime
	// wedge (F16). To add an MCP surface next to a runtime, use --upstream.
	upstream := flag.String("upstream", "", "(`mcp` mode) run as a thin client proxying to this runtime's /v1/_mcp instead of booting a local runtime")
	flag.Parse()
	return *upstream, *showVersion
}

// runThinClient serves `loomcycle mcp --upstream <url>` (RFC R): a
// stdio↔/v1/_mcp proxy, with no runtime of its own.
func runThinClient(upstream string) {
	proxyCtx, stopProxy := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopProxy()
	pc := lcmcp.NewProxyClient(lcmcp.ProxyConfig{
		Upstream: upstream,
		Token:    os.Getenv("LOOMCYCLE_MCP_UPSTREAM_TOKEN"),
		Logf:     log.Printf, // stderr; never stdout (stdout is the JSON-RPC wire)
		// Self-recovery knobs (defaults applied by NewProxyClient when unset).
		// The thin client boots with NO config load, so these are read straight
		// from the environment.
		ResponseHeaderTimeout: mcpProxyHeaderTimeout(),
		ReconnectDelays:       mcpProxyReconnectDelays(),
	})
	log.Printf("mcp: thin-client mode → upstream %s (no local runtime)", upstream)
	err := pc.Serve(proxyCtx, os.Stdin, os.Stdout)
	switch {
	case err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		// Clean stop: stdin EOF (client disconnect) or a received
		// signal. Exit 0 so a supervisor doesn't treat a graceful
		// stop as a crash and loop-restart.
		log.Printf("mcp: proxy stopped")
	default:
		log.Printf("mcp: proxy ended: %v", err)
		os.Exit(1)
	}
}
