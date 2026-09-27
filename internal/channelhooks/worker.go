// Package channelhooks decides the messages of channels that carry hooks.
//
// A write to a hooked channel stores the message at the hook instant, where
// no reader can see it (see channels.StorePublisher.Write). The worker here
// claims those messages, runs the channel's channel_publish hooks on each one
// a hook at a time, and settles it: released (as it was, or with the body the
// hooks rewrote), or dropped.
//
// Durability. The message itself says whether it still awaits a decision —
// only the hook instant makes it invisible, and a release or a drop is a
// compare-and-set on that instant, so a message is settled once however many
// workers race for it. The progress row (which hook is next, the body so far,
// the attempts) only saves work: a lost row means the chain runs again, and
// hook authors are told a call may repeat.
//
// Leases, not a lock. Each claimed message carries a lease its worker renews
// while it works. A replica that dies simply stops renewing, and another
// claims the message when the lease runs out. An advisory lock would have
// pinned a database connection for as long as a hook took — and a hook may
// wait for a person.
//
// Order. Messages are claimed oldest first, but decided concurrently, so they
// may be delivered in a different order than they were published.
package channelhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// DecisionsChannel receives a record of every decision a channel hook makes,
// at tenant scope in the tenant whose definition carries the hook. An
// operator declares it (publisher: system) to read it.
const DecisionsChannel = "_system/channel_hooks/decisions"

// Def is what the worker needs from a channel's definition, read when it
// decides (not when the message was written): a hook added or removed since
// applies, and so does a hold.
type Def struct {
	Hold  bool
	Hooks hooks.EventHooks
}

// DefResolver resolves a channel's definition in the tenant whose definition
// carries its hooks. ok is false for a channel declared nowhere; an error is a
// fault reading it.
type DefResolver func(ctx context.Context, tenant, channel string) (def Def, ok bool, err error)

// Config wires a Worker.
type Config struct {
	Store      store.Store
	Dispatcher *hooks.Dispatcher
	// Lookup resolves the HookDefs a channel names.
	Lookup hooks.LookupDef
	Defs   DefResolver
	// Writer records decisions on DecisionsChannel; nil records none.
	Writer channels.Writer
	// Bus wakes the worker (channels.HookWakeKey) and, on a release, the
	// channel's readers. Scheduler wakes them at a release's deliver_at.
	Bus       *channels.Bus
	Scheduler *channels.Scheduler

	// Owner names this worker on its leases; unique per replica.
	Owner string
	// Concurrency bounds the messages this worker decides at once, PerChannel
	// those of one channel. Defaults 16 and 4.
	Concurrency int
	PerChannel  int
	// MaxWait bounds how long a message waits for a decision — through a hook
	// that keeps failing closed, or a hold nobody answers — before it is
	// dropped. A message's TTL, less a margin, bounds it too. Default 15m.
	MaxWait time.Duration
	// MaxBodyBytes caps a rewritten body, as a publish's payload is capped.
	// 0: no cap.
	MaxBodyBytes int
	// Lease is how long a claim holds without renewal (default 60s); Poll how
	// often the worker looks for work when nothing wakes it (default 5s).
	Lease time.Duration
	Poll  time.Duration
}

// Worker decides the messages of hooked channels. Run starts it.
type Worker struct {
	cfg Config
	sem chan struct{}

	mu    sync.Mutex
	perCh map[string]chan struct{}

	inFlight  atomic.Int64
	decisions sync.Map // kind -> *atomic.Int64
	freed     chan struct{}
	wg        sync.WaitGroup
	now       func() time.Time
}

// New returns a Worker with cfg's defaults filled in.
func New(cfg Config) *Worker {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 16
	}
	if cfg.PerChannel <= 0 {
		cfg.PerChannel = 4
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 15 * time.Minute
	}
	if cfg.Lease <= 0 {
		cfg.Lease = time.Minute
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 5 * time.Second
	}
	return &Worker{
		cfg:   cfg,
		sem:   make(chan struct{}, cfg.Concurrency),
		perCh: map[string]chan struct{}{},
		freed: make(chan struct{}, 1),
		now:   time.Now,
	}
}

// Stats is a snapshot of the worker's counters.
type Stats struct {
	InFlight  int64
	Decisions map[string]int64 // by kind: release, rewrite_body, drop, hold, unavailable
}

