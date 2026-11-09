package auditchain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Appender is the store surface a StoreSink writes through.
type Appender interface {
	AuditAppend(ctx context.Context, rows []store.AuditEventRow, seal store.AuditSealFunc, opened *store.AuditEventRow) ([]store.AuditEventRow, error)
}

// syncAppendTimeout bounds a caller's wait for a state change's append,
// since Sink.Record carries no context: a stalled database must not hold the
// operation being recorded for long. The queue's writer, which no caller
// waits on, allows longer per batch.
const (
	syncAppendTimeout  = 5 * time.Second
	writerBatchTimeout = 10 * time.Second
	// writerBatch is how many queued observations the writer appends per
	// pass, one transaction per tenant among them.
	writerBatch = 64
	// DefaultQueueCapacity is the observation queue's size when StartAsync
	// is given none.
	DefaultQueueCapacity = 4096
	// checkpointRows and checkpointInterval bound what a rollback of a
	// tenant chain can remove undetected: its head is checkpointed into the
	// operator chain once this many rows were written since the last
	// checkpoint, or this long after the first of them, and at a clean
	// shutdown. Constants, like the queue sizes: the cost is one operator-
	// chain row per tenant per interval at most, which no deployment needs
	// to tune.
	checkpointRows     = 100
	checkpointInterval = 5 * time.Minute
)

// ActionEventsDropped is the operator-chain summary of observations the
// queue dropped.
const ActionEventsDropped = "audit_events_dropped"

// eventClass is how the store-backed sink writes an event.
type eventClass int

const (
	// classSync: written before Record returns. Every state change, every
	// high-signal detection, and any action not classified below.
	classSync eventClass = iota
	// classAnonymous: an observation caused by a request nobody has
	// authenticated (a failed sign-in, a refused assertion or OAuth call).
	// Queued apart, so a flood of them cannot push out anyone else's.
	classAnonymous
	// classObserved: an observation caused by an authenticated caller or
	// run (a denial, a lookup, a key used, a read-only git operation).
	classObserved
)

// classify sorts an action. An observation is something refused, denied,
// looked up or read, recorded per request or per tool call and caused by
// whoever sends it; it is queued and written by one writer per queue, so a
// flood cannot hold the database's connections or a chain's head lock
// against the requests behind it, and on overflow it is dropped and
// counted. Everything else is waited for:
//   - state changes (tokens, rules, users, approvals, erasure, sign-ins
//     that succeeded, gate syncs that ran, a git pull or push);
//   - high-signal detections, which must never be dropped however full a
//     queue is: a lockout (auth_login_locked), a replayed authorization
//     code or refresh token (oauth_code_reuse, oauth_refresh_reuse), a
//     session downgrade refused (auth_downgrade_refused);
//   - refused privileged attempts by an authenticated caller
//     (auth_manage_refused, auth_gate_sync_refused): rare, and worth more
//     than a count.
//
// SQL Memory's per-statement records (sql_query, sql_exec) never reach the
// store sink: they are written to the JSONL file only.
func classify(action string) eventClass {
	switch action {
	case "auth_login_failed", "auth_assertion_refused", "oauth_refused", "oauth_token_refused",
		// A stale code or refresh token presented again after its sign-in
		// was already ended: the alarm was written once, these only count.
		"oauth_code_reuse_repeat", "oauth_refresh_reuse_repeat", "oauth_revoke_repeat":
		return classAnonymous
	case "auth_link_refused",
		"acl_denied", "acl_unavailable", "acl_shadow_diverged", "acl_resolution_failed",
		"identity_lookup", "crypto_op", "git_clone", "git_fetch", "git_ls_remote", "git_submodule_update":
		return classObserved
	}
	return classSync
}

// IsObservation reports whether the store-backed sink queues an action
// rather than writing it before Record returns.
func IsObservation(action string) bool { return classify(action) != classSync }

// dropSourcesShown bounds the sources a drop summary names, and
// dropSourcesTracked how many distinct sources a tally counts before it
// folds the rest into "other".
const (
	dropSourcesShown   = 10
	dropSourcesTracked = 256
)

// dropTally counts one chain's dropped observations.
type dropTally struct {
	counts  map[string]int
	sources map[string]int
}

