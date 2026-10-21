package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/memory/embedders"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/localapi"
)

// buildTools is boot phase 3: every built-in tool, plus the skill and help
// sets and the embedder they share. Tools that need the store, the
// credential engine or the server get those later, from the phases that
// build them.
func (b *boot) buildTools() {
	cfg := b.cfg
	pr := b.pr

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
	// RFC BA: fold inline `skills:` yaml defs into the runtime set so they are
	// invokable + listable on demand via the Skill tool. Before RFC BA the
	// config-load resolveSkills bundled their bodies into prompts (and merged
	// them into a throwaway set); now that bundling is gone, they must live in
	// the runtime set the Skill/SkillDef tools + Library share. Inline wins on
	// a name collision with a SkillsRoot skill (config is authoritative).
	for name, spec := range cfg.Skills {
		skillSet.Add(&skills.Skill{Name: name, Description: spec.Description, Tools: spec.Tools, Body: spec.Body})
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

	// RFC BB: build the search-provider registry + resolver + operator host-key
	// map for the WebSearch fallback circuit. Nil (no providers configured, and
	// no BRAVE_API_KEY back-compat default) → WebSearch refuses "not configured".
	searchRegistry, searchResolver, searchHostKeys := buildSearchProviders(cfg)

	// Bashbox is a named var so its CredResolve can be late-bound after the
	// credential engine is constructed below (RFC BD per-tenant repo token).
	bashboxTool := &builtin.Bashbox{
		Enabled:              cfg.Env.BashboxEnabled,
		FallbackCommands:     cfg.Env.BashboxFallbackCommands,
		FallbackAllowedEnv:   cfg.Env.BashboxFallbackAllowedEnv,
		FallbackAllowedCreds: cfg.Env.BashboxFallbackAllowedCreds,
	}

	// Bash is likewise a named var so its CredResolve can be late-bound after
	// the credential engine — it injects per-tenant creds into the raw child env.
	bashTool := &builtin.Bash{
		Enabled:      cfg.Env.BashEnabled,
		AllowedCreds: cfg.Env.BashAllowedCreds,
	}

	b.allTools = []tools.Tool{
		// The file/exec tools resolve their sandbox root per-call from the
		// run's VolumePolicy (RFC AH); an agent bound to no volume is refused
		// (sandbox-by-default — the legacy READ_ROOT/WRITE_ROOT/BASH_CWD jail
		// was retired in Phase 3).
		&builtin.Read{},
		&builtin.Write{},
		&builtin.Edit{},
		&builtin.Grep{},
		&builtin.Glob{},
		&builtin.NotebookEdit{},
		httpTool,
		&builtin.WebFetch{HTTP: httpTool},
		&builtin.WebSearch{Registry: searchRegistry, Resolver: searchResolver, HostKeys: searchHostKeys, ShowProvenance: cfg.Env.WebSearchProvenance},
		// Bash — constructed above so CredResolve can be late-bound after the
		// credential engine exists (per-tenant cred injection into the child env).
		bashTool,
		// Bashbox — a TRUE in-process sandbox (gbash): no OS process, paths
		// rooted at the volume, no network, and read-only volumes are honored
		// (writes hit an in-RAM overlay). Opt-in like Bash. Constructed above so
		// CredResolve can be late-bound after the credential engine exists.
		bashboxTool,
		// SkillTool's Store is late-bound below (so DB-active SkillDef
		// rows override the static Set body). Nil-Store before
		// late-binding falls back to the static Set; the assignment
		// after storeIface is constructed switches the resolution
		// order to DB-first.
	}
	skillTool := &builtin.SkillTool{Set: skillSet}
	b.allTools = append(b.allTools, skillTool)
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
	//
	// Built inside its provider's embedder lane (pr.sched), as every model call
	// this process makes is counted in a lane: providers.<id>.lanes.embedder.
	embedder, err := embedders.BuildGated(cfg, pr.sched)
	if err != nil {
		log.Fatalf("embedder: %v", err)
	}
	if embedder != nil {
		log.Printf("embedder: %s/%s (dim=%d)", embedder.Provider(), embedder.Model(), embedder.Dimension())
		// Wrapped ONCE, here, before any consumer takes a reference: the Memory
		// tool, the Document tool and the HTTP server all share this instance, so
		// the observed health covers every embedding the process performs rather
		// than one subsystem's slice of them. ObserveEmbedder(nil) returns nil, so
		// the `if Embedder == nil` checks downstream keep working.
		//
		// Only reachable when an embedder is configured — an unconfigured
		// deployment stays byte-identical, holding a nil embedder as before.
		//
		// The observer is OUTSIDE the lane wrapper: it is what the health
		// report asserts the embedder to, so it must be the outermost value.
		embedder = providers.ObserveEmbedder(embedder)
	}

	memoryTool := &builtin.Memory{
		MaxValueBytes:     cfg.Env.MemoryMaxValueBytes,
		DefaultQuotaBytes: cfg.Env.MemoryMaxScopeBytes,
		Embedder:          embedder,
		// RFC I MR-3b: resolves a per-agent memory_backend NAME to its
		// MemoryBackendDef (static yaml or dynamic substrate Def).
		Cfg: cfg,
	}
	b.allTools = append(b.allTools, memoryTool)

	// Recall tool — the read side of recall-augmented distillation
	// (context.recall). Searches the run-scoped index of spans the loop's
	// distillation evicted, with a silent fallback to the agent's durable memory
	// via the Memory tool (held by pointer, so its late-bound Backend is resolved
	// at call time). Registered unconditionally like Memory: the run-scoped index
	// is only built when an agent set context.recall, but the persistent-memory
	// free-text search is useful for any agent that grants the tool.
	recallTool := &builtin.Recall{Memory: memoryTool}
	b.allTools = append(b.allTools, recallTool)

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
	b.allTools = append(b.allTools, channelTool)

	// AgentDef tool (v0.8.5). Per-agent default-deny via
	// agent_def_scopes yaml; the tool itself is registered globally
	// and the policy gate runs inside Execute.
	agentDefTool := &builtin.AgentDef{
		Cfg:                 cfg,
		MaxDefinitionBytes:  cfg.Env.AgentDefMaxDefinitionBytes,
		MaxDescriptionBytes: cfg.Env.AgentDefMaxDescriptionBytes,
		MaxCodeBytes:        cfg.Env.AgentDefMaxCodeBytes,
	}
	b.allTools = append(b.allTools, agentDefTool)

	// SkillDef tool (v0.8.22). Mirror of AgentDef for runtime skill
	// mutation. Authoring is gated by the agent's RFC BA `skills:`
	// pattern allowlist (via SkillPolicy). Set is shared with the
	// SkillTool above so static-name guard + fork-bootstrap see the
	// same static skills.
	skillDefTool := &builtin.SkillDef{
		Set:                 skillSet,
		MaxBodyBytes:        cfg.Env.SkillDefMaxBodyBytes,
		MaxDescriptionBytes: cfg.Env.SkillDefMaxDescriptionBytes,
	}
	b.allTools = append(b.allTools, skillDefTool)

	// TeamDef tool (RFC AP) — MUST be in allTools so team/assistant +
	// team/orchestrator can author/run teams IN-LOOP (an agent's `tools: [TeamDef]`
	// only takes effect if the instance is agent-available; being reachable via
	// the HTTP/MCP def-route alone is not enough). Store is late-bound below;
	// Spawn + Admit (op=run) are injected by srv.SetTeamDefTool. The definition
	// cap is the team's own (it carries its agents' definitions); the
	// description cap is AgentDef's.
	teamDefTool := &builtin.TeamDef{
		MaxDefinitionBytes:  cfg.Env.TeamDefMaxDefinitionBytes,
		MaxDescriptionBytes: cfg.Env.AgentDefMaxDescriptionBytes,
		// A team's own agents and skills pass AgentDef and SkillDef create's
		// gates, on these instances.
		Agents: agentDefTool,
		Skills: skillDefTool,
	}
	b.allTools = append(b.allTools, teamDefTool)

	// VolumeDef tool (RFC AH Phase 2a) — the dynamic-volume substrate.
	// In-loop (per-agent dispatcher) with a default-deny volume_def_scopes
	// gate, NOT operator-admin-only like MCPServerDef. Store late-bound
	// below. Cfg supplies the dynamic_root + the static-name collision
	// guard. MaxNameLen mirrors the regex ceiling (64).
	volumeDefTool := &builtin.VolumeDef{
		Cfg:        cfg,
		MaxNameLen: 64,
	}
	b.allTools = append(b.allTools, volumeDefTool)

	// RFC AR — the CredentialDef tool (secure per-tenant credential store).
	// Engine (store + Sealer) is late-bound below, once the store + the master
	// key (LOOMCYCLE_SECRET_KEY) are available. In-loop with tenant/user/agent
	// scope isolation; the plaintext value is masked from the persisted
	// transcript at store time (see maskCredentialCreateValue).
	credentialDefTool := &builtin.CredentialDef{}
	b.allTools = append(b.allTools, credentialDefTool)

	// RFC EP — the Crypto tool: encrypt / sign / MAC / TOTP with keys the
	// runtime holds. Engine late-bound with CredentialDef's; an agent uses keys
	// only from the scopes its crypto_scopes grants (tools.CryptoPolicy).
	cryptoTool := &builtin.Crypto{
		MaxBytes:     cfg.Env.CryptoMaxBytes,
		MaxOpsPerRun: cfg.Env.CryptoMaxOpsPerRun,
	}
	b.allTools = append(b.allTools, cryptoTool)

	// RFC AL — the Path tool (Unix-like VFS over the dirents substrate). Store
	// late-bound below alongside the other substrate tools.
	pathTool := &builtin.Path{}
	b.allTools = append(b.allTools, pathTool)

	// RFC AK — the Document tool (chunked-graph documents). Store late-bound
	// below; SqlMem bound in the SQL Memory block (Document requires it); Bus
	// for change events. MaxAssetBytes (RFC BO) caps a stored image's decoded
	// size (0 → the tool's built-in default).
	// Embedder (RFC BU phase 1) makes chunk BODIES semantically searchable via
	// `memory op=search` with prefix "doc.chunk:". Nil when the operator declared
	// no embedder block — bodies are then written exactly as before, unsearchable
	// but never lost, so authoring never depends on an embedder being reachable.
	documentTool := &builtin.Document{
		Bus:           channelBus,
		MaxAssetBytes: int(cfg.Env.MaxDocumentAssetBytes),
		Embedder:      embedder,
		Cfg:           cfg, // RFC CE: resolve document_sources: for set_remote / sync
	}
	b.allTools = append(b.allTools, documentTool)
	// RFC EP P2 — a Crypto `from` / `to` reference reads and writes Memory and
	// Document through these tools' own scope checks.
	cryptoTool.Memory = memoryTool
	cryptoTool.Docs = documentTool

	// RFC BE — the History tool (browse/search/annotate past chats). Store
	// late-bound below; the per-agent history_scope policy gates every op.
	// Cfg is read for one thing: which agents are declared `internal:`, so the
	// runtime's own maintenance sessions stay out of the chat listing.
	historyTool := &builtin.History{Cfg: cfg}
	b.allTools = append(b.allTools, historyTool)

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
	b.allTools = append(b.allTools, evaluationTool)

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
	b.allTools = append(b.allTools, interruptionTool)

	// RFC AP/BD — wire the TeamDef op=run board + interruption escalation deps
	// (both opt-in per run; Spawn + Admit are injected separately by
	// srv.SetTeamDefTool). Board persists a board-bound walk's position onto a
	// Document chunk (chunk.status = current state) + resumes from it; AskHuman
	// escalates an iteration cap to a human via the Interruption tool's `ask`
	// instead of aborting. A run that sets neither board_chunk_id nor
	// interrupt_on_cap is byte-identical to before. documentTool.SqlMem is
	// late-bound below (Board holds the *Document pointer, reading SqlMem at call
	// time), and AskHuman is invoked under the parent run's ctx so the block +
	// resolve + heartbeat all reuse the standard Interruption machinery.
	teamDefTool.Board = documentTool
	teamDefTool.AskHuman = func(ctx context.Context, question string) (string, error) {
		ask, _ := json.Marshal(map[string]any{"op": "ask", "question": question})
		res, err := interruptionTool.Execute(ctx, ask)
		if err != nil {
			return "", err
		}
		if res.IsError {
			// timeout / cancelled / not-enabled-for-agent / no-run-id — surface as
			// an error so the cap escalation fails safe to abort.
			return "", errors.New(res.Text)
		}
		var parsed struct {
			Answer string `json:"answer"`
		}
		_ = json.Unmarshal([]byte(res.Text), &parsed)
		return parsed.Answer, nil
	}

	// Context tool (v0.8.7). Read-only runtime introspection. The Tools
	// field is back-filled with the FULL allTools slice after every
	// other registration (MCP + localapi) so doc/tools ops reflect the
	// complete catalog. Including Context itself in the catalog is
	// intentional — agents introspecting "what tools do I have" should
	// see Context, and `doc(name="Context")` should work. Cfg + Store
	// power the v0.8.7 PR 2 substrate-coupled ops (agents / lineage /
	// evaluations); Store is late-bound below alongside the other
	// substrate tools.
	contextTool := &builtin.Context{
		Cfg:  cfg,
		Help: helpSet,
		// Already resolved at main() entry (ldflags → runtime/debug → "unknown"),
		// so `op=self` reports exactly what `--version` does.
		Build: builtin.BuildInfo{Version: buildVersion, Commit: buildCommit, Time: buildTime},
	}
	b.allTools = append(b.allTools, contextTool)

	b.searchRegistry = searchRegistry
	b.searchResolver = searchResolver
	b.searchHostKeys = searchHostKeys
	b.skillSet = skillSet
	b.helpSet = helpSet
	b.embedder = embedder
	b.bashboxTool = bashboxTool
	b.bashTool = bashTool
	b.skillTool = skillTool
	b.memoryTool = memoryTool
	b.channelBus = channelBus
	b.channelScheduler = channelScheduler
	b.channelTool = channelTool
	b.agentDefTool = agentDefTool
	b.skillDefTool = skillDefTool
	b.teamDefTool = teamDefTool
	b.volumeDefTool = volumeDefTool
	b.credentialDefTool = credentialDefTool
	b.cryptoTool = cryptoTool
	b.pathTool = pathTool
	b.documentTool = documentTool
	b.historyTool = historyTool
	b.evaluationTool = evaluationTool
	b.interruptionTool = interruptionTool
	b.contextTool = contextTool
	b.staticHosts = staticHosts
}

// addLocalAPITools registers one tool per operation of the local_api
// OpenAPI spec, when one is configured.
func (b *boot) addLocalAPITools() {
	cfg := b.cfg

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
			b.allTools = append(b.allTools, laTools...)
		}
	}
}

