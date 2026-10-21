package main

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/metrics"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/scheduler"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// startPauseAndResume wires the pause manager (cluster-wide when
// clustered), fails the no-trace runs lost in the last stop, requeues the
// queued ones, and re-dispatches paused runs in the background.
func (b *boot) startPauseAndResume() {
	cfg := b.cfg
	storeIface := b.storeIface
	srv := b.srv
	bgCtx := b.bgCtx
	advisoryLock := b.advisoryLock

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
			pgStoreForPause, ok := asPostgresStore(storeIface)
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

	// F42 / RFC X Phase 2: re-dispatch any pause_state='paused' runs left in
	// the store at boot (crash recovery / restart on a restored data dir) by
	// reconstructing each run's loop from its transcript. One-shot, run after
	// the server is fully wired (providers/tools/cancel/steer/pause). In a
	// cluster, gate behind an advisory lock so exactly ONE replica resurrects
	// each run; single-replica runs it directly. The lock only orders booting
	// replicas: a run whose loop is still alive on another replica (parked at
	// a cluster pause) is left alone by ResumePausedRuns itself, which checks
	// the owner's heartbeat. Backgrounded so a long-lived resumed loop never
	// blocks the rest of boot.
	if storeIface != nil {
		// A no-trace run has nothing stored to resume from, so the ones this
		// process was running when it last stopped are lost. Say so on their
		// rows and release their idempotency keys, so a caller's retry runs
		// the work instead of being answered with a run that cannot finish.
		// Synchronous and first: nothing may resume or retry before it.
		if n, err := srv.FailLostNoTraceRuns(bgCtx, processStartedAt); err != nil {
			log.Printf("no-trace: could not fail the runs lost in the last stop: %v", err)
		} else if n > 0 {
			log.Printf("no-trace: failed %d run(s) lost in the last stop (not resumable); their idempotency keys are released", n)
		}
		// A run still queued for a slot when this process stopped lost its
		// wait with it. Each is queued again, in its old order, from the
		// request stored when it queued; one that cannot be (no request, or
		// sealed secrets this key cannot open) is failed with the reason and
		// its idempotency keys released.
		if n, failed, err := srv.RestartQueuedRuns(bgCtx, processStartedAt); err != nil {
			log.Printf("queue: could not restart the runs queued when loomcycle last stopped: %v", err)
		} else if n > 0 || failed > 0 {
			log.Printf("queue: %d run(s) queued when loomcycle last stopped are queued again; %d could not be and were failed", n, failed)
		}
		go func() {
			resume := func(ctx context.Context) error {
				n, warnings := srv.ResumePausedRuns(ctx)
				if n > 0 || len(warnings) > 0 {
					log.Printf("resume(boot): re-dispatched %d paused run(s); %d flagged", n, len(warnings))
				}
				return nil
			}
			if advisoryLock != nil {
				if _, err := advisoryLock.TryRun(bgCtx, coord.LockKeyResumePausedRuns, resume); err != nil {
					log.Printf("resume(boot): advisory-gated resume failed: %v", err)
				}
			} else {
				_ = resume(bgCtx)
			}
		}()
	}

	b.pauseMgrRef = pauseMgrRef
}

// startTeamSubscriptions starts the armed team-subscription sweep when
// LOOMCYCLE_TEAM_SUBSCRIPTIONS is on.
func (b *boot) startTeamSubscriptions() {
	cfg := b.cfg
	storeIface := b.storeIface
	srv := b.srv
	bgCtx := b.bgCtx
	advisoryLock := b.advisoryLock

	// RFC CY L5: the armed-subscription sweep. A PROMOTED team whose entry
	// state is a Starter runs on its own when its source channel has work.
	//
	// DEFAULT OFF, deliberately. This is the only part of the runtime that
	// starts agent runs unprompted — turning it on turns the definition plane
	// into a spend commitment, because every message on a subscribed source
	// becomes a wave of runs. An operator should arrive at that on purpose.
	//
	// The sweep needs no arm/disarm bookkeeping: each tick asks the store which
	// teams are promoted, so promote arms and retire/delete disarm with no
	// runtime state to leak or reconcile after a crash.
	if cfg.Env.TeamSubscriptions && srv != nil && storeIface != nil {
		if advisoryLock != nil {
			// Cluster singleton per TEAM (not per process): exactly one replica
			// drives a given team at a time, and the lock is held across the
			// whole walk so a second tick cannot start a concurrent one.
			srv.SetAdvisoryLock(advisoryLock)
		}
		tick := time.Duration(cfg.Env.TeamSubscriptionsTickSeconds) * time.Second
		go func() {
			t := time.NewTicker(tick)
			defer t.Stop()
			for {
				select {
				case <-bgCtx.Done():
					return
				case <-t.C:
					started, err := srv.SweepTeamSubscriptions(bgCtx)
					if err != nil {
						log.Printf("team-subscriptions: sweep: %v", err)
						continue
					}
					if started > 0 {
						log.Printf("team-subscriptions: started %d walk(s)", started)
					}
				}
			}
		}()
		log.Printf("team-subscriptions: enabled (tick=%s, cluster_gated=%v)", tick, advisoryLock != nil)
	}
}

