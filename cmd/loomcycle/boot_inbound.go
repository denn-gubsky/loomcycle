package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"

	a2aapi "github.com/denn-gubsky/loomcycle/internal/api/a2a"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	webhookapi "github.com/denn-gubsky/loomcycle/internal/api/webhook"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// mountA2AServer mounts the A2A server surface when A2A is enabled. It must
// run before the server's mux is built (buildHTTPServer).
func (b *boot) mountA2AServer() {
	cfg := b.cfg
	channelBus := b.channelBus
	storeIface := b.storeIface
	srv := b.srv

	var err error
	// v1.x RFC G — A2A server surface (well-known AgentCard + REST /
	// JSON-RPC / gRPC binding mounts + multi-tenant routing). Default
	// OFF; New returns (nil, nil) when LOOMCYCLE_A2A_ENABLED != 1. The
	// *http.Server (srv) is BOTH the runner.Runner and the
	// connector.Connector the bridge needs, plus the run-table reader.
	// Mount must be registered BEFORE srv.Mux() is called below; gRPC
	// registration happens on the shared grpc.Server further down.
	var a2aServer *a2aapi.Server
	if cfg.Env.A2AServerEnabled {
		// Authenticate A2A peers through the SAME operator-token substrate the
		// HTTP/gRPC planes use — NOT just the legacy LOOMCYCLE_AUTH_TOKEN.
		// srv.ResolvePrincipal handles both operator tokens and the legacy
		// fallback (and correctly rejects a legacy secret HTTP has retired once
		// an admin token exists); srv.AuthConfigured is the shared open-mode
		// decision, evaluated per request. Previously this was gated on the
		// legacy token alone, so an operator-token-only deployment left A2A
		// unauthenticated (deps.Auth nil → anonymous-but-authenticated).
		// RFC AX: pass the deployment gate so the frontier derives each peer's
		// operator-key restriction from its own scopes (anti-bypass for A2A).
		a2aAuth := a2aapi.FrontierAuthenticator(srv.AuthConfigured, srv.ResolvePrincipal, cfg.Env.OperatorKeyRestriction)
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

	b.a2aServer = a2aServer
}

// mountWebhooks mounts the inbound-webhook receiver when webhooks are
// enabled, and materializes the static webhooks into the substrate. It
// must run before the server's mux is built (buildHTTPServer).
func (b *boot) mountWebhooks() {
	cfg := b.cfg
	storeIface := b.storeIface
	credEngine := b.credEngine
	srv := b.srv
	sysPublisher := b.sysPublisher
	runStateBus := b.runStateBus

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
	// The receiver, when webhooks are on: its delivery counters are flushed
	// by a goroutine started once bgCtx exists, below.
	var webhookReceiver *webhookapi.Receiver
	if cfg.Env.WebhooksEnabled && storeIface != nil && srv != nil {
		// The receiver's secret/credential allowlist is the union of the
		// explicit operator knobs (LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST +
		// LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST) and every secret env declared by a
		// STATIC webhook — so an operator-authored webhook resolves its own
		// secret without a separate allowlist entry (the F23 trap). A
		// LOOMCYCLE_*-named VERIFY secret resolves via the namespace auto-allow
		// in resolveSecret even when not seeded here.
		webhookAllowlist := webhookapi.BuildEnvAllowlist(cfg)
		rec := webhookapi.New(webhookapi.Deps{
			Store:                storeIface,
			Cfg:                  cfg,
			Runner:               srv,
			TeamWalks:            srv,
			TeamWebhooks:         srv.ReceiveTeamWebhooks(), // a team's own, while a walk of it runs here
			Publisher:            sysPublisher,
			RunStateBus:          runStateBus,
			EnvAllowlist:         webhookAllowlist,
			Credentials:          credEngine, // auth.secret: a stored verification secret
			AllowUnauthenticated: cfg.Env.WebhooksAllowUnauthenticated,
			Logger:               slog.Default(),
			Counts:               storeIface, // per-definition delivery counters (RFC EK §9)
		})
		webhookReceiver = rec
		srv.SetWebhookMux(func(reg lchttp.MuxRegistrar, adminAuth func(http.Handler) http.Handler) {
			// Receiver POST: unauthed (per-WebhookDef secret). Triage
			// endpoints (recent-deliveries / test): admin-bearer gated.
			rec.Mount(reg)
			rec.MountAdmin(reg, adminAuth)
		})
		log.Printf("webhooks: enabled (POST /v1/_webhooks/{name} + a team's own at POST /v1/_teams/{team}/webhooks/{name}, env_allowlist=%d names via LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST + LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST + static-declared secrets; LOOMCYCLE_* verify-secrets auto-allowed; unauthenticated_mode=%t)",
			len(webhookAllowlist), cfg.Env.WebhooksAllowUnauthenticated)
		// Surface every static webhook that will fail every delivery (inert,
		// or its secret won't resolve) so the operator sees WHY at boot rather
		// than discovering it via a 503 — the F23/F24 discoverability gap.
		for _, warn := range webhookapi.UnresolvableStaticSecrets(cfg, webhookAllowlist, os.Getenv) {
			log.Printf("webhooks: WARNING: %s", warn)
		}
		// Materialize static yaml `webhooks:` into the webhook_defs substrate
		// (F24) — parity with agents/skills/schedules, so a static webhook is
		// listable/forkable in the Library without a prior fork. Idempotent +
		// fork-respecting. A minimal tool instance (Store + Cfg) suffices.
		bootWH := &builtin.WebhookDef{Store: storeIface, Cfg: cfg}
		bootWHCtx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "webhook-bootstrap"})
		if n, err := bootWH.BootstrapStaticWebhooks(bootWHCtx); err != nil {
			log.Printf("webhooks: static-webhook bootstrap: %v (continuing)", err)
		} else if n > 0 {
			log.Printf("webhooks: materialized %d static webhook(s) into the substrate", n)
		}
		// Read-only: name any stored webhook whose runs execute outside its
		// owning tenant (a non-admin could author one before the guard).
		if warns, err := bootWH.ForeignExecTenantWarnings(bootWHCtx); err != nil {
			log.Printf("webhooks: execution-tenant audit: %v (continuing)", err)
		} else {
			for _, warn := range warns {
				log.Printf("webhooks: WARNING: %s", warn)
			}
		}
	} else if cfg.Env.WebhooksEnabled {
		log.Printf("webhooks: disabled (no Store backend or no HTTP server)")
	} else {
		log.Printf("webhooks: disabled (LOOMCYCLE_WEBHOOKS_ENABLED=0)")
	}

	b.webhookReceiver = webhookReceiver
}
