package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Config holds the operator-tunable knobs.
type Config struct {
	// TickInterval is how often the sweeper polls for due rows. 0 →
	// default 30s. Lower values trade DB query frequency for tighter
	// schedule punctuality. Most operators leave it at the default.
	TickInterval time.Duration

	// EnvAllowlist is the set of env var names a schedule can read
	// via user_credentials_from_env. The empty allowlist (default)
	// disables env-credential resolution entirely — a safe-by-default
	// posture. Operators opt in via LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST.
	EnvAllowlist map[string]bool

	// MaxConcurrentFires bounds the number of schedules a single tick
	// fires in parallel. 0 → default runtime.NumCPU()*4. A fire waits only
	// for its run to be admitted (the run itself carries on after the tick),
	// and the tick waits for its batch of fires. Larger values trade memory
	// + concurrency-store pressure for tighter cascading at burst-fire
	// moments (e.g. cron crossings where 100s of forks become due in the
	// same second).
	MaxConcurrentFires int

	// MaxConsolidationTargets bounds how many memory-consolidation targets ONE
	// fan-out tick dispatches (RFC BL P2). 0 → default 32. The per-target
	// watermark makes the fan-out resumable, so the cap defers work to the next
	// tick rather than dropping it — but a truncated tick is always logged,
	// because a silent truncation reads as "every target was covered".
	MaxConsolidationTargets int

	// MaxConsolidationConcurrency bounds PARALLEL consolidation children when
	// the resolved model is not local. 0 → default 4. A local model runtime is
	// always dispatched serially regardless of this value (one shared box).
	// Each child is a full LLM run, so this is deliberately small; the
	// per-provider concurrency cap is the real throttle downstream.
	//
	// A value set EXPLICITLY (> 0) also lifts the serial default for an
	// in-process (code-js / mock) consolidator — see dispatchSerially. That is
	// why defaults() leaves this field alone: filling in 4 there would make
	// "unset" indistinguishable from "the operator asked for 4". Read it via
	// consolidationConcurrency.
	MaxConsolidationConcurrency int

	// InternalAgents names the agents the operator declared `internal:` —
	// loomcycle's own maintenance plumbing. Their sessions are runtime
	// bookkeeping, so the consolidation fan-out neither treats them as evidence
	// of new work nor hands them to a pass to consolidate.
	//
	// Passed as a plain []string rather than the config: this package
	// deliberately takes no dependency on internal/config, and every other
	// operator knob here arrives the same way (see EnvAllowlist). main.go fills
	// it from Config.InternalAgentNames().
	//
	// Empty is the pre-feature behaviour: only the schedule's own agent is
	// excluded, which is what let the extractor children pile up.
	InternalAgents []string

	// OperatorKeyRestriction mirrors the deployment's operator-key gate
	// (LOOMCYCLE_OPERATOR_KEY_RESTRICTION). While it is on, an operator-layer
	// consolidation sweep's runs in OTHER tenants are restricted from the
	// operator's provider key — see runConsolidationTarget. A plain bool for the
	// same reason as InternalAgents: no config dependency here. The value is an
	// env var, which a config reload does not change.
	OperatorKeyRestriction bool

	// ReplicaID names this replica on the slots it claims
	// (schedule_run_state.claimed_by). "" outside cluster mode.
	ReplicaID string

	// fanoutSweepBudget is the time one consolidation fan-out sweep divides
	// among its memory targets (see targetBudget). Not operator-tunable and
	// not a cap on any scheduled run: the fan-out is a bounded maintenance
	// sweep and needs SOME budget to share fairly. 0 → defaultFanoutSweepBudget.
	// Tests shorten it.
	fanoutSweepBudget time.Duration
}

// defaultFanoutSweepBudget is a consolidation fan-out sweep's whole budget —
// the value the scheduler's old per-fire cap gave it.
const defaultFanoutSweepBudget = 10 * time.Minute