func dropSource(ev audit.Event) string {
	switch {
	case ev.SourceAddr != "":
		return ev.SourceAddr
	case ev.ActorSubject != "" || ev.ActorTenant != "":
		return ev.ActorTenant + "/" + ev.ActorSubject
	}
	return "unknown"
}

// obsQueue is one bounded observation queue with its own writer.
type obsQueue struct {
	ch      chan audit.Event
	done    chan struct{}
	dropped map[string]*dropTally // chain tenant → tally; guarded by StoreSink.mu
	// share caps how many events one chain may have waiting (0: no cap),
	// so one tenant flooding its observations cannot take another's room;
	// pending counts them, guarded by StoreSink.mu.
	share   int
	pending map[string]int
	// outstanding counts the events accepted into the queue and not yet
	// written — queued, or in the writer's batch in flight — guarded by
	// StoreSink.mu: what an operator reading Backlog at a drain timeout
	// needs, with no gap while the writer moves events between the two.
	outstanding int
}

// tenantShareDivisor sets the authenticated queue's per-chain share: one
// chain may hold at most capacity/tenantShareDivisor waiting events.
const tenantShareDivisor = 8

// StoreSink records events into the store-backed chain.
type StoreSink struct {
	store Appender
	keys  ChainKeys
	now   func() time.Time

	mu        sync.Mutex
	anonymous *obsQueue // nil until StartAsync: every event is written at once
	observed  *obsQueue
	closed    bool
	// fallback receives a drop summary the store would not take when there
	// is no later pass to retry it (the queue is draining at shutdown).
	fallback audit.Sink

	// unchecked is, per tenant chain, the newest row this sink wrote that no
	// checkpoint covers yet; guarded by mu. cpRows / cpEvery are the
	// checkpoint policy (checkpointRows / checkpointInterval).
	unchecked map[string]*uncheckedHead
	cpRows    int
	cpEvery   time.Duration
	// tick stops the periodic checkpointer StartAsync runs.
	tick chan struct{}
	// cpMu serializes checkpoint writes, so the newest head is never
	// overtaken by an older one in flight; it is never held with mu.
	cpMu sync.Mutex
}

// uncheckedHead is a tenant chain's newest row written past its last
// checkpoint: where it is, how many rows, and since when.
type uncheckedHead struct {
	seq   int64
	hash  string
	rows  int
	since time.Time
}

// SetFallback names the sink (the JSONL mirror) a final drop summary goes to
// when the store refuses it at shutdown, so the count of what the chain
// lacks is not lost with the process. Call it before StartAsync.
func (s *StoreSink) SetFallback(f audit.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fallback = f
}

// NewStoreSink returns a sink appending to s, signing with keys. It refuses
// without a current key: an unsigned chain would promise an integrity it
// cannot keep against a database writer. Until StartAsync, every event is
// written before Record returns.
func NewStoreSink(s Appender, keys ChainKeys) (*StoreSink, error) {
	if s == nil {
		return nil, errors.New("audit: no store")
	}
	if len(keys.Current) == 0 {
		return nil, errors.New("audit: the store-backed audit stream signs its chain with a key derived from LOOMCYCLE_SECRET_KEY, which is not set")
	}
	return &StoreSink{store: s, keys: keys, now: time.Now,
		unchecked: map[string]*uncheckedHead{}, cpRows: checkpointRows, cpEvery: checkpointInterval}, nil
}

// SetCheckpointPolicy changes how often tenant heads are checkpointed: after
// rows rows, or every interval. For tests; call it before recording.
func (s *StoreSink) SetCheckpointPolicy(rows int, interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cpRows, s.cpEvery = max(rows, 1), interval
}

// StartAsync starts the two observation queues (capacity events each; 0
// takes the default) and their writers. Call Close to drain them.
func (s *StoreSink) StartAsync(capacity int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.anonymous != nil || s.closed {
		return
	}
	if capacity <= 0 {
		capacity = DefaultQueueCapacity
	}
	// The anonymous queue has no per-tenant share, by design: its events are
	// caused by requests nobody authenticated, so there is no tenant whose
	// room to protect. Most land on the operator chain; those that name the
	// tenant they concern (oauth_token_refused, the oauth_*_repeat actions)
	// go to that tenant's chain. Authenticated events are shared per chain.
	s.anonymous = s.startQueue(capacity, 0)
	s.observed = s.startQueue(capacity, max(capacity/tenantShareDivisor, 1))
	s.tick = make(chan struct{})
	go s.checkpointer(s.tick, s.cpEvery)
}