// startBootReconciles starts the one-shot background checks: team-local
// agent-name clashes and the help-topic search index.
func (b *boot) startBootReconciles() {
	helpSet := b.helpSet
	embedder := b.embedder
	storeIface := b.storeIface
	srv := b.srv
	bgCtx := b.bgCtx
	advisoryLock := b.advisoryLock

	// RFC BL P1: reconcile the boot-time help-topic search index. The go:embed
	// help corpus is immutable in-process (no hot-reload), so this is one-shot at
	// boot. Content-hash gated — an unchanged corpus re-embeds nothing; only
	// changed/new sections are re-embedded and removed sections pruned. No-op
	// without an embedder or vector support (help search then degrades to an
	// in-memory substring scan). Backgrounded so a first-boot full embed never
	// blocks the rest of boot; advisory-gated in a cluster so exactly ONE replica
	// re-embeds. Non-fatal: on any error help search just degrades.
	// A static agent added since a team was written can share a name with one
	// of that team's own agents; say so at boot rather than at the first walk.
	// Backgrounded and bounded: it reads every active team, and how many there
	// are — and how many agents each declares — is the tenants' to decide, so
	// it must not be able to hold up boot.
	if storeIface != nil {
		go func() {
			ctx, cancel := context.WithTimeout(bgCtx, 30*time.Second)
			defer cancel()
			srv.LogTeamLocalNameClashes(ctx)
		}()
	}

	if storeIface != nil && helpSet != nil {
		go func() {
			reconcile := func(ctx context.Context) error {
				// Indexing the help docs is maintenance: behind live runs.
				stats, err := help.ReconcileIndex(lanegate.WithBackground(ctx), helpSet, storeIface, embedder)
				if err != nil {
					log.Printf("help index: reconcile failed (search degrades): %v", err)
					return nil // non-fatal — degrade, don't fail boot
				}
				switch {
				case stats.Degraded:
					log.Printf("help index: no embedder/vector support — query uses in-memory substring scan")
				case stats.Written > 0 || stats.Pruned > 0 || stats.Failed > 0:
					log.Printf("help index: reconciled (%d written, %d pruned, %d unchanged, %d failed)", stats.Written, stats.Pruned, stats.Unchanged, stats.Failed)
				default:
					log.Printf("help index: up to date (%d sections, 0 embed calls)", stats.Unchanged)
				}
				return nil
			}
			if advisoryLock != nil {
				if _, err := advisoryLock.TryRun(bgCtx, coord.LockKeyHelpIndexReconcile, reconcile); err != nil {
					log.Printf("help index: advisory-gated reconcile failed: %v", err)
				}
			} else {
				_ = reconcile(bgCtx)
			}
		}()
	}
}

// startMetrics starts the process-resource metrics sampler and its sweeper
// when metrics are enabled.
func (b *boot) startMetrics() {
	cfg := b.cfg
	sem := b.sem
	storeIface := b.storeIface
	srv := b.srv
	bgCtx := b.bgCtx
	advisoryLock := b.advisoryLock

	// v0.8.x process-resource metrics sampler. Default OFF;
	// operator opts in via LOOMCYCLE_METRICS_ENABLED=1. When
	// enabled, samples loomcycle's CPU + memory usage at
	// cfg.Env.MetricsSampleInterval cadence while at least one
	// agent run is active (the semaphore's Stats() is the idle
	// gate — no DB write, no /proc read while idle).
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
}

