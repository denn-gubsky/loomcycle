package main

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/gitsource"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	toolsa2a "github.com/denn-gubsky/loomcycle/internal/tools/a2a"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/wiring"
)

// wireMemory is boot phase 6, first part: hand the open store to the
// Memory, Channel and def tools, and build the default memory backend, its
// access-count flusher and the rerank model.
func (b *boot) wireMemory() {
	cfg := b.cfg
	pr := b.pr
	embedder := b.embedder
	memoryTool := b.memoryTool
	channelTool := b.channelTool
	agentDefTool := b.agentDefTool
	skillDefTool := b.skillDefTool
	teamDefTool := b.teamDefTool
	volumeDefTool := b.volumeDefTool
	storeIface := b.storeIface

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
	// memory.go backend(ctx) via memoryTool.Cfg; this default assignment stays
	// and is what every unresolved / unknown-kind backend name degrades to.
	inProcBackend := inprocess.New(storeIface, embedder)
	// RFC BL hybrid retrieval (OQ #4): a per-replica batched access-count
	// flusher. Search records a +1 access for returned entries; the flusher
	// writes the accumulated deltas additively on a ticker + size trigger, so
	// the hot search path never blocks on the write. Stopped (with a final
	// flush) in the graceful-shutdown block below.
	accessFlusher := memory.NewAccessFlusher(storeIface, 0, 0) // 0,0 → package defaults
	accessFlusher.Start()
	inProcBackend.SetAccessFlusher(accessFlusher)
	memoryTool.Backend = inProcBackend
	// The search rerank's model (memory.reranker) — nil when none is declared, and
	// then an agent that enables memory_rerank reports not_configured. A declared
	// one that cannot be built fails boot, as the embedder does. Only a non-nil
	// model is handed on: a typed-nil in the interface fields would be called.
	rerankModel, rrErr := wiring.Reranker(cfg, pr.sched)
	if rrErr != nil {
		log.Fatalf("reranker: %v", rrErr)
	}
	if rerankModel != nil {
		log.Printf("reranker: %s/%s (kind %s)", rerankModel.ProviderID(), rerankModel.ModelID(), rerankModel.Kind())
		inProcBackend.SetRanker(rerankModel)
		memoryTool.Reranker = rerankModel
		// Which kinds of memory it may reorder. Validated at config load, so the parse
		// cannot fail here; unset is nil, which every reader takes as documents only.
		rerankSources, _ := memory.ParseSources(cfg.Memory.Reranker.Sources)
		inProcBackend.SetRerankSources(rerankSources)
		memoryTool.RerankSources = rerankSources
		log.Printf("reranker: reorders %v", cfg.Memory.Reranker.EffectiveSources())
	}
	channelTool.Store = storeIface
	// Wire the pool-stats accessor when the backend is Postgres so
	// the Channel tool's subscribe-race diagnostic log can correlate
	// retry fires with pool exhaustion. SQLite stores and test builds
	// leave it nil; the diagnostic log path then reports zeros for
	// the pool fields and the retry behavior is unchanged. The
	// PoolStatsFn capture lives inside execSubscribe at the case
	// <-waker: arm — see internal/tools/builtin/channel.go.
	if pg, ok := asPostgresStore(storeIface); ok && pg != nil {
		channelTool.PoolStatsFn = func() (total, acquired, idle int32) {
			st := pg.Pool().Stat()
			return st.TotalConns(), st.AcquiredConns(), st.IdleConns()
		}
	}
	agentDefTool.Store = storeIface
	skillDefTool.Store = storeIface
	teamDefTool.Store = storeIface
	volumeDefTool.Store = storeIface

	b.inProcBackend = inProcBackend
	b.accessFlusher = accessFlusher
	b.rerankModel = rerankModel
}

