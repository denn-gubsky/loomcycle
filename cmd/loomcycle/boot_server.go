package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/clienttools"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/decisionbuild"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	toolsa2a "github.com/denn-gubsky/loomcycle/internal/tools/a2a"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	"github.com/denn-gubsky/loomcycle/internal/webui"
	"github.com/denn-gubsky/loomcycle/internal/wiring"
)

// buildServer is boot phase 7: the decision models, the HTTP server
// (lchttp.New) and its first wiring — call lanes, usage hooks, the unit
// generator, credential resolution and the client-tool registry.
func (b *boot) buildServer() {
	cfg := b.cfg
	pr := b.pr
	sem := b.sem
	searchRegistry := b.searchRegistry
	searchResolver := b.searchResolver
	searchHostKeys := b.searchHostKeys
	teamDefTool := b.teamDefTool
	historyTool := b.historyTool
	storeIface := b.storeIface
	rerankModel := b.rerankModel
	credSealer := b.credSealer
	credEngine := b.credEngine
	credResolver := b.credResolver
	credKeyable := b.credKeyable

	// The decision models (the decision: block): a declared block that cannot be
	// built fails boot, as the reranker does. The Decision tool is registered
	// only when there is a block: an agent that lists it on a deployment without
	// one is not offered a tool whose every call would fail (config load says so
	// in a warning). Built before the server so the tool is in its catalogue.
	decisionService, dsErr := decisionbuild.Build(cfg, pr.sched)
	if dsErr != nil {
		fatal("decision: building the decision models failed", "err", dsErr)
	}
	if decisionService != nil {
		for _, m := range decisionService.Models() {
			slog.Info("decision: model", "name", m.Name, "provider", m.Provider, "model", m.Model)
		}
		slog.Info("decision: default", "name", decisionService.Default())
		decisionTool := &builtin.Decision{Service: decisionService}
		b.allTools = append(b.allTools, decisionTool)
		// A team's decision states ask through the same tool.
		teamDefTool.Decision = decisionTool
	}

	// The Context tool's catalog (used by `Context op=tools`) is wired inside
	// lchttp.New — it must include the Agent tool that New appends to the
	// server's tool set, which isn't in allTools here (F45). New re-points any
	// Context tool to the COMPLETE post-append catalog, so don't set it here.
	b.srv = lchttp.New(cfg, pr, b.allTools, sem, storeIface)
	srv := b.srv
	// RFC ES #8: open mode on a listener beyond loopback serves every route to
	// the network. Decided here — the store is open, so an admin token counts —
	// with the server's own predicate, before the listeners start.
	bootAuthConfigured, bootAuthErr := srv.AuthConfiguredErr(context.Background())
	if err := openModeRefusal(bootAuthConfigured, bootAuthErr, &cfg.Env); err != nil {
		// The deployment guides quote this refusal's text; keep it.
		fatal(fmt.Sprintf("auth: %v", err))
	}
	if err := strictModeRefusal(bootAuthConfigured, cfg); err != nil {
		fatal(fmt.Sprintf("auth: %v", err))
	}
	slog.Info("auth: Web UI sign-in", "mode", cfg.ResolvedAuth.ModeName(), "gates", len(cfg.ResolvedAuth.LoginGates()))
	if w := insecureCookiesWarning(&cfg.Env); w != "" {
		slog.Warn(w)
	}
	// RFC FA P2: the password gates, with their pepper, policy and TOTP key.
	// A gate that cannot run is a boot failure, not a gate that silently
	// accepts nobody.
	pwGates, err := lchttp.PasswordGatesFromConfig(cfg)
	if err != nil {
		fatal(fmt.Sprintf("auth: %v", err))
	}
	if pwGates != nil {
		srv.SetPasswordGates(pwGates)
		slog.Info("auth: password gates", "gates", pwGates.Names(), "breached_list_entries", pwGates.PolicySize())
	}
	if w := lchttp.ProxyThrottleWarning(cfg); w != "" {
		slog.Warn(w)
	}
	// RFC FA P3: the header (trusted-proxy) and AWS IAM gates. Like the
	// password gates, one that cannot run fails the boot.
	var edgeDeps lchttp.EdgeGateDeps
	if storeIface != nil {
		edgeDeps.Replay = storeIface
	}
	edgeGates, err := lchttp.EdgeGatesFromConfig(cfg, edgeDeps)
	if err != nil {
		fatal(fmt.Sprintf("auth: %v", err))
	}
	if edgeGates != nil {
		srv.SetEdgeGates(edgeGates)
		slog.Info("auth: edge gates", "gates", edgeGates.Names())
	}
	b.bootAuthConfigured = bootAuthConfigured
	if bootAuthConfigured {
		for _, w := range srv.AuthPostureWarnings(context.Background()) {
			slog.Warn("auth: posture warning", "detail", w)
		}
	}
	// The providers' call lanes (providers.<id>.max_concurrent / lanes). The
	// registry already gates every provider with this scheduler; the server
	// gets the same instance to report it and to reload it.
	srv.SetCallScheduler(pr.sched)
	// A rerank is spent on the searching run's behalf, so its tokens go to that
	// run's ledger and budget. Set here because srv exists only now; no run can
	// have started yet.
	if rerankModel != nil {
		rerankModel.SetOnUsage(srv.SideCallUsage(store.UsagePurposeRerank))
	}
	// The Document derived-unit writer (memory.unit_generator): a declared one that
	// cannot be built fails boot, as the embedder and the reranker do.
	unitGenerator, ugErr := wiring.UnitGenerator(cfg, pr.sched)
	if ugErr != nil {
		fatal("unit generator: building it failed", "err", ugErr)
	}
	if unitGenerator != nil {
		slog.Info("unit generator: enabled", "model", unitGenerator.ModelID())
		// Its calls are charged to the tenant and user whose document they
		// derive units for, and are the throughput samples of its model.
		unitGenerator.ObserveCall = srv.BackgroundCallObserver(store.UsagePurposeUnitGeneration)
		srv.SetUnitGenerator(unitGenerator)
	}
	// A decision is asked on a run's behalf, so its tokens go to that run's
	// ledger and budget. Set here because srv exists only now.
	if decisionService != nil {
		decisionService.SetOnUsage(srv.SideCallUsage(store.UsagePurposeDecision))
	}
	// RFC FG: the File tool's OCR, on this server's model resolution and usage
	// ledger. A configured engine that cannot be built fails boot.
	fileOCR, err := srv.FileOCR()
	if err != nil {
		fatal("transcription: building the OCR engine failed", "err", err)
	}
	if fileOCR != nil {
		b.fileTool.OCR = fileOCR
		slog.Info("transcription: OCR enabled", "engine", fileOCR.Config.OCR.Engine, "tier", fileOCR.Config.OCR.Tier, "model", fileOCR.Config.OCR.Model)
	}
	// RFC FG: the File tool's speech-to-text, on the same ledger.
	fileSTT, err := srv.FileSTT()
	if err != nil {
		fatal("transcription: building the speech-to-text engine failed", "err", err)
	}
	if fileSTT != nil {
		b.fileTool.STT = fileSTT
		slog.Info("transcription: speech-to-text enabled", "engine", fileSTT.Config.STT.Engine,
			"provider", fileSTT.Config.STT.Provider, "model", fileSTT.Config.STT.Model, "media_helper", fileSTT.Config.STT.Media != nil)
	}
	// RFC AR: stamp the credential resolver onto each run so a tenant/user's own
	// provider key (ANTHROPIC_API_KEY, BRAVE_API_KEY, …) overrides the host key.
	srv.SetCredentialResolver(credResolver)
	// RFC EI P3: a queued run's per-run secrets are sealed with the same key,
	// so a restart can start it again. No key ⇒ such a run is failed instead.
	srv.SetQueueSealer(credSealer)
	// RFC AX: the keyability probe backs credential-aware routing for a
	// restricted run (resolves only to providers the tenant can key itself).
	srv.SetCredKeyable(credKeyable)
	// The snapshot scan's check for a webhook's stored verification secret.
	srv.SetCredToCompare(func(ctx context.Context, tenantID, name string) bool {
		ok, err := credEngine.HasToCompare(ctx, tenantID, name)
		return err == nil && ok
	})
	// RFC BE: the History tool's op=recap summarizer. Store was late-bound above
	// (before srv existed); Recap needs server-side provider/model resolution, so
	// it's the off-loop twin of the compaction summarizer (Server.RecapSession).
	// The method value reads s.resolver lazily at call time — set below via
	// SetResolver, long before any runtime recap call.
	historyTool.Recap = srv.RecapSession
	// RFC BB: the search catalog for the routing view (GET /v1/_routing search
	// block). Same registry/resolver the WebSearch tool uses; nil when no search
	// providers are configured.
	srv.SetSearchRouting(searchRegistry, searchResolver, searchHostKeys)

	// RFC BC: the client-tool host registry — live per-principal WebSocket
	// connections over which a client executes tools on the user's machine. The
	// /v1/client-tools handler registers connections here; the dispatch fallback
	// (composed below) + candidateTools advertising read it. Always on (the
	// endpoint is bearer-gated; an empty registry just serves no client-tools).
	clientToolReg := clienttools.NewRegistry(cfg.Env.ClientToolMaxConns)
	srv.SetClientTools(clientToolReg)
}