// checkpointer checkpoints, every interval, each chain with rows no
// checkpoint covers, so a quiet tenant's last rows are not left uncovered
// until the next burst.
func (s *StoreSink) checkpointer(stop <-chan struct{}, every time.Duration) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), writerBatchTimeout)
			s.checkpointAll(ctx, false)
			cancel()
		}
	}
}

// noteWritten records rows appended to a tenant chain and reports whether
// its head is due a checkpoint.
func (s *StoreSink) noteWritten(tenant string, rows []store.AuditEventRow) bool {
	if tenant == "" || len(rows) == 0 {
		return false
	}
	last := rows[len(rows)-1]
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.unchecked[tenant]
	if u == nil {
		u = &uncheckedHead{since: s.now()}
		s.unchecked[tenant] = u
	}
	if last.Seq > u.seq {
		u.seq, u.hash = last.Seq, last.Hash
	}
	u.rows += len(rows)
	return u.rows >= s.cpRows || (s.cpEvery > 0 && s.now().Sub(u.since) >= s.cpEvery)
}

// checkpointAll checkpoints every chain with uncovered rows (force), or
// those due (!force). A failed write is logged and the chain stays uncovered
// for the next pass.
func (s *StoreSink) checkpointAll(ctx context.Context, force bool) {
	s.mu.Lock()
	var due []string
	for t, u := range s.unchecked {
		if force || u.rows >= s.cpRows || (s.cpEvery > 0 && s.now().Sub(u.since) >= s.cpEvery) {
			due = append(due, t)
		}
	}
	s.mu.Unlock()
	sort.Strings(due)
	for _, t := range due {
		if err := s.checkpoint(ctx, t); err != nil {
			slog.Error("audit: checkpointing a chain's head failed; retried on the next pass", "tenant", t, "err", err)
		}
	}
}

// checkpoint writes tenant's newest uncovered head to the operator chain.
// It claims the head under mu and puts it back when the write fails, so a
// concurrent checkpoint never writes the same head twice and a failure is
// retried.
func (s *StoreSink) checkpoint(ctx context.Context, tenant string) error {
	s.cpMu.Lock()
	defer s.cpMu.Unlock()
	s.mu.Lock()
	u := s.unchecked[tenant]
	delete(s.unchecked, tenant)
	s.mu.Unlock()
	if u == nil {
		return nil
	}
	ev := audit.Event{TS: s.now().UTC(), Action: ActionChainCheckpoint, TargetTenant: tenant,
		CheckpointSeq: u.seq, CheckpointHash: u.hash,
		CheckpointHeadMAC: s.keys.sign(macHead, tenant, u.seq, u.hash)}
	payload, err := json.Marshal(ev)
	if err == nil {
		row := RowFor(ev, payload)
		row.TenantID, row.Subject = "", SafeText(tenant)
		_, err = s.store.AuditAppend(ctx, []store.AuditEventRow{row}, s.keys.Seal(), nil)
	}
	if err != nil {
		s.mu.Lock()
		if cur := s.unchecked[tenant]; cur == nil {
			s.unchecked[tenant] = u
		} else {
			// Rows written meanwhile: keep the newer head, count both.
			cur.rows += u.rows
			if u.since.Before(cur.since) {
				cur.since = u.since
			}
		}
		s.mu.Unlock()
		return fmt.Errorf("audit: checkpoint: %w", err)
	}
	return nil
}

func (s *StoreSink) startQueue(capacity, share int) *obsQueue {
	q := &obsQueue{ch: make(chan audit.Event, capacity), done: make(chan struct{}),
		dropped: map[string]*dropTally{}, share: share, pending: map[string]int{}}
	go s.writer(q)
	return q
}

// Backlog reports what the queues still hold — events waiting to be written,
// queued or in the writer's batch in flight, and dropped events per action
// not yet in a summary row — for a shutdown
// whose drain ran out of time to log, so the tally is not lost.
func (s *StoreSink) Backlog() (pending int, dropped map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dropped = map[string]int{}
	for _, q := range []*obsQueue{s.anonymous, s.observed} {
		if q == nil {
			continue
		}
		pending += q.outstanding
		for _, t := range q.dropped {
			for a, n := range t.counts {
				dropped[a] += n
			}
		}
	}
	return pending, dropped
}