// Stats returns the worker's counters.
func (w *Worker) Stats() Stats {
	out := Stats{InFlight: w.inFlight.Load(), Decisions: map[string]int64{}}
	w.decisions.Range(func(k, v any) bool {
		out.Decisions[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

func (w *Worker) count(kind string) {
	v, _ := w.decisions.LoadOrStore(kind, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// Run claims and decides messages until ctx ends, then waits for the ones in
// hand. A message it was deciding when ctx ended keeps its lease until the
// lease runs out, and is then claimed again.
func (w *Worker) Run(ctx context.Context) {
	defer w.wg.Wait()
	for ctx.Err() == nil {
		// Registered before the claim, so a write between the claim and the
		// wait still wakes it.
		var wake chan struct{}
		if w.cfg.Bus != nil {
			wake = w.cfg.Bus.Register(channels.HookWakeKey)
		}
		full := w.claim(ctx)
		if full {
			// Every slot was filled: there may be more waiting.
			w.unregister(wake)
			continue
		}
		timer := time.NewTimer(w.cfg.Poll)
		select {
		case <-ctx.Done():
		case <-wake:
		case <-w.freed:
		case <-timer.C:
		}
		timer.Stop()
		w.unregister(wake)
	}
}

func (w *Worker) unregister(wake chan struct{}) {
	if wake != nil && w.cfg.Bus != nil {
		w.cfg.Bus.Unregister(channels.HookWakeKey, wake)
	}
}

// claim takes as many due messages as there are free slots and starts on
// each. It reports whether it filled every free slot.
func (w *Worker) claim(ctx context.Context) bool {
	free := cap(w.sem) - len(w.sem)
	if free <= 0 {
		return false
	}
	now := w.now()
	items, err := w.cfg.Store.ChannelHookClaim(ctx, w.cfg.Owner, now, now.Add(w.cfg.Lease), free)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("channelhooks: claim: %v", err)
		}
		return false
	}
	for _, it := range items {
		w.sem <- struct{}{}
		w.inFlight.Add(1)
		w.wg.Add(1)
		go func(it store.ChannelHookWork) {
			defer func() {
				w.inFlight.Add(-1)
				<-w.sem
				select {
				case w.freed <- struct{}{}:
				default:
				}
				w.wg.Done()
			}()
			w.process(ctx, it)
		}(it)
	}
	return len(items) == free
}

// channelSlot holds one of a channel's slots until release is called. A
// channel at its bound waits here; the global slot it already holds is the
// price of keeping claims simple, and claims are oldest first.
func (w *Worker) channelSlot(ctx context.Context, tenant, channel string) (release func(), ok bool) {
	key := tenant + "\x00" + channel
	w.mu.Lock()
	sem, found := w.perCh[key]
	if !found {
		sem = make(chan struct{}, w.cfg.PerChannel)
		w.perCh[key] = sem
	}
	w.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	case <-ctx.Done():
		return nil, false
	}
}

// job is one message being decided.
type job struct {
	w        *Worker
	msg      store.ChannelMessage
	key      store.ChannelMessageKey
	progress store.ChannelHookProgress
	deadline time.Time
}

func (w *Worker) process(ctx context.Context, it store.ChannelHookWork) {
	m := it.Message
	j := &job{
		w:        w,
		msg:      m,
		key:      store.ChannelMessageKey{TenantID: m.TenantID, Channel: m.Channel, Scope: m.Scope, ScopeID: m.ScopeID, ID: m.ID},
		progress: it.Progress,
		deadline: w.deadlineFor(m),
	}
	release, ok := w.channelSlot(ctx, m.HookTenant, m.Channel)
	if !ok {
		return
	}
	defer release()

	// Renew the lease while the chain runs. Losing it means another worker
	// has the message; stop, and let the compare-and-set settle who decides.
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go j.renew(jctx, cancel)
	j.decide(jctx)
}

// margin is how long before a message's expiry its decision is due, so a
// Starter's sink message is settled (as an error, at worst) before it can
// expire — an expired sink message would leave the downstream fan-in short.
func margin(ttl time.Duration) time.Duration {
	m := ttl / 10
	if m > time.Minute {
		m = time.Minute
	}
	if m < time.Second {
		m = time.Second
	}
	return m
}

// deadlineFor is when a message's decision is due at the latest.
func (w *Worker) deadlineFor(m store.ChannelMessage) time.Time {
	d := m.PublishedAt.Add(w.cfg.MaxWait)
	if !m.ExpiresAt.IsZero() {
		if e := m.ExpiresAt.Add(-margin(m.ExpiresAt.Sub(m.PublishedAt))); e.Before(d) {
			d = e
		}
	}
	return d
}

func (j *job) renew(ctx context.Context, lost func()) {
	t := time.NewTicker(j.w.cfg.Lease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ok, err := j.w.cfg.Store.ChannelHookRenew(ctx, j.key, j.w.cfg.Owner, j.w.now().Add(j.w.cfg.Lease))
			if err == nil && !ok {
				lost()
				return
			}
		}
	}
}

// heldMarker is the journal a hold leaves: the message waits, at that hook,
// for a person's decision until its deadline.
type heldMarker struct {
	HeldBy string `json:"held_by"`
	Reason string `json:"reason,omitempty"`
}

func (j *job) decide(ctx context.Context) {
	def, declared, err := j.w.cfg.Defs(ctx, j.msg.HookTenant, j.msg.Channel)
	var known *Def
	if err == nil {
		known = &def
	}
	var h heldMarker
	if len(j.progress.Journal) > 0 && json.Unmarshal(j.progress.Journal, &h) == nil && h.HeldBy != "" {
		// Held at a hook. Until a person can be asked, nobody can answer, so
		// the message waits out its deadline and is dropped.
		if j.w.now().Before(j.deadline) {
			j.save(ctx, j.progress.ChainPos, j.progress.Body, j.progress.Attempts, j.deadline, j.progress.LastError, j.progress.Journal)
			return
		}
		reason := "held by hook " + h.HeldBy + ", and nobody decided before the deadline"
		j.record(providers.HookDecisionInfo{Hook: h.HeldBy, Decision: hooks.ChannelDrop, Reason: reason})
		j.drop(ctx, known, reason, h.HeldBy)
		return
	}
	if err != nil {
		j.retry(ctx, nil, "the channel's definition could not be read", err)
		return
	}
	if !declared || len(def.Hooks[hooks.PhaseChannelPublish]) == 0 {
		// Its hooks were taken off since the message was written: nothing
		// is left to decide, so it is delivered as written.
		j.record(providers.HookDecisionInfo{Hook: hooks.ChannelOwner(j.msg.Channel), Decision: hooks.ChannelRelease,
			Reason: "the channel no longer carries hooks"})
		j.release(ctx, &def, j.body(), false)
		return
	}
	set := hooks.NewSet()
	src := hooks.Source{Owner: hooks.ChannelOwner(j.msg.Channel), Tenant: j.msg.HookTenant}
	if err := hooks.ResolveChannel(ctx, src, def.Hooks, j.w.cfg.Lookup, set); err != nil {
		// A gate the channel names must not vanish: whatever its fail mode,
		// a hook that cannot be found fails closed.
		j.retry(ctx, &def, "a hook the channel names could not be resolved", err)
		return
	}
	chain := set.List()
	pos, body, attempts := j.progress.ChainPos, j.body(), j.progress.Attempts
	changed := j.progress.Body != nil
	hctx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: j.msg.HookTenant, AgentID: hooks.ChannelOwner(j.msg.Channel)})
	for pos < len(chain) {
		h := chain[pos]
		name := h.Owner + "/" + h.Name
		res, err := j.w.cfg.Dispatcher.InvokeChannel(hctx, h, hooks.ChannelHookCall{
			Channel: j.msg.Channel, Scope: string(j.msg.Scope), ScopeID: j.msg.ScopeID,
			MessageID: j.msg.ID, PublishedAt: j.msg.PublishedAt, PublishedBy: j.msg.PublishedByUserID,
			Origin: j.msg.Origin, Attempt: attempts + 1, Body: body,
		})
		if err == nil && res.Decision == hooks.ChannelRelease && res.UpdatedBody != nil {
			res.UpdatedBody, err = j.checkBody(res.UpdatedBody)
		}
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down, or the lease was lost
			}
			fm := h.FailMode
			if fm != hooks.FailClosed {
				fm = hooks.FailOpen
			}
			j.record(providers.HookDecisionInfo{Hook: name, Decision: "unavailable", FailMode: string(fm), Reason: hooks.DecisionReason(err)})
			log.Printf("channelhooks: %s on %s message %s failed (fail_mode=%s): %v", name, j.msg.Channel, j.msg.ID, fm, err)
			if fm == hooks.FailOpen {
				pos, attempts = pos+1, 0
				continue
			}
			j.retryAt(ctx, &def, pos, body, attempts+1, hooks.DecisionReason(err), name)
			return
		}
		switch res.Decision {
		case hooks.ChannelDrop:
			reason := res.Reason
			if reason == "" {
				reason = "dropped by hook " + name
			}
			j.record(providers.HookDecisionInfo{Hook: name, Decision: hooks.ChannelDrop, Reason: reason})
			j.drop(ctx, &def, reason, name)
			return
		case hooks.ChannelHold:
			j.record(providers.HookDecisionInfo{Hook: name, Decision: hooks.ChannelHold, Reason: res.Reason})
			marker, _ := json.Marshal(heldMarker{HeldBy: name, Reason: res.Reason})
			j.save(ctx, pos, bodyOrNil(body, changed), attempts, j.deadline, "", marker)
			return
		default:
			j.record(providers.HookDecisionInfo{Hook: name, Decision: res.Kind(), Reason: res.Reason})
			if res.UpdatedBody != nil {
				body, changed = res.UpdatedBody, true
			}
			pos, attempts = pos+1, 0
			if pos < len(chain) {
				// Saved between hooks, so a restart resumes at the next one.
				j.save(ctx, pos, bodyOrNil(body, changed), 0, time.Time{}, "", nil)
			}
		}
	}
	j.release(ctx, &def, body, changed)
}