// wireCredentials builds the credential sealer and engine and every
// consumer of them: the KMS access, the GitHub App minter, git-source
// volumes, peer credentials, the $cred: header substitution, and the
// per-run provider-key resolution.
func (b *boot) wireCredentials() {
	cfg := b.cfg
	secrets := b.secrets
	bashboxTool := b.bashboxTool
	bashTool := b.bashTool
	memoryTool := b.memoryTool
	teamDefTool := b.teamDefTool
	volumeDefTool := b.volumeDefTool
	gitTool := b.gitTool
	credentialDefTool := b.credentialDefTool
	cryptoTool := b.cryptoTool
	documentTool := b.documentTool
	storeIface := b.storeIface

	// RFC AR — build the credential Sealer (per-tenant AES-GCM under the master
	// KEK) + Engine now that the store exists. A malformed LOOMCYCLE_SECRET_KEY
	// is fatal (fail-closed); an empty key leaves the inline backend disabled.
	credSealer, credErr := credential.NewSealer(cfg.Env.SecretKey, cfg.Env.SecretKeyPrevious)
	if credErr != nil {
		log.Fatalf("credential: invalid LOOMCYCLE_SECRET_KEY: %v", credErr)
	}
	credEngine := credential.NewEngine(storeIface, credSealer)
	credentialDefTool.Engine = credEngine
	cryptoTool.Engine = credEngine
	// RFC EP P3 — crypto keys held in an external KMS. A Vault address is the
	// key author's, so it is dialled through the SSRF guard (and must be on
	// the operator's host allowlist); AWS and Google endpoints are fixed.
	kmsAccess := &builtin.KMSAccess{
		Cfg:       cfg,
		HTTP:      netguard.NewGuardedClient(30*time.Second, cfg.Env.HTTPPrivateHostAllowlist),
		AllowHTTP: cfg.Env.KMSVaultAllowHTTP,
	}
	credentialDefTool.KMS = kmsAccess
	cryptoTool.KMS = kmsAccess
	// A team's own webhook may name a stored secret of the author's tenant.
	teamDefTool.Credentials = credEngine
	// $ghapp:<name> — mint a short-lived GitHub App installation token from a
	// stored App config (the private key never leaves loomcycle; only the ~1h,
	// repo-scoped token is substituted). Resolves the App config through the same
	// scope-precedence credential lookup as $cred:.
	ghAppMinter := credential.NewGitHubAppMinter(
		// For the URL the minted token is about to be sent to: an App config
		// restricted by allowed_urls mints only for a covered one.
		func(ctx context.Context, tenantID, agentName, userID, name, targetURL string) (string, bool, error) {
			res, found, err := credEngine.ResolveFor(ctx, tenantID, agentName, userID, name, targetURL)
			if err != nil || !found {
				return "", found, err
			}
			return res.Value, true, nil
		},
		// The token request goes to the config's base_url, which a tenant
		// writes. Through the guard, it cannot be pointed at a private or
		// metadata address unless the operator vouched for that host
		// (LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST, e.g. a GitHub Enterprise
		// server on the internal network).
		netguard.NewGuardedClient(15*time.Second, cfg.Env.HTTPPrivateHostAllowlist))
	// b.srv is assigned later, at lchttp.New; registerSecret reads it at call
	// time so a resolved value can be registered in the server's redactor.
	// registerSecret is only CALLED during a run (long after srv exists), so the
	// forward reference is safe (mirrors credSubstitute's own late binding).
	// registerSecret masks a resolved credential value against a downstream echo:
	// a $cred: value or a tenant/user provider-key that a tool later prints would
	// otherwise land in the persisted transcript unredacted (RFC AV/AR). The
	// server-global redactor dedups by value, so re-registering is cheap.
	registerSecret := func(v string) {
		if b.srv != nil {
			if rd := b.srv.Redactor(); rd != nil {
				rd.Register(v)
			}
		}
	}
	secrets.set(registerSecret)
	// A volume created from a git source (RFC EJ): off unless the operator
	// lists the hosts. The credential a clone names is resolved for the run,
	// and only when it is restricted to URLs covering the clone's.
	gitSource, gerr := gitSourceConfigFromEnv(os.Getenv, cfg.Env.HTTPPrivateHostAllowlist)
	if gerr != nil {
		log.Fatalf("git source: %v", gerr)
	}
	if gitSource != nil {
		gitSource.Credential = builtin.GitCredentialResolver(credEngine, ghAppMinter.TokenForRepository, registerSecret)
		volumeDefTool.Git = gitSource
		log.Printf("git source: enabled for %s (max %d MiB fetched, %d MiB per object, %d at once)",
			strings.Join(gitSource.Hosts, ", "), gitSource.MaxBytes>>20, gitSource.MaxObjectBytes>>20, cap(gitSource.Slots))
		// RFC FB: the Git tool's clone, fetch and pull share these hosts,
		// limits and slots; push has its own host list and a resolver whose
		// $ghapp: tokens may write.
		gitTool.Source = gitSource
		gitTool.PushCredential = builtin.GitPushCredentialResolver(credEngine, ghAppMinter.TokenForRepositoryWrite, registerSecret)
		if len(gitTool.PushHosts) > 0 {
			log.Printf("git: push enabled for %s (max %d MiB per push)", strings.Join(gitTool.PushHosts, ", "), gitTool.PushMaxBytes>>20)
		}
	} else if len(gitTool.PushHosts) > 0 {
		log.Printf("git: LOOMCYCLE_GIT_PUSH_HOSTS is set but LOOMCYCLE_GIT_SOURCE_HOSTS is not; the Git tool has no network, push included")
	}
	// go-git installs file, git, ssh and plain-http clients process-wide;
	// replace them with refusing ones now, before any repository is read.
	gitsource.InstallTransport()
	// A remote memory backend or document source may name its peer bearer as
	// api_key_env "$cred:<name>": a tenant-level credential resolved, through
	// this same engine, in the tenant the def was read in.
	peerCreds := builtin.NewPeerCredentials(credEngine, registerSecret)
	memoryTool.PeerCredentials = peerCreds
	documentTool.PeerCredentials = peerCreds
	// Wire the $cred: header resolver the MCP http pool captured by reference
	// in buildMCP. Identity comes from the per-request ctx (tenant + user + agent), so
	// a pooled client binds each request's own user-scoped token.
	b.credSubstitute = func(ctx context.Context, s, targetURL string) (string, []string, error) {
		// Fast-path: when nothing can resolve (no KEK / no external backend) skip
		// the store reads. Report any $cred: / $ghapp: refs as unresolved so the
		// caller still drops them (never send a literal token downstream).
		if !credEngine.CanResolve() {
			return s, append(credential.RefNames(s), credential.GHAppRefNames(s)...), nil
		}
		// $ghapp: (mint an installation token) then $cred: (durable secret). The two
		// syntaxes don't overlap; each drops its own header on an unresolved/failed
		// ref, and a mint/resolve error aborts so no literal token is sent.
		return substituteForRun(ctx, s, targetURL, registerSecret, ghAppMinter, credEngine)
	}
	// RFC AR: resolve a tenant/user credential by env-var NAME (ANTHROPIC_API_KEY,
	// BRAVE_API_KEY, …) for the run's identity, so a tenant's own key overrides
	// the operator host key (scope agent>user>tenant). Wired onto the Server
	// below; provider drivers + WebSearch consult it via the run ctx. Reused by
	// every transport since gRPC routes through the http Server. A miss (or a
	// store/KEK fault) falls back to the host key; an EXPIRED override fails
	// the call instead — see credentialBinding.
	credBinding := credentialBinding{engine: credEngine, register: registerSecret}
	credResolver := providers.CredentialResolver(credBinding.providerKey)
	// RFC BD: let a software team's Bashbox host-command fallback (git/gh) inject
	// PER-TENANT credentials named in LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS,
	// resolved for the run's own identity (agent>user>tenant) — so a tenant's own
	// GITHUB_TOKEN reaches git/gh instead of one shared operator host token. Only
	// the operator-allowlisted names are injected; the value goes solely into the
	// host child's env (registerSecret masks any transcript echo). A miss keeps
	// the host-env fallback; an expired credential leaves the variable unset.
	bashboxTool.CredResolve = credBinding.envVar("bashbox")
	// Raw Bash mirrors the Bashbox fallback: inject PER-TENANT credentials named
	// in LOOMCYCLE_BASH_ALLOWED_CREDS into the child's env, resolved for the run's
	// own identity (agent>user>tenant) — so a tenant's own GITHUB_TOKEN reaches
	// the command instead of one shared operator host var. Same miss / expired
	// rule as the Bashbox fallback.
	bashTool.CredResolve = credBinding.envVar("bash")
	// RFC AX Layer 1: a metadata-only keyability probe — does (tenant, agent,
	// user) have its OWN usable CredentialDef for env-var `name`? Reuses
	// Engine.Resolve's agent>user>tenant precedence but NEVER decrypts, so
	// building a restricted run's keyable-provider set costs at most three store
	// metadata reads and touches no plaintext. Wired onto the Server beside
	// credResolver; consulted in the server BEFORE the lock-free resolver.
	credKeyable := credBinding.keyable

	b.credSealer = credSealer
	b.credEngine = credEngine
	b.peerCreds = peerCreds
	b.credResolver = credResolver
	b.credKeyable = credKeyable
}