// defaults applies the documented defaults to a zero-value Config.
func (c Config) defaults() Config {
	if c.TickInterval == 0 {
		c.TickInterval = 30 * time.Second
	}
	if c.fanoutSweepBudget == 0 {
		c.fanoutSweepBudget = defaultFanoutSweepBudget
	}
	if c.MaxConcurrentFires == 0 {
		c.MaxConcurrentFires = runtime.NumCPU() * 4
	}
	if c.MaxConsolidationTargets <= 0 {
		c.MaxConsolidationTargets = defaultMaxFanoutTargets
	}
	return c
}

// consolidationConcurrency is the fan-out's parallel width, and whether the
// operator set it rather than inheriting the default.
func (c Config) consolidationConcurrency() (n int, explicit bool) {
	if c.MaxConsolidationConcurrency > 0 {
		return c.MaxConsolidationConcurrency, true
	}
	return defaultMaxFanoutConcurrency, false
}

// Scheduler is the sweeper runtime. One instance per loomcycle
// process; in cluster mode each replica may run its own. Every replica
// lists the same due rows, and the slot claim (ScheduleRunStateClaim, a
// compare-and-set on next_run_at) decides which one fires each slot.
//
// Construction is via New + Start. Stop the goroutine via
// (*Scheduler).Stop or by cancelling the ctx passed to Start.
type Scheduler struct {
	cfg     Config
	store   store.Store
	runner  runner.Runner
	pause   *pause.Manager
	mcp     MCPCaller
	chScope ChannelScopeResolver
	chWrite channels.Writer
	teams   runner.TeamWalkStarter
	logf    func(format string, args ...any)

	// Consolidation fan-out dependencies (RFC BL P2), both optional and both
	// wired by a setter before Start. fanoutLock is the cluster singleton gate
	// (nil = single-replica, run unguarded) and fanoutLockKeyFn derives its key
	// per SCHEDULE DEF so two consolidation schedules never collide;
	// providerResolver decides parallel-vs-serial dispatch (nil = resolve failed
	// = serial). See consolidator.go.
	fanoutLock       AdvisoryLocker
	fanoutLockKeyFn  func(defID string) int64
	providerResolver ProviderResolver

	// fanoutRotation is each fan-out def's tick counter, which picks the
	// tenant its next sweep starts with (see fairTargetOrder). In memory and
	// per replica on purpose: it only has to differ from tick to tick, and a
	// replica that wins the cluster lock advances its own counter, so the
	// start still moves. Starts at 0, so the first tick is in tenant order and
	// a test can predict every later one. One int per fan-out def ever fired.
	fanoutRotationMu sync.Mutex
	fanoutRotation   map[string]int

	// fanoutEscalated is, per fan-out def, the targets whose last pass was cut
	// by its SHARE of the fire budget (see targetBudget) rather than finishing.
	// Each goes first next tick with the whole remaining budget (see
	// notePassBudget). In memory and per replica for the same reason as
	// fanoutRotation: losing it costs one more cut pass, never correctness.
	// Bounded by the targets ever cut; an entry goes on a completed pass.
	fanoutEscalatedMu sync.Mutex
	fanoutEscalated   map[string]map[consolidationTarget]bool

	// reconcileLock gates the reconcile sweep to one replica per tick (nil =
	// single replica, unguarded). See SetReconcileCoordination.
	reconcileLock    AdvisoryLocker
	reconcileLockKey int64

	// fanoutRunning holds the consolidation fan-out defs whose sweep is
	// running on this replica. A sweep runs off the tick, so without it a
	// slot arriving mid-sweep would start a second sweep of the same def on
	// a single replica (across replicas, the fan-out lock does the same).
	fanoutRunning sync.Map

	// wg tracks the sweeper goroutine and sweeps the consolidation sweeps;
	// Stop waits for both (a sweep is bounded by its budget). runs tracks the
	// goroutines that run and finish scheduled runs. Stop does not wait for
	// those — a run may last hours — but tests do.
	wg     sync.WaitGroup
	sweeps sync.WaitGroup
	runs   sync.WaitGroup
	stopCh chan struct{}
	once   sync.Once
}