// Close stops queueing (later observations are written at once), waits
// until the writers have written what was queued, and the drop summaries,
// or ctx ends, and then checkpoints every chain it wrote past its last
// checkpoint, so a clean shutdown leaves nothing a rollback could remove
// undetected.
func (s *StoreSink) Close(ctx context.Context) error {
	s.mu.Lock()
	queues := []*obsQueue{s.anonymous, s.observed}
	if s.anonymous != nil && !s.closed {
		for _, q := range queues {
			close(q.ch)
		}
		close(s.tick)
	}
	s.closed = true
	s.mu.Unlock()
	var drainErr error
	if queues[0] != nil {
	drain:
		for _, q := range queues {
			select {
			case <-q.done:
			case <-ctx.Done():
				drainErr = ctx.Err()
				break drain
			}
		}
	}
	// The checkpoints run even when the drain ran out of time: they are a
	// few rows, and skipping them would leave every chain's last rows open
	// to a rollback. A fresh, short budget, since ctx may be spent.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownCheckpointTimeout)
	defer cancel()
	s.checkpointAll(cctx, true)
	return drainErr
}

// shutdownCheckpointTimeout bounds the checkpoints Close writes.
const shutdownCheckpointTimeout = 3 * time.Second

// Record appends ev to its tenant's chain: a state change or high-signal
// event before it returns, an observation through its queue (never waited
// for; dropped and counted when the queue is full).
func (s *StoreSink) Record(ev audit.Event) error {
	if ev.TS.IsZero() {
		ev.TS = s.now().UTC()
	}
	if c := classify(ev.Action); c != classSync && s.enqueue(c, ev) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), syncAppendTimeout)
	defer cancel()
	return s.appendEvents(ctx, []audit.Event{ev})
}

// enqueue queues an observation, or counts it dropped when its queue is
// full. It reports false when the queues are not running, and the caller
// writes the event itself.
func (s *StoreSink) enqueue(c eventClass, ev audit.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.anonymous == nil || s.closed {
		return false
	}
	q := s.observed
	if c == classAnonymous {
		q = s.anonymous
	}
	tenant := ChainTenant(ev)
	if q.share > 0 && q.pending[tenant] >= q.share {
		s.countDropLocked(q, ev)
		return true
	}
	select {
	case q.ch <- ev:
		q.pending[tenant]++
		q.outstanding++
	default:
		s.countDropLocked(q, ev)
	}
	return true
}

func (s *StoreSink) countDropLocked(q *obsQueue, ev audit.Event) {
	tenant := ChainTenant(ev)
	t := q.dropped[tenant]
	if t == nil {
		t = &dropTally{counts: map[string]int{}, sources: map[string]int{}}
		q.dropped[tenant] = t
	}
	t.counts[ev.Action]++
	src := dropSource(ev)
	if _, seen := t.sources[src]; !seen && len(t.sources) >= dropSourcesTracked {
		src = "other"
	}
	t.sources[src]++
}

func (s *StoreSink) writer(q *obsQueue) {
	defer close(q.done)
	for ev := range q.ch {
		batch := []audit.Event{ev}
	drain:
		for len(batch) < writerBatch {
			select {
			case e, ok := <-q.ch:
				if !ok {
					break drain
				}
				batch = append(batch, e)
			default:
				break drain
			}
		}
		s.writeBatch(q, batch)
		s.mu.Lock()
		q.outstanding -= len(batch)
		s.mu.Unlock()
		s.writeDropSummaries(q, false)
	}
	s.writeDropSummaries(q, true)
}

// writeBatch appends a batch of observations, one transaction per tenant in
// the order each tenant first appears, each tenant's events in queue order.
// A failed append counts its events as dropped.
func (s *StoreSink) writeBatch(q *obsQueue, batch []audit.Event) {
	s.mu.Lock()
	for _, ev := range batch {
		if t := ChainTenant(ev); q.pending[t] > 1 {
			q.pending[t]--
		} else {
			delete(q.pending, t)
		}
	}
	s.mu.Unlock()
	byTenant := map[string][]audit.Event{}
	var order []string
	for _, ev := range batch {
		t := SafeText(ChainTenant(ev))
		if _, seen := byTenant[t]; !seen {
			order = append(order, t)
		}
		byTenant[t] = append(byTenant[t], ev)
	}
	for _, t := range order {
		ctx, cancel := context.WithTimeout(context.Background(), writerBatchTimeout)
		err := s.appendEvents(ctx, byTenant[t])
		cancel()
		if err != nil {
			slog.Error("audit: writing queued observations failed; counted as dropped", "tenant", t, "events", len(byTenant[t]), "err", err)
			s.mu.Lock()
			for _, ev := range byTenant[t] {
				s.countDropLocked(q, ev)
			}
			s.mu.Unlock()
		}
	}
}