// bindStoreToTools hands the store (and the shared memory backend and
// embedder) to the remaining tools.
func (b *boot) bindStoreToTools() {
	embedder := b.embedder
	skillTool := b.skillTool
	pathTool := b.pathTool
	documentTool := b.documentTool
	historyTool := b.historyTool
	evaluationTool := b.evaluationTool
	interruptionTool := b.interruptionTool
	contextTool := b.contextTool
	mcpServerDefTool := b.mcpServerDefTool
	storeIface := b.storeIface
	inProcBackend := b.inProcBackend

	pathTool.Store = storeIface
	documentTool.Store = storeIface
	// Document op=search runs the SAME backend instance as the Memory tool, so a
	// document search and a memory search over the same chunks rank them alike.
	documentTool.Backend = inProcBackend
	historyTool.Store = storeIface
	// RFC BE op=related — the SAME embedder the Memory tool uses; nil (no
	// embedder configured) makes op=related refuse cleanly, every other op works.
	historyTool.Embedder = embedder
	skillTool.Store = storeIface
	evaluationTool.Store = storeIface
	contextTool.Store = storeIface
	// RFC BL P1: the SAME embedder the Memory tool uses powers the help op's
	// `query` search mode; nil (no embedder configured) makes query degrade to
	// a substring scan over the in-memory help set.
	contextTool.Embedder = embedder
	interruptionTool.Store = storeIface
	mcpServerDefTool.Store = storeIface
}