// DeclaredChannel is what the scheduler needs to know about a channel it is
// about to write to: where the message goes, and how long it may stay.
//
// Retention is here because a scheduler write is a CADENCE write. A tick every
// minute that carries neither a TTL nor a bounded-queue cap accumulates half a
// million rows a year on a channel the operator did declare limits for — the
// limits simply never reached the writer.
type DeclaredChannel struct {
	Scope       string // "global" | "user" | "agent"
	DefaultTTL  int    // seconds; 0 = no TTL
	MaxMessages int    // 0 = unbounded
}

// ChannelScopeResolver returns the DECLARED shape of a channel by name.
// ok=false when the channel is declared nowhere (static yaml + runtime
// substrate). Injected so a scheduler publish lands at the channel's declared
// scope instead of blindly under the run's user scope (F37 / RFC T), and
// honours the channel's declared retention. Satisfied by
// (*http.Server).ResolveChannelScope; nil leaves the legacy user-scope
// behavior untouched.
//
// tenantID is the schedule's own tenant: a sweep runs on a ctx with no
// identity, so the resolver cannot find the tenant's runtime channels from ctx.
type ChannelScopeResolver func(ctx context.Context, tenantID, channel string) (DeclaredChannel, bool)

// SetChannelScope wires the channel-scope resolver. Must be called before
// Start (the sweeper reads chScope when dispatching on_complete hooks). A
// no-op-safe setter rather than a New parameter so the many existing
// New(...) call sites stay unchanged.
func (s *Scheduler) SetChannelScope(r ChannelScopeResolver) {
	s.chScope = r
}

// SetChannelWriter wires the channel writer a schedule's channel tick and its
// on_complete channel.publish write through. The writer decides, from the
// channel's definition, whether the message is delivered or held — the
// scheduler only says where it goes and how long it lives. Must be called
// before Start; with none, a channel write fails.
func (s *Scheduler) SetChannelWriter(w channels.Writer) {
	s.chWrite = w
}

// SetTeamWalkStarter wires what a `delivery: team` tick starts its walk
// through. Must be called before Start; with none, a team tick fails.
func (s *Scheduler) SetTeamWalkStarter(w runner.TeamWalkStarter) {
	s.teams = w
}

// writeChannel writes one scheduler message through the channel writer.
func (s *Scheduler) writeChannel(ctx context.Context, req channels.WriteRequest) error {
	if s.chWrite == nil {
		return fmt.Errorf("channel write: no channel writer wired")
	}
	_, err := s.chWrite.Write(ctx, req)
	return err
}

// New constructs a Scheduler. All four runtime dependencies are
// required; mcp is optional (nil disables mcp.call hooks with a
// clear error on attempted dispatch). logf may be nil for silent
// operation (tests + small embeds).
func New(cfg Config, st store.Store, r runner.Runner, p *pause.Manager, mcp MCPCaller, logf func(string, ...any)) *Scheduler {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Scheduler{
		cfg:    cfg.defaults(),
		store:  st,
		runner: r,
		pause:  p,
		mcp:    mcp,
		logf:   logf,
		stopCh: make(chan struct{}),
	}
}

// Start launches the sweeper goroutine. Returns immediately; the
// goroutine runs until ctx is cancelled OR Stop is called. Safe to
// call only once per instance.
func (s *Scheduler) Start(ctx context.Context) {
	s.wg.Add(1)
	go s.run(ctx)
}

// Stop signals the sweeper to exit and blocks until the goroutine
// returns. Idempotent — calling Stop twice is safe but only the
// first call sends the signal.
func (s *Scheduler) Stop() {
	s.once.Do(func() { close(s.stopCh) })
	s.wg.Wait()
	s.sweeps.Wait()
}

