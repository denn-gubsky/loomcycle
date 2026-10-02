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

	// FireTimeout is the per-fire cap on the agent run. 0 → default
	// 10m. Reaching the cap cancels the run via ctx and records
	// last_status=failed last_error="run timeout".
	FireTimeout time.Duration

	// EnvAllowlist is the set of env var names a schedule can read
	// via user_credentials_from_env. The empty allowlist (default)
	// disables env-credential resolution entirely — a safe-by-default
	// posture. Operators opt in via LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST.
	EnvAllowlist map[string]bool

	// MaxConcurrentFires bounds the number of schedules a single tick
	// fires in parallel. 0 → default runtime.NumCPU()*4. Each fire is
	// a goroutine that calls runner.RunOnce synchronously; the tick
	// itself waits for the whole batch to drain before returning so
	// the "one tick at a time" invariant holds. Larger values trade
	// memory + concurrency-store pressure for tighter cascading at
	// burst-fire moments (e.g. cron crossings where 100s of forks
	// become due in the same second).
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
}

// defaults applies the documented defaults to a zero-value Config.
func (c Config) defaults() Config {
	if c.TickInterval == 0 {
		c.TickInterval = 30 * time.Second
	}
	if c.FireTimeout == 0 {
		c.FireTimeout = 10 * time.Minute
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
// process; in cluster mode (v0.12+) each replica runs its own
// Scheduler and per-def advisory locks coordinate which fires.
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

	wg     sync.WaitGroup
	stopCh chan struct{}
	once   sync.Once

	// inFlight tracks def_ids whose fire goroutine is currently
	// running (between slot-acquire and RecordResult). Used to
	// suppress double-fire when a fire takes longer than the tick
	// interval and the next tick still sees the same row as due
	// (because RecordResult hasn't advanced next_run_at yet).
	//
	// Surfaced by the compound test at scale=30000 where every
	// schedule fired twice — every fire's RecordResult write was
	// slower than the 100ms tick under heavy concurrent load, so
	// each row stayed "due" for the next tick. The in-memory
	// tracker is single-replica-only; cluster-mode advisory locks
	// (v0.12+) would cover the cross-replica case symmetrically.
	//
	// Entry lifecycle:
	//   tick():
	//     LoadOrStore(def_id, _) before slot-acquire — skip if loaded
	//   fire goroutine (deferred):
	//     Delete(def_id) after fireOne returns or panics
	//
	// A goroutine that hangs leaks the entry until the fireCtx
	// timeout cancels the RunOnce call (default 10m), which is the
	// existing budget. No separate TTL needed.
	inFlight sync.Map
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
}

// run is the sweeper main loop. Single goroutine — no concurrency
// concerns inside this function.
func (s *Scheduler) run(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cfg.TickInterval)
	defer ticker.Stop()

	s.logf("scheduler: started (tick=%s, fire_timeout=%s)", s.cfg.TickInterval, s.cfg.FireTimeout)
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
// Due rows fire in parallel up to cfg.MaxConcurrentFires goroutines.
// The tick waits for the whole batch to drain before returning so
// the "next tick won't start until this one finishes" invariant
// holds — important because each fire's RecordResult advances
// next_run_at; without the wait, the next tick could re-fire a row
// whose status update is still in flight. The bounded semaphore
// keeps memory + per-user-fairness pressure predictable when 100s
// of forks become due in one cron crossing.
func (s *Scheduler) tick(ctx context.Context) {
	if s.pause != nil && s.pause.State() != pause.StateRunning {
		// Paused / pausing — the runtime is quiesced for snapshot.
		// Skip without advancing next_run_at; next tick re-checks.
		return
	}
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
		// In-flight suppression: skip rows whose fire goroutine from
		// a previous tick is still running (RecordResult hasn't yet
		// advanced next_run_at, so the row would otherwise re-fire).
		// LoadOrStore is atomic so the racy "check then store" hole
		// is closed. See the inFlight field's commentary for the
		// lifecycle + why it solves the x30000 over-fire finding.
		if _, alreadyFiring := s.inFlight.LoadOrStore(row.DefID, time.Now()); alreadyFiring {
			continue
		}
		// Slot-acquire is ctx-aware so cancellation during a slow
		// tick doesn't block waiting for slots indefinitely.
		select {
		case <-ctx.Done():
			// We reserved the inFlight slot above but won't fire;
			// release it so the next tick can pick up this def
			// freely.
			s.inFlight.Delete(row.DefID)
			// Skip remaining rows; in-flight fires continue (their
			// own fireCtx still has a timeout). Wait below drains.
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(row store.ScheduleDueRow) {
			defer wg.Done()
			defer func() { <-sem }()
			// Always release the in-flight reservation, even on
			// panic. defer order is LIFO so this runs AFTER the
			// recover() below; that ordering means a panicking fire
			// still clears its in-flight slot before the next tick.
			defer s.inFlight.Delete(row.DefID)
			// Recover panics so one bad fire doesn't bring down
			// the sweeper goroutine. Log + advance with a parked
			// next_run_at so the def doesn't re-present every tick.
			defer func() {
				if r := recover(); r != nil {
					s.logf("scheduler: PANIC in fireOne(def_id=%s): %v", row.DefID, r)
					// Best-effort park — re-use the same 1h fallback
					// the cron-resolve failure path uses.
					if rerr := s.store.ScheduleRunStateRecordResult(context.Background(), store.ScheduleRunResult{
						DefID:      row.DefID,
						LastStatus: "failed",
						LastError:  "panic in fireOne",
						LastRunAt:  time.Now(),
						NextRunAt:  time.Now().Add(1 * time.Hour),
					}); rerr != nil {
						// exp7 I3: if the park write also fails the def re-fires
						// next tick — log so the panic→re-fire chain isn't silent.
						s.logf("scheduler: def %q panic-park record-result failed: %v", row.DefID, rerr)
					}
				}
			}()
			s.fireOne(ctx, row, now)
		}(row)
	}
	wg.Wait()
}

// fireOne handles one due schedule end-to-end: unmarshal the def,
// build RunInput, call runner.RunOnce, record result, advance
// next_run_at, dispatch on_complete hooks. Errors are logged but
// never bubble out — one failed schedule shouldn't block the rest
// of the tick.
func (s *Scheduler) fireOne(ctx context.Context, row store.ScheduleDueRow, now time.Time) {
	def, err := unmarshalDef(row.Definition)
	if err != nil {
		s.recordFireFailure(ctx, row.DefID, "", "decode_def", err, now)
		return
	}
	if rehomed, ok := rehomeToOwningTenant(row, def); ok {
		s.logf("scheduler: schedule %q (def %s) names no tenant in its definition but is owned by tenant %q — executing in %q; re-save it to record the tenant",
			row.Name, row.DefID, row.OwnerTenantID, row.OwnerTenantID)
		def = rehomed
	}
	if (def.Enabled != nil && !*def.Enabled) || def.CaptureDisabled != nil {
		// Skip-but-advance: the operator disabled this schedule via the
		// substrate (or the yaml template set enabled:false). Bump
		// next_run_at to keep listDue's bounded set from re-presenting
		// this row every tick.
		s.advanceOnly(ctx, row.DefID, def, "skipped_disabled", now)
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

	// RFC BL P2: a consolidation schedule dispatches one run per memory TARGET
	// with new work rather than one blanket run — the pass operates on exactly
	// one target, so "consolidate everything" is N runs. See consolidator.go;
	// it does its own result bookkeeping.
	if isConsolidationFanout(def) {
		s.fireConsolidationFanout(ctx, row, def, now)
		return
	}

	in := buildRunInput(def, s.cfg.EnvAllowlist, s.logf)

	// Cap the per-fire run time. The runner's ctx-cancellation cascades
	// down to provider calls + tool calls so timeout cleanly aborts.
	fireCtx, cancel := context.WithTimeout(ctx, s.cfg.FireTimeout)
	defer cancel()

	var registeredAgentID, registeredRunID string
	cb := runner.RunCallbacks{
		OnRegistered: func(agentID, runID, _, _ string) {
			registeredAgentID = agentID
			registeredRunID = runID
		},
	}
	runErr := s.runner.RunOnce(fireCtx, in, cb)
	status := "completed"
	errStr := ""
	// F36: every real fire counts toward max_fires (a wedged/always-failing
	// schedule still retires). The exceptions — an agent that cannot be
	// resolved (F38) and a paused runtime — never started a run; see
	// classifyFire, which the fan-out shares.
	countAsFire := true
	if runErr != nil {
		class := classifyFire(runErr)
		status = class.status()
		errStr = runErr.Error()
		countAsFire = class.countsAsFire()
		switch class {
		case fireUnknownAgent:
			s.logf("scheduler: schedule %q could not resolve agent %q in tenant %q — not counting toward max_fires; check the agent exists in this tenant (F38)",
				row.Name, def.Agent, def.TenantID)
		case firePaused:
			s.logf("scheduler: schedule %q was refused because the runtime paused after this tick began — not counting toward max_fires", row.Name)
		}
		if errors.Is(runErr, context.DeadlineExceeded) {
			// Disambiguate fireCtx (per-fire timeout) from parent ctx
			// (scheduler shutting down). Both surface as
			// context.DeadlineExceeded via errors.Is. Checking
			// fireCtx.Err() lets us emit a more accurate status.
			if fireCtx.Err() != nil && ctx.Err() == nil {
				status = "failed"
				errStr = "fire timeout exceeded"
			} else {
				// Parent ctx deadline (or both — treat as shutdown).
				status = "failed"
				errStr = "scheduler context deadline exceeded"
			}
		}
	}

	recordCtx, done := s.recordFireOutcome(ctx, row, def, now, fireOutcome{
		RunID:       registeredRunID,
		Status:      status,
		Err:         errStr,
		CountAsFire: countAsFire,
	})
	defer done()

	// Dispatch hooks only on success — RFC E says on_complete fires on
	// "successful runs." Failed/skipped runs don't notify. Use recordCtx (the
	// survival ctx) not the parent: a run that completes just as shutdown
	// begins still recorded its result above, so its on_complete hooks
	// (channel publish / memory set / mcp.call) must fire too rather than be
	// dropped on a cancelled parent ctx.
	if status == "completed" {
		s.dispatchHooks(recordCtx, row.Name, def, registeredRunID, registeredAgentID)
	}
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
	payload, err := json.Marshal(map[string]any{
		"schedule_name": scheduleName,
		"fired_at":      now.UTC().Format(time.RFC3339Nano),
		"delivery":      "channel",
		"payload":       def.Metadata,
	})
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

// recordFireOutcome advances next_run_at, records the result, and applies the
// max_fires lifetime cap.
//
// Returns the ctx it used plus a cleanup func. The ctx is a SURVIVAL ctx when
// the parent is already cancelled (mid-shutdown): without it the store write
// fails silently, next_run_at stays in the past, and the schedule re-fires
// immediately on the next startup. Callers with follow-on work — on_complete
// hooks — dispatch on the same ctx for the same reason, and must call the
// cleanup func when they are done with it.
func (s *Scheduler) recordFireOutcome(ctx context.Context, row store.ScheduleDueRow, def scheduleDef, now time.Time, out fireOutcome) (context.Context, func()) {
	next, nextErr := s.computeNext(def, now)
	if nextErr != nil {
		// Without a valid next_run_at, the sweeper would re-fire this
		// def every tick. Park it 1 hour in the future so the operator
		// gets a breathing window to fix the def before re-firing.
		s.logf("scheduler: schedule %q cron-resolve failed: %v — parking 1h", row.Name, nextErr)
		next = now.Add(1 * time.Hour)
	}
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
		NextRunAt:  next,
		// RFC S / F36: this IS a fire (any status counts toward the cap, so
		// a wedged/always-failing schedule still retires). The disabled-skip
		// advance (advanceOnly) leaves this false; F38 leaves it false for an
		// unresolved-agent config error.
		CountAsFire: out.CountAsFire,
	}); err != nil {
		s.logf("scheduler: record result for %q: %v", row.Name, err)
	}

	// RFC S / F36: auto-retire after the Nth fire. Re-read the just-
	// incremented fire_count (cheap, and only when a cap is set) and retire
	// the def once it reaches max_fires. Retired defs are skipped by the
	// due-query JOIN, so this is the last fire. Uses recordCtx so it still
	// runs mid-shutdown. Multi-replica: fire_count += 1 is atomic, so the
	// cap is exact single-replica and at most over-fired by the racing
	// replica count — acceptable for a lifetime bound.
	if def.MaxFires > 0 {
		if st, gerr := s.store.ScheduleRunStateGet(recordCtx, row.DefID); gerr != nil {
			s.logf("scheduler: max_fires read state for %q: %v", row.Name, gerr)
		} else if st.FireCount >= def.MaxFires {
			if rerr := s.store.ScheduleDefSetRetired(recordCtx, row.DefID, true); rerr != nil {
				s.logf("scheduler: max_fires retire %q (def %s) after %d fires: %v", row.Name, row.DefID, st.FireCount, rerr)
			} else {
				s.logf("scheduler: %q reached max_fires=%d — retired def %s", row.Name, def.MaxFires, row.DefID)
			}
		}
	}
	return recordCtx, done
}

// advanceOnly is the disabled-schedule path: bump next_run_at without
// recording a run.
func (s *Scheduler) advanceOnly(ctx context.Context, defID string, def scheduleDef, reason string, now time.Time) {
	next, err := s.computeNext(def, now)
	if err != nil {
		next = now.Add(1 * time.Hour)
	}
	// exp7 I3: a dropped result-write leaves next_run_at unadvanced, so the
	// def re-presents on every subsequent tick (a re-fire loop). A genuinely
	// dead store can't advance state at all — logging is the most this path
	// can do, but it makes the re-fire cause visible instead of silent.
	if err := s.store.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
		DefID:      defID,
		LastStatus: reason,
		LastRunAt:  now,
		NextRunAt:  next,
	}); err != nil {
		s.logf("scheduler: def %q advance-only (%s) record-result failed: %v", defID, reason, err)
	}
}

// recordFireFailure records an outcome when we never reached the
// runner (decode failure, etc.). Same advance-by-1h fallback if
// the def's cron can't be resolved.
func (s *Scheduler) recordFireFailure(ctx context.Context, defID, runID, status string, err error, now time.Time) {
	s.logf("scheduler: def %q fire-failed (%s): %v", defID, status, err)
	// exp7 I3: surface a dropped result-write — see advanceOnly above.
	if rerr := s.store.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
		DefID:      defID,
		LastRunID:  runID,
		LastStatus: status,
		LastError:  err.Error(),
		LastRunAt:  now,
		NextRunAt:  now.Add(1 * time.Hour),
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