// startScheduler materializes the static schedules and starts the
// scheduler when it is enabled.
func (b *boot) startScheduler() {
	cfg := b.cfg
	storeIface := b.storeIface
	srv := b.srv
	sysPublisher := b.sysPublisher
	bgCtx := b.bgCtx
	advisoryLock := b.advisoryLock
	pauseMgrRef := b.pauseMgrRef

	// v1.x RFC E scheduler runtime. Default OFF; operator opts in via
	// LOOMCYCLE_SCHEDULER_ENABLED=1. Sweeper goroutine fires due
	// schedules from the substrate (schedule_run_state table). NIL
	// MCPCaller is passed because mcp.call hook dispatch is wired in
	// a follow-up PR — channel.publish + memory.set hooks work today.
	if cfg.Env.SchedulerEnabled && storeIface != nil && srv != nil {
		envAllowlist := cfg.SchedulerCredentialEnvAllowlist()
		schedCfg := scheduler.Config{
			TickInterval: time.Duration(cfg.Env.SchedulerTickSeconds) * time.Second,
			EnvAllowlist: envAllowlist,
			// RFC BL P2 consolidation fan-out caps. Zero here means "use the
			// scheduler's documented defaults" (Config.defaults()), so an operator
			// who sets neither env var still gets 32 targets / 4 concurrent.
			MaxConsolidationTargets:     cfg.Env.MaxConsolidationTargets,
			MaxConsolidationConcurrency: cfg.Env.MaxConsolidationConcurrency,
			// The agents declared `internal:` — the fan-out neither reads their
			// sessions as evidence of new work nor hands them to a pass. Passed
			// as names because internal/scheduler takes no config dependency.
			InternalAgents: cfg.InternalAgentNames(),
			// The same gate the runtime enforces: while it is on, an
			// operator-layer sweep's runs in other tenants may not spend the
			// operator's provider key.
			OperatorKeyRestriction: cfg.Env.OperatorKeyRestriction,
			// Names this replica on the slots it claims. Every replica may run
			// the scheduler: the slot claim fires each slot once.
			ReplicaID: cfg.Env.ReplicaID,
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
		// Read-only: name any stored schedule whose runs execute outside its
		// owning tenant (a non-admin could author one before the guard).
		if warns, err := bootSched.ForeignExecTenantWarnings(bootCtx); err != nil {
			log.Printf("scheduler: execution-tenant audit: %v (continuing)", err)
		} else {
			for _, warn := range warns {
				log.Printf("scheduler: WARNING: %s", warn)
			}
		}

		// srv satisfies runner.Runner via its RunOnce method (the same
		// seam HTTP + gRPC drive interactive runs through). pauseMgr is
		// guaranteed non-nil in this branch (storeIface != nil ⇒ pause
		// manager constructed earlier in this function).
		sched := scheduler.New(schedCfg, storeIface, srv, pauseMgrRef, nil, log.Printf)
		// Resolve on_complete: channel.publish to the channel's DECLARED scope
		// (F37 / RFC T) so a hook on a scope:global channel lands at global,
		// not under the run's user scope. Must be set before Start.
		// Adapt the server's channel def to the scheduler's DeclaredChannel:
		// the two packages meet here rather than importing each other for the
		// sake of one struct.
		sched.SetChannelScope(func(ctx context.Context, tenantID, name string) (scheduler.DeclaredChannel, bool) {
			def, ok := srv.ResolveChannelScope(ctx, tenantID, name)
			if !ok {
				return scheduler.DeclaredChannel{}, false
			}
			return scheduler.DeclaredChannel{
				Scope:       def.Scope,
				DefaultTTL:  def.DefaultTTL,
				MaxMessages: def.MaxMessages,
			}, true
		})
		// A schedule's channel writes go through the channel writer, which
		// decides from the channel's definition whether they are held.
		sched.SetChannelWriter(sysPublisher)
		// A `delivery: team` tick starts its walk through the server, which
		// owns the walk — the same instance that runs a tick's agent run.
		sched.SetTeamWalkStarter(srv)
		// RFC DZ: a concurrency_policy: replace slot cancels the schedule's
		// running run through the server, on whichever replica runs it.
		sched.SetRunCanceller(srv)
		// RFC BL P2 consolidation fan-out: the provider resolver decides
		// parallel-vs-serial dispatch (a local model runtime is serialized), and
		// the advisory lock makes exactly one replica per tick enumerate the
		// targets — otherwise every replica would dispatch a full fan-out and
		// burn N× the tokens. advisoryLock is nil in single-replica mode, which
		// leaves the fan-out correctly unguarded; guard the assignment so a
		// typed-nil never reaches the non-nil interface check.
		sched.SetProviderResolver(srv)
		if advisoryLock != nil {
			sched.SetFanoutCoordination(advisoryLock, coord.MemoryConsolidatorLockKey)
			// RFC DZ: one replica per tick finishes the scheduled runs whose
			// replica died. Only saves work — a run is finished once whoever
			// tries — so single-replica leaves it unguarded.
			sched.SetReconcileCoordination(advisoryLock, coord.LockKeyScheduleReconcile)
		}
		sched.Start(bgCtx)
		log.Printf("scheduler: enabled (tick=%ds, env_allowlist=%d names)",
			cfg.Env.SchedulerTickSeconds, len(cfg.Env.SchedulerEnvAllowlist))
	} else if cfg.Env.SchedulerEnabled {
		log.Printf("scheduler: disabled (no Store backend or no HTTP server)")
	} else {
		log.Printf("scheduler: disabled (LOOMCYCLE_SCHEDULER_ENABLED=0)")
	}
}

// logChannelSummary names the operator-declared channels at boot.
func (b *boot) logChannelSummary() {
	cfg := b.cfg

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
}

// startSessionGC starts the session-lock map GC and the resident
// sub-agent sweeper.
func (b *boot) startSessionGC() {
	cfg := b.cfg
	srv := b.srv
	bgCtx := b.bgCtx

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
	// RFC BK: idle-reap resident interactive sub-agents (per-replica; in-process
	// registry, no cluster coordination needed).
	go srv.RunResidentSweeper(bgCtx)

	b.sessionGCInterval = sessionGCInterval
	b.sessionGCMaxIdle = sessionGCMaxIdle
}

// startVolumeAndAgentSweepers starts the dynamic-agent TTL sweeper and the
// ephemeral-volume crash-recovery sweeper.
func (b *boot) startVolumeAndAgentSweepers() {
	cfg := b.cfg
	storeIface := b.storeIface
	bgCtx := b.bgCtx
	advisoryLock := b.advisoryLock

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

	// RFC AH Phase 2b ephemeral-volume crash-recovery sweeper. The inline
	// run-completion purge is the primary path; this backstops a host that
	// crashed before the inline purge ran. Each tick lists (root_run_id,
	// tenant_id) of TERMINAL-and-NOT-paused runs that still own ephemeral
	// rows, then runs the SAME fenced RemoveAll the inline purge uses +
	// deletes the rows. Cluster-gated so one replica per tick does the work.
	// Needs a dynamic_root to have anything to clean (no root → no ephemeral
	// volumes); skipped otherwise.
	if dynRoot, hasDynRoot := builtin.DynamicVolumeRoot(cfg); cfg.Env.EphemeralVolumeSweepInterval > 0 && storeIface != nil && hasDynRoot {
		go runAdvisoryGatedSweeper(bgCtx, cfg.Env.EphemeralVolumeSweepInterval, advisoryLock, coord.LockKeyEphemeralVolumeSweeper, "ephemeral_volumes",
			func(ctx context.Context) {
				cands, err := storeIface.EphemeralVolumeSweepCandidates(ctx)
				if err != nil {
					log.Printf("ephemeral_volumes sweep: candidates: %v", err)
					return
				}
				purged := 0
				for _, c := range cands {
					// Fenced RemoveAll of the per-run subtree (re-derived;
					// never a stored path). removed=false → already gone.
					if _, perr := builtin.PurgeEphemeralRunTree(dynRoot, c.RootRunID, "ephemeral sweeper"); perr != nil {
						log.Printf("ephemeral_volumes sweep: purge run=%s: %v", c.RootRunID, perr)
						// Still drop the rows below — a purge fault must not
						// wedge the row forever (the dir may be partly gone).
					}
					if _, derr := storeIface.EphemeralVolumeDeleteByRun(ctx, c.RootRunID); derr != nil {
						log.Printf("ephemeral_volumes sweep: delete rows run=%s: %v", c.RootRunID, derr)
						continue
					}
					purged++
				}
				if purged > 0 {
					log.Printf("ephemeral_volumes sweep: purged %d terminal run tree(s)", purged)
				}
			})
		log.Printf("ephemeral_volumes: sweeper enabled (interval=%s, dynamic_root=%s)", cfg.Env.EphemeralVolumeSweepInterval, dynRoot)
	}
}