// run is the sweeper main loop. Single goroutine — no concurrency
// concerns inside this function.
func (s *Scheduler) run(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cfg.TickInterval)
	defer ticker.Stop()

	s.logf("scheduler: started (tick=%s)", s.cfg.TickInterval)
	defer s.logf("scheduler: stopped")

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick processes one sweeper iteration. Skips when pause manager
// reports runtime != StateRunning (matches the v0.8.17 pause/resume
// composition rule from RFC E).
//
// Each tick first finishes tracked runs that ended without being finished
// (reconcile). Due rows then fire in parallel up to cfg.MaxConcurrentFires
// goroutines. Each fire first claims its slot (claimSlot), so a row listed
// by two ticks — or by two replicas — fires once. A fire returns once its
// run is admitted, so the batch drains in milliseconds however long the runs
// last. The bounded semaphore keeps memory + per-user-fairness pressure
// predictable when 100s of forks become due in one cron crossing.
func (s *Scheduler) tick(ctx context.Context) {
	if s.pause != nil && s.pause.State() != pause.StateRunning {
		// Paused / pausing — the runtime is quiesced for snapshot.
		// Skip without advancing next_run_at; next tick re-checks.
		return
	}
	s.reconcile(ctx)
	now := time.Now()
	due, err := s.store.ScheduleRunStateListDue(ctx, now)
	if err != nil {
		s.logf("scheduler: list due: %v", err)
		return
	}
	if len(due) == 0 {
		return
	}

	// Buffered semaphore caps concurrent fires. Each goroutine
	// acquires a slot before calling fireOne and releases on exit.
	// A nil semaphore (size <= 0) would mean unbounded parallelism;
	// the Config.defaults() floor ensures size is always positive.
	sem := make(chan struct{}, s.cfg.MaxConcurrentFires)
	var wg sync.WaitGroup
	for _, row := range due {
		// Slot-acquire is ctx-aware so cancellation during a slow
		// tick doesn't block waiting for slots indefinitely.
		select {
		case <-ctx.Done():
			// Skip remaining rows; their slots stay unclaimed, so the
			// next tick (or another replica) takes them. Wait below drains.
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(row store.ScheduleDueRow) {
			defer wg.Done()
			defer func() { <-sem }()
			// Recover panics so one bad fire doesn't bring down the
			// sweeper goroutine.
			defer func() {
				if r := recover(); r != nil {
					s.logf("scheduler: PANIC in fireOne(def_id=%s): %v", row.DefID, r)
					s.parkAfterPanic(row)
				}
			}()
			s.fireOne(ctx, row, now)
		}(row)
	}
	wg.Wait()
}

// fireOne handles one due schedule: unmarshal the def, claim its slot
// (which advances next_run_at), and start what the def delivers. A run is
// started and tracked; it is finished — outcome recorded, on_complete
// dispatched — when it ends (see tracked.go). Errors are logged but never
// bubble out — one failed schedule shouldn't block the rest of the tick.
func (s *Scheduler) fireOne(ctx context.Context, row store.ScheduleDueRow, now time.Time) {
	def, err := unmarshalDef(row.Definition)
	if err != nil {
		// No cron to compute the next slot from: park this one an hour out
		// so it does not re-present every tick.
		if s.claimSlot(ctx, row, now.Add(time.Hour), now) {
			s.recordFireFailure(ctx, row.DefID, "", "decode_def", err, now)
		}
		return
	}
	if rehomed, ok := rehomeToOwningTenant(row, def); ok {
		s.logf("scheduler: schedule %q (def %s) names no tenant in its definition but is owned by tenant %q — executing in %q; re-save it to record the tenant",
			row.Name, row.DefID, row.OwnerTenantID, row.OwnerTenantID)
		def = rehomed
	}

	// Claim the slot BEFORE firing. Taking it advances next_run_at, so the
	// row is no longer due however long the fire runs, and another tick or
	// replica that listed it too loses the claim and fires nothing.
	next, nextErr := s.computeNext(def, now)
	if nextErr != nil {
		// Without a valid next_run_at, the sweeper would re-fire this
		// def every tick. Park it 1 hour in the future so the operator
		// gets a breathing window to fix the def before re-firing.
		s.logf("scheduler: schedule %q cron-resolve failed: %v — parking 1h", row.Name, nextErr)
		next = now.Add(1 * time.Hour)
	}
	if !s.claimSlot(ctx, row, next, now) {
		return
	}

	if (def.Enabled != nil && !*def.Enabled) || def.CaptureDisabled != nil {
		// Skip: the operator disabled this schedule via the substrate (or
		// the yaml template set enabled:false). The claim already moved
		// next_run_at on, so the row stops re-presenting every tick.
		s.recordSkip(ctx, row.DefID, "skipped_disabled", now)
		return
	}
	if s.spentWhileRunning(ctx, row, def) {
		// max_fires is used up by runs that have not finished; the def is
		// retired when one does.
		return
	}

	// RFC CY: a channel tick is a fire with no run. Everything below — the
	// consolidation fan-out, RunInput, the runner, the fire timeout,
	// on_complete — presumes an agent, so delivery is decided FIRST.
	//
	// Order matters against the fan-out check in particular: that one keys off
	// a metadata flag, and on a channel tick `metadata` is opaque payload
	// rather than configuration. Deciding delivery first is what makes that
	// sentence true.
	if def.Delivery == "channel" {
		s.fireChannelDelivery(ctx, row, def, now)
		return
	}

	// A team tick starts a walk and no agent run of its own. Decided here for
	// the reason the channel tick is: the fan-out check below reads `metadata`
	// and the rest reads `agent`, neither of which a team tick has.
	if def.Delivery == "team" {
		s.fireTeamDelivery(ctx, row, def, now)
		return
	}

	// RFC BL P2: a consolidation schedule dispatches one run per memory TARGET
	// with new work rather than one blanket run — the pass operates on exactly
	// one target, so "consolidate everything" is N runs. See consolidator.go;
	// it does its own result bookkeeping.
	if isConsolidationFanout(def) {
		s.startConsolidationFanout(ctx, row, def, now)
		return
	}

	in := buildRunInput(def, s.cfg.EnvAllowlist, s.logf)
	// The slot's own key on the run: a second run for the same slot is
	// refused by the runs table before its loop starts, whatever went wrong
	// with the claim. It also names the slot a run was fired for.
	in.IdempotencyKey = slotRunKey(row.DefID, row.NextRunAt)
	s.fireRun(ctx, row, def, in, now)
}

// rehomeToOwningTenant returns def executing in its row's owning tenant when the
// body names no tenant but a real tenant owns the row, and ok=false otherwise.
//
// Every write path stamps the author's tenant into the body now, but a row a
// tenant wrote before that stamp existed has an empty body tenant — and the
// whole fire path (the run, the channel tick, the consolidation fan-out, the
// on_complete hooks) reads only the body. Left alone, tenant X's schedule
// would run and publish in the operator layer "" instead of X. Its owner is
// the tenant that wrote it, so that is where it runs. A row bootstrapped from
// the operator's yaml keeps its empty tenant: the yaml said "operator layer",
// whoever's fork materialised it.
func rehomeToOwningTenant(row store.ScheduleDueRow, def scheduleDef) (scheduleDef, bool) {
	if def.TenantID != "" || row.OwnerTenantID == "" || row.BootstrappedFromStatic {
		return def, false
	}
	def.TenantID = row.OwnerTenantID
	return def, true
}

// fireChannelDelivery is the RFC CY tick that publishes instead of running.
//
// WHY IT EXISTS. A workflow driven off a channel needs a clock. Reaching a
// channel on a cron used to mean burning an agent run whose only job was to
// publish — a model call, a run row, and a provider bill for a message the
// scheduler can write itself. This is the symmetric twin of the
// `delivery: channel` a Webhook already has: one is an external event landing
// on a channel, this one is time landing on a channel.
//
// The message carries what there is to say when no agent ran: which schedule
// fired, when, and the def's operator-authored metadata as the payload. A
// downstream reader keys off schedule_name the same way it keys off an
// on_complete hook's.
//
// Bookkeeping is the SAME as a run fire — next_run_at advances, max_fires
// counts, a failure is recorded as failed — because from the schedule's side a
// tick is a fire whatever it delivered. A publish failure counts too: the tick
// happened, and a channel that is undeclared or unreachable will fail the same
// way next time, so hiding it from the cap would let a broken schedule run
// forever.
func (s *Scheduler) fireChannelDelivery(ctx context.Context, row store.ScheduleDueRow, def scheduleDef, now time.Time) {
	status := "completed"
	errStr := ""
	if err := s.publishTick(ctx, row.Name, def, now); err != nil {
		status = "failed"
		errStr = err.Error()
		s.logf("scheduler: schedule %q channel delivery to %q failed: %v", row.Name, def.Channel, err)
	}
	_, done := s.recordFireOutcome(ctx, row, def, now, fireOutcome{
		Status:      status,
		Err:         errStr,
		CountAsFire: true,
	})
	done()
}

// publishTick writes one cadence message to the def's channel, at the
// channel's DECLARED scope (the same resolution an on_complete channel.publish
// hook uses, so a global channel's tick is visible to a global reader instead
// of buried under a user scope).
func (s *Scheduler) publishTick(ctx context.Context, scheduleName string, def scheduleDef, now time.Time) error {
	if def.Channel == "" {
		return fmt.Errorf("delivery=channel missing `channel`")
	}
	payload, err := TickPayload(scheduleName, now, def.Metadata)
	if err != nil {
		return fmt.Errorf("marshal tick: %w", err)
	}
	target, err := s.resolvePublishTarget(ctx, def.TenantID, def.Channel, def.UserID, def.Agent)
	if err != nil {
		return err
	}
	req := channels.WriteRequest{
		Channel: def.Channel,
		// RFC N: the owning tenant comes from the def, never from anywhere a
		// caller could influence.
		TenantID:    def.TenantID,
		Scope:       target.Scope,
		ScopeID:     target.ScopeID,
		Payload:     payload,
		PublishedBy: def.UserID,
		MaxMessages: target.MaxMessages,
	}
	if target.DefaultTTL > 0 {
		req.ExpiresAt = now.Add(time.Duration(target.DefaultTTL) * time.Second)
	}
	// A held channel stores the tick without delivering it — the writer
	// decides that from the channel's definition, so a cron tick cannot walk
	// past a breakpoint.
	return s.writeChannel(ctx, req)
}

// TickPayload is the message a channel tick publishes: which schedule fired,
// when, and what its author attached. A team's own schedules publish the same
// shape, so one reader handles both.
func TickPayload(scheduleName string, firedAt time.Time, payload any) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"schedule_name": scheduleName,
		"fired_at":      firedAt.UTC().Format(time.RFC3339Nano),
		"delivery":      "channel",
		"payload":       payload,
	})
}