func bodyOrNil(body json.RawMessage, changed bool) json.RawMessage {
	if !changed {
		return nil
	}
	return body
}

// body is the message as the chain has left it so far.
func (j *job) body() json.RawMessage {
	if j.progress.Body != nil {
		return j.progress.Body
	}
	return j.msg.Payload
}

// sinkKeys are the fields a downstream Starter counts a sink message by; a
// hook's rewrite keeps them as the runtime wrote them.
var sinkKeys = []string{"wave", "wave_size", "index", "agent", "run_id"}

// checkBody refuses a rewritten body over the size cap, and keeps a Starter
// sink message's envelope: a rewrite may change what a run produced, never
// which run it was.
func (j *job) checkBody(body json.RawMessage) (json.RawMessage, error) {
	if j.w.cfg.MaxBodyBytes > 0 && len(body) > j.w.cfg.MaxBodyBytes {
		return nil, fmt.Errorf("updated_body is %d bytes; the channel takes at most %d", len(body), j.w.cfg.MaxBodyBytes)
	}
	if j.msg.Origin != channels.OriginStarterSink {
		return body, nil
	}
	var orig, upd map[string]json.RawMessage
	if err := json.Unmarshal(j.msg.Payload, &orig); err != nil {
		return nil, fmt.Errorf("the sink message is not an object: %w", err)
	}
	if err := json.Unmarshal(body, &upd); err != nil || upd == nil {
		return nil, errors.New("updated_body of a Starter's result must be an object")
	}
	for _, k := range sinkKeys {
		if v, ok := orig[k]; ok {
			upd[k] = v
		} else {
			delete(upd, k)
		}
	}
	return json.Marshal(upd)
}