// openSQLMemory opens the SQL Memory manager when it is enabled and wires
// it into the tools and the server.
func (b *boot) openSQLMemory() {
	cfg := b.cfg
	memoryTool := b.memoryTool
	documentTool := b.documentTool
	contextTool := b.contextTool
	srv := b.srv

	// RFC AA SQL Memory (Phase 1). Off by default; constructed only when the
	// operator enables it. The manager hosts per-scope sqlite databases that
	// SEPARATE from the main store — an agent's arbitrary SQL can only ever
	// reach its own scope file. Wired into the Memory tool (sql_query/sql_exec)
	// and the server (run-scope drop at top-level run completion).
	// Declared here (not inside the block) so the RFC BM retention sweeper below
	// can reclaim a retired agent's SQL-Memory scope; nil when SQL Memory is off.
	var sqlMemMgr *sqlmem.Manager
	if cfg.Storage.SqlMemEnabled {
		sqlMemRoot := cfg.Storage.SqlMemRoot
		if sqlMemRoot == "" {
			sqlMemRoot = filepath.Join(cfg.Env.DataDir, "sqlmem")
		}
		sqlMemCfg := sqlmem.Config{
			Root:               sqlMemRoot,
			PgDSN:              cfg.Storage.SqlMemPgDSN,
			QuotaBytes:         cfg.Storage.SqlMemQuotaBytes,
			StatementTimeoutMS: cfg.Storage.SqlMemStatementTimeoutMS,
			MaxRows:            cfg.Storage.SqlMemMaxRows,
			TxnTimeoutMS:       cfg.Storage.SqlMemTxnTimeoutMS,
			MaxOpenTxns:        cfg.Storage.SqlMemMaxOpenTxns,
			MaxTxnDepth:        cfg.Storage.SqlMemMaxTxnDepth,
			ScopeTTLMS:         cfg.Storage.SqlMemScopeTTLMS,
			GCIntervalMS:       cfg.Storage.SqlMemGCIntervalMS,
			TotalMaxBytes:      cfg.Storage.SqlMemTotalMaxBytes,
			SharedSchemas:      cfg.Storage.SqlMemSharedSchemas,
		}
		// SQL Memory FOLLOWS the main store backend (RFC AA decision 4): a
		// postgres deploy gets schema-per-scope in the SEPARATE aux DB; a
		// sqlite deploy gets file-per-scope. The aux DSN is required on
		// postgres and ignored on sqlite.
		var (
			smErr      error
			sqlMemTier string
		)
		if cfg.Storage.Backend == "postgres" {
			if strings.TrimSpace(cfg.Storage.SqlMemPgDSN) == "" {
				fatal("sqlmem: SQL Memory on a postgres backend requires a separate aux DSN — set LOOMCYCLE_SQLMEM_PG_DSN (a DIFFERENT database from the main store)")
			}
			sqlMemMgr, smErr = sqlmem.NewPostgres(context.Background(), sqlMemCfg)
			sqlMemTier = "postgres (schema-per-scope, aux DB)"
		} else {
			if strings.TrimSpace(cfg.Storage.SqlMemPgDSN) != "" {
				slog.Warn("sqlmem: LOOMCYCLE_SQLMEM_PG_DSN is set but the backend is sqlite — ignoring it (file-per-scope)", "dir", sqlMemRoot)
			}
			if len(cfg.Storage.SqlMemSharedSchemas) > 0 {
				slog.Warn("sqlmem: sqlmem_shared_schemas is set but the backend is sqlite — ignoring it (read-only shared schemas are a postgres-tier feature, RFC AA Phase 3g)")
			}
			sqlMemMgr, smErr = sqlmem.New(sqlMemCfg)
			sqlMemTier = "sqlite (file-per-scope, root=" + sqlMemRoot + ")"
		}
		if smErr != nil {
			fatal("sqlmem: opening SQL Memory failed", "err", smErr)
		}
		// Audit sink: reuse the operator audit log file when configured (the
		// records carry a distinct action=sql_query/sql_exec), else NopSink.
		var sqlAudit audit.Sink = audit.NopSink{}
		if cfg.Env.AuditLogPath != "" {
			fs, aerr := audit.NewFileSink(cfg.Env.AuditLogPath)
			if aerr != nil {
				fatal("sqlmem audit: opening the audit log failed", "path", cfg.Env.AuditLogPath, "err", aerr)
			}
			sqlAudit = fs
		}
		memoryTool.SqlMem = sqlMemMgr
		documentTool.SqlMem = sqlMemMgr
		// Context op=capabilities reports SQL Memory / Documents availability
		// from the CONSTRUCTED manager, not the config flag.
		contextTool.SqlMem = sqlMemMgr
		memoryTool.SqlAudit = sqlAudit
		memoryTool.SqlAuditMode = cfg.Storage.SqlMemAuditMode
		// Reuse the server's redactor (same secret-classified env) so an
		// audited statement is masked exactly like the rest of the transcript.
		memoryTool.Redactor = srv.Redactor()
		srv.SetSqlMem(sqlMemMgr)
		// Close the manager (all open scope handles) on shutdown, after the
		// store closer so an in-flight finish-path drop still has a live handle.
		b.addCleanup(func() { _ = sqlMemMgr.Close() })
		slog.Info("sqlmem: SQL Memory enabled", "tier", sqlMemTier, "quota_bytes", cfg.Storage.SqlMemQuotaBytes,
			"max_rows", cfg.Storage.SqlMemMaxRows, "timeout_ms", cfg.Storage.SqlMemStatementTimeoutMS,
			"audit", cfg.Storage.SqlMemAuditMode)
	}

	b.sqlMemMgr = sqlMemMgr
}