// fireTeamDelivery is the tick that starts a team walk.
//
// The walk is started DETACHED, exactly as `TeamDef op=run mode=detach` starts
// one, so the tick is over as soon as the walk exists. The walk's run is
// tracked like any scheduled run: the schedule reads "running" with the walk's
// run as last_run_id, and records how the walk ended when it ends (the
// reconciler finishes it — nothing on this replica waits for a walk). There
// is no on_complete to dispatch: a team schedule refuses one.
//
// The walk runs as the DEFINITION says and as nothing else: its execution
// tenant (after the legacy re-home fireOne already applied), its user, and the
// restriction bits captured from its author — the fields a run tick puts in
// RunInput, here in TeamWalkInput. The team is looked up in that tenant only.
//
// A team that cannot start — missing, retired, a variable it does not declare,
// a value it refuses — is a mistake in the definition, not a fire: it is
// logged, recorded as the schedule's last error, and does not use up
// max_fires, as an agent that cannot be resolved does not. Every other outcome
// is classified the way a run's is (classifyFire).
func (s *Scheduler) fireTeamDelivery(ctx context.Context, row store.ScheduleDueRow, def scheduleDef, now time.Time) {
	out := fireOutcome{Status: "completed", CountAsFire: true}
	runID, err := s.startTeamWalk(ctx, def, slotRunKey(row.DefID, row.NextRunAt))
	if errors.Is(err, store.ErrDuplicateIdempotencyKey) {
		// This slot already has its walk. Whoever started it records it.
		s.logf("scheduler: schedule %q slot %s already has a walk — not starting it twice", row.Name, row.NextRunAt.UTC().Format(time.RFC3339Nano))
		return
	}
	if err == nil && s.trackRun(ctx, row, runID, now) {
		return
	}
	if err != nil {
		class := classifyFire(err)
		out.Status, out.Err, out.CountAsFire = class.status(), err.Error(), class.countsAsFire()
		switch class {
		case fireTeamNotStartable:
			s.logf("scheduler: schedule %q could not start team %q in tenant %q — not counting toward max_fires; fix the schedule or the team: %v",
				row.Name, def.Team, def.TenantID, err)
		case firePaused:
			s.logf("scheduler: schedule %q was refused because the runtime paused after this tick began — not counting toward max_fires", row.Name)
		default:
			s.logf("scheduler: schedule %q team delivery to %q failed: %v", row.Name, def.Team, err)
		}
	}
	out.RunID = runID
	_, done := s.recordFireOutcome(ctx, row, def, now, out)
	done()
}