// retry is a failure before any hook ran (the definition, or a reference):
// back off and try the whole chain again, until the deadline.
func (j *job) retry(ctx context.Context, def *Def, what string, err error) {
	if ctx.Err() != nil {
		return
	}
	log.Printf("channelhooks: %s message %s: %s: %v", j.msg.Channel, j.msg.ID, what, err)
	j.retryAt(ctx, def, j.progress.ChainPos, bodyOrNil(j.body(), j.progress.Body != nil), j.progress.Attempts+1, what, "")
}

// retryAt saves the chain at pos to try again after a backoff; past the
// deadline, the message is dropped.
func (j *job) retryAt(ctx context.Context, def *Def, pos int, body json.RawMessage, attempts int, lastErr, by string) {
	next := j.w.now().Add(backoff(attempts))
	if !next.Before(j.deadline) {
		reason := "hook unavailable: " + lastErr
		if by != "" {
			reason = "hook " + by + " unavailable: " + lastErr
		}
		j.record(providers.HookDecisionInfo{Hook: nonEmpty(by, hooks.ChannelOwner(j.msg.Channel)), Decision: hooks.ChannelDrop, Reason: reason})
		j.drop(ctx, def, reason, by)
		return
	}
	j.save(ctx, pos, body, attempts, next, lastErr, nil)
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// backoff is the wait before attempt n+1: 1s doubling, capped at a minute.
func backoff(attempts int) time.Duration {
	d := time.Second
	for i := 1; i < attempts && d < time.Minute; i++ {
		d *= 2
	}
	if d > time.Minute {
		d = time.Minute
	}
	return d
}

// save records the chain's progress and gives the lease up, so the message
// is claimed again at next (zero: at once, by whoever claims first).
func (j *job) save(ctx context.Context, pos int, body json.RawMessage, attempts int, next time.Time, lastErr string, journal json.RawMessage) {
	p := store.ChannelHookProgress{ChainPos: pos, Body: body, Journal: journal, Attempts: attempts, NextAttemptAt: next, LastError: lastErr}
	leaseUntil := j.w.now()
	if next.IsZero() {
		// Between hooks: keep the lease, the chain goes on.
		leaseUntil = leaseUntil.Add(j.w.cfg.Lease)
	}
	if _, err := j.w.cfg.Store.ChannelHookSaveProgress(ctx, j.key, j.w.cfg.Owner, p, leaseUntil); err != nil && ctx.Err() == nil {
		log.Printf("channelhooks: %s message %s: save progress: %v", j.msg.Channel, j.msg.ID, err)
	}
	j.progress = p
}

// release delivers the message: into the channel's hold when it is held,
// else no earlier than its publisher's deliver_at.
func (j *job) release(ctx context.Context, def *Def, body json.RawMessage, changed bool) {
	// A zero instant is "now" by the store's clock, the one a publish is
	// stamped with, so the release sorts after any cursor that moved on.
	var to time.Time
	held := def != nil && def.Hold
	switch {
	case held:
		to = store.ChannelHeldVisibleAt()
	case j.msg.RequestedVisibleAt.After(time.Now()):
		to = j.msg.RequestedVisibleAt
	}
	ok, err := j.w.cfg.Store.ChannelReleaseHookHeld(ctx, j.key, bodyOrNil(body, changed), to)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("channelhooks: %s message %s: release: %v", j.msg.Channel, j.msg.ID, err)
		}
		return
	}
	if !ok {
		return // settled elsewhere, expired, trimmed or purged
	}
	switch {
	case held:
	case !to.IsZero() && j.w.cfg.Scheduler != nil:
		j.w.cfg.Scheduler.Schedule(j.msg.Channel, j.msg.ID, to)
	case j.w.cfg.Bus != nil:
		j.w.cfg.Bus.Notify(j.msg.Channel)
	}
}

