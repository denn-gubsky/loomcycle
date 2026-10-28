package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	webhookapi "github.com/denn-gubsky/loomcycle/internal/api/webhook"
	"github.com/denn-gubsky/loomcycle/internal/changesub"
	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/heartbeat"
	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/retention"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/usage"
)

// startBackground is boot phase 8, first part: the shared background
// context every later goroutine runs under (cancelled first at shutdown),
// the webhook sweeper and delivery-count flusher, and the heartbeat runner.
func (b *boot) startBackground() {
	heartbeatRunner := b.heartbeatRunner
	heartbeatSpecs := b.heartbeatSpecs
	webhookReceiver := b.webhookReceiver

	// Background goroutines (heartbeat sweeper + session-lock map GC)
	// share a single ctx tied to the signal handler below — graceful
	// shutdown cancels them alongside the HTTP server. We rely on the
	// goroutines to exit promptly on ctx.Done(); both are documented
	// to do so in their packages.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	b.addCleanup(bgCancel)
	if !b.bootAuthConfigured {
		go warnOpenMode(bgCtx, openModeWarnInterval, b.srv.AuthConfiguredErr, slog.Default())
	}

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
		slog.Info("system channels: starting heartbeat goroutines", "goroutines", len(heartbeatSpecs))
		heartbeatRunner.Start(bgCtx)
	}

	b.bgCtx = bgCtx
	b.bgCancel = bgCancel
	b.countFlusherDone = countFlusherDone
}

// startCluster wires multi-replica coordination when LOOMCYCLE_REPLICA_ID
// is set: the backplane, replica heartbeat, DB-backed user quotas, the
// cross-replica cancel / steer / turn-cancel coordinators, bus fanout, the
// replicas sweeper and the cluster session lock. advisoryLock stays nil in
// single-replica mode.
func (b *boot) startCluster() {
	cfg := b.cfg
	sem := b.sem
	storeIface := b.storeIface
	srv := b.srv
	bgCtx := b.bgCtx

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
			fatal("coord: REPLICA_ID set but store is not *storepostgres.Store (openStore guard bypassed?)")
		}
		bp, err := coord.NewPostgresBackplane(coord.PostgresBackplaneConfig{
			Pool:      pgStore.Pool(),
			DSN:       cfg.Storage.PgDSN,
			ReplicaID: cfg.Env.ReplicaID,
		})
		if err != nil {
			fatal("coord: backplane init failed", "err", err)
		}
		b.addCleanup(func() {
			if err := bp.Close(); err != nil {
				slog.Warn("coord: backplane close failed", "err", err)
			}
		})
		replicaStore := coord.NewReplicaStore(pgStore.Pool())
		hostname, _ := os.Hostname()
		hb := coord.NewHeartbeat(replicaStore, coord.HeartbeatConfig{
			ReplicaID: cfg.Env.ReplicaID,
			Hostname:  hostname,
			Version:   buildVersion,
		})
		// Joined before the pool closes (a cleanup, so it runs before the
		// store's): the heartbeat's shutdown DELETE of this replica's row must
		// land. A row it leaves behind ages into the replicas sweeper, which
		// reads this replica as dead (RFC ES F1).
		hbDone := make(chan struct{})
		go func() {
			defer close(hbDone)
			hb.Run(bgCtx)
		}()
		bgCancel := b.bgCancel
		b.addCleanup(func() {
			bgCancel() // a no-op after the shutdown; ends the heartbeat on any other exit
			select {
			case <-hbDone:
			case <-time.After(heartbeatJoinTimeout):
				slog.Warn("coord: the heartbeat's shutdown DELETE did not finish in time", "timeout", heartbeatJoinTimeout)
			}
		})
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
			fatal("coord: cancel coordinator init failed", "err", err)
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
				fatal("coord: steer coordinator init failed", "err", err)
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
				fatal("coord: turn-cancel coordinator init failed", "err", err)
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
				fatal("coord: runstate bus subscribe failed", "err", err)
			}
		}
		if cb := srv.ChannelBus(); cb != nil {
			cb.SetBackplane(bp)
			if err := cb.SubscribeBackplane(bgCtx, bp); err != nil {
				fatal("coord: channel bus subscribe failed", "err", err)
			}
		}
		// RFC L Decision 11: flush the local auth-token cache when a peer
		// replica creates/rotates/retires a token.
		if err := srv.SubscribeTokenInvalidations(bgCtx, bp); err != nil {
			fatal("coord: token-cache invalidation subscribe failed", "err", err)
		}
		// RFC EY: drop a tenant's compiled ACL sets when a peer replica
		// changed its rules, groups or bindings.
		if r := srv.ACLResolver(); r != nil {
			if err := r.Subscribe(bgCtx, bp); err != nil {
				fatal("coord: acl invalidation subscribe failed", "err", err)
			}
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

		// Message kept verbatim: examples/cluster/README.md quotes it.
		slog.Info(fmt.Sprintf("coord: cluster mode active — replica_id=%s heartbeat=30s backplane=postgres-listen-notify user_quotas=db-backed cancel_ack_timeout=%s bus_fanout=on singleton_sweepers=on session_lock=pg-advisory", cfg.Env.ReplicaID, ackTimeout),
			"replica_id", cfg.Env.ReplicaID, "cancel_ack_timeout", ackTimeout)
	}

	b.advisoryLock = advisoryLock
}