// startTeamWalk asks the wired starter for one detached walk of the def's
// team, under the slot's key: a second walk for the same slot is refused.
func (s *Scheduler) startTeamWalk(ctx context.Context, def scheduleDef, slotKey string) (string, error) {
	if s.teams == nil {
		return "", fmt.Errorf("delivery=team: no team walk starter wired")
	}
	in := buildTeamWalkInput(def)
	in.IdempotencyKey = slotKey
	return s.teams.StartTeamWalk(ctx, in)
}

// fireOutcome is what one fire produced, whatever kind of fire it was. It
// exists so the bookkeeping below is written once: every fire has to advance
// next_run_at and count against max_fires, and a fire that skips either one
// re-presents on the next tick or never retires.
type fireOutcome struct {
	RunID       string
	Status      string
	Err         string
	CountAsFire bool
}

// claimSlot takes the row's slot: it moves next_run_at from the value the
// tick listed to next, if no one has moved it since. false means another
// tick or replica took the slot (or the store could not answer), and the
// caller must not fire. Every fire claims before it does anything, which is
// what makes a slot fire once across replicas and keeps a fire that outlasts
// its cadence from leaving its row due.
func (s *Scheduler) claimSlot(ctx context.Context, row store.ScheduleDueRow, next, now time.Time) bool {
	won, err := s.store.ScheduleRunStateClaim(ctx, store.ScheduleSlotClaim{
		DefID:     row.DefID,
		Slot:      row.NextRunAt,
		NextRunAt: next,
		ClaimedBy: s.cfg.ReplicaID,
		ClaimedAt: now,
	})
	if err != nil {
		// Not claimed is the safe reading: the slot stays due and is retried.
		s.logf("scheduler: schedule %q claim slot: %v", row.Name, err)
		return false
	}
	return won
}