// drop removes the message — except a Starter's sink message, which is
// delivered as an error result instead: the downstream fan-in counts one per
// run, and a vanished one would leave it waiting. def may be nil when the
// channel's definition could not be read; the error result then goes out
// without its hold, which carries nothing of the dropped output.
func (j *job) drop(ctx context.Context, def *Def, reason, by string) {
	if j.msg.Origin == channels.OriginStarterSink {
		j.release(ctx, def, sinkError(j.msg.Payload, reason), true)
		return
	}
	if _, err := j.w.cfg.Store.ChannelDropHookHeld(ctx, j.key); err != nil && ctx.Err() == nil {
		log.Printf("channelhooks: %s message %s: drop: %v", j.msg.Channel, j.msg.ID, err)
	}
}

// sinkError turns a Starter's sink message into the error result a dropped
// one is delivered as: its envelope, status error, no output.
func sinkError(payload json.RawMessage, reason string) json.RawMessage {
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(payload, &m)
	delete(m, "output")
	m["status"], _ = json.Marshal("error")
	m["error"], _ = json.Marshal("dropped by a channel hook: " + reason)
	out, _ := json.Marshal(m)
	return out
}

// record publishes a decision on DecisionsChannel, in the tenant whose
// definition carries the hooks, and counts it.
func (j *job) record(d providers.HookDecisionInfo) {
	j.w.count(d.Decision)
	if j.w.cfg.Writer == nil {
		return
	}
	d.Phase, d.Channel, d.MessageID = string(hooks.PhaseChannelPublish), j.msg.Channel, j.msg.ID
	payload, err := json.Marshal(d)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := j.w.cfg.Writer.Write(ctx, channels.WriteRequest{
		Channel: DecisionsChannel, TenantID: j.msg.HookTenant, Scope: store.MemoryScopeTenant,
		Payload: payload, ExpiresAt: j.w.now().Add(7 * 24 * time.Hour),
		PublishedBy: channels.SystemPublisherUserID, MaxMessages: 1000,
	}); err != nil && !strings.Contains(err.Error(), "context canceled") {
		log.Printf("channelhooks: record decision on %s: %v", j.msg.Channel, err)
	}
}
