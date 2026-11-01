package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/codehook"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// wireSubstrateTools hands the server the substrate def tools that are not
// agent-available (MCPServerDef, ScheduleDef, HookDef, the A2A defs,
// WebhookDef, MemoryBackendDef, DocumentSourceDef) and the TeamDef tool.
func (b *boot) wireSubstrateTools() {
	cfg := b.cfg
	teamDefTool := b.teamDefTool
	interruptionTool := b.interruptionTool
	dynamicMCPRegistry := b.dynamicMCPRegistry
	mcpServerDefTool := b.mcpServerDefTool
	storeIface := b.storeIface
	credEngine := b.credEngine
	peerCreds := b.peerCreds
	srv := b.srv

	// v0.9.x — wire the MCPServerDef substrate tool. NOT in allTools
	// (operator-admin-only); reached via Connector.MCPServerDef + the
	// admin endpoint + the LoomCycle MCP meta-tool.
	srv.SetMCPServerDefTool(mcpServerDefTool)
	// A snapshot restore reloads the registry with boot's own function, so
	// the MCP server defs it restored go live without a restart.
	srv.SetMCPRegistryRefresh(func(ctx context.Context) (int, error) {
		return builtin.RehydrateMCPRegistry(ctx, storeIface, cfg.MCPServers, dynamicMCPRegistry, slog.Default())
	})
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
		Credentials:         credEngine, // user_bearer: a stored credential the author holds
	})
	// RFC AP Phase 2 — wire the TeamDef substrate tool (team-workflow graphs).
	// Tenant-confined at the route (ScopeTenant), but the same dedicated-slot
	// wiring as ScheduleDef; reached via Connector.TeamDef + POST /v1/_teamdef +
	// the LoomCycle MCP meta-tool `teamdef`. Its definition cap is
	// LOOMCYCLE_TEAM_DEF_MAX_DEFINITION_BYTES.
	// Same instance that's agent-available in allTools — SetTeamDefTool injects
	// Spawn + Admit (op=run) into it, and keeps the HTTP /v1/_teamdef + MCP
	// `teamdef` + Connector surfaces pointing at it.
	srv.SetTeamDefTool(teamDefTool)
	// RFC DK — code-js hook bodies, opt-in. A body's only tool is the same
	// Interruption instance agents use, run under the hook's own grant.
	// The HookDef substrate compiles a code-js body when it is saved, with the
	// same runner that will run it; without code hooks a code body is refused.
	hookDefTool := &builtin.HookDef{
		Cfg:                 cfg, // the static agents, for the no-trace gate
		Credentials:         peerCreds,
		Store:               storeIface,
		MaxDescriptionBytes: cfg.Env.AgentDefMaxDescriptionBytes,
	}
	if cfg.Env.CodeHooksEnabled {
		runner := codehook.New(interruptionTool)
		srv.SetCodeHookRunner(runner)
		hookDefTool.CompileCode = runner.Compile
	}
	srv.SetHookDefTool(hookDefTool)
	// A hook's webhook headers bind credentials the same way an MCP server's do:
	// resolved per call from the run's identity, the value never stored in the
	// definition that attaches the hook. Here, not beside credSubstitute: srv
	// does not exist until lchttp.New below that.
	srv.SetHookHeaderSubstitute(func(ctx context.Context, s, targetURL string) (string, []string, error) {
		return b.credSubstitute(ctx, s, targetURL)
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
		Credentials:         credEngine,
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
		PeerCredentials:     peerCreds,
		MaxDefinitionBytes:  cfg.Env.AgentDefMaxDefinitionBytes,
		MaxDescriptionBytes: cfg.Env.AgentDefMaxDescriptionBytes,
	})
	// RFC CE — wire the DocumentSourceDef substrate tool (runtime-authored peer
	// document sources). Same operator-admin-only posture as MemoryBackendDef;
	// reached via Connector.DocumentSourceDef + the admin endpoint + the
	// LoomCycle MCP meta-tool, and consumed by the Document tool's
	// set_remote/sync via lookup.DocumentSource.
	srv.SetDocumentSourceDefTool(&builtin.DocumentSourceDef{
		Store:               storeIface,
		Cfg:                 cfg,
		PeerCredentials:     peerCreds,
		MaxDefinitionBytes:  cfg.Env.AgentDefMaxDefinitionBytes,
		MaxDescriptionBytes: cfg.Env.AgentDefMaxDescriptionBytes,
	})

	b.hookDefTool = hookDefTool
}