// parkAfterPanic is the recovery for a fire that panicked. If the panic came
// before the claim, the row is still due and would panic again every tick, so
// its slot is claimed with next_run_at an hour out — the same park a cron
// that cannot be resolved gets. If the fire had already claimed, the claim
// here loses (next_run_at has moved) and only the failure is recorded.
func (s *Scheduler) parkAfterPanic(row store.ScheduleDueRow) {
	ctx := context.Background()
	now := time.Now()
	s.claimSlot(ctx, row, now.Add(time.Hour), now)
	if err := s.store.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
		DefID:      row.DefID,
		LastStatus: "failed",
		LastError:  "panic in fireOne",
		LastRunAt:  now,
	}); err != nil {
		// exp7 I3: a dropped write is otherwise silent.
		s.logf("scheduler: def %q panic record-result failed: %v", row.DefID, err)
	}
}

// slotRunKey is the idempotency key of the run fired for one slot of one
// schedule. The runs table holds a key once, so a second run for the same
// slot is refused before it starts.
func slotRunKey(defID string, slot time.Time) string {
	return fmt.Sprintf("sched:%s:%d", defID, slot.UnixMicro())
}

// recordFireOutcome records the result and applies the max_fires lifetime
// cap. next_run_at is not touched: the claim moved it before the fire.
//
// Returns the ctx it used plus a cleanup func. The ctx is a SURVIVAL ctx when
// the parent is already cancelled (mid-shutdown): without it the store write
// fails silently and the outcome is lost. Callers with follow-on work —
// on_complete hooks — dispatch on the same ctx for the same reason, and must
// call the cleanup func when they are done with it.
func (s *Scheduler) recordFireOutcome(ctx context.Context, row store.ScheduleDueRow, def scheduleDef, now time.Time, out fireOutcome) (context.Context, func()) {
	recordCtx := ctx
	done := func() {}
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		// Bounded 5s so the survival path can't hang shutdown.
		recordCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		done = cancel
	}
	if err := s.store.ScheduleRunStateRecordResult(recordCtx, store.ScheduleRunResult{
		DefID:      row.DefID,
		LastRunID:  out.RunID,
		LastStatus: out.Status,
		LastError:  out.Err,
		LastRunAt:  now,
		// RFC S / F36: this IS a fire (any status counts toward the cap, so
		// a wedged/always-failing schedule still retires). The disabled skip
		// (recordSkip) leaves this false; F38 leaves it false for an
		// unresolved-agent config error.
		CountAsFire: out.CountAsFire,
	}); err != nil {
		s.logf("scheduler: record result for %q: %v", row.Name, err)
	}

	// RFC S / F36: auto-retire after the Nth fire. Uses recordCtx so it still
	// runs mid-shutdown. Multi-replica: each slot is claimed by one replica
	// and fire_count += 1 is atomic, so the cap is exact.
	s.retireIfSpent(recordCtx, row.DefID, row.Name, def)
	return recordCtx, done
}