// startRunSweepers starts the sweepers that must see advisoryLock: the
// heartbeat sweeper, the channel-hook worker, the configured-run sweeper,
// and the usage and retention sweepers.
func (b *boot) startRunSweepers() {
	cfg := b.cfg
	channelScheduler := b.channelScheduler
	documentTool := b.documentTool
	interruptionTool := b.interruptionTool
	storeIface := b.storeIface
	srv := b.srv
	sqlMemMgr := b.sqlMemMgr
	bgCtx := b.bgCtx
	advisoryLock := b.advisoryLock

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
		slog.Warn("heartbeat: sweeper disabled (LOOMCYCLE_HEARTBEAT_SWEEPER=0 or no Store)")
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
		slog.Info("channel hooks: worker started", "owner", hookOwner, "concurrency", cfg.Env.ChannelHooksConcurrency,
			"per_channel", cfg.Env.ChannelHooksPerChannel, "max_wait", cfg.Env.ChannelHooksMaxWait)
		go srv.NewChannelHookWorker(hookOwner, channelScheduler, interruptionTool).Run(bgCtx)
	} else {
		for name, ch := range cfg.Channels {
			if ch.HasHooks() {
				slog.Warn("channel hooks: a channel declares hooks, but channel hooks are off (LOOMCYCLE_CHANNEL_HOOKS=1 turns them on); they are skipped and its messages are delivered unhooked", "channel", name)
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
		slog.Warn("usage: sweeper disabled (LOOMCYCLE_USAGE_SWEEPER=0 or no Store)")
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
		slog.Info("retention: sweeper disabled (LOOMCYCLE_RETENTION_ENABLED != 1 or no Store)")
	}
}

// seedServerState seeds the token-budget tracker and the throughput
// estimate from the ledger, and provisions the declared principals'
// identity documents in the background.
func (b *boot) seedServerState() {
	srv := b.srv
	bgCtx := b.bgCtx

	// RFC AW: seed the per-scope token-budget tracker from the RFC AV ledger
	// (month-to-date counters + the token_limits ceilings). Non-fatal — a
	// seeding fault leaves the counters empty (fail-open: budgets must never
	// block the runtime from starting). No-op when the server has no store.
	if err := srv.SeedLimits(bgCtx); err != nil {
		slog.Error("limits: seed failed (budgets start empty until next write)", "err", err)
	}
	// RFC DT: seed the per-model throughput estimate from the ledger's latest
	// timed calls. Non-fatal — an empty estimate learns again from live calls,
	// and nothing depends on it yet (measure-only).
	if err := srv.SeedThroughput(bgCtx); err != nil {
		slog.Warn("throughput: seed failed (estimate starts empty)", "err", err)
	}

	// Identity Documents for config-declared principals. They exist because an operator
	// wrote them into the yaml, so they are established before anybody authenticates —
	// which makes them the case lazy provisioning serves worst, since their templates
	// should be waiting rather than appearing on some later run. Backgrounded: a hand-
	// written principal list is small, but it is one store round-trip each and boot
	// should not wait on a convenience. Best-effort inside.
	go srv.ProvisionDeclaredPrincipalDocs(bgCtx)
}

// authGateLinkRetention is how long a used, revoked or expired invite or
// reset row is kept before the sweeper deletes it.
const authGateLinkRetention = 7 * 24 * time.Hour

// startStoreSweepers starts the periodic store sweepers: expired tokens,
// memory TTL, drained queue rows, the change feed and its push engine,
// dead-link GC, channels and interrupts.
func (b *boot) startStoreSweepers() {
	cfg := b.cfg
	storeIface := b.storeIface
	expiredTokenRetention := b.expiredTokenRetention
	sqlMemMgr := b.sqlMemMgr
	bgCtx := b.bgCtx
	advisoryLock := b.advisoryLock

	// Expired-token sweeper (the retention was resolved with the auth wiring
	// above). One replica per cluster per tick; the delete is idempotent.
	if expiredTokenRetention > 0 && storeIface != nil {
		go runAdvisoryGatedSweeper(bgCtx, time.Hour, advisoryLock, coord.LockKeyExpiredTokenSweeper, "expired-token",
			func(ctx context.Context) {
				n, err := storeIface.OperatorTokenDefDeleteExpired(ctx, time.Now().Add(-expiredTokenRetention))
				if err != nil {
					slog.Error("expired-token sweep: failed", "err", err)
					return
				}
				if n > 0 {
					slog.Info("expired-token sweep: deleted expired token rows", "rows", n)
				}
			})
	}

	// Web UI session sweeper (RFC FA §5): a revoked or expired session can no
	// longer authenticate, so its row only takes space. Hourly, one replica
	// per cluster per tick; the delete is idempotent.
	if storeIface != nil {
		go runAdvisoryGatedSweeper(bgCtx, time.Hour, advisoryLock, coord.LockKeyUISessionSweeper, "ui-session",
			func(ctx context.Context) {
				n, err := storeIface.UISessionDeleteExpired(ctx, time.Now())
				if err != nil {
					slog.Error("ui-session sweep: failed", "err", err)
					return
				}
				if n > 0 {
					slog.Info("ui-session sweep: deleted ended sessions", "rows", n)
				}
				// Started redirect logins (RFC FA P1): a used or expired one
				// can never be consumed again, so deleting it changes no
				// answer — a replayed callback finds nothing either way.
				m, err := storeIface.AuthLoginStateDeleteExpired(ctx, time.Now())
				if err != nil {
					slog.Error("ui-session sweep: deleting login states failed", "err", err)
					return
				}
				if m > 0 {
					slog.Info("ui-session sweep: deleted finished login states", "rows", m)
				}
				// App sign-ins (RFC FL): expired codes and ended refresh
				// families. An hour's margin keeps a tombstone (a family
				// revoked before its exchange created it) past any exchange
				// still running.
				o, err := storeIface.OAuthDeleteExpired(ctx, time.Now().Add(-time.Hour))
				if err != nil {
					slog.Error("ui-session sweep: deleting ended app sign-ins failed", "err", err)
					return
				}
				if o > 0 {
					slog.Info("ui-session sweep: deleted ended app sign-ins", "rows", o)
				}
			})
	}

	// Password gate sweeper (RFC FA P2): invites and resets that can no
	// longer be used, kept a week for the audit trail's sake, and throttle
	// counters whose window and block have both ended. The rows hold only
	// code hashes; deleting them changes no answer.
	if storeIface != nil {
		go runAdvisoryGatedSweeper(bgCtx, time.Hour, advisoryLock, coord.LockKeyAuthGateSweeper, "auth-gate",
			func(ctx context.Context) {
				now := time.Now()
				inv, err := storeIface.AuthInviteDeleteExpired(ctx, now.Add(-authGateLinkRetention))
				if err == nil {
					var rst int
					rst, err = storeIface.AuthResetDeleteExpired(ctx, now.Add(-authGateLinkRetention))
					inv += rst
				}
				var thr int
				if err == nil {
					thr, err = storeIface.AuthThrottleDeleteExpired(ctx, now)
				}
				// RFC FA P5: device logins live ten minutes; an expired
				// row only answers "expired" until it goes.
				if err == nil {
					var dev int
					dev, err = storeIface.DeviceAuthDeleteExpired(ctx, now)
					inv += dev
				}
				if err != nil {
					slog.Error("auth-gate sweep: failed", "err", err)
					return
				}
				if inv+thr > 0 {
					slog.Info("auth-gate sweep: deleted dead links and idle counters", "links", inv, "counters", thr)
				}
			})
	}

	// User removal policy (RFC FA §3.7c): disables users dormant past
	// auth.lifecycle.dormant_after and advances each removal by one stage
	// (unbind, then erase only when erase_after is set). Hourly, one
	// replica per cluster per tick.
	if storeIface != nil && b.srv != nil {
		go runAdvisoryGatedSweeper(bgCtx, time.Hour, advisoryLock, coord.LockKeyAuthLifecycleSweeper, "auth-lifecycle",
			func(ctx context.Context) { b.srv.AuthLifecycleSweep(ctx, time.Now()) })
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
					slog.Error("memory sweep: failed", "err", err)
					return
				}
				if swept > 0 {
					slog.Info("memory sweep: deleted expired rows", "rows", swept)
				}
			})
		slog.Info("memory: sweeper enabled", "interval", cfg.Env.MemorySweepInterval)
	} else {
		slog.Warn("memory: sweeper disabled (LOOMCYCLE_MEMORY_SWEEP_MS=0 or no Store)")
	}

	// Drained consolidation-queue rows. An ack only stamps drained_at, so
	// without this the queue keeps every row ever enqueued, raw chat payload
	// included. Hourly and advisory-gated like the change-feed prune below.
	if ttl := cfg.Env.MemoryPendingDrainedTTL; ttl > 0 && storeIface != nil {
		go runAdvisoryGatedSweeper(bgCtx, time.Hour, advisoryLock, coord.LockKeyMemoryPendingSweeper, "memory_pending",
			func(ctx context.Context) {
				pruned, err := memory.PruneDrainedPending(ctx, storeIface, ttl, time.Now(), memory.PendingPruneBatch)
				if err != nil {
					slog.Error("memory_pending prune: failed", "err", err)
				}
				if pruned > 0 {
					slog.Info("memory_pending prune: deleted rows drained longer than the ttl", "rows", pruned, "ttl", ttl)
				}
			})
		slog.Info("memory: drained-queue prune enabled", "interval", time.Hour, "ttl", ttl)
	} else if storeIface != nil {
		slog.Warn("memory: drained-queue prune disabled (LOOMCYCLE_MEMORY_PENDING_DRAINED_TTL_MS=0)")
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
					slog.Error("memory_changes prune: failed", "err", err)
					return
				}
				if pruned > 0 {
					slog.Info("memory_changes prune: deleted rows older than the window", "rows", pruned, "window", window)
				}
			})
		slog.Info("memory: change-feed retention sweeper enabled", "interval", time.Hour, "window", window)
	}

	// RFC CD Part C (push) — deliver change events to operator-declared HTTP
	// callbacks. Only when the feed is enabled AND subscriptions are declared;
	// advisory-gated so ONE replica delivers per tick (the persisted cursor is
	// shared). Each subscription's callback is dialed through its own
	// SSRF-guarded client (private hosts blocked unless allow_private_host).
	if memoryChangesEnabled() && storeIface != nil && len(cfg.ChangeSubscriptions) > 0 {
		subs := resolveChangeSubscriptions(cfg)
		if len(subs) > 0 {
			deliverer := changesub.New(storeIface, resolveChangeSubSecret, slog.Default())
			interval := changeSubDeliveryInterval()
			go runAdvisoryGatedSweeper(bgCtx, interval, advisoryLock, coord.LockKeyChangeSubDelivery, "change_subscriptions",
				func(ctx context.Context) { deliverer.RunOnce(ctx, subs) })
			slog.Info("memory: change-subscription push engine enabled", "subscriptions", len(subs), "interval", interval)
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
					slog.Error("deadlink gc: list scopes failed", "err", err)
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
						slog.Error("deadlink gc: scope reconcile failed", "tenant", sk.Tenant, "scope", sk.Scope, "scope_id", sk.ScopeID, "err", err)
						continue
					}
					if !rep.Empty() {
						verb := "collected"
						if cfg.Env.DeadLinkGCDryRun {
							verb = "would collect"
						}
						slog.Info("deadlink gc: "+verb, "report", rep)
					}
				}
			})
		slog.Info("deadlink gc: enabled", "interval", cfg.Env.DeadLinkGCInterval, "dry_run", cfg.Env.DeadLinkGCDryRun)
	} else if storeIface != nil && sqlMemMgr == nil {
		slog.Info("deadlink gc: disabled (no SQL Memory — no chunk tables to reconcile)")
	} else {
		slog.Warn("deadlink gc: disabled (LOOMCYCLE_DEADLINK_GC_MS=0 or no Store)")
	}

	// Channel tool TTL sweeper (v0.8.4). Same shape as MemorySweeper.
	if cfg.Env.ChannelsSweepInterval > 0 && storeIface != nil {
		go runAdvisoryGatedSweeper(bgCtx, cfg.Env.ChannelsSweepInterval, advisoryLock, coord.LockKeyChannelsSweeper, "channels",
			func(ctx context.Context) {
				swept, err := storeIface.ChannelSweepExpired(ctx)
				if err != nil {
					slog.Error("channels sweep: failed", "err", err)
					return
				}
				if swept > 0 {
					slog.Info("channels sweep: deleted expired rows", "rows", swept)
				}
			})
		slog.Info("channels: sweeper enabled", "interval", cfg.Env.ChannelsSweepInterval)
	} else if storeIface != nil {
		slog.Warn("channels: sweeper disabled (LOOMCYCLE_CHANNELS_SWEEP_MS=0)")
	}

	// v0.8.16 Interruption tool TTL sweeper. Marks pending rows
	// whose expires_at < now as timed_out.
	if cfg.Env.ChannelsSweepInterval > 0 && storeIface != nil {
		go runAdvisoryGatedSweeper(bgCtx, cfg.Env.ChannelsSweepInterval, advisoryLock, coord.LockKeyInterruptsSweeper, "interrupts",
			func(ctx context.Context) {
				swept, err := storeIface.InterruptSweepExpired(ctx)
				if err != nil {
					slog.Error("interrupts sweep: failed", "err", err)
					return
				}
				if swept > 0 {
					slog.Info("interrupts sweep: timed out expired rows", "rows", swept)
				}
			})
		slog.Info("interrupts: sweeper enabled", "interval", cfg.Env.ChannelsSweepInterval)
	}
}

// heartbeatJoinTimeout bounds the wait for the replica heartbeat's shutdown
// DELETE, which runs under its own 5 s timeout.
const heartbeatJoinTimeout = 6 * time.Second