// loadMCPRegistry loads the stored MCP server defs into the dynamic
// registry and audits their environment references.
func (b *boot) loadMCPRegistry() {
	cfg := b.cfg
	dynamicMCPRegistry := b.dynamicMCPRegistry
	mcpServerDefTool := b.mcpServerDefTool
	storeIface := b.storeIface

	// v0.9.x — load active mcp_server_defs rows into the in-process
	// registry so previous-deployment registrations survive a restart.
	// The pool's build callback consults this registry on first agent
	// call (after a tool-not-in-frozen-allTools fallthrough via the
	// existing v0.8.1 lazy resolver). Errors are logged + non-fatal —
	// a corrupted row blocks only its own server from being callable.
	// A snapshot restore runs the same function (SetMCPRegistryRefresh below).
	if _, err := builtin.RehydrateMCPRegistry(context.Background(), storeIface, cfg.MCPServers, dynamicMCPRegistry, log.Printf); err != nil {
		log.Printf("mcp_server_defs: list active failed at boot: %v (dynamic MCP servers will be empty until first registration)", err)
	} else if size := dynamicMCPRegistry.Size(); size > 0 {
		log.Printf("mcp_server_defs: loaded %d active registration(s) into pool registry", size)
	}
	// Read-only: name every stored version that reads the server environment
	// without being operator-authored. The pool refuses to dial those unless
	// LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV=0; each line says which.
	if warns, err := mcpServerDefTool.OperatorEnvWarnings(context.Background()); err != nil {
		log.Printf("mcp_server_defs: environment-reference audit: %v (continuing)", err)
	} else {
		for _, warn := range warns {
			log.Printf("mcp_server_defs: WARNING: %s", warn)
		}
	}
	// Read-only: name every active server a tenant registered at runtime
	// whose host is localhost or a private IP the operator has not vouched
	// for. Those dial through the egress guard and are refused at each call.
	if warns, err := mcpServerDefTool.PrivateHostWarnings(context.Background()); err != nil {
		log.Printf("mcp_server_defs: private-host audit: %v (continuing)", err)
	} else {
		for _, warn := range warns {
			log.Printf("mcp_server_defs: WARNING: %s", warn)
		}
	}
}

// addA2ATools registers the outbound A2A peer tools when A2A is enabled.
func (b *boot) addA2ATools() {
	cfg := b.cfg
	storeIface := b.storeIface

	// v1.x RFC G — outbound A2A: register one synthetic
	// `a2a__<peer>__<skill>` tool per (operator-registered peer,
	// expected_skill) pair, mirroring the static MCP registration above.
	// They land in allTools and are filtered per-agent by `tools`
	// exactly like `mcp__<server>__<tool>` tools. Gated behind the same
	// LOOMCYCLE_A2A_ENABLED master switch as the server surface: with A2A
	// disabled, no outbound tools are registered. The per-call resolver is
	// lookup.A2AAgent (yaml > active substrate def) so a substrate fork of
	// a registered peer is picked up without a restart.
	if cfg.Env.A2AServerEnabled {
		a2aResolve := func(ctx context.Context, name string) (config.A2AAgent, bool) {
			// RFC N: resolve the peer under the run's tenant (own → static →
			// shared ""). At boot enumeration the ctx carries no identity, so
			// this is "" (the shared/operator set); per-call Execute re-runs
			// it with the live run identity.
			return lookup.A2AAgent(ctx, storeIface, cfg, tools.RunIdentity(ctx).TenantID, name)
		}
		a2aTools := toolsa2a.RegisterTools(context.Background(), cfg, storeIface, a2aResolve, nil, log.Printf, toolsa2a.WithCredentialResolver(lateCredSubstitute(&b.credSubstitute)))
		if len(a2aTools) > 0 {
			b.allTools = append(b.allTools, a2aTools...)
			log.Printf("a2a: registered %d outbound peer tool(s)", len(a2aTools))
		}
	}
}