// recordSkip records a claimed slot that started nothing (a disabled
// schedule). It is not a fire, so it does not count toward max_fires.
func (s *Scheduler) recordSkip(ctx context.Context, defID, reason string, now time.Time) {
	// exp7 I3: a dropped result-write is otherwise silent. The claim already
	// moved next_run_at, so it cannot cause a re-fire loop, but the operator
	// should still see why last_status is stale.
	if err := s.store.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
		DefID:      defID,
		LastStatus: reason,
		LastRunAt:  now,
	}); err != nil {
		s.logf("scheduler: def %q skip (%s) record-result failed: %v", defID, reason, err)
	}
}

// recordFireFailure records an outcome when we never reached the
// runner (decode failure, etc.). The caller has claimed the slot.
func (s *Scheduler) recordFireFailure(ctx context.Context, defID, runID, status string, err error, now time.Time) {
	s.logf("scheduler: def %q fire-failed (%s): %v", defID, status, err)
	// exp7 I3: surface a dropped result-write — see recordSkip above.
	if rerr := s.store.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
		DefID:      defID,
		LastRunID:  runID,
		LastStatus: status,
		LastError:  err.Error(),
		LastRunAt:  now,
	}); rerr != nil {
		s.logf("scheduler: def %q record fire-failure result failed: %v", defID, rerr)
	}
}

// computeNext picks the cron from the def and returns the next-fire time
// strictly after `now`.
func (s *Scheduler) computeNext(def scheduleDef, now time.Time) (time.Time, error) {
	expr, err := ResolveCron(def.Schedule, def.UserTierSchedules, def.UserTier)
	if err != nil {
		return time.Time{}, err
	}
	return NextFireAfter(expr, def.Timezone, now)
}