// logToolPosture reports at boot which tools are disabled or widened, and
// which agents have no tools at all.
func (b *boot) logToolPosture() {
	cfg := b.cfg
	staticHosts := b.staticHosts
	searchRegistry := b.searchRegistry
	searchResolver := b.searchResolver

	if len(cfg.Volumes) == 0 {
		log.Printf("note: file tools (Read/Write/Edit/Glob/Grep/Bash) require a `volumes:` binding — none configured; agents with no volume binding have no filesystem access")
	}
	if len(staticHosts) == 0 && !cfg.Env.HTTPCallerAuthoritative {
		log.Printf("note: HTTP + WebFetch tools are registered but disabled — set LOOMCYCLE_HTTP_HOST_ALLOWLIST to enable (a comma-separated host list, or `*` for all public hosts; or LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE=1 to delegate the allowlist to the caller)")
	}
	for _, h := range staticHosts {
		if h == "*" {
			log.Printf("note: LOOMCYCLE_HTTP_HOST_ALLOWLIST=* — HTTP + WebFetch may reach ANY public host; the dial-time guard still blocks private/loopback/link-local/metadata IPs (add specific internal hosts via LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST)")
			break
		}
	}
	if cfg.Env.HTTPCallerAuthoritative {
		log.Printf("note: HTTP_CALLER_AUTHORITATIVE=1 — caller's allowed_hosts is the sole policy; operator's static list is fallback only")
	}
	if len(cfg.Env.HTTPPrivateHostAllowlist) > 0 {
		log.Printf("note: HTTP_PRIVATE_HOST_ALLOWLIST=%v — these hosts may resolve to private IPs (e.g. localhost callbacks)", cfg.Env.HTTPPrivateHostAllowlist)
	}
	if searchRegistry == nil {
		log.Printf("note: WebSearch tool is registered but disabled — configure search_providers (or set BRAVE_API_KEY for the default Brave provider)")
	} else {
		log.Printf("search: %d provider(s) configured; priority %v", len(searchRegistry.IDs()), searchResolver.Cascade(nil))
	}
	if cfg.Env.BashEnabled {
		log.Printf("WARNING: Bash tool is enabled (LOOMCYCLE_BASH_ENABLED=1). This is NOT a true sandbox — run loomcycle inside a container or VM if you expose this to untrusted prompts.")
		log.Printf("note: Bash requires a rw volume binding — it runs in the bound volume's root and refuses an agent with no rw volume")
	} else {
		log.Printf("note: Bash tool is registered but disabled — set LOOMCYCLE_BASH_ENABLED=1 to enable (NOT a true sandbox; see docs)")
	}
	if cfg.Env.BashboxEnabled {
		log.Printf("note: Bashbox tool is enabled (LOOMCYCLE_BASHBOX_ENABLED=1) — a TRUE in-process sandbox (gbash): no OS process, paths rooted at the volume, no network; read-only volumes are honored. gbash is alpha; gate per agent via tools.")
		if len(cfg.Env.BashboxFallbackCommands) > 0 {
			log.Printf("WARNING: Bashbox host-command fallback is enabled (LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS=%s). These commands ESCAPE the sandbox and run on the real host shell (with host filesystem + network) — only allowlist commands you trust, and only enable for trusted prompts. Fallback requires a read-write volume.", strings.Join(cfg.Env.BashboxFallbackCommands, ","))
		}
	} else {
		log.Printf("note: Bashbox tool is registered but disabled — set LOOMCYCLE_BASHBOX_ENABLED=1 to enable (in-process sandbox; see docs)")
	}

	// Per-agent tool policy is default-deny: an agent with no tools
	// in YAML gets ZERO tools at the dispatcher. Warn at startup so
	// operators don't discover this only when an agent inexplicably can't
	// do anything. We log per-agent rather than once so the operator sees
	// which definitions are affected.
	for name, def := range cfg.Agents {
		if len(def.Tools) == 0 {
			log.Printf("note: agent %q has no tools — it will see zero tools (intentional default-deny; add tools to its YAML to expose tools)", name)
		}
	}
}