// wireAuth wires the audit sinks, the OperatorTokenDef tool and the auth
// knobs read from the environment (token cache, user-token TTL, default
// run rate, strict limits, expired-token retention).
func (b *boot) wireAuth() {
	cfg := b.cfg
	agentDefTool := b.agentDefTool
	credentialDefTool := b.credentialDefTool
	cryptoTool := b.cryptoTool
	storeIface := b.storeIface
	srv := b.srv
	hookDefTool := b.hookDefTool

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
			fatal("audit: opening the audit log failed", "path", cfg.Env.AuditLogPath, "err", aerr)
		}
		tokenAudit = fs
		slog.Info("audit: OperatorTokenDef mutations are recorded", "path", cfg.Env.AuditLogPath)
		// RFC EP: every keyed Crypto op and every crypto-key change.
		cryptoTool.Audit = tokenAudit
		credentialDefTool.Audit = tokenAudit
		// RFC FB: every Git network operation.
		b.gitTool.Audit = tokenAudit
	}
	// Subject erasure shares the same sink — but NOT the same posture: with no audit
	// path configured, erasure REFUSES rather than proceeding unrecorded. It is the one
	// operation nothing can undo, so "we cannot say who did this" is a reason not to do
	// it. Wiring the sink even when it is a NopSink would defeat that, so this passes
	// nil when there is no file to write to.
	if cfg.Env.AuditLogPath != "" {
		srv.SetEraseAudit(tokenAudit)
		slog.Info("audit: subject erasures are recorded", "path", cfg.Env.AuditLogPath)
	} else {
		slog.Warn("audit: no LOOMCYCLE_AUDIT_LOG_PATH — subject erasure is DISABLED " +
			"(it will not delete without a durable record of who asked)")
	}
	// Run-tree deletes are recorded to the same file when there is one. With
	// none they still work and are logged: nil here, not the NopSink, so the
	// server knows nothing durable was written.
	if cfg.Env.AuditLogPath != "" {
		srv.SetRunDeleteAudit(tokenAudit)
		// Bulk statement rulings and their reverts (RFC FS): who ruled on
		// what, as a unit. Nil with no file, as above.
		srv.SetStatementReviewAudit(tokenAudit)
		// An operator changing a no-trace agent's mode, its hooks, or a
		// HookDef it runs. Nil with no file, as above: the process log line
		// is the record then.
		agentDefTool.Audit = tokenAudit
		hookDefTool.Audit = tokenAudit
		// A run's or an MCP session's lookup of another person or a group.
		b.identityTool.Audit = tokenAudit
	}
	graceSecs := 0
	if v := os.Getenv("LOOMCYCLE_OPERATOR_TOKEN_ROTATION_GRACE_SECONDS"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n >= 0 {
			graceSecs = n
		}
	}
	// A config principal whose tenant differs only by case from a minted token's
	// tenant would share its volume directory on a case-insensitive filesystem.
	// Config load already compared principals with each other; the token side
	// needs the store, so it is checked here, fatally, like a config error.
	if err := builtin.CheckPrincipalTenantsAgainstTokens(context.Background(), storeIface, cfg.PrincipalTenants()); err != nil {
		fatal("config: principals: checking their tenants against the operator tokens failed", "err", err)
	}
	// RFC ES D2: the effective token pepper — explicit, else derived from
	// LOOMCYCLE_SECRET_KEY, else none. One hasher for every mint and lookup.
	tokenHasher, err := cfg.Env.TokenHasher()
	if err != nil {
		fatal("auth: token pepper: deriving it failed", "err", err)
	}
	srv.SetTokenHasher(tokenHasher)
	switch tokenHasher.Source() {
	case auth.PepperNone:
		if srv.AuthConfigured(context.Background()) {
			slog.Warn("auth: operator tokens are hashed with no pepper — anyone who can write the database can mint a working token. Set LOOMCYCLE_SECRET_KEY (a pepper is derived from it) or LOOMCYCLE_OPERATOR_TOKEN_PEPPER.")
		}
	case auth.PepperDerived:
		// Before serving: report the token rows on an older form (RFC ES
		// F3/C2). Boot changes none of them; an unpeppered hash never
		// authenticates until `loomcycle tokens rewrap` wraps it.
		rep, rerr := srv.ReportTokenHashes(context.Background())
		if rerr != nil {
			slog.Warn("auth: reading the token table to report older token hashes failed", "err", rerr)
			break
		}
		logTokenHashes(rep, tokenHasher.Fingerprint())
	}
	srv.SetOperatorTokenDefTool(&builtin.OperatorTokenDef{
		Store:                  storeIface,
		Hasher:                 tokenHasher,
		Audit:                  tokenAudit,
		RotationGraceSeconds:   graceSecs,
		LegacyTokenSet:         srv.LegacyTokenSet,
		AdminPrincipalDeclared: srv.AdminPrincipalResolved,
		ConfigTenants:          cfg.PrincipalTenants(),
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
	// RFC EY: groups + ACL rule lists. The resolver is always wired; the
	// mode decides what a decision means (compat by default: the scope
	// gates decide and an explicit deny is honoured on top).
	aclMode, err := authz.ParseMode(cfg.Env.ACLMode)
	if err != nil {
		fatal("authz: invalid access control mode", "err", err)
	}
	aclResolver := aclres.New(storeIface, aclMode, time.Duration(cfg.Env.ACLCacheTTLSeconds)*time.Second)
	srv.SetACLResolver(aclResolver)
	// The Identity tool compiles another person's set (can, explain) and
	// reads the config-declared groups through the same resolver.
	b.identityTool.ACL = aclResolver
	slog.Info("authz: access control (LOOMCYCLE_ACL)", "mode", aclMode, "set_cache_ttl_seconds", cfg.Env.ACLCacheTTLSeconds, "live", cfg.Env.ACLLive)
	// Delegated user-token mints and revocations are recorded to the same
	// sink as operator-token mutations. Not wired when there is no file: the
	// handler treats a nil sink as "not recorded".
	if cfg.Env.AuditLogPath != "" {
		srv.SetTokenAudit(tokenAudit)
	}
	// A cap on how long a delegated user token may live. Unset = no cap; with
	// a cap, a mint that asks for no lifetime gets the cap.
	if v := os.Getenv("LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS"); v != "" {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n > 0 {
			srv.SetUserTokenMaxTTL(time.Duration(n) * time.Second)
			slog.Info("auth: user tokens have a maximum lifetime", "max_ttl_seconds", n)
		} else {
			fatal("auth: LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS is not a positive number of seconds", "value", v)
		}
	}
	// A default rate for run starts, for a caller whose token carries none.
	// Unset = no default. Operators (admin, tenant operator, the legacy token)
	// are never bound by the default.
	if v := os.Getenv("LOOMCYCLE_RUN_RATE_PER_MINUTE"); v != "" {
		rate := auth.RateLimit{}
		n, perr := strconv.Atoi(v)
		if perr != nil {
			fatal("auth: LOOMCYCLE_RUN_RATE_PER_MINUTE is not a number", "value", v)
		}
		rate.PerMinute = n
		if b := os.Getenv("LOOMCYCLE_RUN_RATE_BURST"); b != "" {
			if rate.Burst, perr = strconv.Atoi(b); perr != nil {
				fatal("auth: LOOMCYCLE_RUN_RATE_BURST is not a number", "value", b)
			}
		}
		if err := rate.Validate(); err != nil {
			fatal("auth: LOOMCYCLE_RUN_RATE_PER_MINUTE / _BURST is invalid", "err", err)
		}
		srv.SetDefaultRunRate(rate)
		slog.Info("auth: default run rate", "per_minute", rate.PerMinute, "burst", rate.EffectiveBurst())
	}
	// Strict token budgets: also check a hard budget before each model call
	// and each sub-agent start, so a run in flight stops when the budget is
	// gone. Unset keeps the check at run admission only. Any other value is a
	// mistake worth stopping for: a deployment that asked for enforcement and
	// misspelled it must not run without it.
	switch v := os.Getenv("LOOMCYCLE_LIMITS_ENFORCE"); v {
	case "":
	case "strict":
		srv.SetStrictLimits(true)
		slog.Info("limits: strict enforcement (a hard budget stops runs in flight and refuses sub-agents)")
	default:
		fatal("limits: LOOMCYCLE_LIMITS_ENFORCE is not a known value (strict, or unset)", "value", v)
	}
	// Expired token rows are removed once they have been dead for a while. A
	// deployment minting short-lived tokens daily would otherwise keep every
	// one of them. Only rows that carry an expiry are ever removed; a token
	// minted without one stays until an operator retires it, and stays after.
	expiredTokenRetention := 7 * 24 * time.Hour
	if v := os.Getenv("LOOMCYCLE_EXPIRED_TOKEN_RETENTION_SECONDS"); v != "" {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n >= 0 {
			expiredTokenRetention = time.Duration(n) * time.Second
		} else {
			fatal("auth: LOOMCYCLE_EXPIRED_TOKEN_RETENTION_SECONDS is not a number of seconds (0 keeps expired rows)", "value", v)
		}
	}

	b.expiredTokenRetention = expiredTokenRetention
}