// wireServerCatalog hands the server what it reports and enumerates: the
// build identity, MCP fallback and pool inspector, the dynamic-tool
// enumerator, the skill set and the embedder.
func (b *boot) wireServerCatalog() {
	cfg := b.cfg
	skillSet := b.skillSet
	embedder := b.embedder
	cryptoTool := b.cryptoTool
	dynamicMCPRegistry := b.dynamicMCPRegistry
	mcpPool := b.mcpPool
	skippedStaticMCP := b.skippedStaticMCP
	mcpLazyResolver := b.mcpLazyResolver
	storeIface := b.storeIface
	srv := b.srv

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
	srv.SetCryptoStats(cryptoTool.Stats) // RFC EP: loomcycle_crypto_ops_total
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
	// tools filter. MCP: each dynamic-registry server's persisted
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
	srv.SetDynamicToolEnumerator(func(ctx context.Context, tenant string, wantServers map[string]bool) []tools.Tool {
		// F33: dynamic MCP enumeration (cached tools for every server; a
		// bounded run-start handshake for a REFERENCED server whose cache is
		// empty) lives in the testable mcp.DynamicToolsForRun helper.
		out := mcp.DynamicToolsForRun(ctx, mcpPool, dynamicMCPRegistry, mcpActiveDefReader{storeIface}, tenant, wantServers, runStartMCPDiscoveryTimeout, slog.Default())
		// Recover STATIC (yaml) MCP servers skipped at boot without a restart —
		// the static sibling of the F33 dynamic re-probe above. Scoped to the
		// servers this run references (wantServers) and to those that actually
		// failed boot (skippedStaticMCP), so a healthy server is never re-dialed.
		out = append(out, mcp.StaticToolsForRun(ctx, mcpPool, cfg.MCPServers, skippedStaticMCP, tenant, wantServers, runStartMCPDiscoveryTimeout, slog.Default())...)
		if cfg.Env.A2AServerEnabled {
			resolve := func(ctx context.Context, name string) (config.A2AAgent, bool) {
				// RFC N: the enumerator's `tenant` is authoritative (server
				// derived it from the principal/session/parent), so resolve the
				// per-run peer set under it rather than re-deriving from ctx.
				return lookup.A2AAgent(ctx, storeIface, cfg, tenant, name)
			}
			out = append(out, toolsa2a.RegisterTools(ctx, cfg, storeIface, resolve, nil, slog.Default(), toolsa2a.WithCredentialResolver(lateCredSubstitute(&b.credSubstitute)))...)
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
}

// wireBuses builds the system channel publisher, the steering registry,
// the run-state bus and (not yet started) the heartbeat runner.
func (b *boot) wireBuses() {
	cfg := b.cfg
	channelBus := b.channelBus
	channelScheduler := b.channelScheduler
	channelTool := b.channelTool
	documentTool := b.documentTool
	interruptionTool := b.interruptionTool
	storeIface := b.storeIface
	srv := b.srv

	// v0.8.6 SystemPublisher — backs the POST /v1/_channels/_system/...
	// admin endpoint AND the cadence/event-hook publishers added in
	// PR 3. Wires the same Bus + Scheduler as the agent tool so
	// long-poll subscribers wake whether the publish came from an
	// agent, an internal goroutine, or the admin endpoint.
	sysPublisher := &channels.StorePublisher{
		Store:     storeIface,
		Bus:       channelBus,
		Scheduler: channelScheduler,
		// One wiring point makes every channel write honour the channel's
		// definition — see StorePublisher.Defs.
		Defs: srv.ChannelWriteDef,
		// Off, a write to a channel that carries hooks is refused.
		HooksEnabled: cfg.Env.ChannelHooksEnabled && storeIface != nil,
	}
	srv.SetSystemPublisher(sysPublisher)
	// Wire the SystemPublisher onto the Interruption tool too — same
	// instance, so _system/interrupts/* publishes from inside the
	// tool wake the same Channel long-poll subscribers.
	interruptionTool.SystemPublisher = sysPublisher
	// The Channel tool writes through the same writer, so an agent's publish
	// honours the channel's definition as it stands at the write, not as it
	// stood when the run started.
	channelTool.Writer = sysPublisher
	// So does Document's change feed.
	documentTool.Writer = sysPublisher
	// v0.8.16 — wire the same Bus to the server so the resolve
	// handler can wake the blocked tool's bus.Wait via the
	// "intr:<id>" key. Without this the resolve writes the row but
	// the tool re-checks storage only when its own timer fires.
	srv.SetInterruptionBus(channelBus)
	// PR 2 / interactive terminal — operator mid-run steering registry.
	// Enables POST /v1/runs/{run_id}/input to inject an instruction into an
	// in-flight run. Default per-run buffer depth (16); single-replica
	// (cross-replica routing is a later phase).
	srv.SetSteerRegistry(steer.NewRegistry(0))
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
		fatal("heartbeat specs: loading them failed", "err", err)
	}
	var heartbeatRunner *channels.HeartbeatRunner
	if len(heartbeatSpecs) > 0 {
		heartbeatRunner = channels.NewHeartbeatRunner(sysPublisher, buildCommit, heartbeatSpecs)
	}

	b.sysPublisher = sysPublisher
	b.runStateBus = runStateBus
	b.heartbeatSpecs = heartbeatSpecs
	b.heartbeatRunner = heartbeatRunner
}

// buildModelResolver builds the model-resolution matrix; its first probe
// runs now, before any traffic.
func (b *boot) buildModelResolver() {
	cfg := b.cfg
	pr := b.pr
	srv := b.srv

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

	b.resolver = resolver
}

// loadWebUI picks the Web UI to serve and logs the outcome. The bundle URL is
// logged by host only: its path or query string can carry a token.
func loadWebUI(cfg *config.Config) (*webui.Bundle, webui.Status) {
	if cfg.Env.WebUIBundleURL == "" {
		b, st, err := webui.LoadConfigured(cfg.Env.WebUIDir)
		switch {
		case err != nil:
			slog.Warn("webui: LOOMCYCLE_WEBUI_DIR refused, serving the embedded UI", "dir", cfg.Env.WebUIDir, "err", err)
		case b != nil:
			slog.Info("webui: serving UI from LOOMCYCLE_WEBUI_DIR", "version", b.UIVersion(), "dir", cfg.Env.WebUIDir)
		}
		return b, st
	}
	host := webui.RedactedURL(cfg.Env.WebUIBundleURL)
	b, st, err := webui.LoadFromURL(webui.URLSource{
		URL:                  cfg.Env.WebUIBundleURL,
		SHA256:               cfg.Env.WebUIBundleSHA256,
		DataDir:              cfg.Env.DataDir,
		PrivateHostAllowlist: cfg.Env.HTTPPrivateHostAllowlist,
		Logger:               slog.Default(),
	})
	if err != nil {
		slog.Warn("webui: LOOMCYCLE_WEBUI_BUNDLE_URL refused, serving the embedded UI", "host", host, "err", err)
		return b, st
	}
	slog.Info("webui: serving UI from LOOMCYCLE_WEBUI_BUNDLE_URL", "version", b.UIVersion(), "host", host, "sha256", cfg.Env.WebUIBundleSHA256)
	return b, st
}

// buildHTTPServer builds the *http.Server around the server's mux. It is
// not started here; startListeners does that.
func (b *boot) buildHTTPServer() {
	cfg := b.cfg
	srv := b.srv
	a2aServer := b.a2aServer

	// The Web UI is chosen once, before the mux is built: a verified bundle
	// from LOOMCYCLE_WEBUI_DIR or LOOMCYCLE_WEBUI_BUNDLE_URL (config refuses
	// both at once), else the embedded UI. A refused bundle is logged and
	// never fails the boot — the operator still gets a working UI.
	uiBundle, uiStatus := loadWebUI(cfg)
	srv.SetWebUIBundle(uiBundle, uiStatus)

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
		// Bound idle keep-alive connections so abandoned clients don't
		// accumulate open fds over time. ReadTimeout/WriteTimeout stay OFF
		// deliberately: SSE run streams + long-poll subscribes are long-lived
		// and a read/write deadline would sever them mid-stream. IdleTimeout
		// only fires between requests on a keep-alive connection, so it's
		// SSE-safe.
		IdleTimeout: 120 * time.Second,
	}

	b.httpServer = httpServer
}