// writeDropSummaries writes one summary row per chain that lost
// observations — into that chain, the operator chain for events with no
// tenant — with the counts per action and the top sources, and keeps a
// tally counted if its row cannot be written.
func (s *StoreSink) writeDropSummaries(q *obsQueue, final bool) {
	s.mu.Lock()
	if len(q.dropped) == 0 {
		s.mu.Unlock()
		return
	}
	tallies := q.dropped
	q.dropped = map[string]*dropTally{}
	s.mu.Unlock()
	for tenant, t := range tallies {
		ev := audit.Event{TS: s.now().UTC(), Action: ActionEventsDropped, TargetTenant: tenant,
			DroppedCounts: t.counts, DroppedSources: topSources(t.sources, dropSourcesShown)}
		ctx, cancel := context.WithTimeout(context.Background(), writerBatchTimeout)
		err := s.appendEvents(ctx, []audit.Event{ev})
		cancel()
		if err != nil && final {
			// No later pass will retry it: hand it to the mirror, or at least
			// the process log, rather than lose what the chain lacks.
			s.mu.Lock()
			fallback := s.fallback
			s.mu.Unlock()
			if fallback == nil || fallback.Record(ev) != nil {
				slog.Error("audit: the final dropped-observations summary could not be written",
					"tenant", tenant, "dropped", t.counts, "err", err)
			}
			continue
		}
		if err != nil {
			slog.Error("audit: writing a dropped-observations summary failed", "tenant", tenant, "err", err)
			s.mu.Lock()
			keep := q.dropped[tenant]
			if keep == nil {
				q.dropped[tenant] = t
			} else {
				for a, n := range t.counts {
					keep.counts[a] += n
				}
				for src, n := range t.sources {
					keep.sources[src] += n
				}
			}
			s.mu.Unlock()
		}
	}
}

// topSources keeps the n sources with the most events (ties by name).
func topSources(sources map[string]int, n int) map[string]int {
	keys := make([]string, 0, len(sources))
	for k := range sources {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if sources[keys[i]] != sources[keys[j]] {
			return sources[keys[i]] > sources[keys[j]]
		}
		return keys[i] < keys[j]
	})
	out := map[string]int{}
	for _, k := range keys[:min(n, len(keys))] {
		out[k] = sources[k]
	}
	return out
}

// appendEvents appends events of one tenant's chain in one transaction.
func (s *StoreSink) appendEvents(ctx context.Context, evs []audit.Event) error {
	rows := make([]store.AuditEventRow, 0, len(evs))
	for _, ev := range evs {
		payload, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("audit: marshal: %w", err)
		}
		rows = append(rows, RowFor(ev, payload))
	}
	var opened *store.AuditEventRow
	if rows[0].TenantID != "" {
		o, err := openedRow(rows[0].TenantID, evs[0].TS)
		if err != nil {
			return err
		}
		opened = &o
	}
	written, err := s.store.AuditAppend(ctx, rows, s.keys.Seal(), opened)
	if err != nil {
		return fmt.Errorf("audit: append: %w", err)
	}
	if s.noteWritten(rows[0].TenantID, written) {
		// The event is recorded; a checkpoint that fails is retried later
		// and never fails the caller.
		if err := s.checkpoint(ctx, rows[0].TenantID); err != nil {
			slog.Error("audit: checkpointing a chain's head failed; retried later", "tenant", rows[0].TenantID, "err", err)
		}
	}
	return nil
}

// openedRow is the operator-chain record that tenant's chain exists.
func openedRow(tenant string, ts time.Time) (store.AuditEventRow, error) {
	ev := audit.Event{TS: ts, Action: ActionChainOpened, TargetTenant: tenant}
	payload, err := json.Marshal(ev)
	if err != nil {
		return store.AuditEventRow{}, fmt.Errorf("audit: marshal: %w", err)
	}
	row := RowFor(ev, payload)
	row.TenantID, row.Subject = "", tenant
	return row, nil
}
